# 복식부기 원장과 가짜 은행·가짜 코인 입출금 설계

> 상태: 확인 질문 4건 답변 반영 완료. **설계 최종 검토 대기 / 구현 미착수.**
> 선행: B-3 매칭 quantum 완료(`2056b0c`)

## 0. 용어

처음 쓰는 말을 먼저 풀어 둔다.

| 말 | 뜻 |
|---|---|
| **복식부기(double-entry)** | 돈이 움직일 때마다 **빠진 곳**과 **들어간 곳**을 짝으로 적는 방식. 한 사건의 기록을 다 더하면 0이 된다. |
| **계정(account)** | 돈이 담기는 칸. "홍길동의 쓸 수 있는 KRW", "홍길동의 주문에 묶인 BTC", "수수료 금고" 처럼 목적별로 나눈다. |
| **전기(posting)** | 계정 하나에 대한 한 줄 기록. `+1000` 또는 `-1000`. |
| **분개(journal entry)** | 한 사건에 속한 전기들의 묶음. 이 묶음 안에서 자산별 합이 0이어야 한다. |
| **멱등(idempotent)** | 같은 요청이 두 번 와도 결과가 한 번 한 것과 같음. |
| **역분개(reversal)** | 잘못 적은 기록을 지우지 않고, 부호가 반대인 기록을 새로 적어 상쇄하는 것. |
| **아웃박스(outbox)** | 다른 처리로 넘길 일을 같은 트랜잭션 안의 표에 적어 두고, 나중에 읽어서 실행하는 방식. 이미 이 저장소에서 체결 전달에 쓰고 있다. |

---

## 1. 현재 코드 조사 결과

요구한 9개 흐름을 전부 찾았다. 파일과 줄 번호는 조사 시점(`2056b0c`) 기준이다.

### 1.1 wallet과 ledger 모델

**`internal/model/wallet.go`** — 한 행이 `(UserID, CoinSymbol)` 하나를 담는다.

```go
type Wallet struct {
    ID, UserID, CoinSymbol
    AvailableBalance  // 쓸 수 있는 양
    LockedBalance     // 주문에 묶인 양
    KRW               // ← 레거시 거울 필드
    Quantity          // ← 레거시 거울 필드
    AvgBuyPrice
}
```

**문제 1 — 같은 사실을 두 곳에 적는다.** `KRW`와 `Quantity`는 `AvailableBalance + LockedBalance`와 같아야 하는 값을 따로 들고 있다. `walletBalanceUpdate`(balance.go:200 부근)가 매번 둘을 다시 계산해 채운다. 한쪽만 갱신되면 조용히 어긋난다.

**문제 2 — 읽기 경로에 레거시 분기가 있다.**

```go
func walletAvailableBalance(wallet *model.Wallet) decimal.Decimal {
    if wallet.AvailableBalance.IsZero() && wallet.LockedBalance.IsZero() {
        if wallet.CoinSymbol == KRW && wallet.KRW.GreaterThan(0) { return wallet.KRW }
        if wallet.CoinSymbol != KRW && wallet.Quantity.GreaterThan(0) { return wallet.Quantity }
    }
    return wallet.AvailableBalance
}
```

"둘 다 0이면 옛 필드를 본다"는 규칙이다. 잔액이 정말 0인 지갑과 아직 옮기지 않은 지갑을 구분하지 못한다.

**`internal/model/ledger_entry.go`** — **한쪽만 적는 기록이다.**

```go
type LedgerEntry struct {
    UserID, CoinSymbol, EntryType
    AvailableDelta, LockedDelta          // 이 사용자에게 얼마나 변했나
    AvailableBalanceAfter, LockedBalanceAfter
    ReferenceType, ReferenceID, ReferenceKey
}
```

**상대편이 없다.** 매수자 KRW가 −10,000이 되어도 "그 10,000이 어디로 갔는지"는 기록에 없다. 종류는 `DEV_FUND` / `ORDER_HOLD` / `ORDER_RELEASE` / `TRADE_SETTLEMENT` 넷뿐이다.

### 1.2 개발용 자산 지급

`internal/service/dev_wallet_service.go` — `FundWallet`이 지갑을 upsert하고 `DEV_FUND` 원장 1줄을 같은 트랜잭션에 적는다. **돈이 무에서 생긴다.** 상대 계정이 없다.

### 1.3 주문 생성 시 자산 잠금

`internal/service/hold_coordinator.go:135` 트랜잭션 하나에서:
1. 멱등성 키 선점
2. 지갑 잠금 조회
3. `applyBuyOrderHold` / `applySellOrderHold`로 available → locked 이동
4. 주문 INSERT
5. `BatchUpdateBalances`
6. `ORDER_HOLD` 원장 일괄 INSERT

**잔액 변경과 원장이 같은 트랜잭션에 있다.** 이 성질은 유지해야 한다.

### 1.4 주문 취소 시 잠금 해제

`order_service.go:421·435`(사용자 취소), `907·924`(엔진 취소 확정)에서 `releaseBuyOrderHold` / `releaseSellOrderHold` → `ORDER_RELEASE` 원장. 역시 같은 트랜잭션이다.

### 1.5 체결 시 자산 이동

`internal/service/settlement_service.go:147-192`. 지갑 4개(매수자 KRW·코인, 매도자 KRW·코인)를 잠그고 한 트랜잭션에서 갱신 + `TRADE_SETTLEMENT` 원장 4줄 + outbox PROCESSED 마킹.

```
executionDebit  = executionQuote + BuyerFee     // 매수자가 실제로 내는 돈
sellerQuoteNet  = executionQuote − SellerFee    // 매도자가 실제로 받는 돈
```

### 1.6 수수료 처리

`internal/service/fee.go` — 매수자·매도자 모두 체결금액의 고정 비율을 **KRW로** 낸다.

**문제 3 — 수수료가 사라진다.** 매수자에게서 `+BuyerFee`만큼 더 빼고 매도자에게 `−SellerFee`만큼 덜 주는데, **그 돈을 받는 계정이 없다.** 시스템 전체 KRW 총액이 체결마다 줄어든다.

현재는 검산 쿼리가 이 구멍을 보정한다 — `reconciliation_repository.go:73`:

```sql
Σ(wallets.available + locked) + Σ(trades.buyer_fee + seller_fee) == Σ(DEV_FUND delta)
```

즉 **"사라진 수수료를 따로 더해서" 맞춘다.** 수수료 계정이 있으면 이 보정항이 필요 없다.

### 1.7 시장가 주문의 남은 금액 반환

`order_service.go:614-645`(`completeMarketBuyOrder`). 예산에서 쓴 돈+수수료를 뺀 나머지를 `releaseBuyOrderHold`로 풀고 `ORDER_RELEASE` 원장을 적는다. 매도는 `completeMarketSellOrder`(659행 부근)에서 남은 수량을 푼다.

### 1.8 정산 실패와 재실행

- `model.FailedSettlement` — 체결 식별자 `TradeIdempotencyKey`에 유니크 인덱스, `RetryCount`, `Status(OPEN/RESOLVED)`
- `settlement_retry_worker.go` — 저장된 가격·수량으로 체결을 재구성해 다시 정산. **수수료는 저장하지 않고 매번 다시 계산한다**(244행 주석). 수수료율이 사용자별로 달라지면 깨진다.
- `outbox_replayer.go` — 미처리 outbox를 재실행
- 멱등 근거는 `trades.idempotency_key` 유니크 제약. 같은 체결이 두 번 오면 기존 행을 찾아 `duplicateSettlementResult`로 끝낸다.

### 1.9 현재 reconciliation 검사

`reconciliation_worker.go` + `reconciliation_repository.go`:
1. 지갑별 `available/locked` 음수 검사
2. **자산 보존** — 위 §1.6의 보정 포함 쿼리
3. 5분 넘게 안 끝난 시장가 주문

**원장만으로 잔액을 다시 계산하는 검사는 없다.** `DEV_FUND` 합계와 지갑 합계를 비교할 뿐이라, 중간의 hold/release/settlement가 어긋나도 총량만 맞으면 통과한다.

### 1.10 입출금

**없다.** `Deposit`/`Withdraw` 관련 코드가 저장소에 존재하지 않는다. 전부 신규다.

---

## 2. 설계 목표와 그것을 어떻게 만족시키는가

| 목표 | 방법 |
|---|---|
| 모든 이동을 빠진 곳·들어간 곳 한 쌍 이상으로 기록 | `postings` 표. 분개 하나에 전기 2줄 이상 |
| 한 사건의 합계가 자산별로 정확히 0 | DB 제약 + INSERT 시 검증 + 검산 작업 |
| 잔액 변경과 원장이 같은 DB 작업 | 잔액을 **별도로 저장하지 않는다.** 계정 잔액이 곧 전기의 합이다 |
| 같은 요청이 다시 와도 한 번만 | 분개의 `idempotency_key`에 유니크 제약 |
| 삭제·수정 없이 역분개로 정정 | `postings`·`journal_entries`에 UPDATE/DELETE 금지. `reverses_journal_id`로 상쇄 |
| 사용 가능/잠긴 자산을 별도 계정 | `USER_AVAILABLE` / `USER_LOCKED` 두 종류 |
| 수수료·외부 계정 | `FEE_INCOME`, `EXTERNAL_BANK`, `EXTERNAL_CHAIN` |
| 원장만으로 잔액 검산 | `account_balances`는 전기 합의 **캐시**일 뿐. 언제든 다시 계산 가능 |

### 2.1 핵심 결정 — 잔액을 따로 두지 않는다

지금은 `wallets`가 진짜 잔액이고 `ledger_entries`는 사후 기록이다. 그래서 둘이 어긋날 수 있다.

새 구조에서는 **전기의 합이 잔액의 정의**다. `account_balances`는 매번 합계를 내지 않기 위한 캐시이고, 같은 트랜잭션 안에서 전기와 함께 갱신된다. 검산은 "캐시 == 전기 합"을 확인한다.

이렇게 하면 "잔액과 원장이 어긋난다"는 상태가 **정의상 오류**가 되고, 검산이 그것을 반드시 잡는다.

---

## 3. 계정 종류

계정은 `(종류, 소유자, 자산)`으로 유일하다.

