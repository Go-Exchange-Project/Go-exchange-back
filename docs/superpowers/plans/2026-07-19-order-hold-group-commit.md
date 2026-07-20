# ② 자금 홀드 그룹커밋 구현 계획 (2차 리팩토링 · 가용성)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development(권장) 또는 superpowers:executing-plans로 태스크별 실행. Steps use checkbox (`- [ ]`) syntax.

**Goal:** 동시 주문 생성의 자금 홀드를 단일 코디네이터가 적응형 배치로 모아 한 트랜잭션에 persist+hold함으로써 DB 왕복을 N:1로 상각해 처리량 천장을 올린다.

**Architecture:** 새 `HoldCoordinator`(단일 goroutine)가 HTTP 홀드 요청을 `~5ms` 창으로 모아 `HoldBatch` 트랜잭션(전역 락 순서 + fold-검증 + 개별 잔고부족 격리)을 실행하고 요청별 결과를 시그널한다. 배치 DB 실패는 단건 폴백, 입력 만석은 503. 기존 단건 경로는 `persistAndHold`로 추출해 no-coordinator·폴백·정합성 진실 3역을 겸한다.

**Tech Stack:** Go, GORM, PostgreSQL, shopspring/decimal, prometheus.

**스펙 문서:** `docs/superpowers/specs/2026-07-19-order-hold-group-commit-design.md`

## Global Constraints

- **등가성이 최상위 불변식**: 배치 홀드의 최종 상태(지갑·원장·주문)는 같은 순서로 단건 처리 N회 결과와 필드 단위로 동일. Task 3의 등가성 테스트가 회귀 검증.
- 자금 홀드는 여전히 동기·원자적(overspend 불가). ExecutionCh 백프레셔(A-3)·엔진 무관.
- 전역 락 순서: 지갑 ID 오름차순 `LockByIDs`(정산 `lockSettlementWallets`와 동일) — 정산·취소 txn과 데드락 불성립.
- 개별 잔고 부족(흔함)은 배치 안에서 격리 — 통과분만 INSERT/홀드, 실패분은 행 0 + ConflictError(409). 배치 DB 실패(드묾)만 단건 폴백.
- **결과 대기엔 타임아웃 없음**(고아 주문 방지). 입력(제출)을 바운디드로 막는다(만석 → 503).
- 산술·검증 헬퍼(`applyBuyOrderHold`/`applySellOrderHold`/`ledgerEntryFromWalletUpdate`/`foldWalletBalanceUpdate`/`quoteAmountWithTradingFee`)는 재사용만, 복제 금지.
- **HoldBatch txn 실패 시 주문 struct의 ID를 0으로 리셋**(B-4 `8b3007f`의 phantom ID 교훈 — RETURNING이 채운 ID가 롤백돼도 남아 폴백 재삽입을 오염시킴).
- 성능 실증은 ⑤(23번). **이 계획의 성공 기준은 "등가성 + 격리 + 폴백 + 회귀 그린"까지** — 처리량 수치 주장 금지.
- 통합 테스트: 테스트 DB + DSN(포트 55432), `-v`로 SKIP 0 확인. 커밋은 태스크 단위 commit-message 스킬(author→reviewer, 한글). Bash 실패 시 PowerShell.

## 파일 구조

- `internal/repository/order_repository.go` — `CreateOrders([]*model.Order)` 배치 INSERT.
- `internal/service/hold_coordinator.go`(신규) — `HoldCoordinator`, `holdRequest`/`holdResult`, `HoldBatch`, `Run`/`collectBatch`/`Submit`, fallback.
- `internal/service/order_service.go` — `persistAndHold` 추출, `CreateOrder`가 코디네이터 경유(또는 직접).
- `config/runtime.go` — `HoldBatchSizeFromEnv`.
- `internal/metrics/metrics.go` — `HoldBatchSize`/`HoldBatchFallbacksTotal`.
- `cmd/main.go` — 코디네이터 기동·배선·종료 drain·게이지 등록.
- 각 `*_test.go`.

---

### Task 1: 주문 배치 INSERT 프리미티브

