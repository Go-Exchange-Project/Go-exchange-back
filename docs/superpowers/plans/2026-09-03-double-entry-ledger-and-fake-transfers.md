# 복식부기 원장과 가짜 은행·가짜 코인 입출금 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 모든 자산 이동을 빠진 곳·들어간 곳 한 쌍으로 기록하는 복식부기 원장으로 교체하고, 실제 서비스와 같은 요청→확인→잔액 반영 과정을 재현하는 가짜 은행·가짜 코인 입출금을 추가한다.

**Architecture:** 잔액을 별도로 저장하지 않는다 — 계정 잔액은 전기(posting)의 합이고 `account_balances`는 같은 트랜잭션에서 갱신되는 캐시일 뿐이다. `postings`에 직접 INSERT하는 코드는 `LedgerService` 하나뿐이며, 입출금 확정은 알림·조회가 공유하는 단일 경로(`ResolveTransfer`)가 행 잠금 아래에서 한 번만 수행한다. 기존 `wallets`·`ledger_entries`는 옮기지 않고 버린다.

**Tech Stack:** Go 1.x + GORM + goose SQL migration + PostgreSQL, gin HTTP, testify. 프런트는 Vite + React + Playwright.

**설계 기준 문서:** [2026-09-02-double-entry-ledger-and-fake-transfers-design.md](../specs/2026-09-02-double-entry-ledger-and-fake-transfers-design.md) — **기준 SHA `b7aaa84`**. 이 계획의 모든 §참조는 그 SHA의 절 번호다.

**조사 기준 SHA:** 아래 파일·줄 번호는 전부 `b7aaa84` 시점에 직접 읽어 확인했다.

---

## Global Constraints

프로젝트 전체에 걸리는 규칙이다. 모든 Task의 요구사항에 암묵적으로 포함된다.

- **거래 수수료율은 `0.0005`(0.05%)이고 이번 작업에서 바꾸지 않는다.** 바뀌는 것은 수수료가 가는 곳뿐이다(사라짐 → `FEE_INCOME`). 출처: [config/market_rules.json:3](../../../config/market_rules.json), [internal/service/market_rules_registry.go:55](../../../internal/service/market_rules_registry.go).
- **입출금 수수료는 0이다.** `fee_amount`·`fee_asset` 필드는 만들되 값은 0이고, **금액 0짜리 `FEE_INCOME` 전기를 만들지 않는다**(설계 §14 D1).
- **`postings` INSERT는 `LedgerService`만 한다.** 다른 어떤 서비스도 `postings`·`journal_entries`·`account_balances`에 직접 쓰지 않는다(설계 §13.1).
- **`journal_entries`·`postings`에 UPDATE·DELETE를 하지 않는다.** 정정은 역분개뿐이다(설계 §4.3).
- **중복은 `INSERT ... ON CONFLICT (…) DO NOTHING RETURNING`의 반환 없음으로 판정한다.** 유니크 위반을 일으켜 잡지 않는다 — PostgreSQL에서 그 위반은 트랜잭션을 abort시킨다(설계 §7·§8.8).
- **시간이 돈을 움직이지 않는다.** 경과 시간 임계값은 `review_required_at`을 켜는 데만 쓴다(설계 §8.4·§8.6).
- **옛 데이터를 옮기는 코드, 두 방식 동시 기록, 임시 호환 경로를 만들지 않는다.** 개발 DB는 비우고 새 스키마로 다시 만든다(설계 §10).
- **변이 테스트를 만들지 않는다.** 같은 계약을 여러 계층에서 반복 검증하지 않는다.
- **전체 `go test ./...` / `-race` / `vet` / `build`와 프런트 `test`·`lint`·`build`는 Task 7에서 각각 1회만 실행한다.** Task 1~6은 각자의 focused test만 1회 실행한다.
- **GCP 측정을 포함하지 않는다.**
- 통합 테스트는 `GOEXCHANGE_TEST_DATABASE_DSN`이 설정돼야 돈다([internal/testdb/integration.go:16](../../../internal/testdb/integration.go)). 설정되지 않으면 `t.Skip`이므로, 이 계획의 모든 통합 테스트 실행 전에 DSN을 export한다.

---

## 조사 결과 — 손대야 하는 실제 위치

계획을 세우기 전에 요구된 6개 영역을 직접 확인했다.

### A. `wallets`·`ledger_entries`를 읽고 쓰는 프로덕션 파일 (테스트 제외 18개)

| 파일 | 역할 |
|---|---|
| [internal/model/wallet.go](../../../internal/model/wallet.go) | `Wallet` 모델. 레거시 거울 필드 `KRW`·`Quantity` 포함 |
| [internal/model/ledger_entry.go](../../../internal/model/ledger_entry.go) | 단식 `LedgerEntry` 모델 |
| [internal/repository/wallet_reporsitory.go](../../../internal/repository/wallet_reporsitory.go) | 311줄. 잠금·배치 갱신 전부 |
| [internal/repository/ledger_repository.go](../../../internal/repository/ledger_repository.go) | 40줄. `Create`/`CreateMany`/`ListByUserID` |
| [internal/service/balance.go](../../../internal/service/balance.go) | 213줄. hold·release·settle 산술 8개 + `walletAvailableBalance` 0-fallback |
| [internal/service/ledger.go](../../../internal/service/ledger.go) | 44줄. `ledgerEntryFromWalletUpdate`·`devFundLedgerEntry` |
| [internal/service/hold_coordinator.go](../../../internal/service/hold_coordinator.go) | 577줄. 배치 hold 트랜잭션 |
| [internal/service/order_service.go](../../../internal/service/order_service.go) | 1014줄. release 경로 6곳 |
| [internal/service/settlement_service.go](../../../internal/service/settlement_service.go) | 436줄. 단건 정산 |
| [internal/service/settlement_batch.go](../../../internal/service/settlement_batch.go) | 417줄. **배치 정산 — 설계 §1.5가 조사하지 않은 두 번째 정산 경로** |
| [internal/service/settlement_retry_worker.go](../../../internal/service/settlement_retry_worker.go) | 재시도 시 수수료 재계산 |
| [internal/service/dev_wallet_service.go](../../../internal/service/dev_wallet_service.go) | 103줄. 지갑 직접 upsert |
| [internal/service/fee.go](../../../internal/service/fee.go) | 수수료 산술 |
| [internal/service/failed_settlement_service.go](../../../internal/service/failed_settlement_service.go) | 실패 기록 |
| [internal/repository/reconciliation_repository.go](../../../internal/repository/reconciliation_repository.go) | 137줄. 보정 포함 검산 쿼리 |
| [internal/service/reconciliation_worker.go](../../../internal/service/reconciliation_worker.go) | 227줄. 검사 3종 |
| [internal/handler/order_handler.go](../../../internal/handler/order_handler.go) | `ListWallets` + `WalletResponse` |
| [cmd/main.go](../../../cmd/main.go) | AutoMigrate 목록·라우트·worker 배선 |

### B. `HoldCoordinator`와 주문 해제 경로

- 잠금: [hold_coordinator.go:127](../../../internal/service/hold_coordinator.go) `HoldBatch` — 한 트랜잭션에서 멱등성 키 선점 → `FindByKeys` → **ID 오름차순 `LockByIDs`** → 순차 fold 검증 → 주문 배치 INSERT → `BatchUpdateBalances` → `ledgerRepo.CreateMany`(265행).
- 단건 폴백: [hold_coordinator.go:502](../../../internal/service/hold_coordinator.go) `persistAndHoldIdempotent` → [order_service.go:851](../../../internal/service/order_service.go) `holdOrderAssets`.
- **해제 호출 지점 6곳** — 전부 `ledgerEntryFromWalletUpdate(..., LedgerEntryTypeOrderRelease, ...)`를 만든다:

| 위치 | 상황 |
|---|---|
| [order_service.go:410](../../../internal/service/order_service.go) `releaseInitialHold` (421·435행) | 접수 직후 실패 롤백 |
| [order_service.go:896](../../../internal/service/order_service.go) `releaseOrderHold` (907·924행) | 사용자 취소·엔진 취소 확정 |
| [order_service.go:615](../../../internal/service/order_service.go) `completeMarketBuyOrder` (631행) | 시장가 매수 잔여 예산 반환 |
| [order_service.go:649](../../../internal/service/order_service.go) `completeMarketSellOrder` (659행) | 시장가 매도 잔여 수량 반환 |

### C. `SettlementService`와 수수료

- **정산 경로가 둘이다.** [settlement_service.go:96](../../../internal/service/settlement_service.go) `SettleTrade`(전기 4줄, 191행) 와 [settlement_batch.go:31](../../../internal/service/settlement_batch.go) `SettleTradeBatch`(전기 N×4줄, 328행). 배치는 "같은 헬퍼를 재사용해 SettleTrade를 N회 실행한 결과와 정확히 같아야 한다"는 등가성 불변식을 주석으로 명시한다(settlement_batch.go:27-30). **둘 다 전환해야 하고, 등가성도 유지해야 한다.**
- 수수료: [fee.go:23-27](../../../internal/service/fee.go) `applyTradeFeePolicy`가 `trade.BuyerFee`·`SellerFee`를 `executionQuote × 0.0005`로 채운다. 양쪽 KRW. 받는 계정이 없다.
- 산술: `executionDebit = executionQuote + BuyerFee`([settlement_service.go:147](../../../internal/service/settlement_service.go)), `sellerQuoteNet = executionQuote − SellerFee`(148행).
- 지갑 잠금: [settlement_service.go:228](../../../internal/service/settlement_service.go) `lockSettlementWallets` — 2단계(ID 확보 → ID 오름차순 `FOR UPDATE`).

### D. 잔액 API 응답과 프런트 소비 필드 — **좋은 소식**

[order_handler.go:272](../../../internal/handler/order_handler.go) `WalletResponse`:

```
id, coin_symbol, available_balance, locked_balance, total_balance, avg_buy_price
```

프런트 [src/lib/api.ts:20](../../../../Go-exchange-front/src/lib/api.ts) `interface Wallet`가 **정확히 같은 6개 필드**를 소비하고, 그 외 필드를 쓰지 않는다. `krw`·`quantity` 레거시 필드는 **응답에 이미 없다.**

→ **원장 전환만으로는 프런트를 고칠 필요가 없다.** 설계 §13.5의 위험은 실제로는 없다. 프런트 변경은 입출금 화면(신규)뿐이다.

### E. 개발용 지급 API

- 라우트: [cmd/main.go:383](../../../cmd/main.go) `dev.POST("/wallets/fund", devHandler.FundWallet)`, dev-tools 미들웨어 뒤.
- 핸들러: [internal/handler/dev_handler.go](../../../internal/handler/dev_handler.go) — 요청 본문은 `{coin_symbol, amount}`.
- **요청 키가 없다.** [dev_wallet_service.go:27](../../../internal/service/dev_wallet_service.go) `FundWallet`은 멱등하지 않다. 같은 요청을 두 번 보내면 두 번 지급된다. 설계 §5.1이 요구하는 "같은 요청 번호로 재시도하면 한 번만 지급"을 지키려면 **요청 본문에 키를 추가해야 한다**(Task 3에서 처리, 보고 항목 4번).
- 프런트 호출: [src/lib/api.ts:237](../../../../Go-exchange-front/src/lib/api.ts) `fundWallet(token, {coin_symbol, amount})`.

### F. main lifecycle에서 worker를 시작·종료하는 위치

- 시작: [cmd/main.go:251-268](../../../cmd/main.go) — `backgroundCtx, cancelBackground := context.WithCancel(...)` 직후 `go settlementRetryWorker.Run(backgroundCtx)`(262행), `go reconciliationWorker.Run(backgroundCtx)`(268행).
- 종료: 252행 `defer cancelBackground()`. 별도 drain 대기가 없는 백그라운드 worker 계열이다.
- **`TransferStatusPoller`는 이 자리에 같은 방식으로 붙인다**(Task 6). 자산을 잠그지 않고 조회만 하므로 `settlementRetryWorker`와 같은 등급이며, 매칭 엔진 종료 체인(386-442행)에는 넣지 않는다.
- 스키마 배선: [cmd/main.go:55-71](../../../cmd/main.go) `config.DB.AutoMigrate(...)` 목록 + `dbmigration.Up(config.DB)`. goose SQL 마이그레이션은 [migrations/](../../../migrations/)에 `001`~`008`까지 있고 다음 번호는 **`009`**다. 통합 테스트도 같은 목록을 쓴다([internal/testdb/integration.go:31](../../../internal/testdb/integration.go)).

---

## File Structure

| 파일 | 책임 |
|---|---|
| `migrations/009_double_entry_ledger.sql` | 7개 표의 CHECK·UNIQUE·부분 인덱스. GORM이 표현 못 하는 것만 |
| `internal/model/account.go` | `Account`, `AccountBalance` + 계정 종류 상수 |
| `internal/model/journal.go` | `JournalEntry`, `Posting` + 사건·참조 종류 상수 |
| `internal/model/transfer.go` | `TransferRequest`, `TransferStatusEvent` + 상태·결과 상수 |
| `internal/model/user_asset_stat.go` | `UserAssetStat` — 평균 매수가. **원장이 아니라 통계다**(설계 §10) |
| `internal/repository/account_repository.go` | 계정 확보·잔액 캐시 잠금·잔액 조회 |
| `internal/repository/journal_repository.go` | 분개·전기 INSERT(멱등), 잔액 델타 적용 |
| `internal/repository/transfer_repository.go` | 요청 행 잠금, 사건 INSERT(멱등), 상태 갱신, 조회 대상 선별 |
| `internal/repository/ledger_reconciliation_repository.go` | 검산 4종 쿼리 |
| `internal/service/ledger_service.go` | **분개를 만드는 유일한 곳.** 합계 0 검증·멱등·잔액 캐시·역분개 |
| `internal/service/transfer_service.go` | `ResolveTransfer`(terminal 전용) / `RecordObservation`(미확정 전용) / 입출금 접수 |
| `internal/service/fake_transfer_processor.go` | 가짜 은행·체인. 알림 + `GetTransferStatus` 창구 |
| `internal/service/transfer_status_poller.go` | 간격 확대 조회 worker |
| `internal/handler/transfer_handler.go` | 입출금 HTTP |
| `Go-exchange-front/src/pages/Assets.tsx` | 자산 전용 페이지(`/assets`). 입출금은 여기서만 한다 |

**삭제되는 파일:** `internal/model/wallet.go`, `internal/model/ledger_entry.go`, `internal/repository/wallet_reporsitory.go`, `internal/repository/ledger_repository.go`, `internal/service/ledger.go` (Task 5).

---

## 전환 구간에 대한 경고 — 반드시 먼저 읽는다

**Task 3 시작부터 Task 5 끝까지, 기존 주문·체결 통합 테스트는 빨간 상태다.** 이것은 결함이 아니라 계획된 구간이다.

이유: 원장 전환은 **쓰기 경로 전체를 한 번에 바꿔야 한다.** 개발용 지급만 원장으로 옮기고 주문은 아직 `wallets`에 쓰면, 지급한 돈이 주문에 보이지 않는다. 두 방식을 함께 굴려 그 창을 메우는 것이 바로 금지된 "동시 기록·임시 호환 경로"다. 그래서 창을 메우지 않고 **빠르게 통과한다.**

- Task 3·4는 각자의 focused test만 통과시키면 된다. 다른 통합 스위트를 실행하지 않는다.
- **Task 5 끝에서 `internal/service` 통합 스위트 전체가 다시 녹색이 되어야 한다.** 그것이 전환 완료의 판정이다.
- **Task 3 시작부터 Task 5의 스위트가 녹색이 될 때까지 커밋도 푸시도 하지 않는다.** 빨간 상태를 히스토리에 남기면 그 SHA로 되돌아갈 수 없고, CI가 도는 브랜치에 올리면 남의 작업까지 막는다. 전환 전체가 한 커밋이다.
- **빨간 전환 상태를 체크포인트로 보고하지 않는다.** 검토는 창이 열리기 전(CP1, Task 2 뒤)과 닫힌 뒤(CP2, Task 5 뒤)에만 한다.

---

## Task 1: 7개 표 스키마·모델·리포지토리

**Files:**
- Create: `migrations/009_double_entry_ledger.sql`
- Create: `internal/model/account.go`, `internal/model/journal.go`, `internal/model/transfer.go`, `internal/model/user_asset_stat.go`
- Create: `internal/repository/account_repository.go`, `internal/repository/journal_repository.go`, `internal/repository/transfer_repository.go`
- Modify: `cmd/main.go:55-66` (AutoMigrate 목록에 6개 모델 추가), `internal/testdb/integration.go:31` (같은 목록)
- Test: `internal/dbmigration/ledger_schema_integration_test.go`