| 종류 | 소유자 | 뜻 | 부호 규약 |
|---|---|---|---|
| `USER_AVAILABLE` | 사용자 ID | 사용자가 지금 쓸 수 있는 양 | 0 이상 |
| `USER_LOCKED` | 사용자 ID | 주문에 묶인 양 | 0 이상 |
| `FEE_INCOME` | 없음(시스템) | 거래소가 받은 수수료 | 0 이상 |
| `EXTERNAL_BANK` | 없음(시스템) | 가짜 은행 바깥 세상 | **음수 허용** |
| `EXTERNAL_CHAIN` | 없음(시스템) | 가짜 블록체인 바깥 세상 | **음수 허용** |
| `DEV_MINT` | 없음(시스템) | 개발용 지급의 출처 | **음수 허용** |

**외부 계정이 음수인 것이 정상이다.** 사용자가 1,000원을 입금하면 은행 쪽은 −1,000이 된다. 이것은 "바깥에서 안으로 1,000이 들어왔다"는 뜻이지 빚이 아니다. 시스템 전체 합이 항상 0이 되는 것은 이 계정들 덕분이다.

**`DEV_MINT`를 따로 두는 이유:** 개발용 지급은 진짜 입금이 아니다. 섞어 두면 "실제로 들어온 돈"과 "테스트로 만든 돈"을 구분할 수 없다.

---

## 4. 새 테이블

### 4.1 `accounts`

| 필드 | 타입 | 설명 |
|---|---|---|
| `id` | bigserial PK | |
| `account_type` | varchar(32) NOT NULL | 위 6종 |
| `owner_user_id` | bigint NULL | 사용자 계정만 채운다. 시스템 계정은 NULL |
| `asset` | varchar(16) NOT NULL | `KRW`, `BTC`, `ETH` |
| `allows_negative` | boolean NOT NULL | 외부·MINT만 true |
| `created_at` | timestamptz NOT NULL | |

제약:
- `UNIQUE (account_type, owner_user_id, asset)` — NULL을 유일성에 포함시키려면 `COALESCE(owner_user_id, 0)`을 쓴 표현식 유니크 인덱스를 쓴다
- `CHECK`: `account_type IN (...)`
- `CHECK`: 사용자 계정은 `owner_user_id IS NOT NULL`, 시스템 계정은 NULL

### 4.2 `journal_entries` — 사건 하나

| 필드 | 타입 | 설명 |
|---|---|---|
| `id` | bigserial PK | |
| `event_type` | varchar(32) NOT NULL | `DEPOSIT`, `WITHDRAWAL`, `ORDER_HOLD`, `ORDER_RELEASE`, `TRADE`, `DEV_FUND`, `REVERSAL` |
| `idempotency_key` | varchar(160) NOT NULL | **유니크.** 같은 사건이 두 번 기록되는 것을 DB가 막는다 |
| `reference_type` | varchar(32) NOT NULL | `ORDER`, `TRADE`, `TRANSFER`, `DEV_FUND` |
| `reference_id` | bigint NOT NULL | |
| `reverses_journal_id` | bigint NULL | 역분개일 때 원본 분개 |
| `created_at` | timestamptz NOT NULL | |

제약:
- `UNIQUE (idempotency_key)`
- `CHECK`: `event_type = 'REVERSAL'`일 때만 `reverses_journal_id IS NOT NULL`
- `UNIQUE (reverses_journal_id)` — **한 분개는 최대 한 번만 되돌린다**

### 4.3 `postings` — 계정별 한 줄

| 필드 | 타입 | 설명 |
|---|---|---|
| `id` | bigserial PK | |
| `journal_id` | bigint NOT NULL FK | |
| `account_id` | bigint NOT NULL FK | |
| `asset` | varchar(16) NOT NULL | 계정의 자산과 같아야 한다(비정규화, 검산용) |
| `amount` | numeric NOT NULL | 들어오면 양수, 나가면 음수 |
| `created_at` | timestamptz NOT NULL | |

제약·인덱스:
- `CHECK (amount <> 0)` — 0짜리 전기는 의미가 없다
- `INDEX (account_id, id)` — 계정별 합계용
- `INDEX (journal_id)`

**`journal_entries`와 `postings`에는 UPDATE·DELETE를 하지 않는다.** DB 역할 권한으로 막고, 정정은 반드시 역분개로 한다.

### 4.4 `account_balances` — 전기 합의 캐시

| 필드 | 타입 | 설명 |
|---|---|---|
| `account_id` | bigint PK FK | |
| `balance` | numeric NOT NULL | |
| `last_posting_id` | bigint NOT NULL | 어디까지 반영했는지 |
| `updated_at` | timestamptz NOT NULL | |

제약:
- `CHECK`: `allows_negative`가 false인 계정은 `balance >= 0` — 계정 종류가 다른 표에 있으므로, 이 검사는 **트리거 또는 INSERT 시 애플리케이션 검증**으로 건다. 사용자 계정 음수는 자산이 사라진다는 뜻이므로 반드시 막아야 한다.

### 4.5 `transfer_requests` — 가짜 입출금 요청

| 필드 | 타입 | 설명 |
|---|---|---|
| `id` | bigserial PK | |
| `user_id` | bigint NOT NULL | |
| `direction` | varchar(16) NOT NULL | `DEPOSIT` / `WITHDRAWAL` |
| `rail` | varchar(16) NOT NULL | `BANK`(KRW) / `CHAIN`(BTC·ETH) |
| `asset` | varchar(16) NOT NULL | |
| `amount` | numeric NOT NULL CHECK > 0 | 사용자가 보내려는 순수 금액 |
| `fee_amount` | numeric NOT NULL DEFAULT 0 CHECK >= 0 | **접수 시점에 확정해 저장한다.** 첫 구현은 항상 0 |
| `fee_asset` | varchar(16) NOT NULL | 수수료 자산. 지금은 `asset`과 같다 |
| `status` | varchar(16) NOT NULL | **돈의 처리 상태만.** `RECEIVED` → `PROCESSING` → `COMPLETED` / `FAILED` |
| `client_request_key` | varchar(128) NOT NULL | 사용자 쪽 중복 요청 차단. `UNIQUE (user_id, client_request_key)` |
| `external_ref` | varchar(128) NOT NULL | 외부 거래번호 역할. `UNIQUE` |
| `resolution_journal_id` | bigint NULL FK | **확정을 만든 분개.** 완료 분개와 출금 실패 반환 분개를 **둘 다** 이 열로 연결한다 |
| `hold_journal_id` | bigint NULL FK | 출금 접수 시 만든 잠금 분개 |
| `last_checked_at` | timestamptz NULL | 외부 상태를 마지막으로 조회한 시각(§8.6) |
| `next_check_at` | timestamptz NULL | 다음 조회 예정 시각. 조회 작업이 이 열로 대상을 고른다 |
| `check_attempts` | int NOT NULL DEFAULT 0 | 조회 간격을 늘리는 근거 |
| `review_required_at` | timestamptz NULL | **운영자 확인이 필요해진 시각.** 처리 상태와 무관한 별도 표시 |
| `review_reason` | varchar(64) NULL | `EXTERNAL_UNKNOWN`, `EXTERNAL_UNREACHABLE`, `PENDING_TOO_LONG`, `CONFLICTING_TERMINAL_OUTCOME`, `TERMINAL_BEFORE_DISPATCH` |
| `failure_reason` | text | |
| `created_at`, `updated_at` | timestamptz | |

제약:
- `CHECK`: `direction IN ('DEPOSIT', 'WITHDRAWAL')`
- `CHECK`: `rail IN ('BANK', 'CHAIN')`
- `CHECK`: `status IN ('RECEIVED', 'PROCESSING', 'COMPLETED', 'FAILED')`
- `CHECK`: `status = 'COMPLETED'`면 `resolution_journal_id IS NOT NULL`
- `CHECK`: `direction = 'WITHDRAWAL'` ⟺ `hold_journal_id IS NOT NULL` — **출금만 잠금 분개를 가진다.** 출금은 접수 트랜잭션에서 반드시 만들고(§6), 입금은 만들 수 없다
- `CHECK`: `status = 'FAILED'`이고 `direction = 'WITHDRAWAL'`이면 `resolution_journal_id IS NOT NULL` — 잠근 돈이 있었으면 푼 기록도 있어야 한다. 입금 실패는 분개가 없으므로 이 제약에 걸리지 않는다
- `CHECK`: `rail = 'BANK'`면 `asset = 'KRW'`
- `CHECK`: `fee_amount > 0`이면 `fee_asset` 필수(첫 구현에서는 발동하지 않는다)
- `CHECK`: `review_required_at`과 `review_reason`은 **함께 NULL이거나 함께 값이 있다**
- `INDEX (next_check_at) WHERE status = 'PROCESSING'` — 조회 대상 선별용

**허용값을 CHECK로 고정하는 이유.** 이 열들은 애플리케이션이 분기하는 근거다(§8.8·§8.9). 오타 하나로 `PROCESSNG`이 들어가면 그 요청은 어느 분기에도 걸리지 않고, 조회 대상에서도 빠져 **잠긴 채 조용히 남는다.** Go 상수만으로는 마이그레이션·운영 쿼리·직접 UPDATE를 막지 못하므로 DB에서 막는다. `transfer_status_events`의 `source`·`outcome`도 같은 이유로 고정한다(§4.6).

**확정 상태와 확인 표시를 잇는 CHECK 제약은 두지 않는다.** "확정되면 확인 표시가 자동으로 풀린다"는 규칙은 편해 보이지만, **확정된 뒤에 확인 표시를 켜야 하는 경우를 DB가 금지해 버린다.** 그런 경우가 실제로 있다 — 완료로 확정한 출금에 뒤늦게 실패 결과가 오면(§8.8) 돈은 그대로 두고 사람을 불러야 하는데, 그 UPDATE가 제약에 걸려 거부된다. 표시를 지우는 것은 **확정 트랜잭션이 명시적으로 하는 일**이지 DB가 강제할 불변식이 아니다.

**확정 트랜잭션은 네 열을 한 UPDATE로 바꾼다:**

```sql
UPDATE transfer_requests
   SET status = 'COMPLETED',        -- 또는 'FAILED'
       resolution_journal_id = :journal_id,
       review_required_at = NULL,   -- 확인 표시 해제
       review_reason      = NULL,
       next_check_at      = NULL,   -- 조회 중단
       updated_at         = now()
 WHERE id = :id AND status = 'PROCESSING';
```

`WHERE status = 'PROCESSING'`이 이 UPDATE를 **한 번만** 성공하게 만든다(§8.8).

**돈의 처리 상태와 운영자 확인 표시를 나누는 이유.** 한 열에 섞으면 "확인 필요"가 처리 종료 상태가 되고, **담당자가 놓친 출금은 영원히 잠긴 채 남는다.** 나눠 두면 확인 표시가 켜져 있어도 자동 조회는 계속 돌고, 외부 시스템이 복구되는 순간 정상 경로로 마무리된다. 확인 표시는 **사람을 부르는 깃발일 뿐, 처리를 멈추는 스위치가 아니다.** 그리고 깃발은 확정 전이든 후든 켤 수 있어야 한다.