**Files:**
- Modify: `internal/repository/order_repository.go`
- Test: `internal/repository/order_repository_integration_test.go`

**Interfaces:**
- Produces: `(*OrderRepository).CreateOrders(orders []*model.Order) error` — Task 3이 통과 주문 일괄 삽입에 사용. GORM이 각 원소의 `ID`를 채운다.

- [ ] **Step 1: 실패 통합 테스트** — 2건 배치 삽입 후 각 `ID`가 채워지고 순서가 보존되는지:

```go
func TestIntegrationCreateOrdersAssignsIDsInOrder(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(830)
	defer cleanupRepositoryUsers(t, db, userID)
	repo := NewOrderRepository(db)
	orders := []*model.Order{
		{UserID: userID, CoinSymbol: "BTC", Side: model.OrderSideBuy, OrderType: model.OrderTypeLimit, Status: model.OrderStatusPending, Price: decimal.NewFromInt(100), Amount: decimal.NewFromInt(1)},
		{UserID: userID, CoinSymbol: "BTC", Side: model.OrderSideBuy, OrderType: model.OrderTypeLimit, Status: model.OrderStatusPending, Price: decimal.NewFromInt(200), Amount: decimal.NewFromInt(2)},
	}
	require.NoError(t, repo.CreateOrders(orders))
	require.NotZero(t, orders[0].ID)
	require.NotZero(t, orders[1].ID)
	assert.Less(t, orders[0].ID, orders[1].ID, "삽입 순서대로 ID가 오름차순 배정돼야 한다")
}
```

Run: `go test ./internal/repository/... -run TestIntegrationCreateOrders -v` → FAIL(undefined).

- [ ] **Step 2: 구현** — GORM 배치 create(슬라이스 create가 각 원소 ID를 채움):

```go
func (r *OrderRepository) CreateOrders(orders []*model.Order) error {
	if len(orders) == 0 {
		return nil
	}
	return r.DB.Create(&orders).Error
}
```

- [ ] **Step 3: 통과 + 회귀** — Run: `go test ./internal/repository/... -count=1` → PASS.
- [ ] **Step 4: Commit** — 초안: `feat(repository): 주문 배치 INSERT CreateOrders 추가 (2차 ②)`

---

### Task 2: 단건 경로 `persistAndHold` 추출 (동작 불변 리팩터링)

**Files:**
- Modify: `internal/service/order_service.go`
- Test: 기존 `CreateOrder` 통합 테스트(무수정 그린으로 검증)

**Interfaces:**
- Produces: `persistAndHold(db *gorm.DB, orderRepo *repository.OrderRepository, walletRepo *repository.WalletRepository, ledgerRepo *repository.LedgerRepository, order *model.Order) error` — `CreateOrder`(no-coordinator)·Task 4(폴백) 공유.

- [ ] **Step 1: 추출** — 현재 `CreateOrder`의 DB 트랜잭션 블록을 패키지 함수로 추출(로직 그대로):

```go
// persistAndHold는 주문 1건을 한 트랜잭션에 영속화하고 자금을 홀드한다.
// no-coordinator 경로와 배치 실패 폴백이 공유하는 단건 경로 — 정합성의 진실.
func persistAndHold(db *gorm.DB, orderRepo *repository.OrderRepository, walletRepo *repository.WalletRepository, ledgerRepo *repository.LedgerRepository, order *model.Order) error {
	return db.Transaction(func(tx *gorm.DB) error {
		or := orderRepo.WithTx(tx)
		wr := walletRepo.WithTx(tx)
		lr := ledgerRepo.WithTx(tx)
		if err := or.CreateOrder(order); err != nil {
			return err
		}
		return holdOrderAssets(wr, lr, order)
	})
}
```

`CreateOrder`의 해당 트랜잭션 호출을 `persistAndHold(s.OrderRepository.DB, s.OrderRepository, s.WalletRepository, s.LedgerRepository, order)`로 교체(이 태스크에선 아직 코디네이터 미도입 — 동작 완전 동일).

