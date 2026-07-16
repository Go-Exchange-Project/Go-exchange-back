# 정산 그룹커밋 (B-4) 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 심볼 파티셔닝 정산 워커가 큐에 쌓인 trade 여러 건을 한 트랜잭션으로 정산하는 `SettleTradeBatch`를 구현한다. trade당 DB 왕복 ~12회 + COMMIT 1회 → 배치당 상수 ~9회 + COMMIT 1회. 배치가 실패하면 통째로 롤백하고 기존 단건 경로로 폴백한다.

**Architecture:** `internal/repository`에 배치 프리미티브 4개(주문 일괄 락/일괄 UPDATE, 지갑 키 일괄 조회/일괄 생성)를 추가하고, `internal/service/settlement_batch.go`의 `SettleTradeBatch`가 기존 단건 경로의 검증·산술 헬퍼(`applyTradeFill`, `settleBuyerKRW` 등)를 **그대로 재사용**해 메모리 사본에 순차 적용한 뒤 배치로 쓴다. `cmd/main.go`의 워커 루프가 논블로킹 drain으로 배치를 수집하고, 실패 시 기존 단건 경로로 폴백한다. **단건 `SettleTrade`는 서명·동작 모두 불변** — 리플레이·재시도 워커·폴백이 계속 쓴다.

**Tech Stack:** Go, GORM, PostgreSQL 16, `shopspring/decimal`, `prometheus/client_golang`.

**스펙 문서:** `docs/superpowers/specs/2026-07-15-settlement-group-commit-design.md`

## Global Constraints

- **등가성이 최상위 불변식**: 배치 정산의 최종 상태(지갑·원장·주문·trade)는 같은 순서로 단건 정산 N회를 실행한 결과와 필드 단위로 정확히 같아야 한다. Task 2의 등가성 테스트가 이를 회귀 검증한다.
- 검증·산술은 기존 헬퍼를 공유한다. `applyTradeFill`, `validateOrderStatusForSettlement`, `settlementParticipants`, `settleBuyerKRW`, `creditBuyerCoinWithAcquisitionCost`, `settleSellerCoin`, `creditAvailable`, `ledgerEntryFromWalletUpdate`, `applyTradeFeePolicy`, `prepareTradeForSettlement`, `validateIdempotentTradePayload` 중 어느 것도 배치용으로 복제하지 않는다. 시그니처 변경도 금지(단건 경로가 그대로 써야 함).
- 락 순서: 주문 먼저(ID 오름차순 일괄), 지갑 나중(ID 오름차순 일괄) — 단건 경로와 같은 순서, 스코프만 배치로 확장. 이 순서가 데드락 불성립 논증의 근거다.
- `settlementBatchMaxSize = 32` 하드코딩(환경변수 아님). 티머 없음 — 큐에 **이미 쌓인** 것만 논블로킹 drain.
- `MarketOrderDone`은 배치 경계: trade만 묶고, Done을 만나면 배치를 끊고 Done은 기존 단건 경로로 처리한다(엔진 방출 순서 보존).
- 배치 트랜잭션이 어떤 이유로든 실패하면 재시도 없이 즉시 단건 폴백. 폴백은 기존 워커 본문(transient 재시도 + 실패 내구 기록 + MarkProcessed)을 그대로 쓴다.
- 배치 안에서 outbox 마킹은 `markSettledOutbox`와 나란한 `markSettledOutboxBatch(tx, ids)`로 같은 트랜잭션에서 수행한다. 단건과 달리 `RowsAffected != len(ids)`면 에러를 반환한다(롤백 → 폴백이 건별로 정리하므로 안전).
- 통합 테스트는 `GOEXCHANGE_TEST_DATABASE_DSN` + 테스트 DB 컨테이너가 필요하다. 통합 테스트가 전부 0.00s로 실패하면 컨테이너부터 확인: `docker compose -f docker-compose.test.yml up -d --wait`.
- 커밋은 태스크 단위로 하되 CLAUDE.md의 `commit-msg-author` → `commit-msg-reviewer` 절차를 따른다(아래 커밋 스텝의 메시지는 초안 참고용).

---

### Task 1: 리포지토리 배치 프리미티브 4개