**Interfaces:**
- Consumes: 없음(토대)
- Produces:
  ```go
  // internal/model/account.go
  type AccountType string
  const (
      AccountUserAvailable AccountType = "USER_AVAILABLE"
      AccountUserLocked    AccountType = "USER_LOCKED"
      AccountFeeIncome     AccountType = "FEE_INCOME"
      AccountExternalBank  AccountType = "EXTERNAL_BANK"
      AccountExternalChain AccountType = "EXTERNAL_CHAIN"
      AccountDevMint       AccountType = "DEV_MINT"
  )
  type Account struct {
      ID uint; AccountType AccountType; OwnerUserID *uint
      Asset string; AllowsNegative bool; CreatedAt time.Time
  }
  type AccountBalance struct {
      AccountID uint; Balance decimal.Decimal
      LastPostingID uint64; UpdatedAt time.Time
  }

  // internal/model/journal.go
  type JournalEventType string   // DEPOSIT WITHDRAWAL ORDER_HOLD ORDER_RELEASE TRADE DEV_FUND REVERSAL
  type JournalReferenceType string // ORDER TRADE TRANSFER DEV_FUND
  type JournalEntry struct {
      ID uint; EventType JournalEventType; IdempotencyKey string
      ReferenceType JournalReferenceType; ReferenceID uint
      ReversesJournalID *uint; CreatedAt time.Time
  }
  type Posting struct {
      ID uint64; JournalID uint; AccountID uint
      Asset string; Amount decimal.Decimal; CreatedAt time.Time
  }

  // internal/model/transfer.go
  type TransferDirection string // DEPOSIT WITHDRAWAL
  type TransferRail string      // BANK CHAIN
  type TransferStatus string    // RECEIVED PROCESSING COMPLETED FAILED
  type TransferOutcome string   // SUCCESS FAILURE PENDING UNKNOWN
  type TransferEventSource string // CALLBACK POLL
  type TransferRequest struct {
      ID uint; UserID uint; Direction TransferDirection; Rail TransferRail
      Asset string; Amount, FeeAmount decimal.Decimal; FeeAsset string
      Status TransferStatus; ClientRequestKey, ExternalRef string
      ResolutionJournalID, HoldJournalID *uint
      LastCheckedAt, NextCheckAt *time.Time; CheckAttempts int
      ReviewRequiredAt *time.Time; ReviewReason string
      FailureReason string; CreatedAt, UpdatedAt time.Time
  }
  type TransferStatusEvent struct {
      ID uint64; TransferRequestID uint; Source TransferEventSource
      EventKey string; Outcome TransferOutcome
      Payload datatypes.JSON; ReceivedAt time.Time
  }

  // internal/model/user_asset_stat.go
  // 평균 매수가는 자산이 아니라 통계이므로 원장에 넣지 않는다(설계 §10).
  // 표는 Task 1에서 만들고, 값을 채우는 것은 Task 4의 매수 정산 1곳뿐이다.
  type UserAssetStat struct {
      ID uint; UserID uint; Asset string
      AvgBuyPrice decimal.Decimal
      UpdatedAt time.Time
  } // UNIQUE (user_id, asset)

  // internal/repository/account_repository.go
  type AccountSpec struct { AccountType model.AccountType; OwnerUserID *uint; Asset string }
  func NewAccountRepository(db *gorm.DB) *AccountRepository
  func (r *AccountRepository) WithTx(tx *gorm.DB) *AccountRepository
  func (r *AccountRepository) EnsureAccounts(specs []AccountSpec) ([]model.Account, error)
  func (r *AccountRepository) LockBalances(accountIDs []uint) ([]model.AccountBalance, error) // ID 오름차순 FOR UPDATE
  // 자산별로 USER_AVAILABLE·USER_LOCKED 잔액을 합치고 user_asset_stats를 LEFT JOIN한다.
  // 통계 행이 없으면 AvgBuyPrice는 0이다 — 매수한 적 없는 자산이 정상적으로 그렇다.
  // AvailableAccountID는 WalletResponse.id로 그대로 나가 기존 6필드 계약을 유지한다.
  func (r *AccountRepository) ListUserBalances(userID uint) ([]UserAssetBalance, error)
  type UserAssetBalance struct {
      AvailableAccountID uint
      Asset              string
      Available, Locked  decimal.Decimal
      AvgBuyPrice        decimal.Decimal
  }

  // internal/repository/journal_repository.go
  func NewJournalRepository(db *gorm.DB) *JournalRepository
  func (r *JournalRepository) WithTx(tx *gorm.DB) *JournalRepository
  // ON CONFLICT (idempotency_key) DO NOTHING RETURNING id. created=false면 기존 분개를 조회해 반환한다.
  func (r *JournalRepository) InsertOrGet(entry *model.JournalEntry) (created bool, err error)
  func (r *JournalRepository) CreatePostings(postings []model.Posting) error
  func (r *JournalRepository) ApplyBalanceDeltas(deltas []BalanceDelta) error
  type BalanceDelta struct { AccountID uint; Delta decimal.Decimal; LastPostingID uint64 }

  // internal/repository/transfer_repository.go
  func NewTransferRepository(db *gorm.DB) *TransferRepository
  func (r *TransferRepository) WithTx(tx *gorm.DB) *TransferRepository
  func (r *TransferRepository) LockByID(id uint) (*model.TransferRequest, error) // SELECT ... FOR UPDATE
  func (r *TransferRepository) FindByExternalRef(ref string) (*model.TransferRequest, error)
  // 접수 멱등의 근거. ON CONFLICT (user_id, client_request_key) DO NOTHING RETURNING.
  // created=false면 기존 요청을 조회해 반환한다 — SQL 오류를 일으켜 잡지 않는다.
  // 본문이 다른지 판단하는 것은 호출자 몫이다(TransferService가 409로 바꾼다).
  func (r *TransferRepository) InsertOrGetByUserRequestKey(req *model.TransferRequest) (created bool, existing *model.TransferRequest, err error)
  func (r *TransferRepository) SetHoldJournal(id uint, journalID uint) error
  func (r *TransferRepository) SetDispatched(id uint, externalRef string) error // RECEIVED → PROCESSING
  // ON CONFLICT (event_key) DO NOTHING RETURNING id
  func (r *TransferRepository) InsertEventIfAbsent(event *model.TransferStatusEvent) (created bool, err error)
  func (r *TransferRepository) DueForCheck(now time.Time, limit int) ([]model.TransferRequest, error)
  ```

- [ ] **Step 1: `migrations/009_double_entry_ledger.sql` 작성**

  goose 형식(`-- +goose Up`, 여러 문장을 묶을 때는 `-- +goose StatementBegin`)을 따른다. 기존 [migrations/008_order_idempotency_keys.sql](../../../migrations/008_order_idempotency_keys.sql)이 참고 형식이다. AutoMigrate가 만드는 표에 **GORM이 표현하지 못하는 것만** 여기서 건다:

  | 대상 | 제약 |
  |---|---|
  | `accounts` | `UNIQUE (account_type, COALESCE(owner_user_id, 0), asset)` 표현식 인덱스 / `CHECK account_type IN (6종)` / `CHECK` 사용자 계정 ⟺ `owner_user_id IS NOT NULL` |
  | `journal_entries` | `UNIQUE (idempotency_key)` / `UNIQUE (reverses_journal_id)` / `CHECK event_type='REVERSAL' ⟺ reverses_journal_id IS NOT NULL` |
  | `postings` | `CHECK (amount <> 0)` / `INDEX (account_id, id)` / `INDEX (journal_id)` |
  | `transfer_requests` | `CHECK direction IN ('DEPOSIT','WITHDRAWAL')` / `CHECK rail IN ('BANK','CHAIN')` / `CHECK status IN ('RECEIVED','PROCESSING','COMPLETED','FAILED')` / `CHECK rail='BANK' → asset='KRW'` / `CHECK amount > 0` / `CHECK fee_amount >= 0` / `CHECK direction='DEPOSIT' → hold_journal_id IS NULL` / `CHECK status='COMPLETED' → resolution_journal_id IS NOT NULL` / `CHECK status IN ('PROCESSING','COMPLETED','FAILED') → external_ref IS NOT NULL` / `CHECK status='FAILED' AND direction='WITHDRAWAL' → resolution_journal_id IS NOT NULL` / `CHECK` review 두 열 함께 NULL 또는 함께 값 / `UNIQUE (user_id, client_request_key)` / `UNIQUE (external_ref) WHERE external_ref IS NOT NULL` / `INDEX (next_check_at) WHERE status='PROCESSING'` |
  | `transfer_status_events` | `UNIQUE (event_key)` / `CHECK source IN ('CALLBACK','POLL')` / `CHECK outcome IN ('SUCCESS','FAILURE','PENDING','UNKNOWN')` / `INDEX (transfer_request_id, received_at)` |
  | `user_asset_stats` | `UNIQUE (user_id, asset)` / `CHECK avg_buy_price >= 0` |

  **`status`와 `review_required_at`을 잇는 CHECK는 넣지 않는다** — 확정 후에도 확인 표시를 켜야 하는 경우가 있다(설계 §4.5·§8.8).

  **출금의 `hold_journal_id`는 즉시 CHECK로 강제하지 않는다.** 설계 §4.5는 `direction='WITHDRAWAL' ⟺ hold_journal_id IS NOT NULL`을 즉시 CHECK로 적었지만, **그러면 출금 요청을 만들 수 없다.** 순환이 생기기 때문이다:

  ```
  잠금 분개를 만들려면 → reference_id로 쓸 요청 id가 필요하다
  요청 id를 얻으려면   → 요청을 INSERT해야 한다
  요청을 INSERT하려면  → hold_journal_id가 이미 있어야 한다   ← 즉시 CHECK
  ```

  요청 INSERT를 먼저 하는 것은 선택이 아니다 — `client_request_key`를 선점해야 같은 요청이 두 번 잠그지 않는다(Step 6). 그래서 **커밋 시점의 최종 행만 보는 `DEFERRABLE INITIALLY DEFERRED` constraint trigger로 대체한다:**

  ```
  한 트랜잭션 안에서
    INSERT transfer_requests(direction='WITHDRAWAL', hold_journal_id=NULL)
    → 잠금 분개 생성
    → UPDATE transfer_requests SET hold_journal_id = :journalID
    → COMMIT                                  ← 통과
  ```
  ```
  hold_journal_id를 채우지 않고 COMMIT           ← 실패
  ```

  `direction='DEPOSIT' → hold_journal_id IS NULL`은 순환이 없으므로 즉시 CHECK로 남긴다.

  **`external_ref`는 `RECEIVED` 동안 NULL이다.** 외부 제출 전에는 외부 거래번호가 없다. 그래서 `UNIQUE`를 `WHERE external_ref IS NOT NULL` 부분 인덱스로 걸고, 제출 이후 상태에서는 NOT NULL을 CHECK로 강제한다.

- [ ] **Step 2: 모델 4개 파일 작성 후 AutoMigrate 목록에 배선**

  위 Produces의 구조체를 GORM 태그와 함께 작성한다. `cmd/main.go:55-66`과 `internal/testdb/integration.go:31`의 목록에 **7개 모두** 추가한다:

  ```
  &model.Account{}, &model.AccountBalance{}, &model.JournalEntry{}, &model.Posting{},
  &model.TransferRequest{}, &model.TransferStatusEvent{}, &model.UserAssetStat{}
  ```

  **두 목록은 항상 같아야 한다** — 다르면 통합 테스트와 운영 스키마가 갈린다.

  `user_asset_stats`도 여기서 함께 만든다. Task 4에서 뒤늦게 추가하면 **이미 적용된 009를 고치거나 010을 새로 만드는 흐름**이 생기는데, 적용된 마이그레이션을 수정하는 습관은 만들지 않는다.

- [ ] **Step 3: 리포지토리 3개 작성**

  위 Produces의 시그니처대로 구현한다. 세 가지만 주의한다.
  - `InsertOrGet`·`InsertEventIfAbsent`는 `clause.OnConflict{Columns: …, DoNothing: true}` + `RETURNING`을 쓴다. `RowsAffected == 0`이 "이미 있음"이다.
  - `LockBalances`는 반드시 `account_id` 오름차순으로 정렬한 뒤 `FOR UPDATE` 한다. 기존 [wallet_reporsitory.go:104](../../../internal/repository/wallet_reporsitory.go) `LockByIDs`와 같은 이유(AB-BA 데드락 방지)이며 그 구현을 본떠도 된다.
  - `ApplyBalanceDeltas`는 [wallet_reporsitory.go:274](../../../internal/repository/wallet_reporsitory.go) `BatchUpdateBalances`의 `UPDATE … FROM (VALUES …)` 패턴을 따라 1왕복으로 처리한다. 없는 행은 0에서 시작하도록 upsert한다.