- [ ] **Step 2: 회귀** — Run: `go test ./internal/service/... -count=1` → 기존 CreateOrder·접수 분리(①)·정산 테스트 **무수정 그린**(동작 불변의 증거).
- [ ] **Step 3: Commit** — 초안: `refactor(order): 단건 persist+hold 경로를 persistAndHold로 추출 (2차 ②)`

---

### Task 3: `HoldBatch` 배치 트랜잭션 (정합성 코어)

**Files:**
- Create: `internal/service/hold_coordinator.go`
- Create: `internal/service/hold_coordinator_integration_test.go`

**Interfaces:**
- Produces: `holdResult{Order *model.Order, Err error}`, `HoldCoordinator{DB, OrderRepo, WalletRepo, LedgerRepo, ...}`, `(*HoldCoordinator).HoldBatch(orders []*model.Order) ([]holdResult, error)` — Task 4가 사용. 반환 `([]holdResult, nil)`이면 커밋 성공(`holdResult.Err`가 개별 잔고부족 ConflictError일 수 있음); `(nil, err)`이면 txn-레벨 실패(폴백 대상, 이때 모든 orders의 `.ID`는 0으로 리셋됨).

- [ ] **Step 1: 실패 통합 테스트** — 등가성 + 개별 격리:

```go
// 등가성: 같은 주문 시퀀스를 HoldBatch(userA) vs persistAndHold 순차(userB)로 처리 →
// 지갑 잔고·원장 엔트리·주문 상태가 사용자 대응되게 동일. 같은 유저 다중 주문(fold) 포함.
func TestIntegrationHoldBatchMatchesSequentialSingleHold(t *testing.T)

// 개별 격리: 배치에 잔고 충분 2건 + 부족 1건 → 충분분은 홀드·주문 PENDING,
// 부족분은 holdResult.Err=ConflictError·주문 행 0. 통과분 지갑/원장만 반영.
func TestIntegrationHoldBatchIsolatesInsufficientFunds(t *testing.T)

// 같은 유저 fold: 잔고 100인 유저가 한 배치에 60+50 두 매수 → 첫째 통과(잔고 40),
// 둘째 부족(ConflictError). overspend 방지 = 단건 순차와 동일.
func TestIntegrationHoldBatchFoldsSameUserBalance(t *testing.T)
```

(기존 통합 헬퍼 `openServiceIntegrationDB`/`serviceTestUserID`/`cleanupServiceUsers`, 지갑 시드 헬퍼 재사용.)

Run: `go test ./internal/service/... -run TestIntegrationHoldBatch -v` → FAIL(undefined).

- [ ] **Step 2: 타입 + HoldBatch 구현** (`hold_coordinator.go`) — B-4 `SettleTradeBatch` 구조 계승:

```go
type holdRequest struct {
	order    *model.Order
	resultCh chan holdResult
}

type holdResult struct {
	Order *model.Order // 성공 시 ID 채워짐
	Err   error        // nil=성공, ConflictError=잔고부족, 그 외=시스템
}

type HoldCoordinator struct {
	DB         *gorm.DB
	OrderRepo  *repository.OrderRepository
	WalletRepo *repository.WalletRepository
	LedgerRepo *repository.LedgerRepository

	BatchSize     int           // 기본 64
	FlushInterval time.Duration // 기본 5ms
	Logger        *log.Logger

	input chan holdRequest
	done  chan struct{}
}

// holdWalletKey: 매수=유저 KRW, 매도=유저 코인.
func holdWalletKey(order *model.Order) repository.WalletKey {
	if order.Side == model.OrderSideBuy {
		return repository.WalletKey{UserID: order.UserID, CoinSymbol: model.KRWAssetSymbol}
	}
	return repository.WalletKey{UserID: order.UserID, CoinSymbol: order.CoinSymbol}
}

// holdAmountFor: holdOrderAssets와 동일 산술. 매수 지정가=quoteAmountWithTradingFee(Price*Amount),
// 매수 시장가=QuoteAmount, 매도=Amount.
func holdAmountFor(order *model.Order) decimal.Decimal {
	if order.Side == model.OrderSideBuy {
		if order.OrderType == model.OrderTypeMarket {
			return order.QuoteAmount
		}
		return quoteAmountWithTradingFee(order.Price.Mul(order.Amount))
	}
	return order.Amount
}

// HoldBatch는 배치를 한 트랜잭션에 persist+hold한다. 통과분만 INSERT/홀드하고 실패분은
// holdResult.Err로 격리한다. txn-레벨 실패면 (nil, err) 반환 + 모든 orders.ID를 0으로
// 리셋(phantom ID 방지). 성공 시 결과는 orders 인덱스와 1:1.
func (c *HoldCoordinator) HoldBatch(orders []*model.Order) ([]holdResult, error) {
	results := make([]holdResult, len(orders))

	err := c.DB.Transaction(func(tx *gorm.DB) error {
		orderRepo := c.OrderRepo.WithTx(tx)
		walletRepo := c.WalletRepo.WithTx(tx)
		ledgerRepo := c.LedgerRepo.WithTx(tx)

		// 1. 지갑 키 수집(dedup) → 2. FindByKeys로 ID 확보 → ID 오름차순 LockByIDs.
		keySet := map[repository.WalletKey]bool{}
		keys := make([]repository.WalletKey, 0, len(orders))
		for _, o := range orders {
			k := holdWalletKey(o)
			if !keySet[k] {
				keySet[k] = true
				keys = append(keys, k)
			}
		}
		found, err := walletRepo.FindByKeys(keys)
		if err != nil {
			return err
		}
		ids := make([]uint, 0, len(found))
		for i := range found {
			ids = append(ids, found[i].ID)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		locked, err := walletRepo.LockByIDs(ids)
		if err != nil {
			return err
		}
		walletByKey := map[repository.WalletKey]*model.Wallet{}
		for i := range locked {
			w := &locked[i]
			walletByKey[repository.WalletKey{UserID: w.UserID, CoinSymbol: w.CoinSymbol}] = w
		}

		// 3. 순차 fold-검증. 통과분만 수집.
		type passingHold struct {
			idx   int
			order *model.Order
			entry model.LedgerEntry // ReferenceID는 INSERT 후 채움
		}
		var passing []passingHold
		changedWallets := map[uint]*model.Wallet{}

		for i, order := range orders {
			wallet := walletByKey[holdWalletKey(order)]
			if wallet == nil { // 지갑 없음 = 잔고 부족과 동일
				results[i] = holdResult{Err: NewConflictErrorf("insufficient available balance")}
				continue
			}
			amount := holdAmountFor(order)
			var update WalletBalanceUpdate
			var herr error
			if order.Side == model.OrderSideBuy {
				update, herr = applyBuyOrderHold(wallet, amount)
			} else {
				update, herr = applySellOrderHold(wallet, amount)
			}
			if herr != nil { // ConflictError(잔고 부족) 격리
				results[i] = holdResult{Err: herr}
				continue
			}
			// 원장 엔트리는 fold 전에 계산(delta = update - 현재 잔고).
			entry := ledgerEntryFromWalletUpdate(wallet, update, model.LedgerEntryTypeOrderHold, model.LedgerReferenceTypeOrder, 0, "")
			foldWalletBalanceUpdate(wallet, update) // 다음 주문이 차감된 잔고를 본다
			changedWallets[wallet.ID] = wallet
			passing = append(passing, passingHold{idx: i, order: order, entry: entry})
		}

		if len(passing) == 0 {
			return nil // 전원 실패 — 쓸 것 없음, results엔 개별 에러
		}

		// 4. 통과 주문 배치 INSERT (ID 채워짐).
		passingOrders := make([]*model.Order, len(passing))
		for j := range passing {
			passingOrders[j] = passing[j].order
		}
		if err := orderRepo.CreateOrders(passingOrders); err != nil {
			return err
		}

		// 5. 변경 지갑 일괄 UPDATE.
		updates := make([]repository.WalletBatchUpdate, 0, len(changedWallets))
		for _, w := range changedWallets {
			updates = append(updates, repository.WalletBatchUpdate{
				WalletID: w.ID, AvailableBalance: w.AvailableBalance, LockedBalance: w.LockedBalance,
				KRW: w.KRW, Quantity: w.Quantity, AvgBuyPrice: w.AvgBuyPrice,
			})
		}
		if err := walletRepo.BatchUpdateBalances(updates); err != nil {
			return err
		}

		// 6. OrderHold 원장 일괄 INSERT(새 order.ID 참조).
		entries := make([]model.LedgerEntry, len(passing))
		for j := range passing {
			e := passing[j].entry
			e.ReferenceID = passing[j].order.ID
			entries[j] = e
		}
		if err := ledgerRepo.CreateMany(entries); err != nil {
			return err
		}

		// 통과분 결과 채움.
		for j := range passing {
			results[passing[j].idx] = holdResult{Order: passing[j].order}
		}
		return nil
	})

	if err != nil {
		for _, o := range orders { // phantom ID 방지(B-4 8b3007f 교훈)
			o.ID = 0
		}
		return nil, err
	}
	return results, nil
}
```