이 분리는 결제 업계의 통상 구조와 같다. Modern Treasury의 payment order도 돈의 진행 상태와 대조 확인 상태를 별도 필드로 두고, 늦게 도착하는 실패까지 처리하도록 안내한다.

**`fee_amount`를 접수 시점에 저장하는 이유.** 처리 도중에 수수료를 다시 계산하면 그사이 정책이 바뀌었을 때 잠근 금액과 최종 차감액이 달라진다. 그 차액이 곧 사라지거나 남아도는 돈이다. §13.2가 지적하는 `SettlementRetryWorker`의 재계산 문제와 같은 부류이므로, 입출금에서는 처음부터 저장값을 쓴다.

**첫 구현의 수수료는 0이지만 0원 전기를 만들지 않는다.** 금액 0짜리 `FEE_INCOME` 전기는 아무 사실도 기록하지 않으면서 분개 수만 늘린다. `fee_amount > 0`일 때만 전기 한 줄이 생긴다.

### 4.6 `transfer_status_events` — 외부에서 알게 된 사실의 기록

알림으로 알았든 우리가 조회해서 알았든 **"외부 상태를 한 번 알게 된 사건"이라는 점에서 같다.** 그래서 표 하나에 담고 `source`로 구분한다. 표를 나누면 운영자가 두 곳을 번갈아 보며 시간순으로 맞춰야 한다.

| 필드 | 타입 | 설명 |
|---|---|---|
| `id` | bigserial PK | |
| `transfer_request_id` | bigint **NOT NULL** FK → `transfer_requests(id)` | 어느 요청의 사건인가. **요청과의 연결은 이 열 하나뿐이다** |
| `source` | varchar(16) NOT NULL | `CALLBACK`(외부가 알려줌) / `POLL`(우리가 물어봄) |
| `event_key` | varchar(160) NOT NULL | **UNIQUE.** 중복 차단 |
| `outcome` | varchar(16) NOT NULL | `SUCCESS` / `FAILURE` / `PENDING` / `UNKNOWN` |
| `payload` | jsonb | **허용 목록에 있는 필드만**(아래) |
| `received_at` | timestamptz NOT NULL | |

제약:
- `UNIQUE (event_key)`
- `CHECK`: `source IN ('CALLBACK', 'POLL')`
- `CHECK`: `outcome IN ('SUCCESS', 'FAILURE', 'PENDING', 'UNKNOWN')`
- `INDEX (transfer_request_id, received_at)` — 운영자가 한 요청의 사건을 시간순으로 본다

**`external_ref`를 이 표에 두지 않는다.** 요청과의 연결은 `transfer_request_id` FK 하나로 충분하고, 같은 값을 두 표에 적으면 언젠가 서로 다른 값이 들어간다 — §1.1의 `Wallet.KRW`가 정확히 그 문제였다. 외부 식별자가 필요하면 `transfer_requests.external_ref`를 조인해서 읽는다.

**들어온 알림은 요청 id로 먼저 바꾼다.** 외부는 `external_ref`를 들고 오므로, `transfer_requests.external_ref`(UNIQUE)로 요청을 찾아 그 id를 쓴다. **찾지 못하면 사건을 만들지 않고 거부한다** — FK가 그것을 강제한다. 우리가 모르는 요청의 알림을 저장할 이유가 없다.

**`payload`는 받은 그대로 넣지 않는다.** 외부가 보낸 것을 통째로 저장하면 우리가 예상하지 못한 내용이 원장 옆 표에 그대로 쌓인다. 저장하는 필드를 코드에 목록으로 고정하고, **목록에 없는 필드는 버린다.**

| 저장하는 필드 | 왜 |
|---|---|
| 외부 거래 식별자 | 대조 |
| 외부 상태 코드·사유 문자열 | 실패 원인 파악 |
| 외부가 보고한 금액·자산 | 우리 기록과 다른지 확인 |
| 외부 타임스탬프 | 순서 재구성 |

목록에 없는 필드가 오면 **버리되 그 사실을 로그로 남긴다.** 조용히 버리면 외부 규격이 바뀐 것을 눈치채지 못한다.

**로그에는 필드 이름과 개수만 적고 값은 적지 않는다.**

```
unknown transfer payload fields: count=2 names=[account_holder, memo]
```

값까지 남기면 허용 목록으로 막은 내용이 로그를 통해 그대로 새어 나간다. 규격이 바뀐 것을 아는 데 필요한 것은 **"모르는 필드가 왔다"와 "그 이름이 무엇인가"**뿐이다.

`event_key` 규칙:

| source | 키 |
|---|---|
| `CALLBACK` | `callback:{rail}:{외부 event id}` |
| `POLL` | `poll:{transfer_request_id}:{check_attempts}` |

**`CALLBACK` 키에 `rail`을 넣는 이유.** 외부 event id는 가짜 은행과 가짜 체인이 각자 발급하므로 서로 다른 두 시스템이 같은 문자열을 쓸 수 있다. `rail`을 앞에 붙이면 그 충돌이 구조적으로 불가능하다.

**`POLL` 키에 `transfer_request_id`를 쓰는 이유.** 이 표는 요청을 FK로만 가리키므로(위) 키에도 같은 식별자를 쓴다. `external_ref`를 쓰면 §4.6이 없애기로 한 중복 연결이 키 문자열 안에서 되살아난다. `check_attempts`가 붙어 조회 시도마다 새 키가 된다 — 같은 시도가 재실행되면 같은 키가 되어 중복으로 걸러진다.

**중복은 `ON CONFLICT (event_key) DO NOTHING RETURNING`으로 판정한다.** 반환된 행이 없으면 이미 본 사건이다. 유니크 위반을 **일으킨 뒤 잡지 않는다** — PostgreSQL에서 그 위반은 트랜잭션을 abort시켜 이후 문장을 전부 막는다(§7·§8.8).

**알림과 조회가 같은 결과를 따로 가져오는 것은 막지 않는다.** 조회로 `SUCCESS`를 먼저 알고 나중에 알림이 도착하면 `event_key`가 다르므로 두 줄 다 남는다. 그것이 옳다 — 실제로 두 번 알게 된 것이니 기록도 두 줄이어야 한다. **돈이 두 번 움직이는 것은 §7의 3층과 §8.8의 행 잠금이 막는다:** 둘 다 같은 분개 멱등성 키 `withdraw-settle:{external_ref}`를 만들므로 두 번째는 새 분개를 만들지 못한다.

`PENDING`·`UNKNOWN` 조회 결과도 남긴다. 운영자에게 "언제 물어봤고 무엇을 들었나"를 보여 주는 것이 이 표의 두 번째 역할이다. 조회 간격이 지수적으로 늘어나므로(§8.6) 줄 수는 요청당 수십 건을 넘지 않는다.

---

## 5. 사건별 기록 예시

수수료율은 실제 값 **0.05%**(`0.0005`)를 쓴다
([market_rules.json](../../../config/market_rules.json) `fee_rate`,
[market_rules_registry.go:55](../../../internal/service/market_rules_registry.go) `defaultTradingFeeRate`).

### 5.1 개발용 지급 — 사용자 1에게 KRW 1,000,000

| 계정 | 자산 | 금액 |
|---|---|---|
| `DEV_MINT` | KRW | −1,000,000 |
| `USER_AVAILABLE(1)` | KRW | +1,000,000 |

합계 0. 지금은 상대편 없이 한 줄만 적는다(§1.2).

**개발용 지급은 유지하되, 잔액을 직접 늘리는 방식은 없앤다.** 지금 `dev_wallet_service.go`는 지갑을 직접 upsert한다. 새 구조에서는 위 분개를 `LedgerService`에 통과시키는 것 외의 경로가 없다. 지켜야 할 조건:

| 조건 | 어떻게 |
|---|---|
| 잔액 직접 수정 금지 | `postings` INSERT는 `LedgerService`만 한다(§13.1과 같은 규칙) |
| 원장과 잔액 변경이 한 DB 작업 | 분개 + 전기 2줄 + 캐시 2건이 한 트랜잭션 |
| 같은 요청 번호면 한 번만 지급 | `idempotency_key = devfund:{user}:{asset}:{요청 UUID}`(§7) |
| 개발 도구가 켜진 환경에서만 | 기존 dev-tools 활성화 조건 유지 |
| 개발 도구 전용 비밀값 검증 | 기존 검증 유지 |
| 거래·입출금 수수료 없음 | 전기 2줄뿐. `FEE_INCOME` 없음 |
| 실제 서비스 화면에 노출 금지 | 기존과 동일 |

**용도를 분리한다.** 셋을 섞으면 "이 돈이 어디서 왔나"를 원장에서 구분할 수 없다.

| 쓰는 곳 | 수단 |
|---|---|
| 단위·통합 테스트, k6 사전 준비 | 개발용 자산 지급(`DEV_MINT`) |
| 사용자가 입금 과정을 체험하는 E2E | 가짜 은행·가짜 코인 입금(`EXTERNAL_BANK` / `EXTERNAL_CHAIN`) |
| 운영 환경 | 개발용 지급 비활성화 |

### 5.2 가짜 은행 입금 — 사용자 1이 KRW 500,000 입금

**접수 시점:** 기록하지 않는다. `transfer_requests`만 `RECEIVED`로 만든다. **돈은 아직 오지 않았다.**

**완료 알림 도착:**

| 계정 | 자산 | 금액 |
|---|---|---|
| `EXTERNAL_BANK` | KRW | −500,000 |
| `USER_AVAILABLE(1)` | KRW | +500,000 |

**실패 알림 도착:** 분개 없음. 상태만 `FAILED`.

### 5.3 가짜 코인 입금 — 사용자 1이 BTC 0.5 입금

| 계정 | 자산 | 금액 |
|---|---|---|
| `EXTERNAL_CHAIN` | BTC | −0.5 |
| `USER_AVAILABLE(1)` | BTC | +0.5 |

### 5.4 가짜 은행 출금 — 사용자 1이 KRW 200,000 출금

**두 단계로 나눈다.** 잠금과 차감은 역할이 다르므로 둘 다 필요하다.

| 시점 | 하는 일 |
|---|---|
| 접수 | 사용 가능 잔액에서 빼서 **출금 대기 금액**으로 옮긴다 |
| 완료 | 출금 대기 금액을 실제로 고객 자산에서 **제거**한다 |
| 실패 | 출금 대기 금액을 사용 가능 잔액으로 **되돌린다** |