- [ ] **Step 4: 스키마 테스트 작성**

  `internal/dbmigration/ledger_schema_integration_test.go`에 `TestLedgerSchemaIntegration`을 만든다. 패키지는 `dbmigration_test`다(testdb가 dbmigration을 import하므로 import cycle 방지 — [cancel_command_integration_test.go](../../../internal/dbmigration/cancel_command_integration_test.go) 머리 주석과 같은 이유). 그 파일이 007을 검증하는 방식을 그대로 따라 다음 넷만 확인한다:
  - 허용값 밖 `status`·`outcome` INSERT가 거부된다
  - `direction='DEPOSIT'`인데 `hold_journal_id`가 있으면 거부된다
  - 같은 `event_key` 두 번째 INSERT가 `ON CONFLICT DO NOTHING`으로 0행이 된다
  - `postings`에 `amount = 0`이 거부된다
  - **deferrable trigger 하위 경우 2건** — 한 트랜잭션에서 출금을 `hold_journal_id=NULL`로 INSERT한 뒤 UPDATE로 채우고 COMMIT하면 **통과하고**, 채우지 않고 COMMIT하면 **실패한다.** 즉시 CHECK였다면 첫 번째가 INSERT 시점에 막힌다 — 이 두 경우가 그 모순이 없다는 증거다
  - **`external_ref` 하위 경우 2건** — `RECEIVED`는 `external_ref=NULL`로 INSERT되고, 같은 NULL이 여러 행에 있어도 부분 유니크 인덱스가 막지 않는다. `status='PROCESSING'`인데 `external_ref IS NULL`이면 거부된다

  **새 테스트 함수를 만들지 않는다** — 전부 `TestLedgerSchemaIntegration`의 `t.Run` 하위 경우다.

- [ ] **Step 5: 테스트 실행**

  ```bash
  go test ./internal/dbmigration/ -run TestLedgerSchemaIntegration -v
  ```
  Expected: PASS. DSN 미설정이면 SKIP이 뜨므로, SKIP이면 DSN을 설정하고 다시 돌린다 — SKIP을 통과로 읽지 않는다.

---

## Task 2: LedgerService·잔액 캐시·검산 4종

**Files:**
- Create: `internal/service/ledger_service.go`
- Create: `internal/repository/ledger_reconciliation_repository.go`
- Test: `internal/service/ledger_service_integration_test.go`

**Interfaces:**
- Consumes: Task 1의 `AccountRepository`, `JournalRepository`, 모델 전부
- Produces:
  ```go
  type PostingInput struct {
      AccountType model.AccountType
      OwnerUserID *uint          // 시스템 계정은 nil
      Asset       string
      Amount      decimal.Decimal // 양수=들어옴, 음수=나감
  }
  type JournalInput struct {
      EventType         model.JournalEventType
      IdempotencyKey    string
      ReferenceType     model.JournalReferenceType
      ReferenceID       uint
      ReversesJournalID *uint
      Postings          []PostingInput
  }
  type LedgerService struct { Accounts *repository.AccountRepository; Journals *repository.JournalRepository }
  func NewLedgerService(db *gorm.DB) *LedgerService

  // Record는 호출자의 트랜잭션 안에서 실행된다. 트랜잭션을 스스로 열지 않는다 —
  // HoldCoordinator는 한 트랜잭션에 분개 여러 개를 넣고, 정산은 주문 갱신과 같은
  // 트랜잭션이어야 하기 때문이다(설계 §6).
  // created=false면 같은 idempotency_key의 기존 분개를 반환하며 전기를 만들지 않는다.
  func (s *LedgerService) Record(tx *gorm.DB, in JournalInput) (entry *model.JournalEntry, created bool, err error)
  func (s *LedgerService) Reverse(tx *gorm.DB, journalID uint) (*model.JournalEntry, error)

  // internal/repository/ledger_reconciliation_repository.go
  type LedgerReconciliationRepository struct { DB *gorm.DB }
  func (r *LedgerReconciliationRepository) CheckUnbalancedJournals() ([]UnbalancedJournalRow, error)   // 검사 1
  func (r *LedgerReconciliationRepository) CheckBalanceCacheDrift() ([]BalanceDriftRow, error)         // 검사 2
  func (r *LedgerReconciliationRepository) CheckAssetTotals() ([]AssetTotalRow, error)                 // 검사 3
  func (r *LedgerReconciliationRepository) CheckNegativeAccounts() ([]NegativeAccountRow, error)       // 검사 4
  ```

- [ ] **Step 1: `Record`의 실패 테스트 4개를 먼저 쓴다**

  `internal/service/ledger_service_integration_test.go`에 아래 넷을 쓴다. 구현이 없으므로 컴파일 실패로 시작한다.

  | 테스트 | 고정하는 계약 | 담당 T |
  |---|---|---|
  | `TestLedgerRejectsUnbalancedJournal` | 자산별 합이 0이 아닌 `JournalInput`은 거부되고 분개·전기·잔액 모두 남지 않는다 | **T1** |
  | `TestLedgerRecordIsIdempotent` | ① 같은 `IdempotencyKey`로 두 번 → 두 번째는 `created=false`와 기존 분개, 전기 수·잔액 불변 ② **잔액을 다 써서 지금이라면 음수가 될 상태로 만든 뒤 같은 키로 재시도 → 여전히 성공하고 기존 분개를 반환한다**(멱등 판정이 잔액 검사보다 먼저다) | **T2** |
  | `TestLedgerRollsBackJournalPostingAndBalance` | 전기 INSERT 후 잔액 갱신 전 강제 롤백 → 분개·전기·캐시 **모두** 없음 | **T3** |
  | `TestLedgerRejectsNegativeUserAccount` | `USER_AVAILABLE`을 음수로 만드는 전기 → 거부. `EXTERNAL_BANK`는 음수 허용 | **T9** |

  T3의 롤백은 시간이나 실행 순서에 의존하지 않는다 — 호출자 트랜잭션 안에서 `Record` 성공 후 테스트가 명시적으로 오류를 반환해 롤백시킨다.

- [ ] **Step 2: 실패 확인**

  ```bash
  go test ./internal/service/ -run 'TestLedger(RejectsUnbalancedJournal|RecordIsIdempotent|RollsBackJournalPostingAndBalance|RejectsNegativeUserAccount)' -v
  ```
  Expected: 컴파일 실패 (`undefined: NewLedgerService`).

- [ ] **Step 3: `LedgerService.Record` 구현**

  순서를 고정한다. **멱등 판정이 잔액 검사보다 먼저다.**

  1. 순수 입력 검증 — `Postings`의 자산별 합이 전부 0인가(T1), `Amount == 0`인 전기가 없는가(Global Constraints), 필수값이 있는가. DB를 보지 않는 검사만 여기 둔다
  2. `Journals.InsertOrGet`
  3. **`created == false`면 기존 분개를 반환하고 즉시 종료** — 계정도 잠그지 않고 잔액도 보지 않는다(T2)
  4. `EnsureAccounts`로 필요한 계정을 확보(없으면 생성)
  5. `LockBalances`를 **account_id 오름차순**으로 호출
  6. 잠근 잔액 + 델타를 계산해 `AllowsNegative == false`인 계정이 음수가 되면 오류(T9)
  7. `CreatePostings` → `ApplyBalanceDeltas`

  **왜 멱등 판정이 먼저인가.** 잔액 검사를 앞에 두면, **이미 처리된 요청이 지금 잔액 때문에 실패한다.** 예를 들어 출금이 확정돼 잔액이 줄어든 뒤 같은 알림이 재전송되면, 3번에서 끝나야 할 재관측이 6번의 음수 검사에 걸려 오류가 된다. 그러면 외부가 알림을 계속 재전송하고 우리는 계속 실패를 돌려주는 고리에 들어간다(설계 §8.8).

  **신규 분개를 INSERT한 뒤 6번에서 실패해도 찌꺼기는 남지 않는다.** `Record`는 호출자의 트랜잭션 안에서 돌고, 오류를 반환하면 호출자가 트랜잭션 전체를 롤백한다. 분개만 남고 전기가 없는 상태는 커밋되지 않는다 — T3가 이것을 고정한다.

- [ ] **Step 4: 검산 4종 쿼리 구현**

  설계 §9의 SQL 넷을 그대로 옮긴다. 검사 2는 `account_balances.balance`와 `SUM(postings.amount)`를 `IS DISTINCT FROM`으로 비교한다. 기존 [reconciliation_repository.go](../../../internal/repository/reconciliation_repository.go)의 페이지네이션 방식(`afterID` + `limit`)을 따라 큰 표에서도 한 번에 다 읽지 않게 한다. **`ReconciliationWorker` 배선은 Task 5에서 한다** — 지금 붙이면 아직 원장이 비어 있어 옛 검사와 새 검사가 동시에 돌아간다.

- [ ] **Step 5: 통과 확인**

  ```bash
  go test ./internal/service/ -run 'TestLedger(RejectsUnbalancedJournal|RecordIsIdempotent|RollsBackJournalPostingAndBalance|RejectsNegativeUserAccount)' -v
  ```
  Expected: 4 PASS.

- [ ] **Step 6: 커밋**

  Task 1·2를 한 커밋으로 묶지 않는다. `commit-message` 스킬을 거쳐 각각 커밋한다(CLAUDE.md §5). **전환 구간에 들어가기 전 마지막 커밋 지점이다.**

---

## ✅ CP1 — 원장 핵심 검토 (Task 2 후)

**멈추고 검토를 요청한다.** 여기가 전환 구간 **직전**이다 — 다음 커밋은 Task 5가 녹색이 된 뒤에야 나온다.

확인할 것:

1. `postings`에 INSERT하는 코드가 `LedgerService` 하나뿐인가 — `grep -rn "Posting{" internal/ --include=*.go | grep -v _test`로 확인
2. `Record`의 순서가 **멱등 판정 → 계정 확보 → 잠금 → 음수 검사**인가. 이미 처리된 요청이 현재 잔액 때문에 실패하지 않는가(T2 ②)
3. T1·T2·T3·T9가 각각 무엇을 고정하는지, 서로 겹치지 않는지
4. 7개 표가 마이그레이션·모델·AutoMigrate 목록 **세 곳 모두**에 있는가
5. 출금 INSERT를 막는 즉시 CHECK가 없고, deferrable trigger가 두 하위 경우로 검증됐는가

---

## Task 3: 계정 프로비저닝·개발용 지급·잔액 조회 API 전환

> **이 Task부터 전환 구간이다.** 위 "전환 구간에 대한 경고"를 읽지 않았다면 지금 읽는다.

**Files:**
- Modify: `internal/service/dev_wallet_service.go` (전면 재작성), `internal/handler/dev_handler.go`, `internal/handler/order_handler.go:180-197,272-279,321-330`
- Modify: `Go-exchange-front/src/lib/api.ts` (`fundWallet`에 요청 키 추가)
- Test: `internal/service/dev_fund_ledger_integration_test.go`

**Interfaces:**
- Consumes: `LedgerService.Record`, `AccountRepository.ListUserBalances`
- Produces:
  ```go
  type FundWalletInput struct {
      UserID     uint
      CoinSymbol string
      Amount     string
      RequestKey string // 신규. 멱등 근거
  }
  type DevWalletService struct { DB *gorm.DB; Ledger *LedgerService; Accounts *repository.AccountRepository }
  func NewDevWalletService(db *gorm.DB) *DevWalletService
  func (s *DevWalletService) FundWallet(in FundWalletInput) (*repository.UserAssetBalance, error)
  ```
  `WalletResponse`의 JSON 필드 6개(`id`·`coin_symbol`·`available_balance`·`locked_balance`·`total_balance`·`avg_buy_price`)는 **그대로 유지한다.** 프런트가 정확히 이 여섯 개를 소비한다(조사 D).

- [ ] **Step 1: 개발용 지급 테스트를 먼저 쓴다**

  `internal/service/dev_fund_ledger_integration_test.go`:
  - `TestDevFundCreatesMintToAvailableJournal` — 지급 후 `DEV_MINT` 전기 `−amount`, `USER_AVAILABLE` 전기 `+amount`, 자산별 합 0
  - `TestDevFundSameRequestKeyPaysOnce` — 같은 `RequestKey`로 두 번 → 잔액은 한 번만 증가

  두 번째는 T2(원장 계층 멱등)와 다르다. 여기서 고정하는 것은 **dev funding이 요청 키를 실제로 분개 멱등성 키로 넘기는가**이지, 원장이 멱등한가가 아니다.

- [ ] **Step 2: 실패 확인**

  ```bash
  go test ./internal/service/ -run 'TestDevFund' -v
  ```
  Expected: 컴파일 실패 또는 두 번째 지급이 잔액을 두 배로 만들어 FAIL.

- [ ] **Step 3: `DevWalletService`를 원장 경유로 재작성**

  `upsertFundedWallet`([dev_wallet_service.go:74](../../../internal/service/dev_wallet_service.go))과 `devFundLedgerEntry`([ledger.go:32](../../../internal/service/ledger.go)) 호출을 지우고, 한 트랜잭션에서 `Ledger.Record`를 1회 호출한다.

  - `EventType: DEV_FUND`, `ReferenceType: DEV_FUND`, `IdempotencyKey: "devfund:{userID}:{asset}:{RequestKey}"` (설계 §7)
  - 전기 2줄: `{DEV_MINT, nil, asset, −amount}`, `{USER_AVAILABLE, &userID, asset, +amount}`
  - **잔액을 직접 쓰는 코드가 이 서비스에 남으면 안 된다.** `gorm.Expr("… + ?")` 사용처가 0인지 확인한다.
  - `RequestKey`가 비면 검증 오류로 거부한다. 자동 생성하지 않는다 — 자동 생성하면 재시도가 항상 새 키가 되어 멱등이 무의미해진다.

- [ ] **Step 4: 잔액 조회 API를 `account_balances` 기반으로 바꾼다**

  `OrderHandler.ListWallets`([order_handler.go:180](../../../internal/handler/order_handler.go))가 `WalletRepository.ListByUserID` 대신 `AccountRepository.ListUserBalances`를 부르게 한다. `walletResponse`([order_handler.go:321](../../../internal/handler/order_handler.go))는 `UserAssetBalance`를 받도록 바꾸되 **JSON 필드 이름·개수·문자열 형식을 바꾸지 않는다.**

  `ListUserBalances`는 자산별로 `USER_AVAILABLE`과 `USER_LOCKED` 두 계정을 합쳐 한 행을 만들고, `user_asset_stats`를 `LEFT JOIN`해 `avg_buy_price`를 붙인다(표는 Task 1에 있다). 통계 행이 없으면 0이다. `total_balance = available + locked`.

  **`WalletResponse.id`는 `USER_AVAILABLE` 계정 ID로 고정한다.** 자산마다 계정이 둘이므로 어느 쪽을 쓸지 정해야 하고, 정하지 않으면 응답의 `id`가 실행마다 달라진다. 기존 6필드 계약은 이렇게 유지된다.

  값을 실제로 채우는 것은 Task 4의 매수 정산이다 — 그때까지는 매수한 적 없는 자산과 같은 0이다.

- [ ] **Step 5: 프런트 `fundWallet`에 요청 키 추가**

  [Go-exchange-front/src/lib/api.ts:237](../../../../Go-exchange-front/src/lib/api.ts)의 `fundWallet` 입력에 `request_key: string`을 추가하고, 호출부에서 `crypto.randomUUID()`로 생성해 넘긴다. 재시도 시에는 **같은 키를 다시 보낸다** — 버튼을 다시 누르는 것과 네트워크 재시도를 구분하는 것이 이 키의 목적이다.

- [ ] **Step 6: 통과 확인**

  ```bash
  go test ./internal/service/ -run 'TestDevFund' -v
  ```
  Expected: 2 PASS. **다른 통합 스위트는 실행하지 않는다** — 지금은 전환 구간이고 빨간 것이 정상이다.

---

## Task 4: 주문 잠금·해제와 체결 정산 전환

**Files:**
- Modify: `internal/service/hold_coordinator.go:127-280`, `internal/service/order_service.go` (해제 4개 함수), `internal/service/settlement_service.go:96-199`, `internal/service/settlement_batch.go:48-330`
- Modify: `internal/model/failed_settlement.go` (수수료 저장 필드 추가), `internal/service/settlement_retry_worker.go:244` 부근
- Test: `internal/service/ledger_order_settlement_integration_test.go`

**Interfaces:**
- Consumes: `LedgerService.Record(tx, JournalInput)`
- Produces: 새 공개 시그니처 없음. 기존 시그니처를 유지한 채 내부 구현만 바꾼다 — 호출자(핸들러·worker)를 건드리지 않기 위해서다.
  ```go
  // internal/service/order_ledger.go (신규, 같은 패키지)
  // 주문 잠금·해제의 전기 2줄을 만드는 공통 헬퍼. 매수는 KRW, 매도는 코인.
  func orderHoldPostings(order *model.Order, amount decimal.Decimal) []PostingInput
  func orderReleasePostings(order *model.Order, amount decimal.Decimal) []PostingInput
  func tradePostings(trade *model.Trade, buyerID, sellerID uint,
      reservedDebit, executionDebit, sellerQuoteNet decimal.Decimal) []PostingInput
  ```

- [ ] **Step 1: 자산 보존 통합 테스트를 먼저 쓴다**

  `internal/service/ledger_order_settlement_integration_test.go`에 `TestOrderAndSettlementPreserveAssets`를 만든다. 하나의 시나리오를 끝까지 돌린다:

  개발용 지급(매수자 KRW·매도자 코인) → 지정가 매수·매도 접수(잠금) → 체결 정산 → 매수자 취소분 해제.

  단언:
  - 체결 분개의 KRW 전기 합이 0이다 — `USER_LOCKED(매수자) −(체결액+수수료)`, `USER_AVAILABLE(매도자) +(체결액−수수료)`, `FEE_INCOME +(수수료×2)`
  - 코인 전기 합이 0이다
  - `FEE_INCOME` 잔액이 `BuyerFee + SellerFee`와 정확히 같다
  - 검산 4종이 전부 위반 0건이다

  **이 테스트가 §1.6의 "수수료가 사라진다"를 닫는 증거다.**

- [ ] **Step 2: 실패 확인**

  ```bash
  go test ./internal/service/ -run TestOrderAndSettlementPreserveAssets -v
  ```
  Expected: FAIL — 아직 잠금·정산이 `wallets`에 쓴다.

- [ ] **Step 3: 주문 잠금·해제를 원장으로 전환**

  `HoldBatch`([hold_coordinator.go:127](../../../internal/service/hold_coordinator.go))의 지갑 조회·fold·`BatchUpdateBalances`·`ledgerRepo.CreateMany`를 **주문 1건당 `Ledger.Record` 1회**로 바꾼다. 트랜잭션 경계는 그대로 둔다 — 멱등성 키 선점과 주문 INSERT가 같은 트랜잭션에 있어야 한다는 성질(조사 B)은 유지해야 한다.

  - 잠금 키: `order-hold:{orderID}` / 해제 키: `order-release:{orderID}:{사유}` (설계 §7). **사유를 빼면 사용자 취소와 시장가 잔액 반환이 같은 키가 되어 한쪽이 조용히 무시된다.**
  - 잔고 부족은 `Record`가 T9 검증에서 오류를 내므로, 그 오류를 기존 `NewConflictErrorf("insufficient available balance")`로 변환해 `holdResult.Err`에 넣는다. **배치 전체를 롤백시키지 않는다** — 기존 격리 동작(hold_coordinator.go:196-213)을 유지한다.
  - 해제 4개 지점(조사 B의 표) 전부를 같은 헬퍼로 바꾼다. 하나라도 남으면 `wallets` DROP이 컴파일 오류로 잡아 준다.