**Files:**
- Modify: `internal/repository/order_repository.go`
- Modify: `internal/repository/wallet_reporsitory.go`
- Modify: `internal/repository/order_repository_integration_test.go` (없으면 생성 — 기존 통합 테스트 헬퍼 `openRepositoryIntegrationDB`/`repositoryTestUserID`/`cleanupRepositoryUsers` 재사용)
- Modify: `internal/repository/wallet_repository_integration_test.go`

**Interfaces:**
- Produces: `(*OrderRepository).LockByIDs(ids []uint) ([]model.Order, error)` — Task 2가 배치 내 고유 주문을 ID 오름차순으로 일괄 락하는 데 쓴다.
- Produces: `repository.OrderExecutionBatchUpdate{OrderID, FilledAmount, FilledQuoteAmount, Status}`, `(*OrderRepository).BatchUpdateExecutions([]OrderExecutionBatchUpdate) error` — Task 2가 주문 최종 상태를 1왕복으로 쓴다.
- Produces: `repository.WalletKey{UserID, CoinSymbol}`, `(*WalletRepository).FindByKeys([]WalletKey) ([]model.Wallet, error)`, `(*WalletRepository).CreateZeroBalanceWallets([]WalletKey) error` — Task 2가 참가자 지갑을 일괄 확보하는 데 쓴다.

- [x] **Step 1: 실패하는 통합 테스트 작성**

주문 쪽 (`order_repository_integration_test.go`):

```go
func TestIntegrationOrderLockByIDsReturnsAllRequestedRowsInIDOrder(t *testing.T) {
	// 주문 2개 생성 → LockByIDs([id2, id1]이 아니라 정렬된 [id1, id2]) → 2행, ID 오름차순 확인
}

func TestIntegrationOrderLockByIDsFailsWhenARowIsMissing(t *testing.T) {
	// 존재하는 ID 1개 + 존재하지 않는 ID 1개 → 에러 (락 개수 불일치는 배치 전체 실패여야 함)
}

func TestIntegrationBatchUpdateExecutionsUpdatesAllColumns(t *testing.T) {
	// 주문 2개 생성 → BatchUpdateExecutions로 서로 다른 filled/filledQuote/status 지정
	// → 재조회해서 세 컬럼 모두 반영 확인 (UpdateOrderExecution과 같은 컬럼 집합: status, filled_amount, filled_quote_amount)
}

func TestIntegrationBatchUpdateExecutionsFailsOnRowCountMismatch(t *testing.T) {
	// 존재하지 않는 주문 ID 포함 → 에러
}
```

지갑 쪽 (`wallet_repository_integration_test.go`에 추가):

```go
func TestIntegrationFindByKeysReturnsOnlyMatchingWallets(t *testing.T) {
	// (userA, KRW), (userA, BTC), (userB, KRW) 생성 → FindByKeys([(userA,KRW),(userB,KRW)]) → 2행
}

func TestIntegrationCreateZeroBalanceWalletsIsIdempotent(t *testing.T) {
	// 같은 키로 2회 호출 → 에러 없음, 지갑 1개, 잔고 전부 0
	// (기존 createZeroBalanceWallet와 동일한 ON CONFLICT DO NOTHING 의미론)
}
```

- [x] **Step 2: 실패 확인**

Run: `go build ./...`
Expected: FAIL — `undefined: OrderExecutionBatchUpdate` 등

- [x] **Step 3: 구현**

`order_repository.go` — `WalletRepository.LockByIDs`(wallet_reporsitory.go:99)를 미러링:

```go
// LockByIDs는 배치 정산용으로 주문들을 ID 오름차순 FOR UPDATE로 잠급니다.
// 요청한 ID 수와 잠근 행 수가 다르면 에러입니다 — 배치는 부분 성공이 없습니다.
func (r *OrderRepository) LockByIDs(ids []uint) ([]model.Order, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var orders []model.Order
	err := r.DB.
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id IN ?", ids).
		Order("id ASC").
		Find(&orders).Error
	if err != nil {
		return nil, err
	}
	if len(orders) != len(ids) {
		return nil, fmt.Errorf("order lock expected %d rows, locked %d", len(ids), len(orders))
	}
	return orders, nil
}
```