접수 시 잠그기만 하고 완료 시 차감하지 않으면 나간 돈이 장부에 남고, 접수 시 바로 빼면 실패했을 때 되돌릴 근거가 없다.

**접수 시점(분개 1) — `출금액 + 수수료`를 함께 잠근다.** 수수료를 빼놓고 잠그면 완료 시점에 수수료 낼 돈이 이미 다른 주문에 쓰였을 수 있다. 아래 예시는 첫 구현대로 수수료 0이므로 잠금액이 출금액과 같다.

| 계정 | 자산 | 금액 |
|---|---|---|
| `USER_AVAILABLE(1)` | KRW | −200,000 |
| `USER_LOCKED(1)` | KRW | +200,000 |

**완료 알림(분개 2) — 묶인 돈을 바깥으로 보낸다:**

| 계정 | 자산 | 금액 |
|---|---|---|
| `USER_LOCKED(1)` | KRW | −200,000 |
| `EXTERNAL_BANK` | KRW | +200,000 |

**실패 알림(분개 2′) — 묶인 돈을 되돌린다:**

| 계정 | 자산 | 금액 |
|---|---|---|
| `USER_LOCKED(1)` | KRW | −200,000 |
| `USER_AVAILABLE(1)` | KRW | +200,000 |

접수 즉시 available에서 빼기 때문에, 출금 처리 중에 같은 돈으로 주문을 낼 수 없다.

**수수료가 0보다 클 때의 완료 분개**(첫 구현에서는 발생하지 않는다):

| 계정 | 자산 | 금액 |
|---|---|---|
| `USER_LOCKED(1)` | KRW | −(출금액 + 수수료) |
| `EXTERNAL_BANK` | KRW | +출금액 |
| `FEE_INCOME` | KRW | +수수료 |

**수수료 금액은 `transfer_requests.fee_amount`에 저장된 값을 쓴다.** 처리 도중에 다시 계산하지 않는다. 수수료가 0이면 `FEE_INCOME` 줄 자체를 만들지 않는다.

### 5.5 매수 주문 잠금 — 100,000원짜리 주문(수수료 포함 100,050원)

| 계정 | 자산 | 금액 |
|---|---|---|
| `USER_AVAILABLE(1)` | KRW | −100,050 |
| `USER_LOCKED(1)` | KRW | +100,050 |

### 5.6 주문 취소 해제

| 계정 | 자산 | 금액 |
|---|---|---|
| `USER_LOCKED(1)` | KRW | −100,050 |
| `USER_AVAILABLE(1)` | KRW | +100,050 |

### 5.7 체결 — 사용자 1이 사용자 2에게서 BTC 0.001을 100,000원에 산다

수수료는 양쪽 각 50원(100,000 × 0.0005).

| 계정 | 자산 | 금액 | 뜻 |
|---|---|---|---|
| `USER_LOCKED(1)` | KRW | −100,050 | 매수자 묶인 돈에서 빠짐 |
| `USER_AVAILABLE(2)` | KRW | +99,950 | 매도자가 받는 돈(수수료 뺀 값) |
| `FEE_INCOME` | KRW | +100 | 양쪽 수수료 |
| `USER_LOCKED(2)` | BTC | −0.001 | 매도자 묶인 코인에서 빠짐 |
| `USER_AVAILABLE(1)` | BTC | +0.001 | 매수자가 받는 코인 |

**자산별 합계:** KRW `−100,050 + 99,950 + 100 = 0` ✅ / BTC `−0.001 + 0.001 = 0` ✅

지금은 `FEE_INCOME` 줄이 없어서 KRW 합이 −100이 된다(§1.6).

**매수자 잠금액이 체결액보다 클 때**(지정가가 유리하게 체결된 경우) 남는 차액은 같은 분개에 한 줄 더 넣어 available로 되돌린다:

| 계정 | 자산 | 금액 |
|---|---|---|
| `USER_LOCKED(1)` | KRW | −(잠금액) |
| `USER_AVAILABLE(1)` | KRW | +(잠금액 − 체결액 − 수수료) |
| ... | | |

### 5.8 시장가 주문 잔액 반환

| 계정 | 자산 | 금액 |
|---|---|---|
| `USER_LOCKED(1)` | KRW | −(남은 예산) |
| `USER_AVAILABLE(1)` | KRW | +(남은 예산) |

### 5.9 역분개 — 잘못 기록한 체결을 되돌린다

원본 분개의 모든 전기를 부호만 뒤집어 새 분개로 적는다. `event_type='REVERSAL'`, `reverses_journal_id=원본 id`.

| 계정 | 자산 | 금액 |
|---|---|---|
| `USER_LOCKED(1)` | KRW | **+**100,050 |
| `USER_AVAILABLE(2)` | KRW | **−**99,950 |
| `FEE_INCOME` | KRW | **−**100 |
| ... | | |

역분개도 자산별 합이 0이다. 원본은 그대로 남는다.

---

## 6. 서비스별 DB 처리 경계

**규칙: 하나의 사건은 하나의 트랜잭션이다.** 분개·전기·잔액 캐시·상태 변경이 함께 성공하거나 함께 실패한다.

| 서비스 | 한 트랜잭션에 들어가는 것 |
|---|---|
| 개발용 지급 | 분개 + 전기 2줄 + 잔액 캐시 2건 |
| 입금 접수 | `transfer_requests` INSERT (`RECEIVED`) — **분개 없음** |
| 입금 완료 알림 | `transfer_status_events` INSERT + 요청 `COMPLETED` + 분개 + 전기 2줄 + 잔액 캐시 |
| 출금 접수 | `transfer_requests` INSERT + **잠금 분개** + 전기 2줄 + 잔액 캐시 |
| 출금 완료/실패 알림 | 콜백 INSERT + 요청 상태 + **정산 분개** + 전기 2줄(수수료 > 0이면 3줄) + 잔액 캐시 |
| 출금 상태 조회 — 확정 | 결과가 `SUCCESS`·`FAILURE`면 위 줄과 **같은 코드 경로(`ResolveTransfer`)·같은 트랜잭션·같은 멱등성 키.** 조회 전용 정산 경로를 따로 만들지 않는다. 트랜잭션은 `transfer_requests` 행 `SELECT ... FOR UPDATE`로 시작한다(§8.8) |
| 출금 상태 조회 — 미확정 | 결과가 `PENDING`·`UNKNOWN`이면 **`RecordObservation`**(별도 함수). 역시 행 `SELECT ... FOR UPDATE`로 시작하고 잠근 뒤 상태를 다시 확인한다(§8.9): `PROCESSING`이면 사건 + 조회 일정 갱신, terminal이면 **사건만**, `RECEIVED`면 거부. **분개를 만드는 코드가 이 경로에 없다** |
| 주문 잠금 | (기존 `HoldCoordinator` 유지) 멱등성 키 + 주문 INSERT + 분개 + 전기 + 잔액 캐시 |
| 주문 취소 해제 | 주문 상태 + 분개 + 전기 + 잔액 캐시 |
| 체결 정산 | 주문 체결량 + 분개 + 전기 5줄 + 잔액 캐시 4건 + outbox PROCESSED |
| 시장가 완료 | 주문 상태 + 분개 + 전기 2줄 + 잔액 캐시 |

**잠금 순서를 고정한다.** 교착을 막기 위해 한 트랜잭션에서 여러 계정을 잠글 때는 항상 `account_id` 오름차순으로 `SELECT ... FOR UPDATE` 한다. 지금은 지갑을 `lockSettlementWallets`(settlement_service.go:228)에서 잠그는데, 새 구조에서도 같은 원칙을 계정 단위로 옮긴다.

---

## 7. 중복 요청 방지

층이 세 개다. 각 층이 막는 것이 다르다.

| 층 | 열쇠 | 막는 것 |
|---|---|---|
| 1. 사용자 요청 | `transfer_requests.UNIQUE(user_id, client_request_key)` | 같은 버튼 두 번 누르기 |
| 2. 외부 사건 | `transfer_status_events.UNIQUE(event_key)` | 가짜 은행이 **같은 알림**을 두 번 보냄 |
| 3. 원장 | `journal_entries.UNIQUE(idempotency_key)` | 위 둘을 뚫고 온 재실행 |

**3층이 마지막 방어선이다.** 어떤 경로로 오든 같은 사건은 같은 `idempotency_key`를 만들어야 하고, 두 번째 INSERT는 DB가 거부한다.

**2층은 서로 다른 경로를 막지 못한다.** 알림과 상태 조회는 `event_key`가 다르므로 둘 다 통과한다(§4.6). 같은 출금이 알림과 조회로 두 번 확정되는 것을 막는 것은 3층과 **§8.8의 행 잠금**이다. 따라서 조회 경로가 만드는 멱등성 키는 알림 경로와 **글자 하나까지 같아야 한다** — `external_ref`만으로 만들고 조회 시각·시도 횟수 같은 것을 섞지 않는다.

**3층 충돌은 오류가 아니라 답이다.** `idempotency_key` 중복은 "이 사건은 이미 기록됐다"는 사실을 알려 주는 것이지 실패가 아니다. 세 층 어디서 충돌하든 처리는 같다: **기존 기록을 찾아 그것으로 진행한다.**

**그래서 유니크 위반을 일으킨 뒤 잡지 않는다.** PostgreSQL에서 유니크 위반은 트랜잭션을 abort시켜 그 뒤 문장을 전부 막는다. `INSERT ... ON CONFLICT DO NOTHING RETURNING`으로 **위반이 발생하지 않게** 쓰고, 반환 행이 없는 것을 "이미 있음"으로 읽는다(§8.8).

키 만드는 규칙(결정적이어야 한다):

| 사건 | `idempotency_key` |
|---|---|
| 개발용 지급 | `devfund:{user}:{asset}:{요청 UUID}` |
| 입금 완료 | `deposit:{external_ref}` |
| 출금 접수 잠금 | `withdraw-hold:{transfer_request_id}` |
| 출금 완료 | `withdraw-settle:{external_ref}` |
| 출금 실패 반환 | `withdraw-refund:{external_ref}` |
| 주문 잠금 | `order-hold:{order_id}` |
| 주문 해제 | `order-release:{order_id}:{사유}` |
| 체결 | `trade:{trade_idempotency_key}` — 기존 `trades.idempotency_key` 재사용 |
| 역분개 | `reversal:{원본 journal_id}` |