- [ ] **Step 3: 통과** — Run: `go test ./internal/service/... -run TestIntegrationHoldBatch -v` → PASS(3건). `import "sort"` 확인.
- [ ] **Step 4: Commit** — 초안: `feat(order): 자금 홀드 배치 트랜잭션 HoldBatch 추가 (2차 ②)`

---

### Task 4: 코디네이터 goroutine + 폴백 + 제출 + 종료 + 설정 + 메트릭

**Files:**
- Modify: `internal/service/hold_coordinator.go`
- Modify: `config/runtime.go`, `config/runtime_test.go`
- Modify: `internal/metrics/metrics.go` (+ 테스트)
- Test: `internal/service/hold_coordinator_integration_test.go`

**Interfaces:**
- Produces: `NewHoldCoordinator(...)`, `(*HoldCoordinator).Run()`, `(*HoldCoordinator).Submit(order) (*model.Order, error)`, `(*HoldCoordinator).Shutdown()` — Task 5(main·CreateOrder)가 사용.
- Produces: `config.HoldBatchSizeFromEnv() int`(기본 64), `metrics.HoldBatchSize`/`HoldBatchFallbacksTotal`.

- [ ] **Step 1: 설정 (TDD)** — `EnvGOExchangeHoldBatchSize`+`HoldBatchSizeFromEnv`(기본 64, `OutboxBatchSizeFromEnv` 패턴, 3케이스). Run: `go test ./config/... -run TestHoldBatchSize -v` → PASS.

- [ ] **Step 2: 메트릭 (TDD)** — `hold_batch_size`(버킷 1,2,4,8,16,32,64,128), `hold_batch_fallbacks_total`, 그리고 입력 채널 게이지 등록 함수 `RegisterHoldCoordinatorInputGauge(lenFn func() int)`(기존 `RegisterMatchingEngineShardOrderChannelLenGauges` 패턴 — 게이지 정의 + main이 넘기는 len 클로저를 `promauto.NewGaugeFunc`로 등록). `reconciliation_test.go` 스타일 검증.

- [ ] **Step 3: 실패 테스트 (코디네이터 동작)**:

```go
// Submit→Run 왕복: 여유 시 성공 order+id 반환.
func TestIntegrationHoldCoordinatorSubmitHolds(t *testing.T)
// 입력 만석: 코디네이터 미기동(input 안 비워짐) + 작은 input cap을 채운 뒤 Submit → 503(Unavailable).
func TestHoldCoordinatorSubmitReturnsUnavailableWhenInputFull(t *testing.T)
// 폴백: HoldBatch가 실패하도록(예: 닫힌 DB 또는 주입) → 각 요청이 persistAndHold 폴백으로 처리되고 fallbacks_total 증가.
func TestIntegrationHoldCoordinatorFallsBackOnBatchError(t *testing.T)
// 종료 drain: 다수 제출 후 Shutdown → 전부 결과 시그널(유실 0), Run 반환.
func TestIntegrationHoldCoordinatorShutdownDrains(t *testing.T)
```

- [ ] **Step 4: 구현** — `Run`/`collectBatch`(OutboxWriter 미러링)/`processBatch`(폴백)/`Submit`(바운디드)/`Shutdown`:

```go
const defaultHoldBatchSize = 64
const defaultHoldFlushInterval = 5 * time.Millisecond
const holdCoordinatorInputCap = 1024

func NewHoldCoordinator(db *gorm.DB, orderRepo *repository.OrderRepository, walletRepo *repository.WalletRepository, ledgerRepo *repository.LedgerRepository, batchSize int) *HoldCoordinator {
	if batchSize <= 0 {
		batchSize = defaultHoldBatchSize
	}
	return &HoldCoordinator{
		DB: db, OrderRepo: orderRepo, WalletRepo: walletRepo, LedgerRepo: ledgerRepo,
		BatchSize: batchSize, FlushInterval: defaultHoldFlushInterval,
		input: make(chan holdRequest, holdCoordinatorInputCap), done: make(chan struct{}),
	}
}

// Submit은 요청을 입력에 바운디드(논블로킹) 제출한다. 입력이 만석이면 즉시 503.
// 제출 성공 후 결과 대기엔 타임아웃이 없다(고아 방지 — 제출된 요청은 항상 유한 시간에 시그널).
func (c *HoldCoordinator) Submit(order *model.Order) (*model.Order, error) {
	req := holdRequest{order: order, resultCh: make(chan holdResult, 1)}
	select {
	case c.input <- req:
	default:
		return nil, NewUnavailableErrorf("order intake is saturated, please retry shortly")
	}
	res := <-req.resultCh
	return res.Order, res.Err
}

// Run은 input이 닫힐 때까지 배치를 수집·처리하고, 닫힌 뒤 잔여를 처리하고 done을 닫는다.
func (c *HoldCoordinator) Run() {
	defer close(c.done)
	for {
		first, ok := <-c.input
		if !ok {
			return
		}
		batch, open := c.collectBatch([]holdRequest{first})
		c.processBatch(batch)
		if !open {
			return
		}
	}
}

func (c *HoldCoordinator) collectBatch(batch []holdRequest) ([]holdRequest, bool) {
	timer := time.NewTimer(c.FlushInterval)
	defer timer.Stop()
	for len(batch) < c.BatchSize {
		select {
		case req, ok := <-c.input:
			if !ok {
				return batch, false
			}
			batch = append(batch, req)
		case <-timer.C:
			return batch, true
		}
	}
	return batch, true
}

func (c *HoldCoordinator) processBatch(reqs []holdRequest) {
	orders := make([]*model.Order, len(reqs))
	for i := range reqs {
		orders[i] = reqs[i].order
	}
	results, err := c.HoldBatch(orders)
	if err != nil {
		metrics.HoldBatchFallbacksTotal.Inc()
		c.logf("hold batch of %d failed, falling back to per-order: %v", len(reqs), err)
		for i := range reqs {
			ferr := persistAndHold(c.DB, c.OrderRepo, c.WalletRepo, c.LedgerRepo, reqs[i].order)
			reqs[i].resultCh <- holdResult{Order: reqs[i].order, Err: ferr}
		}
		return
	}
	metrics.HoldBatchSize.Observe(float64(len(reqs)))
	for i := range reqs {
		reqs[i].resultCh <- results[i]
	}
}

// Shutdown은 입력을 닫아 drain을 트리거하고 Run 종료를 기다린다.
func (c *HoldCoordinator) Shutdown() {
	close(c.input)
	<-c.done
}

func (c *HoldCoordinator) logf(format string, args ...interface{}) {
	logger := c.Logger
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf(format, args...)
}
```

- [ ] **Step 5: 통과 + -race** — Run: `go test ./internal/service/... ./config/... ./internal/metrics/... -count=1` → PASS. `go test ./internal/service/... -race -count=1` → PASS.
- [ ] **Step 6: Commit** — 초안: `feat(order): 홀드 코디네이터 goroutine·폴백·바운디드 제출 추가 (2차 ②)`

---

### Task 5: CreateOrder 배선 + main.go