`BatchUpdateExecutions` — `BatchUpdateBalances`(wallet_reporsitory.go:230)의 VALUES-join 패턴을 미러링. 컬럼은 `UpdateOrderExecution`(order_repository.go:42)과 정확히 같은 3개만:

```go
type OrderExecutionBatchUpdate struct {
	OrderID           uint
	FilledAmount      decimal.Decimal
	FilledQuoteAmount decimal.Decimal
	Status            model.OrderStatus
}

func (r *OrderRepository) BatchUpdateExecutions(updates []OrderExecutionBatchUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	rows := make([]string, 0, len(updates))
	args := make([]interface{}, 0, len(updates)*4)
	for i, u := range updates {
		base := i * 4
		rows = append(rows, fmt.Sprintf(
			"($%d::bigint, $%d::text, $%d::numeric, $%d::numeric)",
			base+1, base+2, base+3, base+4,
		))
		args = append(args, u.OrderID, string(u.Status), u.FilledAmount, u.FilledQuoteAmount)
	}
	sql := fmt.Sprintf(`
		UPDATE orders AS o
		SET
			status = v.status,
			filled_amount = v.filled_amount,
			filled_quote_amount = v.filled_quote_amount
		FROM (VALUES %s) AS v(id, status, filled_amount, filled_quote_amount)
		WHERE o.id = v.id`,
		strings.Join(rows, ", "),
	)
	result := r.DB.Exec(sql, args...)
	if result.Error != nil {
		return result.Error
	}
	if int(result.RowsAffected) != len(updates) {
		return fmt.Errorf("order batch update affected %d rows, expected %d", result.RowsAffected, len(updates))
	}
	return nil
}
```

`wallet_reporsitory.go`:

```go
// WalletKey는 (user, asset) 조합으로 지갑을 식별합니다.
type WalletKey struct {
	UserID     uint
	CoinSymbol string
}

// FindByKeys는 (user_id, coin_symbol) 조합들을 1왕복으로 조회합니다. 없는 키는
// 결과에서 빠질 뿐 에러가 아닙니다 — 생성 여부 판단은 호출자 몫입니다.
func (r *WalletRepository) FindByKeys(keys []WalletKey) ([]model.Wallet, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	pairs := make([][]interface{}, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, []interface{}{key.UserID, key.CoinSymbol})
	}
	var wallets []model.Wallet
	err := r.DB.Where("(user_id, coin_symbol) IN ?", pairs).Find(&wallets).Error
	return wallets, err
}

// CreateZeroBalanceWallets는 createZeroBalanceWallet의 배치 버전입니다(같은
// ON CONFLICT DO NOTHING 의미론 — 경쟁 생성과 겹쳐도 안전).
func (r *WalletRepository) CreateZeroBalanceWallets(keys []WalletKey) error {
	if len(keys) == 0 {
		return nil
	}
	wallets := make([]model.Wallet, 0, len(keys))
	for _, key := range keys {
		wallets = append(wallets, model.Wallet{
			UserID:           key.UserID,
			CoinSymbol:       key.CoinSymbol,
			KRW:              decimal.Zero,
			Quantity:         decimal.Zero,
			AvailableBalance: decimal.Zero,
			LockedBalance:    decimal.Zero,
			AvgBuyPrice:      decimal.Zero,
		})
	}
	return r.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}, {Name: "coin_symbol"}},
		DoNothing: true,
	}).Create(&wallets).Error
}
```

- [x] **Step 4: 테스트 통과 확인**

Run: `go test ./internal/repository/... -run "TestIntegrationOrderLockByIDs|TestIntegrationBatchUpdateExecutions|TestIntegrationFindByKeys|TestIntegrationCreateZeroBalanceWallets" -v`
Expected: PASS. (`(user_id, coin_symbol) IN ?` 튜플 IN이 GORM에서 의도대로 렌더링되는지 이 통합 테스트가 실증한다 — 실패하면 raw SQL로 전환)

- [x] **Step 5: 리포지토리 패키지 전체 회귀**

Run: `go test ./internal/repository/...`
Expected: PASS

- [x] **Step 6: Commit** (author→reviewer 절차)