- [ ] **Step 4: 체결 정산 두 경로를 함께 전환**

  `SettleTrade`([settlement_service.go:96](../../../internal/service/settlement_service.go))와 `SettleTradeBatch`([settlement_batch.go:48](../../../internal/service/settlement_batch.go))가 **같은 `tradePostings` 헬퍼**를 쓰게 한다. 배치는 `Record`를 체결 건수만큼 호출한다 — 한 트랜잭션에 분개 여러 개가 들어가는 것은 설계 §13.4가 허용한 형태다.

  전기 5줄(설계 §5.7)에 매수자 잠금 초과분 반환 1줄이 더 붙을 수 있다:

  | 계정 | 자산 | 금액 |
  |---|---|---|
  | `USER_LOCKED(매수자)` | KRW | −reservedDebit |
  | `USER_AVAILABLE(매수자)` | KRW | +(reservedDebit − executionDebit) — **0이면 이 줄을 만들지 않는다** |
  | `USER_AVAILABLE(매도자)` | KRW | +sellerQuoteNet |
  | `FEE_INCOME` | KRW | +(BuyerFee + SellerFee) |
  | `USER_LOCKED(매도자)` | 코인 | −quantity |
  | `USER_AVAILABLE(매수자)` | 코인 | +quantity |

  `IdempotencyKey: "trade:{trade.IdempotencyKey}"`. 기존 `trades.idempotency_key`를 그대로 재사용하므로 중복 정산은 원장 계층에서도 막힌다.

  **`settlement_batch.go:27-30`의 등가성 불변식을 깨지 않는다** — 배치 결과가 단건 N회와 같아야 한다. 헬퍼를 공유하면 자동으로 성립한다.

- [ ] **Step 5: `FailedSettlement`에 수수료를 저장하고 재시도가 그 값을 쓰게 한다**

  설계 §13.2. `model.FailedSettlement`에 `BuyerFee`·`SellerFee`·`FeeRate` 필드를 추가하고, [settlement_retry_worker.go:244](../../../internal/service/settlement_retry_worker.go) 부근의 재계산을 저장값 사용으로 바꾼다. 재계산 값이 원본과 다르면 분개 합계가 어긋난다.

- [ ] **Step 6: `avg_buy_price` 갱신 — 표는 이미 있다**

  `user_asset_stats`는 Task 1에서 만들었다. 여기서는 **값을 채우는 것만** 한다. 009를 고치지 않는다.

  ```go
  // internal/service/order_ledger.go
  // 매수 정산 1곳에서만 불린다. 산술은 기존 balance.go:148
  // creditBuyerCoinWithAcquisitionCost와 같다 — 기존 평단가×기존수량 + 취득원가를
  // 새 수량으로 나눈다.
  func applyAvgBuyPrice(tx *gorm.DB, userID uint, asset string,
      quantity, acquisitionCost decimal.Decimal) error
  ```

  **`SettleTrade`와 `SettleTradeBatch`가 이 헬퍼 하나를 공유한다.** 각자 계산하면 등가성 불변식(settlement_batch.go:27-30)이 `avg_buy_price`에서만 조용히 깨진다.

  `TestOrderAndSettlementPreserveAssets`의 단언에 **`avg_buy_price`가 단건 N회와 배치 1회에서 같다**를 추가한다. 새 테스트 함수를 만들지 않는다.

- [ ] **Step 7: 통과 확인**

  ```bash
  go test ./internal/service/ -run TestOrderAndSettlementPreserveAssets -v
  ```
  Expected: PASS.

---

## Task 5: `wallets`·`ledger_entries` 제거와 DB 재생성

**Files:**
- Delete: `internal/model/wallet.go`, `internal/model/ledger_entry.go`, `internal/repository/wallet_reporsitory.go`, `internal/repository/ledger_repository.go`, `internal/service/ledger.go`
- Modify: `internal/service/balance.go` (원장으로 대체된 함수 제거), `internal/repository/reconciliation_repository.go`, `internal/service/reconciliation_worker.go`, `cmd/main.go:55-66`, `internal/testdb/integration.go:31`
- Create: `migrations/010_drop_legacy_wallets.sql`
- Test: 기존 `internal/service` 통합 스위트 전체

- [ ] **Step 1: 옛 표를 참조하는 코드를 전부 지운다**

  Delete 목록의 파일을 지우고 `go build ./...`를 돌린다. **컴파일 오류가 남은 참조 지점의 완전한 목록이다.** 하나씩 지워 나간다. 이 단계에서 `walletAvailableBalance`의 0-fallback 분기(§1.1 문제 2)와 레거시 거울 필드 `KRW`·`Quantity`(문제 1)가 함께 사라진다.

  `balance.go`에서 `applyBuyOrderHold`·`releaseBuyOrderHold` 등 원장으로 대체된 함수는 지우고, 순수 산술(`amountAfterFee` 등 `fee.go`에 있는 것)은 남긴다.

- [ ] **Step 2: 검산 워커를 새 검사 4종으로 교체**

  `ReconciliationWorker`([reconciliation_worker.go:53](../../../internal/service/reconciliation_worker.go) `RunOnce`)의 `runLedgerWalletCheck`·`runAssetConservationCheck`를 Task 2의 검사 1~4로 바꾼다. `runStaleMarketOrderCheck`는 원장과 무관하므로 그대로 둔다.

  **[reconciliation_repository.go:73](../../../internal/repository/reconciliation_repository.go)의 수수료 보정항이 여기서 사라진다.** `Σ(wallets) + Σ(fees) == Σ(DEV_FUND)`는 수수료가 사라지는 것을 전제한 쿼리였다. 새 검사 3은 보정 없이 `Σ postings.amount == 0`이다.

- [ ] **Step 3: DROP 마이그레이션 작성**

  `migrations/010_drop_legacy_wallets.sql`에 `DROP TABLE IF EXISTS wallets, ledger_entries CASCADE;`를 넣는다. AutoMigrate 목록(두 곳)에서도 `&model.Wallet{}`·`&model.LedgerEntry{}`를 뺀다. **목록에서 빼지 않고 DROP만 하면 다음 기동에서 AutoMigrate가 다시 만든다.**

- [ ] **Step 4: 개발 DB 재생성**

  개발·테스트 DB를 비우고 새 스키마로 다시 만든다. 옛 데이터를 옮기지 않는다.

  ```bash
  psql "$GOEXCHANGE_TEST_DATABASE_DSN" -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"
  ```

- [ ] **Step 5: 전환 창이 닫혔는지 확인 — 이 Task의 판정**

  ```bash
  go test ./internal/service/ ./internal/repository/ ./internal/handler/ -count=1
  ```
  Expected: 전부 PASS. **Task 3에서 열린 빨간 창이 여기서 닫힌다.** 하나라도 빨간 것이 남으면 전환이 끝나지 않은 것이므로 다음 Task로 넘어가지 않는다.

---

## ✅ CP2 — 기존 주문·체결 경로의 완전 전환 검토 (Task 4~5 후)

**멈추고 검토를 요청한다.** 확인할 것:

1. `grep -rn "wallets\|ledger_entries" internal/ cmd/ --include=*.go`가 0건인가
2. `SettleTrade`와 `SettleTradeBatch`가 같은 헬퍼를 쓰고 등가성 불변식이 유지되는가
3. 수수료가 `FEE_INCOME`에 실제로 쌓이고, 검산 3의 보정항이 사라졌는가
4. 해제 4개 지점이 전부 전환됐고 멱등성 키에 사유가 들어갔는가
5. `FailedSettlement`가 수수료를 저장하고 재시도가 저장값을 쓰는가
6. 전환 창이 닫혔는가 — Task 5 Step 5의 실제 출력

---

## Task 6: 가짜 입출금·확정 경로·조회 worker·역분개

**Files:**
- Create: `internal/service/transfer_service.go`, `internal/service/fake_transfer_processor.go`, `internal/service/transfer_status_poller.go`, `internal/handler/transfer_handler.go`
- Modify: `cmd/main.go` (라우트 + `backgroundCtx` 자리에 poller 배선)
- Test: `internal/service/transfer_integration_test.go`

**Interfaces:**
- Consumes: `LedgerService.Record`·`Reverse`, `TransferRepository` 전부
- Produces:
  ```go
  type ExternalTransferProcessor interface {
      // Submit은 안정적인 dispatchKey("transfer:{requestID}")를 받고 external_ref를
      // 돌려준다. 같은 dispatchKey로 다시 부르면 외부 송금을 새로 만들지 않고
      // 같은 external_ref를 돌려준다 — 제출 후 응답 전에 죽어도 재시도가 돈을
      // 두 번 보내지 않는다.
      Submit(dispatchKey string, req model.TransferRequest) (externalRef string, err error)
      GetTransferStatus(externalRef string) (model.TransferOutcome, map[string]any, error)
  }
  type TransferService struct {
      DB        *gorm.DB
      Ledger    *LedgerService
      Transfers *repository.TransferRepository
      Processor ExternalTransferProcessor
      Now       func() time.Time
      afterLock func() // 테스트 전용. A 경로에만, 일회성. 프로덕션은 항상 nil
  }
  type DepositInput struct { UserID uint; Rail model.TransferRail; Asset, Amount, ClientRequestKey string }
  type WithdrawalInput struct { UserID uint; Rail model.TransferRail; Asset, Amount, ClientRequestKey string }
  type ResolveInput struct {
      TransferRequestID uint; Source model.TransferEventSource
      EventKey string; Outcome model.TransferOutcome // SUCCESS 또는 FAILURE만
      Payload map[string]any
  }
  type ObservationInput struct {
      TransferRequestID uint; EventKey string
      Outcome model.TransferOutcome // PENDING 또는 UNKNOWN만
      Payload map[string]any
  }
  func (s *TransferService) RequestDeposit(in DepositInput) (*model.TransferRequest, error)
  func (s *TransferService) RequestWithdrawal(in WithdrawalInput) (*model.TransferRequest, error)
  func (s *TransferService) ResolveTransfer(in ResolveInput) error
  func (s *TransferService) RecordObservation(in ObservationInput) error

  type TransferStatusPoller struct {
      Transfers *repository.TransferRepository
      Service   *TransferService
      Interval  time.Duration
      Batch     int
      Logger    *log.Logger
  }
  func (p *TransferStatusPoller) Run(ctx context.Context)
  func (p *TransferStatusPoller) RunOnce()
  ```