**Files:**
- Modify: `internal/service/order_service.go`(`CreateOrder`가 코디네이터 경유), `OrderService`에 `HoldCoordinator` 필드.
- Modify: `cmd/main.go`(코디네이터 생성·`go Run()`·`OrderService` 주입·종료 drain·게이지 등록).
- Test: `internal/service/order_acceptance_integration_test.go`(코디네이터 경유 경로 추가).

**Interfaces:**
- Consumes: 전 태스크 전부.

- [ ] **Step 1: CreateOrder 분기** — `OrderService`에 `HoldCoordinator *HoldCoordinator` 필드 추가. `CreateOrder`의 `persistAndHold(...)` 직접 호출을 분기로:

```go
	// [②] 코디네이터 있으면 배치 경유, 없으면(테스트·미배선) 단건 직접.
	if s.HoldCoordinator != nil {
		held, err := s.HoldCoordinator.Submit(order)
		if err != nil {
			return nil, err
		}
		order = held
	} else {
		if err := persistAndHold(s.OrderRepository.DB, s.OrderRepository, s.WalletRepository, s.LedgerRepository, order); err != nil {
			return nil, err
		}
	}
```

(① 입장 게이트는 이 앞에, 바운디드 엔진 핸드오프는 이 뒤에 — 무변경.)

- [ ] **Step 2: main.go 배선** — 코디네이터 생성·기동·주입·종료·게이지:

```go
	holdCoordinator := service.NewHoldCoordinator(config.DB, orderRepo, walletRepo, repository.NewLedgerRepository(config.DB), config.HoldBatchSizeFromEnv())
	go holdCoordinator.Run()
	orderService.HoldCoordinator = holdCoordinator
	metrics.RegisterHoldCoordinatorInputGauge(func() int { return holdCoordinator.InputLen() })
```

종료 순서(HTTP Shutdown 뒤, 엔진 Stop 앞)에 `holdCoordinator.Shutdown()` 삽입 — HTTP가 in-flight CreateOrder를 먼저 드레인하므로 `input` close 시 제출 경쟁 없음(send-on-closed 없음). `InputLen() int { return len(c.input) }` 접근자 추가.

- [ ] **Step 3: 통합 테스트** — 코디네이터를 주입한 `OrderService.CreateOrder`가 정상 홀드·200, 잔고 부족 시 409, 리컨실리에이션 위반 0.

- [ ] **Step 4: 전체 검증** — `go build ./...` + `go vet` + `go test ./... -count=1`(통합 SKIP 0) + `go test ./internal/service/... ./cmd/... -race -count=1` → PASS. 기존 정산·부트스트랩·outbox 통합 무수정 그린.

- [ ] **Step 5: Commit** — 초안: `feat(order): CreateOrder를 홀드 코디네이터에 배선 (2차 ②)`

---

### Task 6: 검증 + 완료 문서 + README

**Files:**
- Create: `docs/refactor/12_2차②_자금홀드_그룹커밋_완료.md`
- Modify: `docs/refactor/README.md`(2차 ② ✅)

- [ ] **Step 1: 전체 검증** — build + vet + 전체 스위트(통합 SKIP 0) + `-race`(matching·service·cmd) 전부 PASS.
- [ ] **Step 2: 완료 문서** — 왜(21·22번 DB CPU 병목, holdOrderAssets 26%) / 어떻게(코디네이터 그룹커밋 + fold-검증 + 개별 격리 + 단건 폴백, 스펙 링크) / 결과(등가성·격리·폴백·shutdown 테스트 요약, **처리량 수치 주장 금지 — 실증은 ⑤/23번 병기**).
- [ ] **Step 3: README** — 2차 표 ② 🔨→✅ + 완료 문서 링크.
- [ ] **Step 4: Commit + 푸시 + CI** — author→reviewer, `gh run watch` 그린.

---

## 다음 (범위 밖)

③(취소 우선 경로), ④(입장 정책 정교화 — ①의 게이트 + ②의 입력 만석 두 입장 지점 통합·Retry-After), ⑤(23번 실증 — 이때 `hold_batch_size` 분포로 배칭 발동·천장 상승 측정). 단일 코디네이터가 병목이면 샤딩 코디네이터 승격(측정 후).