초안: `feat(settlement): 그룹커밋용 주문·지갑 배치 리포지토리 프리미티브 추가`

---

### Task 2: `SettleTradeBatch` — 배치 정산 트랜잭션 코어

**Files:**
- Create: `internal/service/settlement_batch.go`
- Create: `internal/service/settlement_batch_integration_test.go`

**Interfaces:**
- Consumes: Task 1의 프리미티브 전부, 기존 정산 헬퍼 전부(Global Constraints 목록), `repository.WalletBatchUpdate`, `LedgerRepository.CreateMany`.
- Produces: `service.TradeBatchItem{Trade *model.Trade, OutboxEventID uint64}`, `(s *SettlementService) SettleTradeBatch(items []TradeBatchItem) ([]SettlementResult, error)` — Task 4의 워커가 쓴다. 반환 `[]SettlementResult`는 items와 같은 인덱스(Applied면 브로드캐스트 대상). **에러 반환 = 아무것도 커밋 안 됨**(전체 롤백) — 호출자는 단건 폴백.

**핵심 알고리즘** (한 `s.DB.Transaction` 안, 순서 고정):

```
0. (tx 밖) 각 trade에 prepareTradeForSettlement + applyTradeFeePolicy — 단건과 동일
1. 중복 분리: SELECT trades WHERE idempotency_key IN (전체 키)   [1왕복]
   - 발견분: validateIdempotentTradePayload → 불일치면 에러(전체 폴백), 일치면 Duplicate 결과
2. 신규 trade 배치 INSERT (ON CONFLICT DO NOTHING)               [1왕복]
   - RowsAffected != 신규 건수 → 에러 (1과 2 사이에 경쟁자가 껴들었다는 뜻 —
     폴백의 단건 멱등 경로가 건별로 정확히 처리)
3. 주문 일괄 락: 신규 trade들의 고유 주문 ID, 오름차순 정렬 → LockByIDs  [1왕복]
4. 정적 검증 (trade마다, 락된 주문으로): side·심볼 일치, settlementParticipants
   → 실패 시 에러(전체 폴백). 동시에 지갑 키 수집:
   - buyerKRW(있어야 함), buyerCoin(없으면 생성), sellerKRW(없으면 생성), sellerCoin(있어야 함)
5. 지갑 확보: FindByKeys                                          [1왕복]
   - 없는 키 중 생성 허용 역할만 CreateZeroBalanceWallets → 다시 FindByKeys  [없을 때만 +2왕복]
   - 생성 불허 역할(buyerKRW/sellerCoin)이 없으면 에러
6. 지갑 일괄 락: 고유 지갑 ID 오름차순 → LockByIDs                  [1왕복]
7. 순차 산술 (trade를 큐 순서대로, 전부 메모리에서 — 왕복 0):
   - validateOrderStatusForSettlement(메모리 상태 — 선행 trade가 FILLED로
     만든 주문에 대한 후행 trade는 여기서 실패한다: 단건 순차 실행과 동일한 관점)
   - applyTradeFill(buy/sell) → 주문 메모리 상태 갱신은 산술 뒤로 미룸
   - reservedBuyDebitAmount/executionDebit/amountAfterFee → 지갑 업데이트 4개 계산
   - ledgerEntryFromWalletUpdate 4개는 반드시 fold **전에** 생성
     (delta = update − 현재 잔고이므로 순서가 정합성 그 자체)
   - fold: 지갑 메모리 사본에 update 5필드 반영, 주문에 filled/filledQuote/status 반영
     → 다음 trade가 이 상태를 본다
8. 배치 쓰기: BatchUpdateExecutions(터치된 주문 최종 상태)          [1왕복]
   BatchUpdateBalances(터치된 지갑 최종 상태)                       [1왕복]
   CreateMany(원장 4×적용건수)                                     [1왕복]
   markSettledOutboxBatch(모든 outboxID>0 — 중복 건 포함)           [1왕복]
```

fold 헬퍼는 단건 경로가 DB에 쓰는 것과 정확히 같은 5필드를 복사한다(등가성이 구성적으로 보장됨):