**주문 해제에 사유를 넣는 이유:** 한 주문이 "사용자 취소"와 "시장가 잔액 반환" 양쪽으로 해제될 일은 없지만, 키가 겹치면 둘 중 하나가 조용히 무시된다. 사유를 넣어 그 위험을 없앤다.

---

## 8. 실패·재시작 복구

### 8.1 원칙

**모든 복구는 "다시 시도"다. 보정 계산을 하지 않는다.** 같은 입력으로 다시 실행하면 멱등성 키가 두 번째를 막는다. 그래서 "몇 번 실행됐는지" 추적할 필요가 없다.

### 8.2 경우별

| 상황 | 복구 |
|---|---|
| 분개 커밋 전 프로세스 죽음 | 아무것도 남지 않는다. 요청은 이전 상태 그대로. 재시도하면 정상 진행 |
| 분개 커밋 후, 응답 전 죽음 | 재시도 시 `idempotency_key` 충돌 → 기존 분개를 찾아 성공으로 응답 |
| 출금 접수 후 외부 알림이 안 옴 | §8.4의 상태 판정에 따른다. 시간만으로는 결정하지 않는다 |
| 체결 정산 실패 | 기존 `FailedSettlement` + `SettlementRetryWorker` 유지. 재시도가 같은 `trade:{key}`를 쓰므로 중복 정산이 불가능 |
| 잔액 캐시만 어긋남 | 전기에서 다시 계산해 덮어쓴다. 전기가 진실이다 |

### 8.3 가짜 외부 처리기

`FakeTransferProcessor`가 `PROCESSING` 요청을 읽어 지연·실패·중복 알림을 흉내 낸다. **테스트에서는 시계를 흉내 내지 않고 직접 콜백을 호출한다** — 시간에 의존하는 테스트는 이 저장소에서 이미 여러 번 문제를 일으켰다.

가짜 처리기는 알림을 보내는 것 외에 **상태 조회 창구**도 제공한다: `GetTransferStatus(external_ref) → SUCCESS / FAILURE / PENDING / UNKNOWN`. 실제 은행·체인 연동에도 이 창구가 있고, 이것이 없으면 §8.4의 판정을 시간 추측으로 대신하게 된다.

재현해야 하는 다섯 경우:

| # | 시나리오 | 기대 결과 |
|---|---|---|
| 1 | 완료 알림 유실 → 상태 조회에서 `SUCCESS` 확인 | 최종 차감(완료 분개) |
| 2 | 완료 알림 유실 → 상태 조회에서 `FAILURE` 확인 | 자동 반환(실패 분개). 잠금 해제 |
| 3 | 외부 시스템도 응답 없음(`UNKNOWN`) | 잠금 유지 + 운영 확인 표시. **자동으로 돈을 움직이지 않는다.** 조회는 계속된다 |
| 4 | 3번 이후 외부가 복구되어 `SUCCESS` 확인 | 완료 분개를 **정확히 1회** 생성, 확인 표시 해제. 사람이 손대지 않아도 마무리된다 |
| 5 | 확정 후 **반대** 결과가 뒤늦게 도착 | 돈·상태 불변, 분개 추가 없음, `CONFLICTING_TERMINAL_OUTCOME` 확인 표시(§8.8) |

가짜 처리기는 **알림과 조회를 같은 시점에 내보낼 수 있어야 한다.** 그래야 §8.8의 행 잠금이 실제로 검증된다. 1번을 "알림이 아예 안 온다"로만 만들면 두 경로가 경쟁하는 상황이 한 번도 재현되지 않는다.

### 8.4 출금 미완료 판정 — 시간은 경고 기준이지 처리 기준이 아니다

시간이 지났다는 사실만으로는 출금 실패가 증명되지 않는다. **오래 걸린 성공을 실패로 단정해 반환하면, 바깥으로 나간 돈을 안에서도 돌려준 것이 되어 자산이 복제된다.** 그래서 판정은 시간이 아니라 상태로 한다.

| 상태 | 근거 | 돈 | 운영 확인 표시 | 조회 |
|---|---|---|---|---|
| 외부 전송 **전**임이 확실 | `RECEIVED`이고, 처리기에 넘어간 기록도 **terminal 사건 관측도 전혀 없음** | 일정 시간 후 **자동 취소·잠금 해제 가능** | — | 종료 |
| 외부 전송 **실패** 확인 | 실패 알림 또는 조회 `FAILURE` | 잠금 해제(반환 분개) | 확정 UPDATE가 해제 | 종료 |
| 외부 전송 **성공** 확인 | 성공 알림 또는 조회 `SUCCESS` | 최종 차감(완료 분개) | 확정 UPDATE가 해제 | 종료 |
| 확정 후 **반대** 결과 도착 | 이미 terminal인데 반대 outcome | **불변** | 다시 설정(`CONFLICTING_TERMINAL_OUTCOME`) | 이미 종료됨 |
| 보낸 적 없는데 결과 도착 | `RECEIVED`인데 terminal outcome | **불변** | 설정(`TERMINAL_BEFORE_DISPATCH`) | 종료 |
| 알림만 안 왔고 조회는 가능 | 조회 `PENDING` | 잠금 유지 | 임계 초과 시 설정 | **계속** |
| 전송 여부 **불확실** | 조회 `UNKNOWN` 또는 외부 무응답 | 잠금 유지 | 설정 | **계속(간격 확대)** |

**자동 취소가 허용되는 칸은 첫 줄 하나뿐이다.** 나머지는 확정된 사실이 있을 때만 돈이 움직인다. 마지막 줄에서 보듯 **확정 이후에도 확인 표시는 다시 켜질 수 있다** — 그래서 §4.5에 "terminal이면 표시가 NULL"이라는 제약을 두지 않았다.

### 8.5 운영 확인 표시는 조회를 멈추지 않는다

**핵심 규칙: `review_required_at`은 사람을 부르는 깃발이지 처리를 멈추는 스위치가 아니다.**

확인 표시를 처리 종료 상태로 만들면 담당자가 놓친 출금이 **영원히 잠긴 채** 남는다. 반대로 조회를 계속하면 외부 시스템이 복구됐을 때 확정 결과에 따라 안전하게 마무리된다. 사람의 개입은 **더 빨리 끝내기 위한 것**이지, 끝나기 위한 **조건**이 아니어야 한다.

동작:

```
조회 결과 PENDING 또는 UNKNOWN
  → 경고 시간 초과
  → 돈은 그대로 잠금 (분개 없음)
  → review_required_at · review_reason 기록
  → 운영자에게 알림
  → 느린 간격으로 상태 조회 계속

  ├─ 이후 SUCCESS 확인
  │    → 출금 완료 분개를 정확히 1회 생성
  │    → 운영 확인 표시 해제
  │
  ├─ 이후 FAILURE 확인
  │    → 잠금 해제 분개를 정확히 1회 생성
  │    → 운영 확인 표시 해제
  │
  └─ 계속 UNKNOWN
       → 분개 없음 · 잠금 유지 · 확인 표시 유지
```

**"정확히 1회"의 근거는 조회 경로가 알림 경로와 같은 분개 멱등성 키를 쓴다는 것뿐이다**(§7). 조회 전용 정산 코드를 따로 만들면 그 보장이 사라진다. 조회는 **"알림이 왔다면 탔을 그 경로"를 대신 태우는 것**이고, 그 이상을 하지 않는다.

### 8.6 조회 간격

간격을 늘리는 이유는 두 가지다. 외부 서비스가 반복 조회를 제한할 수 있고(Stripe도 과도한 폴링을 제한한다고 안내한다), 하루 넘게 미확정인 요청을 10초마다 두드릴 실익이 없다.

| 항목 | 값 |
|---|---|
| 첫 조회 | 접수 후 10초 |
| 이후 | 직전 간격 × 2 |
| 상한 | 1시간 |
| 운영 확인 표시 임계 | 미확정 상태로 **30분** 경과 |
| 조회 종료 | `SUCCESS`·`FAILURE` 확정 시에만 |

`TransferStatusPoller`가 `status = 'PROCESSING' AND next_check_at <= now()`인 요청을 골라 조회하고, 결과를 `transfer_status_events`에 남긴 뒤 `check_attempts`·`next_check_at`을 갱신한다.

**시간 임계값은 `review_required_at`을 켜는 데만 쓴다. 어떤 분개도 만들지 않는다.** 이 문서에서 시간이 돈을 움직이는 곳은 없다.

### 8.7 보여 주는 것

"자동 조회가 몰래 처리한다"는 우려는 감추지 않는 것으로 해결한다.

| 대상 | 보이는 것 |
|---|---|
| 사용자 | `처리 지연 · 외부 상태 자동 확인 중` |
| 운영자 | 마지막 조회 시각, 다음 조회 시각, 조회 결과 기록(§4.6), `review_reason` |
| 확정 후 | 사용자와 운영자 모두에게 완료 또는 실패 알림 |

돈은 외부의 `SUCCESS` 또는 `FAILURE`가 확인됐을 때만 움직인다. 그 사실이 화면과 `transfer_status_events`에 남으므로, 나중에 "왜 이 시각에 처리됐나"를 되짚을 수 있다.

### 8.8 확정 경로 — 행을 잠그고 한 번만 확정한다

알림과 조회는 **동시에 도착할 수 있다.** 알림이 오는 순간 조회 작업이 같은 요청을 물어보고 있을 수 있고, 둘 다 `SUCCESS`를 들고 온다. 멱등성 키만으로는 부족하다 — 두 트랜잭션이 나란히 분개를 만들려다 하나가 유니크 위반으로 죽으면, 그 요청의 상태 갱신까지 함께 롤백된다.

**그래서 확정은 코드 경로 하나이고, 그 경로는 행 잠금으로 시작한다.**

경로는 둘로 나뉜다. **돈을 움직일 수 있는 경로에는 terminal 결과만 들어간다.**

| 함수 | 받는 outcome | 하는 일 |
|---|---|---|
| `ResolveTransfer` | **`SUCCESS` / `FAILURE`만** | 사건 기록 + 확정(분개·상태) |
| `RecordObservation` | **`PENDING` / `UNKNOWN`만** | 사건 기록 + 조회 일정 갱신. **분개를 만드는 코드가 없다** |

`ResolveTransfer`가 `PENDING`을 받으면 프로그래밍 오류로 즉시 거부한다. 한 함수가 네 outcome을 모두 받으면 "미확정인데 확정 분기로 떨어지는" 실수가 가능해지고, 그 실수는 돈을 움직인다. **분기로 막지 말고 함수를 나눠서 막는다.**