- [ ] **Step 1: T4~T8·T10 테스트를 먼저 쓴다**

  `internal/service/transfer_integration_test.go`:

  | 테스트 | 고정하는 계약 | 담당 T |
  |---|---|---|
  | `TestAllEventsPassReconciliation` | 개발용 지급·입금·출금·잠금·해제·체결·수수료 각 1회 후 검산 1~4 전부 위반 0건 | **T4** |
  | `TestWithdrawalHoldBlocksReuseOfSameFunds` | ① 출금 접수 후 같은 돈으로 주문 → 잔액 부족으로 거절 ② **동일 출금 재시도**(같은 `client_request_key`·같은 본문) → 같은 transfer id, 잠금 분개 1건, 잔액 1회만 잠김 ③ **같은 키·다른 금액** → 409, 추가 잠금 없음 ④ **같은 dispatch key 재제출** → 외부 송금이 새로 생기지 않고 같은 `external_ref` | **T5** |
  | `TestConcurrentTerminalObservations` | 하위 3경우 ⓐⓑⓒ. Step 2 참조 | **T6** |
  | `TestFailureCallbackRefundsLockedFunds` | 실패 알림 → 잠금이 available로 정확히 복귀, 합계 0 | **T7** |
  | `TestReversalNetsToZeroPerAccount` | 역분개 후 원본+역분개 합이 계정별 0 | **T8** |
  | `TestUnknownKeepsLockThenSuccessCompletes` | ① `UNKNOWN` → 분개 0·잠금 유지·`review_required_at` 설정 ② 이어서 `SUCCESS` → 완료 분개 **정확히 1회**·`COMPLETED`·`next_check_at IS NULL`·확인 표시 해제 | **T10** |

- [ ] **Step 2: T6의 결정적 동시성 장벽을 만든다**

  설계 §11.1을 그대로 구현한다. **`sleep`이나 실행 순서에 의존하지 않는다.**

  - A용·B용 DB 연결을 각각 고정으로 잡고 각 연결에서 `SELECT pg_backend_pid()`로 `pidA`·`pidB`를 얻는다. 풀에서 매번 빌리면 잠근 세션과 pid를 물어본 세션이 달라진다.
  - 고루틴 A: `afterLock`이 `release` 채널을 기다리는 `ResolveTransfer(SUCCESS)` 시작 → 행을 잠근 채 정지
  - 관측 1: `afterLock`이 불렸다는 신호를 받는다 = `FOR UPDATE`가 반환됐다
  - 고루틴 B: ⓐ `ResolveTransfer(SUCCESS)` / ⓑ `ResolveTransfer(FAILURE)` / ⓒ `RecordObservation(PENDING)`
  - 관측 2: `require.Eventually`로 아래가 참이 될 때까지 기다린다

    ```sql
    SELECT pg_blocking_pids(:pidB) @> ARRAY[:pidA];
    ```

    `wait_event_type`만 보면 B가 **무언가를** 기다린다는 것까지만 알 수 있다. 그것이 A인지 옆 테스트인지 구별되지 않는다.
  - `release`를 닫고 둘 다 끝난 뒤 단언한다. **승자는 A로 고정된다** — 관측 1·2가 그것을 사실로 만든다.

  | 하위 경우 | 최종 상태 | 분개 | B가 남기는 것 |
  |---|---|---|---|
  | ⓐ | `COMPLETED` | 1건 | 사건 1줄, 확인 표시 없음 |
  | ⓑ | `COMPLETED` | 1건 | 사건 1줄 + `CONFLICTING_TERMINAL_OUTCOME` |
  | ⓒ | `COMPLETED`, `next_check_at IS NULL` | 1건 | `PENDING` 사건 1줄, review 재설정 없음 |

  `pg_blocking_pids`를 쓸 수 없는 환경이면 **건너뛰지 말고 실패시킨다.** 장벽 없이 통과하는 것보다 못 도는 것이 낫다.

- [ ] **Step 3: 실패 확인**

  ```bash
  go test ./internal/service/ -run 'TestAllEventsPassReconciliation|TestWithdrawalHoldBlocksReuseOfSameFunds|TestConcurrentTerminalObservations|TestFailureCallbackRefundsLockedFunds|TestReversalNetsToZeroPerAccount|TestUnknownKeepsLockThenSuccessCompletes' -v
  ```
  Expected: 컴파일 실패.

- [ ] **Step 4: 접수 경로와 제출 생명주기 구현**

  **접수는 항상 `InsertOrGetByUserRequestKey`로 시작한다.** 키를 선점하지 않고 잠금부터 하면 같은 요청이 두 번 잠근다.

  ```
  1. 요청 본문을 정규화한다 (direction·rail·asset·amount)
  2. InsertOrGetByUserRequestKey
     created = false →
        기존 요청의 정규화 본문과 같으면  → 기존 요청을 그대로 반환.
                                            돈·분개·외부 제출 전부 불변
        다르면                            → 409 ConflictError
     created = true  → 3으로
  3. 입금이면 여기서 끝. RECEIVED, external_ref = NULL, 분개 없음
     출금이면 같은 트랜잭션에서
        잠금 분개 Record(withdraw-hold:{id})
        → SetHoldJournal(id, journalID)
     deferrable trigger가 커밋 시점에 hold_journal_id를 확인한다
  ```

  - `amount > 0`은 **서버와 DB 양쪽에서** 강제한다. 서버 검증은 좋은 오류 메시지를 위해, DB CHECK는 검증을 우회하는 경로가 생겨도 막기 위해.
  - 출금 잠금 전기 2줄: `USER_AVAILABLE −(amount+fee)`, `USER_LOCKED +(amount+fee)`. **첫 구현의 `fee_amount`는 0이라 잠금액이 `amount`와 같지만, 산술은 처음부터 `amount + fee`로 쓴다.**
  - `fee_amount`는 접수 시점에 확정해 저장한다. 처리 도중 재계산하지 않는다.
  - **외부 제출은 접수 트랜잭션 밖이다.** `Submit("transfer:{id}", req)`가 `external_ref`를 돌려주면 `SetDispatched(id, ref)`로 `RECEIVED → PROCESSING`을 만든다. 제출 후 응답 전에 죽으면 요청은 `RECEIVED`로 남고, worker가 같은 dispatch key로 재제출한다 — 같은 `external_ref`가 돌아오므로 외부 송금은 하나다.
  - **worker는 두 가지를 한다:** `RECEIVED` 요청의 제출 재시도, `PROCESSING` 요청의 상태 조회. **시간 경과만으로 돈을 반환하는 규칙을 추가하지 않는다.**

- [ ] **Step 5: `ResolveTransfer`와 `RecordObservation` 구현**

  설계 §8.8·§8.9의 절차를 그대로 따른다. **함수를 나눈 것이 핵심이다** — `ResolveTransfer`는 `PENDING`·`UNKNOWN`을 받으면 프로그래밍 오류로 거부하고, `RecordObservation`에는 분개를 만드는 코드가 없다.

  둘 다 `Transfers.LockByID`(= `SELECT ... FOR UPDATE`)로 시작해 잠근 뒤 상태를 **다시 읽는다.**

  `ResolveTransfer` 분기:

  | 잠근 뒤 status | 처리 |
  |---|---|
  | `RECEIVED` | 돈 불변. `review_reason = TERMINAL_BEFORE_DISPATCH`. 분개 없음 |
  | `PROCESSING` | 확정 — 분개 + §4.5의 단일 UPDATE 4열 |
  | terminal, 같은 outcome | 사건만. 확인 표시 손대지 않음 |
  | terminal, 반대 outcome | 돈·상태 불변. `review_reason = CONFLICTING_TERMINAL_OUTCOME` |

  `RecordObservation` 분기: `PROCESSING`이면 조회 일정 갱신(+임계 초과 시 review), terminal이면 **사건만**(status·`next_check_at`·review 두 열 불변), `RECEIVED`면 내부 호출 순서 오류로 거부.

  확정 UPDATE는 네 열을 한 번에 바꾸고 `WHERE id = ? AND status = 'PROCESSING'`을 건다. 그것이 "한 번만"을 만든다.

- [ ] **Step 6: 가짜 처리기와 조회 worker 구현**

  - `FakeTransferProcessor`: `Submit`은 요청을 `PROCESSING`으로 올리고, `GetTransferStatus`는 테스트가 심어 둔 결과를 돌려준다. **테스트에서는 시계를 흉내 내지 않고 콜백을 직접 호출한다.** 알림과 조회를 같은 시점에 내보낼 수 있어야 한다 — 그래야 §8.8의 행 잠금이 실제로 검증된다.
  - `payload`는 허용 목록(외부 거래 식별자 / 상태 코드·사유 / 금액·자산 / 외부 타임스탬프)만 저장한다. 목록 밖 필드는 버리고 **이름과 개수만** 로그에 남긴다. 값은 남기지 않는다.
  - `TransferStatusPoller`: `status='PROCESSING' AND next_check_at <= now()`를 골라 조회하고 결과에 따라 `ResolveTransfer` 또는 `RecordObservation`을 부른다. 간격은 10초 시작 → 2배씩 → 상한 1시간, 확인 표시 임계는 미확정 30분(설계 §8.6).
  - 배선: [cmd/main.go:268](../../../cmd/main.go) `go reconciliationWorker.Run(backgroundCtx)` 바로 아래에 `go transferStatusPoller.Run(backgroundCtx)`를 둔다. 종료는 252행 `defer cancelBackground()`가 처리한다. **매칭 엔진 종료 체인(386-442행)에 넣지 않는다** — 자산을 잠그지 않고 조회만 하므로 drain 대상이 아니다.