```go
// foldWalletBalanceUpdate는 계산된 업데이트를 메모리 지갑에 반영한다 — 단건 경로가
// WalletBatchUpdate로 DB에 쓰는 5개 컬럼과 정확히 같은 필드다. 배치 내 다음 trade는
// 커밋됐을 행과 동일한 상태를 본다.
func foldWalletBalanceUpdate(wallet *model.Wallet, update WalletBalanceUpdate) {
	wallet.AvailableBalance = update.AvailableBalance
	wallet.LockedBalance = update.LockedBalance
	wallet.KRW = update.KRW
	wallet.Quantity = update.Quantity
	wallet.AvgBuyPrice = update.AvgBuyPrice
}
```

```go
// markSettledOutboxBatch는 markSettledOutbox의 배치 버전. 단건과 달리 개수 불일치를
// 에러로 취급한다 — 배치는 전체 롤백 후 폴백이 건별로 정리하므로 엄격한 쪽이 안전하다.
func markSettledOutboxBatch(tx *gorm.DB, ids []uint64) error
```

- [x] **Step 1: 실패하는 통합 테스트 작성**

`settlement_batch_integration_test.go`. 기존 헬퍼(`openServiceIntegrationDB`, `serviceTestUserID`, `cleanupServiceUsers`, `seedSettlementRows`, `seedPendingOutboxRow`, `cleanupOutboxRows` — settlement_deadlock/outbox 통합 테스트 파일에 정의)를 재사용하고, 다중 trade 픽스처가 필요하면 헬퍼를 추가한다.

**(a) 등가성 — 이 태스크의 핵심 테스트.** 서로 다른 유저 집합으로 동일한 픽스처 2벌을 만들고, 같은 trade 시퀀스를 한쪽은 `SettleTradeBatch` 1회, 다른 쪽은 `SettleTrade` 루프로 정산한 뒤 최종 상태를 필드 단위 비교:

```go
// 비교 대상: 역할별 지갑 5필드(Available/Locked/KRW/Quantity/AvgBuyPrice),
// 주문 3필드(FilledAmount/FilledQuoteAmount/Status),
// 원장 (user,symbol)별 엔트리 수 + (AvailableDelta, LockedDelta, AvailableBalanceAfter,
// LockedBalanceAfter) 시퀀스, trade 행 수
func TestIntegrationSettleTradeBatchMatchesSequentialSingleSettlement(t *testing.T)
```

시나리오 3개를 한 시퀀스에 담는다:
1. 독립 trade 2건(참가자 안 겹침)
2. 같은 매수 주문이 trade 2건에 걸침(대형 테이커 — 주문·매수자 지갑이 배치 내에서 진화)
3. 같은 유저가 한 trade의 매수자이자 다른 trade의 매도자(지갑 공유 — fold 순서 검증)

**(b) 멱등성:**

```go
// 같은 배치 2회 정산 → 2회차는 전부 Duplicate, 지갑·원장·주문 무변화, outbox는 마킹됨
func TestIntegrationSettleTradeBatchIsIdempotent(t *testing.T)

// 배치에 기정산 1건 + 신규 2건 혼재 → 기정산은 Duplicate, 신규만 적용, outbox는 3건 모두 마킹
func TestIntegrationSettleTradeBatchSkipsAlreadySettledTrades(t *testing.T)
```

**(c) 실패 원자성:**

```go
// 배치 중간에 불량 trade(취소된 주문) → 에러 반환, trade 행 0, 지갑 무변화,
// outbox 전부 PENDING (settlement_outbox_integration_test.go의 단건 롤백 테스트와 동형)
func TestIntegrationSettleTradeBatchFailureRollsBackEverything(t *testing.T)
```

**(d) outbox 흡수:**

```go
// 성공 배치 → 모든 outbox 행이 같은 트랜잭션에서 PROCESSED
func TestIntegrationSettleTradeBatchMarksAllOutboxRowsProcessed(t *testing.T)
```

- [x] **Step 2: 실패 확인**

Run: `go build ./...`
Expected: FAIL — `undefined: TradeBatchItem` 등

- [x] **Step 3: `settlement_batch.go` 구현**

위 알고리즘대로. 구현 시 주의:

- 신규 trade 배치 INSERT는 `tx.Clauses(clause.OnConflict{Columns: idempotency_key, DoNothing: true}).Create(&newTrades)` — 성공 시 각 `trade.ID`가 채워지므로(outbox InsertBatch와 동일) 원장 `ReferenceID`와 `SettlementResult.TradeID`에 쓴다.
- 주문·지갑 락 결과는 `map[uint]*model.Order` / `WalletKey → *model.Wallet` 맵으로 인덱싱하고, **포인터로만** 산술한다(슬라이스 원소 복사 금지 — fold가 다음 trade에 보여야 함).
- 배치 내 같은 키가 중복 수집되면 dedup 후 정렬(주문 ID, 지갑 ID 각각).
- `SettlementResult`는 items 인덱스에 맞춰 채운다: 중복=Duplicate, 신규=Applied+TradeID.
- 실패는 전부 `return err`로 트랜잭션 롤백 — 부분 커밋 경로가 없어야 한다.

- [x] **Step 4: 테스트 통과 확인**

Run: `go test ./internal/service/... -run TestIntegrationSettleTradeBatch -v`
Expected: PASS (등가성 포함 전부)

- [x] **Step 5: 서비스 패키지 전체 회귀 (단건 경로 불변 확인)**

Run: `go test ./internal/service/...`
Expected: PASS — 기존 SettleTrade 테스트가 전부 그대로 통과해야 한다(시그니처·동작 불변의 증거)

- [x] **Step 6: Commit** (author→reviewer 절차)

초안: `feat(settlement): 여러 체결을 한 트랜잭션으로 정산하는 SettleTradeBatch 추가 (B-4)`

---

### Task 3: 배치 메트릭

**Files:**
- Modify: `internal/metrics/metrics.go`
- Test: `internal/metrics/settlement_batch_test.go`

**Interfaces:**
- Produces: `metrics.SettlementBatchSize prometheus.Histogram`(버킷 1,2,4,8,16,32), `metrics.SettlementBatchFallbacksTotal prometheus.Counter` — Task 4의 워커가 쓴다.

- [x] **Step 1: 실패하는 테스트** — 기존 `reconciliation_test.go` 스타일로 histogram observe/counter inc 확인
- [x] **Step 2: 실패 확인** — Run: `go test ./internal/metrics/... -run TestSettlementBatch -v` → FAIL
- [x] **Step 3: 메트릭 추가** — `settlement_batch_size`(Help: 실제로 묶인 배치 크기 — 1에 몰려 있으면 부하가 낮거나 drain이 안 되는 것), `settlement_batch_fallbacks_total`
- [x] **Step 4: 통과 확인** — Run: `go test ./internal/metrics/...` → PASS
- [x] **Step 5: Commit** (author→reviewer 절차) — 초안: `feat(metrics): 정산 배치 크기·폴백 메트릭 추가`

---

### Task 4: 배치 수집기 + 워커 통합 + 단건 폴백 (`cmd/main.go`)

**Files:**
- Modify: `cmd/main.go`
- Modify: `cmd/main_test.go`

**Interfaces:**
- Consumes: `service.TradeBatchItem`/`SettleTradeBatch`(Task 2), `metrics.SettlementBatchSize`/`SettlementBatchFallbacksTotal`(Task 3), 기존 `processExecutionEvent`.
- Produces: `collectTradeBatch`, `processSingleOutboxEvent`, `settleTradeBatchWithFallback`, `broadcastSettledTrade` — 전부 cmd 패키지 내부 함수(의존성을 파라미터로 받아 main_test.go에서 페이크로 테스트 가능하게, `processExecutionEvent`와 같은 방식).

**리팩터링 순서가 중요하다** — 기존 로직을 먼저 추출(동작 불변)하고, 그 다음 배치를 얹는다:

- [x] **Step 1: `broadcastSettledTrade` 추출 (동작 불변 리팩터링)**

`processTradeSettlement`(main.go:535-557)의 trade JSON 마샬+브로드캐스트 블록을 `broadcastSettledTrade(trade *model.Trade, broadcast func(string, []byte), logger *log.Logger)`로 추출하고 `processTradeSettlement`가 이를 호출하게 바꾼다. 배치 성공 경로가 커밋 후 재사용한다.

Run: `go test ./cmd/...`
Expected: PASS (기존 processTradeSettlement 테스트 그대로)