```
ResolveTransfer(transfer_request_id, source, event_key, outcome ∈ {SUCCESS, FAILURE}, payload):

  트랜잭션 시작

  1. SELECT * FROM transfer_requests
       WHERE id = :id
       FOR UPDATE                     ← 여기서 경쟁이 직렬화된다

  2. INSERT INTO transfer_status_events (...)
       VALUES (...)
       ON CONFLICT (event_key) DO NOTHING
       RETURNING id
     → 반환된 행이 없으면 같은 사건을 이미 봤다. 커밋하고 종료

  3. 분기:

     status = 'RECEIVED'
       → 아직 외부로 보낸 적이 없는데 결과가 왔다. **돈을 움직이지 않는다**
       → review_required_at = now()
       → review_reason = 'TERMINAL_BEFORE_DISPATCH'
       → 커밋. 분개 없음

     status = 'PROCESSING'
       → 확정한다 (아래 4)

     status가 이미 terminal이고, outcome이 그 terminal과 **같음**
       → 사건만 남기고 커밋. 분개 없음. 확인 표시 건드리지 않음

     status가 이미 terminal이고, outcome이 그 terminal과 **반대**
       → 돈과 status를 **그대로 둔다**
       → review_required_at = now()
       → review_reason = 'CONFLICTING_TERMINAL_OUTCOME'
       → 커밋. 분개 없음

  4. 확정:
       INSERT INTO journal_entries (...)
         ON CONFLICT (idempotency_key) DO NOTHING
         RETURNING id
       → 반환된 행이 없으면 SELECT ... WHERE idempotency_key = :key로 기존 id를 읽는다
       → 전기는 새 분개를 만들었을 때만 INSERT한다
       §4.5의 단일 UPDATE (status·resolution_journal_id·review 2열·next_check_at)

  커밋
```

**1번 `FOR UPDATE`가 이 절 전체의 근거다.** 이것이 없으면 3번의 상태 검사와 4번의 UPDATE 사이에 다른 트랜잭션이 끼어들 수 있다. 잠근 뒤에 읽은 `status`는 우리가 커밋할 때까지 바뀌지 않는다.

**`RECEIVED`에서 terminal 결과가 오는 것은 우리 인식과 외부 현실이 어긋났다는 신호다.** §8.4는 이 상태를 "외부 전송 전임이 확실"로 보고 자동 취소를 허용하는 유일한 칸으로 삼는다. 그 전제가 깨졌으므로 **자동 취소도 확정도 하지 않고 사람을 부른다.** 여기서 `SUCCESS`를 그대로 확정하면, 보낸 적 없다고 믿은 돈을 나갔다고 처리하게 된다.

**중복은 예외가 아니라 `ON CONFLICT ... DO NOTHING RETURNING`으로 처리한다.** PostgreSQL에서 유니크 위반은 **트랜잭션 전체를 abort 상태로 만든다.** 그 뒤로는 SAVEPOINT 없이 아무 문장도 실행할 수 없으므로, "위반을 잡아서 같은 트랜잭션에서 커밋한다"는 계획은 성립하지 않는다. 2번과 4번 모두 위반을 **일으키지 않는** 방식으로 쓴다:

| 위치 | 반환 있음 | 반환 없음 |
|---|---|---|
| 2번 사건 INSERT | 처음 보는 사건 → 계속 진행 | 이미 본 사건 → 커밋하고 종료 |
| 4번 분개 INSERT | 새 분개 → 전기 INSERT | 이미 있는 분개 → 기존 id를 SELECT, **전기는 만들지 않는다** |

4번에서 전기를 조건부로 만드는 것이 중요하다. 분개가 이미 있다는 것은 전기도 이미 있다는 뜻이므로, 무조건 INSERT하면 같은 분개에 전기가 두 벌 생겨 §9 검사 1이 깨진다.

**같은 결과의 재관측이 오류가 아닌 이유.** 조회로 `SUCCESS`를 알고 확정한 뒤 알림이 도착하는 것은 **정상이고 흔하다**(§4.6). 이때 할 일은 사건 한 줄을 남기는 것뿐이다. 오류로 처리하면 외부가 알림을 재전송하고, 우리는 계속 실패를 돌려주는 고리에 들어간다.

**반대 결과가 와도 돈을 되돌리지 않는 이유.** 완료로 확정한 출금에 뒤늦게 `FAILURE`가 오면, 참일 수도 있고 외부의 오류일 수도 있다. 어느 쪽인지 **우리는 모른다.** 여기서 자동으로 반환 분개를 만들면, 실제로 나간 돈을 안에서도 돌려준 것이 되어 자산이 복제된다 — §8.4가 막으려는 바로 그 사고다. 역분개로 정정할 수 있지만(§5.9) **그 판단은 사람이 한다.** 시스템은 돈을 그대로 두고 깃발을 켠다.

**분개 충돌을 오류로 처리하지 않는 이유.** 충돌은 "같은 사건이 이미 기록됐다"는 사실이고, 그것은 우리가 원하는 상태다. 예외로 올려 트랜잭션을 죽이면 2번에서 남긴 사건 기록과 4번의 상태 갱신이 함께 사라져, **다음 재시도가 같은 자리에서 또 죽는다.** 기존 분개를 가져와 `resolution_journal_id`에 연결하면 그 요청은 그 자리에서 정상 종료된다. 기존 `duplicateSettlementResult`(§1.8)가 이미 이 패턴이므로 새로 만드는 규칙이 아니다.

**입금에도 같은 경로를 쓴다.** 입금은 잠금 단계가 없어 단순하지만, 알림과 조회가 경쟁하는 구조는 똑같다. 경로를 나누면 한쪽에만 잠금이 빠진다.

### 8.9 미확정 경로도 같은 행을 잠근다

`RecordObservation`은 분개를 만들지 않지만 **`transfer_requests`를 UPDATE한다** — `next_check_at`, `check_attempts`, 경우에 따라 review 두 열. 그러므로 `ResolveTransfer`와 같은 행을 두고 경쟁하고, 잠그지 않으면 확정 직후의 조회가 **끝난 요청의 조회 일정을 되살린다.**

```
RecordObservation(transfer_request_id, event_key, outcome ∈ {PENDING, UNKNOWN}, payload):

  트랜잭션 시작

  1. SELECT * FROM transfer_requests WHERE id = :id FOR UPDATE

  2. INSERT INTO transfer_status_events (...)
       ON CONFLICT (event_key) DO NOTHING RETURNING id
     → 반환 없으면 이미 본 사건. 커밋하고 종료

  3. 잠근 뒤 다시 읽은 status로 분기:

     'PROCESSING'
       → last_checked_at · next_check_at · check_attempts 갱신
       → 임계 초과면 review_required_at · review_reason 설정

     'COMPLETED' / 'FAILED'
       → **사건만 남긴다.** status · next_check_at · review 두 열 모두 건드리지 않는다
       → 커밋

     'RECEIVED'
       → 아직 조회 대상이 아니다. 내부 호출 순서 오류로 거부한다
       → 분개 없음, UPDATE 없음

  커밋
```

**3번의 재확인이 이 절의 이유다.** 조회 작업은 `status = 'PROCESSING'`인 요청을 골라 외부에 물어본다. 그런데 외부 응답을 기다리는 사이에 알림이 도착해 확정될 수 있다. 잠근 뒤 다시 읽지 않으면, 골랐을 때의 낡은 상태를 근거로 `next_check_at`을 다시 채워 **이미 끝난 요청을 영원히 조회하게 된다.** `review_required_at`도 마찬가지로 되살아나, 확정 트랜잭션이 방금 지운 깃발이 다시 켜진다.

**`RECEIVED`를 거부하는 이유는 외부가 아니라 우리 쪽 문제이기 때문이다.** 아직 처리기에 넘기지도 않은 요청을 우리가 조회했다면 그것은 호출 순서 오류다. 조용히 일정만 갱신하면 그 결함이 계속 숨는다. §8.8이 `RECEIVED` + terminal을 **외부와의 불일치**로 보고 사람을 부르는 것과 달리, 이쪽은 **내부 결함**이므로 거부한다.

`ResolveTransfer`와 `RecordObservation`은 둘 다 1번에서 같은 행을 잠그므로 **서로를 직렬화한다.** 어느 쪽이 먼저 잠그든 나중 쪽은 갱신된 상태를 본다.

---

## 9. 잔액 검산

세 가지를 본다. 모두 원장에서만 계산한다.

**검사 1 — 분개별 합계 0**

```sql
SELECT journal_id, asset, SUM(amount)
FROM postings GROUP BY journal_id, asset HAVING SUM(amount) <> 0;
```

한 줄이라도 나오면 심각한 오류다.

**검사 2 — 캐시와 전기 합 일치**

```sql
SELECT a.id, b.balance, COALESCE(SUM(p.amount), 0) AS computed
FROM accounts a
LEFT JOIN account_balances b ON b.account_id = a.id
LEFT JOIN postings p ON p.account_id = a.id
GROUP BY a.id, b.balance
HAVING b.balance IS DISTINCT FROM COALESCE(SUM(p.amount), 0);
```

**이것이 요구한 "원장만 다시 합산해 잔액을 검산"이다.**

**검사 3 — 자산별 전체 합계 0**

```sql
SELECT asset, SUM(amount) FROM postings GROUP BY asset HAVING SUM(amount) <> 0;
```

외부·MINT 계정이 음수를 받아 주므로 전체 합은 항상 0이어야 한다. **§1.6의 수수료 보정항이 사라진다.**

**검사 4 — 사용자·수수료 계정 음수 없음**

기존 `reconciliation_worker`의 검사 1을 계정 단위로 옮긴다.

---

## 10. 기존 wallets·ledger_entries를 어떻게 교체하는가

전제가 "기존 개발 데이터는 버려도 된다"이므로 **두 방식을 함께 굴리지 않는다.**

| 단계 | 내용 |
|---|---|
| 1 | 새 6개 표를 만드는 마이그레이션 추가 |
| 2 | 서비스 계층을 새 원장으로 바꾼다. `wallets`·`ledger_entries` 읽기·쓰기를 전부 제거 |
| 3 | 잔액 조회 API를 `account_balances` 기반으로 바꾼다 |
| 4 | **`wallets`·`ledger_entries` 표를 DROP하는 마이그레이션** |
| 5 | 개발 DB를 비우고 새 스키마로 다시 만든다 |

**옛 데이터를 옮기는 코드를 만들지 않는다.** 만들면 그 코드가 정확한지 검증해야 하고, 버려도 되는 데이터를 위해 그 비용을 치를 이유가 없다.