- [ ] **Step 7: 역분개와 HTTP 라우트**

  `LedgerService.Reverse`는 원본 전기의 부호를 뒤집어 새 분개(`event_type='REVERSAL'`, `reverses_journal_id=원본`, 키 `reversal:{원본 id}`)를 만든다. `UNIQUE (reverses_journal_id)`가 두 번 되돌리는 것을 막는다.

  라우트는 [cmd/main.go:377](../../../cmd/main.go) `authenticated.GET("/wallets", …)` 옆에 붙인다: `POST /transfers/deposits`, `POST /transfers/withdrawals`, `GET /transfers`. 가짜 외부 콜백 수신은 dev 그룹(380-383행)에 둔다 — 운영 라우트가 아니다.

- [ ] **Step 8: 통과 확인**

  ```bash
  go test ./internal/service/ -run 'TestAllEventsPassReconciliation|TestWithdrawalHoldBlocksReuseOfSameFunds|TestConcurrentTerminalObservations|TestFailureCallbackRefundsLockedFunds|TestReversalNetsToZeroPerAccount|TestUnknownKeepsLockThenSuccessCompletes' -v
  ```
  Expected: 6 PASS (T6은 하위 3경우 포함).

---

## Task 7: 프런트 입출금 경험·E2E·전체 검증·문서

**Files:**
- Create: `Go-exchange-front/src/pages/Assets.tsx` + 자산 전용 컴포넌트
- Modify: `Go-exchange-front/src/App.tsx` (라우트), `Go-exchange-front/src/lib/api.ts`, `Go-exchange-front/src/pages/Index.tsx`, `Go-exchange-front/tests/e2e/exchange.spec.ts`
- Modify: `docs/ENGINEERING-SUMMARY.md`, `docs/refactor/README.md`, `.github/workflows/backend-ci.yml`

- [ ] **Step 1: `/assets` 페이지 신설**

  `api.ts`에 `requestDeposit`·`requestWithdrawal`·`fetchTransfers`를 추가한다(`Wallet` 인터페이스는 건드리지 않는다 — 필드가 그대로다). `App.tsx`에 `/assets` 라우트를 추가하고 `pages/Assets.tsx`를 만든다.

  구조: **자산 목록 → 선택한 자산의 잔액 → 입금 / 출금 / 처리 내역 탭.**

  **거래 화면(`Index.tsx`)에는 간단한 잔액과 자산 페이지 이동 링크만 남긴다.** 입출금 폼을 거래 화면에 끼워 넣지 않는다 — 주문과 입출금은 다른 일이고, 한 화면에 섞으면 둘 다 좁아진다.

  **인증 저장값을 공유하는 최소한의 hook/context만 추출한다.** 두 페이지가 토큰을 함께 봐야 하므로 그것만 꺼내고, **거래 상태 전체를 리팩터링하지 않는다.** 이번 작업의 목적이 아니다.

  **상태 표시는 설계 §8.7을 따른다:**

  | 상황 | 사용자에게 보이는 것 |
  |---|---|
  | `PROCESSING` + `review_required_at` 있음 | `처리 지연 · 외부 상태 자동 확인 중` |
  | `COMPLETED` / `FAILED` | 완료 / 실패 + 사유 |

  운영자용 정보(마지막·다음 조회 시각, `review_reason`)는 사용자 화면에 넣지 않는다.

- [ ] **Step 2: E2E 경로 하나 추가**

  [tests/e2e/exchange.spec.ts](../../../../Go-exchange-front/tests/e2e/exchange.spec.ts)에 **한 경로만** 추가한다:

  거래 화면 → 자산 페이지 이동 → 가짜 은행 입금 요청 → 가짜 완료 알림 → 잔액 증가 확인.

  **사용자가 입금 과정을 체험하는 경로는 가짜 입금이고, 테스트 준비용 자산은 개발용 지급이다**(설계 §5.1 용도 분리). 기존 테스트의 `fundWallet` 호출은 그대로 둔다.

- [ ] **Step 3: 백엔드 전체 검증 — 각각 1회**

  ```bash
  go build ./... && go vet ./... && go test ./... -count=1
  ```
  ```bash
  go test ./... -race -count=1
  ```
  Expected: 전부 PASS. 실패하면 원인을 고치고 실패한 명령만 다시 돌린다.

- [ ] **Step 4: 프런트 전체 검증 — 각각 1회**

  ```bash
  npm test && npm run lint && npm run build
  ```
  ```bash
  npm run test:e2e
  ```
  Expected: 전부 PASS.

- [ ] **Step 5: 문서와 CI**

  - `docs/ENGINEERING-SUMMARY.md`에 원장 축을 추가하고 상태를 기록한다. **측정하지 않은 것을 측정했다고 쓰지 않는다** — 이번 작업에 GCP 측정은 없다.
  - `docs/refactor/README.md`에 이번 전환을 반영한다.
  - `.github/workflows/backend-ci.yml`이 새 통합 테스트를 돌리는지 확인한다. DSN이 없으면 조용히 SKIP되므로, **CI에서 SKIP이 아니라 실제로 도는지 로그로 확인한다.**

- [ ] **Step 6: 커밋**

  `commit-message` 스킬을 거친다.

---

## ✅ CP3 — 최종 검토 (Task 6~7 후)

**멈추고 검토를 요청한다.** 확인할 것:

1. T1~T10이 전부 존재하고 각각 한 번씩만 검증되는가 (아래 대응표)
2. T6의 장벽이 `pg_blocking_pids`를 쓰고 `sleep`에 의존하지 않는가
3. `ResolveTransfer`가 terminal만 받고 `RecordObservation`에 분개 코드가 없는가
4. poller가 `backgroundCtx`로 종료되고 매칭 엔진 종료 체인을 막지 않는가
5. 전체 검증 4개 명령의 실제 출력
6. 문서가 측정하지 않은 것을 주장하지 않는가

---

## T1~T10 담당 Task 대응표

| T | 내용 | 담당 Task | 테스트 이름 |
|---|---|---|---|
| T1 | 분개 자산별 합 0 아니면 거부 | **2** | `TestLedgerRejectsUnbalancedJournal` |
| T2 | 같은 멱등성 키 두 번 → 기존 분개, 전기 수 불변 | **2** | `TestLedgerRecordIsIdempotent` |
| T3 | 잔액 갱신 전 롤백 → 분개·전기·캐시 모두 없음 | **2** | `TestLedgerRollsBackJournalPostingAndBalance` |
| T4 | 전 사건 1회 후 검산 1~4 통과 | **6** | `TestAllEventsPassReconciliation` |
| T5 | 출금 접수 후 같은 돈으로 주문 → 거절 | **6** | `TestWithdrawalHoldBlocksReuseOfSameFunds` |
| T6 | 동시 확정 3경우 ⓐⓑⓒ | **6** | `TestConcurrentTerminalObservations` |
| T7 | 실패 콜백 → 잠금이 available로 복귀 | **6** | `TestFailureCallbackRefundsLockedFunds` |
| T8 | 역분개 후 계정별 합 0 | **6** | `TestReversalNetsToZeroPerAccount` |
| T9 | 사용자 계정 음수 전기 거부 | **2** | `TestLedgerRejectsNegativeUserAccount` |
| T10 | `UNKNOWN` 유지 → 이어서 `SUCCESS` 확정 2단계 | **6** | `TestUnknownKeepsLockThenSuccessCompletes` |

**누락 확인:** T1~T10 전부 담당 Task와 테스트 이름이 있다. 빈 칸 없음.

**중복 확인:** 각 T가 정확히 한 Task·한 테스트에만 있다. 같은 계약을 두 계층에서 보지 않는다 —
- 멱등: T2(원장 계층)와 Task 3의 `TestDevFundSameRequestKeyPaysOnce`는 다른 것을 본다. 전자는 `Record`가 멱등한가, 후자는 dev funding이 요청 키를 실제로 키로 넘기는가.
- 자산 보존: T4(전 사건 통합)와 Task 4의 `TestOrderAndSettlementPreserveAssets`는 다른 것을 본다. 후자는 **수수료가 `FEE_INCOME`에 쌓이는가**만 보고, 입출금을 포함하지 않는다. 전자는 입출금까지 포함한 전 사건이다.
- 실패 반환: T7(알림 경로)이 덮으므로, 조회로 `FAILURE`를 확인하는 경로는 **같은 분개·같은 멱등성 키**라 다시 보지 않는다(설계 §11).

**T 외 테스트 2개** — 설계의 T 목록에 없지만 계획에서 추가한 것:

| 테스트 | Task | 이유 |
|---|---|---|
| `TestLedgerSchemaIntegration` | 1 | 마지막 검토에서 추가된 CHECK 제약(허용값 고정)을 검증할 T가 없다. 저장소에 선례가 있다 — [cancel_command_integration_test.go](../../../internal/dbmigration/cancel_command_integration_test.go)가 007을 같은 방식으로 본다 |
| `TestDevFundCreatesMintToAvailableJournal` + `TestDevFundSameRequestKeyPaysOnce` | 3 | 설계 §5.1의 "잔액 직접 수정 금지"와 "요청 번호 멱등"을 고정할 T가 없다 |

**총 테스트 함수 13개** (T 10개 + 추가 3개). 각 Task는 자기 테스트만 1회 실행한다.