- [x] **Step 2: `processSingleOutboxEvent` 추출 (동작 불변 리팩터링)**

현재 워커 goroutine 본문(main.go:165-179: `processExecutionEvent` 호출 + `markedInTx` 분기 + `MarkProcessed`)을 함수로 추출한다. 워커 루프는 이 함수를 호출하는 것으로 바뀌고, 폴백·Done 처리도 이 함수를 쓴다.

Run: `go build ./... ; go test ./cmd/...`
Expected: PASS

- [x] **Step 3: 실패하는 수집기 테스트 작성**

`main_test.go`:

```go
// 큐에 trade 3건이 쌓여 있으면 첫 이벤트 + drain 2건 = 배치 3건, pending 없음
func TestCollectTradeBatchDrainsQueuedTrades(t *testing.T)

// 큐가 비어 있으면 배치 1건으로 즉시 반환 (티머 없음 — 기다리지 않는다)
func TestCollectTradeBatchReturnsImmediatelyWhenQueueIsEmpty(t *testing.T)

// trade 2건 뒤 MarketOrderDone이 오면 배치는 trade 2건에서 끊기고 Done은 pending으로 반환
func TestCollectTradeBatchStopsAtMarketOrderDoneBoundary(t *testing.T)

// maxBatch를 넘게 쌓여 있어도 maxBatch에서 끊는다
func TestCollectTradeBatchCapsAtMaxBatchSize(t *testing.T)

// drain 중 채널이 닫히면 open=false로 반환 (잔여 배치는 호출자가 처리)
func TestCollectTradeBatchReportsClosedChannel(t *testing.T)
```

- [x] **Step 4: 실패 확인**

Run: `go test ./cmd/... -run TestCollectTradeBatch -v`
Expected: FAIL — `undefined: collectTradeBatch`

- [x] **Step 5: 수집기 구현**

```go
const settlementBatchMaxSize = 32

// collectTradeBatch는 first(반드시 trade)에 이어 큐에 이미 쌓인 trade를 논블로킹으로
// 최대 maxBatch까지 모은다. 티머 없음 — 이벤트는 이미 outbox에 커밋된 뒤라 모으려고
// 기다릴 이유가 없다(부하 낮으면 배치 1, 백로그가 있을 때만 커지는 적응형).
// 비-trade(MarketOrderDone)를 만나면 배치를 끊고 pending으로 돌려준다(순서 보존).
func collectTradeBatch(first service.OutboxEvent, queue <-chan service.OutboxEvent, maxBatch int) (batch []service.OutboxEvent, pending *service.OutboxEvent, open bool) {
	batch = append(batch, first)
	open = true
	for len(batch) < maxBatch {
		select {
		case event, ok := <-queue:
			if !ok {
				open = false
				return
			}
			if event.Event.Trade == nil {
				pending = &event
				return
			}
			batch = append(batch, event)
		default:
			return
		}
	}
	return
}
```

Run: `go test ./cmd/... -run TestCollectTradeBatch -v`
Expected: PASS

- [x] **Step 6: 폴백 래퍼 + 워커 루프 교체**

```go
// settleTradeBatchWithFallback: 배치 성공 시 Applied trade만 브로드캐스트.
// 실패 시 전체 롤백된 상태이므로 기존 단건 경로로 건별 재처리 —
// 불량 trade만 실패 기록으로 빠지고 나머지는 정상 정산된다.
func settleTradeBatchWithFallback(batch []service.OutboxEvent, deps...) {
	items := make([]service.TradeBatchItem, len(batch))
	for i, event := range batch {
		items[i] = service.TradeBatchItem{Trade: event.Event.Trade, OutboxEventID: event.OutboxID}
	}
	results, err := settlementService.SettleTradeBatch(items)
	if err != nil {
		metrics.SettlementBatchFallbacksTotal.Inc()
		logger.Printf("settle trade batch of %d failed, falling back to per-trade settlement: %v", len(batch), err)
		for _, event := range batch {
			processSingleOutboxEvent(event, deps...)
		}
		return
	}
	metrics.SettlementBatchSize.Observe(float64(len(batch)))
	for i, result := range results {
		if result.Applied {
			broadcastSettledTrade(batch[i].Event.Trade, broadcast, logger)
		}
	}
}
```