**레거시 필드는 함께 사라진다** — `Wallet.KRW`, `Wallet.Quantity`, `walletAvailableBalance`의 0-fallback 분기(§1.1 문제 1·2)가 모두 없어진다.

**`AvgBuyPrice`는 원장이 아니다.** 평균 매수가는 자산이 아니라 통계다. `user_asset_stats` 같은 별도 표로 옮기고 원장과 섞지 않는다.

---

## 11. 최소 필수 테스트

**같은 사실을 여러 테스트에서 반복하지 않는다. 변이 테스트는 만들지 않는다.**

| # | 테스트 | 무엇을 고정하나 |
|---|---|---|
| T1 | 분개 생성 시 자산별 합이 0이 아니면 거부 | 자산 보존의 근본 |
| T2 | 같은 `idempotency_key`로 두 번 기록 → 두 번째는 기존 분개를 반환, 전기 수 불변 | 중복 방지 |
| T3 | 분개 INSERT 후 잔액 캐시 갱신 전 강제 롤백 → 분개·전기·캐시 **모두** 없음 | 원자성 |
| T4 | 개발용 지급·입금·출금·잠금·해제·체결·수수료 각 1회 후 검사 1~4 전부 통과 | 사건별 기록 정확성 |
| T5 | 출금 접수 후 같은 돈으로 주문 시도 → 잔액 부족으로 거절 | 이중 사용 방지 |
| T6 | **하위 3경우, 모두 실제 동시 실행**(§11.1). 모두 A가 승자다. ⓐ 같은 결과: A `SUCCESS` ∥ B `SUCCESS` → 사건 2줄, 분개 1건, 잔액 1회 변경, 확인 표시 없음 ⓑ 반대 결과: A `SUCCESS` ∥ B `FAILURE` → 사건 2줄, **분개 1건**, `COMPLETED` 유지, `review_reason = CONFLICTING_TERMINAL_OUTCOME` ⓒ 확정 ∥ 미확정: A `ResolveTransfer(SUCCESS)` ∥ B `RecordObservation(PENDING)` → `COMPLETED`, **`next_check_at IS NULL`**, **review 표시 재설정 없음**, `PENDING` 사건 기록됨 | 외부 알림 멱등 + 확정·미확정 경로의 단일 직렬화(§8.8·§8.9) |
| T7 | 실패 콜백 → 묶인 돈이 available로 정확히 복귀, 합계 0 | 실패 경로 자산 보존 |
| T8 | 역분개 후 원본+역분개 합이 계정별 0 | 정정 방식 |
| T9 | 사용자 계정을 음수로 만드는 전기 시도 → 거부 | 자산이 사라지지 않음 |
| T10 | **2단계 한 테스트.** ① 알림 유실 + 조회 `UNKNOWN` → 분개 0건, 잠금 그대로, `review_required_at` 설정 ② 같은 요청에 이어서 조회 `SUCCESS` → 완료 분개 **정확히 1회**, 잠금 제거, `COMPLETED`, 확인 표시 해제 | **자산 복제 방지 + 복구 가능성** — 시간 경과로 반환하지 않고(①), 확인 표시가 처리를 막지도 않는다(②) |

T4가 §5의 표 전체를 한 번에 덮는다. 사건마다 따로 만들지 않는다.

**T6을 늘리지 않고 세 하위 경우로 확장한 이유.** 원래 T6은 "같은 알림 2회"만 봤다. 그것은 `event_key` 유니크(§7 2층)가 막는 경우이고, 실제로 위험한 것은 **경로가 다른 두 관측**이다 — 그때는 2층이 통과시키므로 §8.8의 행 잠금과 분개 멱등성 키만 남는다. 셋은 장벽·픽스처·단언 도구를 전부 공유하고 B의 호출만 다르므로, 테스트 개수는 10개 그대로다.

**ⓒ가 필요한 이유.** ⓐⓑ는 확정끼리의 경쟁이라 §8.8만 검증한다. §8.9의 재확인 분기 — **확정된 요청의 조회 일정을 되살리지 않는다** — 는 확정과 미확정이 겹칠 때만 드러난다. ⓒ가 없으면 `RecordObservation`에서 잠금이나 상태 재확인을 지워도 테스트가 전부 통과하고, 그 결함은 "끝난 출금을 영원히 조회하는" 형태로 운영에서 나타난다. `next_check_at IS NULL`이 그 단언이다.

### 11.1 T6은 순차 호출이 아니라 실제 동시 실행이다

"먼저 확정하고 나중에 다시 보낸다"는 순서로 쓰면 **§8.8의 `FOR UPDATE`를 지워도 테스트가 통과한다.** 두 번째 호출이 시작될 때 첫 번째는 이미 커밋돼 있으므로 잠글 것이 없다. 그러면 이 테스트는 행 잠금을 전혀 검증하지 못한다.

**두 트랜잭션이 겹쳐 있는 순간을 만들어야 한다.** 그리고 그 순간을 `sleep`이나 실행 순서로 만들면 CI에서 깨진다 — B-3에서 같은 종류의 경합을 두 번 겪었다(`docs/superpowers/plans/2026-08-30-matching-quantum.md`). **기다림은 항상 관측 가능한 사실을 대상으로 한다.**

**장벽:** 테스트 전용 훅 하나. 매칭 엔진의 `crashHook`과 같은 방식이다.

```go
// 1번(FOR UPDATE) 직후, 2번 이전에 호출된다. nil이면 아무 일도 없다.
afterLock func()
```

훅의 계약을 좁게 고정한다:

| 항목 | 값 |
|---|---|
| 적용 대상 | **A 경로에만.** B는 훅이 nil인 상태로 돈다 |
| 발동 횟수 | **일회성.** 한 번 불리면 스스로 해제되어 A의 이후 호출에도 다시 걸리지 않는다 |
| 위치 | `FOR UPDATE` 반환 직후, 사건 INSERT 이전 |
| 프로덕션 | 항상 nil |

훅을 양쪽에 걸면 B도 잠금을 얻기 전에 멈춰 **아무도 경쟁하지 않는 상태**가 되고, 여러 번 발동하게 두면 A가 두 번째 호출에서 다시 멈춰 테스트가 이유 없이 매달린다.

**진행:**

```
1. 요청을 PROCESSING 상태로 준비한다
   A용·B용 DB 연결을 각각 고정으로 잡고, 각 연결에서
   SELECT pg_backend_pid()로 pidA · pidB를 얻어 둔다

2. 고루틴 A(pidA 연결): ResolveTransfer(SUCCESS) 시작
     afterLock이 release 채널을 기다린다 → 행을 잠근 채 멈춘다

3. A가 잠갔음을 확인한다 (관측 1)

4. 고루틴 B(pidB 연결): 하위 경우별 호출 시작
     ⓐ ResolveTransfer(SUCCESS)
     ⓑ ResolveTransfer(FAILURE)
     ⓒ RecordObservation(PENDING)
     → 셋 다 1번 FOR UPDATE에서 블록된다

5. B가 **A 때문에** 막혔음을 확인한다 (관측 2)
     ← 이 확인이 이 테스트의 핵심이다

6. release를 닫는다 → A가 진행해 커밋

7. B가 깨어나 잠금을 얻고, 갱신된 terminal 상태를 보고
   §8.8의 3번(ⓐⓑ) 또는 §8.9의 3번(ⓒ) 분기로 간다

8. 둘 다 끝난 뒤 단언한다
```

**관측 1 — A가 잠갔다:** `afterLock` 안에서 테스트에 신호를 보낸다. 훅이 불렸다는 것은 `FOR UPDATE`가 반환됐다는 뜻이다.

**관측 2 — B가 A를 기다린다:** `wait_event_type`만 보면 **B가 무언가를 기다린다**는 것까지만 알 수 있다. 그 무언가가 A인지, 테스트 픽스처가 잡은 다른 잠금인지, 병렬로 도는 옆 테스트인지는 구별되지 않는다. 누가 막고 있는지를 직접 묻는다:

```sql
SELECT pg_blocking_pids(:pidB) @> ARRAY[:pidA];
```

이 값이 참이 될 때까지 `require.Eventually`로 기다린다. **"얼마나 기다렸나"가 아니라 "DB가 무엇을 보고하나"를 조건으로 삼으므로**, 느린 CI에서는 오래 기다릴 뿐 판정은 달라지지 않는다.

`pidA`·`pidB`는 **고정 연결에서 얻어야 한다.** 풀에서 매번 연결을 빌리면 훅 안에서 잠근 세션과 나중에 pid를 물어본 세션이 다를 수 있고, 그러면 `pg_blocking_pids`가 엉뚱한 세션을 본다.

`pg_blocking_pids`를 쓸 수 없는 환경이면 이 테스트는 **건너뛰지 말고 실패해야 한다.** 장벽 없이 통과하는 것보다 못 도는 것이 낫다.

**승자는 A로 고정된다.** 3번에서 A가 잠근 것을 확인한 뒤에야 B를 시작하고, 5번에서 B가 A에 막힌 것을 확인하므로 **A가 먼저 확정한다는 것이 관측된 사실이다.** 따라서 단언은 "먼저 확정한 쪽"이 아니라 A의 결과를 직접 쓴다:

| 하위 경우 | 최종 상태 | 분개 | B가 남기는 것 |
|---|---|---|---|
| ⓐ | `COMPLETED` | 1건 | 사건 1줄. 확인 표시 없음 |
| ⓑ | `COMPLETED`(A의 `SUCCESS`) | 1건 | 사건 1줄 + `CONFLICTING_TERMINAL_OUTCOME` |
| ⓒ | `COMPLETED`, `next_check_at IS NULL` | 1건 | `PENDING` 사건 1줄. review 표시 재설정 없음 |

세 경우 모두 잔액은 한 번만 변한다.

**T10을 늘리지 않고 두 단계로 확장한 이유.** §8.3의 시나리오 2(조회 `FAILURE` → 반환)가 만드는 분개는 실패 알림과 **완전히 같은 분개·같은 멱등성 키**이므로 T7이 이미 덮는다. 반복하지 않는다. 새로 증명해야 하는 것은 두 가지뿐이고, 둘은 **같은 요청의 연속된 두 시점**이므로 테스트 하나 안에 들어간다:

1. 확인 표시가 켜져도 돈은 그대로다 (자산 복제 방지)
2. 확인 표시가 켜진 **뒤에도** 조회가 살아 있어 정상 마무리된다 (영구 잠김 방지)

②를 따로 두지 않으면 "확인 표시 = 처리 종료"라는 결함이 테스트를 통과해 버린다.

---

## 12. 구현 작업 순서

| 순서 | 내용 | 왜 이 순서인가 |
|---|---|---|
| 1 | 6개 표 마이그레이션 + 모델 | 나머지 전부의 토대 |
| 2 | `LedgerService` — 분개 기록(합계 0 검증, 멱등, 캐시 갱신) + T1·T2·T3 | 여기가 틀리면 위가 전부 틀린다 |
| 3 | 검산 4종 + T4 | 이후 단계의 오류를 잡는 그물 |
| 4 | 개발용 지급을 새 원장으로 이전(`DEV_MINT` → `USER_AVAILABLE`, 직접 잔액 수정 제거) | 가장 단순한 사건으로 경로를 검증 |
| 5 | 주문 잠금·해제를 새 원장으로 이전 | 기존 트랜잭션 경계 유지 |
| 6 | 체결 정산을 새 원장으로 이전 (**수수료 계정 도입**) | 가장 복잡. 3번 그물이 있어야 안전 |
| 7 | `wallets`·`ledger_entries` DROP | 옛 경로가 남아 있으면 이전이 안 끝난 것 |
| 8 | 입출금 요청·상태 기계 + 단일 확정 경로 `ResolveTransfer`(행 잠금, §8.8) + 가짜 처리기(알림 + 상태 조회) + `TransferStatusPoller`(간격 확대, 운영 확인 표시) + T5~T7·T10 | 원장이 안정된 뒤 |
| 9 | 역분개 + T8·T9 | |
| 10 | 전체 검증(`go test ./...`, race, vet, build)과 CI | |

---

## 13. 현재 코드에서 가장 위험한 전환 지점

위험한 순서대로 적는다.

**전체에 걸리는 제약: 원장 구조 변경과 수수료 정책 변경을 같이 하지 않는다.** 둘을 한 번에 바꾸면 이전 후 자산 차이가 생겼을 때 원인이 구조 결함인지 요율 변경인지 가릴 수 없다. 이번 작업에서 수수료는 **금액이 흐르는 경로만** 바꾼다(사라짐 → `FEE_INCOME`). 요율·부과 대상·부과 자산은 그대로 둔다.

### 13.1 체결 정산 — 가장 위험

`settlement_service.go:147-192`가 지갑 4개를 동시에 바꾼다. 새 구조에서는 전기 5줄이 되고 **수수료 계정이 처음 등장한다.**

**위험:** 수수료 줄을 빠뜨리면 분개 합계가 0이 아니게 되고, 지금처럼 조용히 넘어가지 않고 T1이 막는다. 반대로 **검증을 우회하는 경로를 하나라도 만들면** 그 순간부터 자산이 샌다.

**대응:** 분개 기록은 `LedgerService` 한 곳만 통과하게 하고, `postings`에 직접 INSERT하는 코드를 다른 곳에 두지 않는다.

### 13.2 `SettlementRetryWorker`의 수수료 재계산

`settlement_retry_worker.go:244` 주석이 스스로 밝히듯, 실패 기록에 수수료를 저장하지 않고 **재시도 때 다시 계산한다.** 지금은 수수료율이 상수라 같은 값이 나온다.

**위험:** 새 구조에서 수수료가 `FEE_INCOME`으로 실제 이동하므로, 재계산 값이 원본과 다르면 분개 합계가 어긋난다. 수수료율을 바꾸는 순간 과거 실패 건이 깨진다.

**대응:** `FailedSettlement`에 `buyer_fee`·`seller_fee`를 저장하고 재시도는 저장값을 쓴다. 작은 변경이지만 이번에 함께 해야 한다.

### 13.3 `walletAvailableBalance`의 0-fallback

balance.go의 "둘 다 0이면 옛 필드를 본다"는 분기가 **새 구조에도 남으면** 잔액을 두 곳에서 읽는 상태가 이어진다.

**대응:** 7단계에서 이 함수 자체를 지운다. 남겨 두면 언젠가 다시 쓰인다.

### 13.4 `HoldCoordinator`의 배치 처리

`hold_coordinator.go:135`가 여러 주문의 잠금을 **한 트랜잭션에 묶어** 처리한다(성능을 위해 B-2에서 도입). 새 구조에서는 주문 하나가 분개 하나이므로, 한 트랜잭션에 분개 여러 개가 들어간다.

**위험:** 분개 하나가 실패하면 배치 전체가 롤백된다. 지금도 그렇지만, 멱등성 키 충돌이 "이미 처리됨"인지 "진짜 오류"인지 배치 안에서 구분해야 한다.

**대응:** 배치 안의 충돌은 개별 요청 결과로 표시하고 트랜잭션은 살린다. 기존 `claimIdempotencyKeys`가 이미 그 패턴이므로 그대로 따른다.

### 13.5 잔액 조회 API의 응답 형태

지금 API는 지갑 행을 그대로 돌려준다(`KRW`, `Quantity` 포함). 새 구조에서는 계정 2개(available/locked)를 합쳐 만들어야 한다.

**위험:** 프론트엔드가 옛 필드 이름을 쓰고 있으면 화면이 깨진다. 백엔드만 바꾸고 끝낼 수 없다.

**대응:** 8단계 전에 프론트엔드가 쓰는 필드를 확인하고, 응답 형태를 정한 뒤 양쪽을 함께 바꾼다. **이 설계는 백엔드만 다루므로, 프론트 변경 범위는 별도로 확인해야 한다.**

---

## 14. 확정된 결정

돈이 사라지거나 중복될 수 있는 네 가지를 물었고, 답이 아래로 확정됐다. 이후 구현은 이 결정을 전제로 한다.

### D1. 거래 수수료 — 현행 정책 유지

**원장 구조를 바꾸는 동안 가격 정책은 건드리지 않는다.** 둘을 같이 바꾸면 자산 차이가 생겼을 때 원인을 가릴 수 없다(§13 머리말).

| 항목 | 값 |
|---|---|
| 거래 수수료 | 매수자·매도자 **각각 체결금액의 0.05%** |
| 수수료 자산 | KRW |
| 입금 수수료 | 없음 |
| 출금 수수료 | 정책 없음 → 첫 구현에서 **0** |

바뀌는 것은 **금액이 가는 곳뿐이다.** 지금은 계산만 하고 사라지는 수수료가 `FEE_INCOME` 계정에 실제로 쌓인다(§1.6 → §5.7).

출금에는 나중을 대비해 `fee_amount`·`fee_asset` 필드를 두되 첫 구현 값은 0이고, **금액 0짜리 수수료 전기는 만들지 않는다**(§4.5).

**처리 도중 수수료를 다시 계산하지 않는다.** 입출금은 접수 시점 저장값을 쓰고(§4.5), 체결 재시도도 저장값을 쓰도록 `FailedSettlement`에 수수료를 저장한다(§13.2).

### D2. 출금 — 접수 시 잠그고 완료 시 차감한다

잠금과 차감은 대체 관계가 아니라 **역할이 다른 두 단계**다.

| 시점 | 하는 일 |
|---|---|
| 접수 | 사용 가능 잔액 → 출금 대기 금액 |
| 완료 | 출금 대기 금액을 고객 자산에서 제거 |
| 실패 | 출금 대기 금액 → 사용 가능 잔액 |

**출금 요청이 DB에 접수되는 순간 `출금액 + 수수료`를 함께 잠근다.** 수수료를 빼놓고 잠그면 완료 시점에 낼 돈이 없을 수 있다. 외부 전송이 확인되면 최종 차감하고, 실패하면 전액 잠금 해제한다(§5.4).

### D3. 출금 미완료 — 시간으로 되돌리지 않는다

**시간 제한은 돈을 움직이는 기준이 아니라 경고를 발생시키는 기준이다.** 시간이 지났다는 사실은 실패의 증거가 아니다. 오래 걸린 성공을 실패로 단정해 반환하면 자산이 복제된다.

판정은 상태로 한다(§8.4의 다섯 칸). 자동 취소가 허용되는 것은 **"외부 전송 전임이 확실"한 경우 하나뿐이고**, 나머지는 성공·실패가 확정돼야 돈이 움직인다.

**돈의 처리 상태와 운영자 확인 표시를 분리한다.** 확정할 수 없으면 잠금을 유지한 채 `review_required_at`·`review_reason`을 켜고 운영자를 부르되, **상태 조회는 멈추지 않는다.** 간격만 늘려 계속 확인하고, 외부 성공·실패가 확인되면 **알림이 왔을 때와 똑같은 경로·똑같은 멱등성 키로** 정확히 1회 마무리한다(§8.5).

확인 표시를 처리 종료 상태로 만들면 담당자가 놓친 출금이 영원히 잠긴다. 사람의 개입은 더 빨리 끝내기 위한 것이지, 끝나기 위한 조건이 아니다.

확정은 **경로 하나(`ResolveTransfer`)**로 모으고 `transfer_requests` 행을 `SELECT ... FOR UPDATE`로 잠근 뒤 `PROCESSING`에서만 한 번 수행한다. 같은 결과의 재관측은 사건만 남기고, **반대 결과는 돈을 되돌리지 않고 확인 표시만 다시 켠다**(§8.8). 확정 이후에도 표시를 켤 수 있어야 하므로 상태와 표시를 잇는 DB 제약은 두지 않는다.

가짜 은행·가짜 코인은 이를 검증할 수 있도록 **상태 조회 창구**를 제공하고 다섯 경우를 재현한다(§8.3). 조회 결과가 `PENDING`·`UNKNOWN`인 경로에는 **분개를 만드는 코드 자체를 두지 않는다**(`RecordObservation`).

### D4. 개발용 지급 — 유지하되 원장을 통과시킨다

부하 테스트가 `/dev/wallets/fund`에 의존하므로 없애지 않는다. 다만 **잔액을 직접 늘리는 지금 방식은 없앤다.** `DEV_MINT` → `USER_AVAILABLE` 정상 분개로 바꾼다(§5.1).

조건 일곱 가지와 용도 분리(테스트·k6 = 개발용 지급 / 사용자 E2E = 가짜 입금 / 운영 = 비활성화)는 §5.1 표에 있다.
---

## 15. 이번 설계에서 다루지 않는 것

- 실제 은행·블록체인 연동
- 옛 데이터 이전
- 출금 수수료 **정책 수립** — 필드는 만들지만 값은 0이다. 요율을 정하는 것은 별도 작업이다(§14 D1)
- 자산별 소수점 자릿수 정책 — 현재 `numeric` 그대로 쓴다
- 프론트엔드 변경(§13.5에서 범위만 표시)
- GCP 측정