워커 루프(pending 이월 처리 주의 — Done이 배치를 끊었으면 다음 반복에서 먼저 소비):

```go
go func(queue chan service.OutboxEvent) {
	defer settlementWg.Done()
	var pending *service.OutboxEvent
	for {
		var event service.OutboxEvent
		if pending != nil {
			event, pending = *pending, nil
		} else {
			received, ok := <-queue
			if !ok {
				return
			}
			event = received
		}
		if event.Event.Trade == nil {
			processSingleOutboxEvent(event, ...)
			continue
		}
		batch, next, open := collectTradeBatch(event, queue, settlementBatchMaxSize)
		pending = next
		settleTradeBatchWithFallback(batch, ...)
		if !open {
			return // 채널 닫힘 — 잔여 배치는 방금 처리했고 pending은 nil
		}
	}
}(queue)
```

`settleTradeBatchWithFallback`의 폴백 분기 테스트를 main_test.go에 추가한다(항상 에러를 반환하는 페이크 settler → 배치 전 건이 단건 경로로 처리되는지, 성공 페이크 → Applied만 브로드캐스트되는지).

- [x] **Step 7: 전체 검증**

Run: `go build ./... ; go test ./cmd/... -v`
Expected: PASS

Run: `go test ./... -count=1`
Expected: 전체 스위트 PASS (통합 테스트 포함 — 테스트 DB 컨테이너 기동 확인)

Run: `go test ./internal/service/... ./cmd/... -race -count=1`
Expected: PASS

- [x] **Step 8: Commit** (author→reviewer 절차)

초안: `feat(settlement): 정산 워커에 그룹커밋 배치 수집과 단건 폴백 연결 (B-4)`

---

### Task 5: 크래시/종료 시나리오 확인 + 문서

**Files:**
- Modify: `docs/refactor/README.md` (6번 B-4 ✅)
- Create: `docs/refactor/6_B-4_정산_그룹커밋_완료.md`

- [x] **Step 1: graceful shutdown 경로 수동 추적**

배치 도입 후에도 종료 도미노가 성립하는지 코드로 확인한다(테스트가 아니라 리뷰):
엔진 Stop → ExecutionCh close → OutboxWriter 잔여 flush → 큐 close → 워커의 `collectTradeBatch`가 `open=false` 반환 → 잔여 배치 처리 후 return → `settlementWg.Wait()`. drain 중 닫힘이 유일하게 새로 생긴 경로다 — Task 4 Step 5의 `TestCollectTradeBatchReportsClosedChannel`이 커버하는지 재확인.

- [x] **Step 2: 크래시 내구성 논증 기록**

배치 커밋 전 크래시 = outbox 전부 PENDING → 부팅 리플레이(단건 경로) 재처리. A-3 크래시 주입 테스트를 재실행할 필요는 없다 — write-ahead 관문(OutboxWriter)은 손대지 않았고, 바뀐 것은 커밋된 이벤트의 소비 묶음 단위뿐이다. 이 논증을 완료 문서에 적는다.

- [x] **Step 3: 완료 문서 + README 갱신**

`6_B-4_정산_그룹커밋_완료.md`: 왜 문제였나(왕복 12×N, 18번 벤치 근거) / 어떻게 해결했나(설계 요지 + 스펙 링크) / 결과(테스트 요약, GCP 측정은 "다음 사이클에 일괄" 병기). README 현황판 6번 ✅.

- [x] **Step 4: Commit + Push** (author→reviewer 절차)

초안: `docs(refactor): B-4 정산 그룹커밋 완료 문서 추가`

푸시 후 `gh run watch`로 CI 그린 확인.

---

## 성능 측정 (이 계획 범위 밖)

"나중에 일괄" 방침대로 다음 GCP 측정 사이클에서 B-1a/B-1b/B-4를 묶어 hold 프로파일 same-session A/B로 측정한다. 그때 `settlement_batch_size` 히스토그램으로 실제 배치 크기 분포를 함께 확인한다(1에 몰려 있으면 그룹커밋이 발동하지 않는 부하라는 뜻).
