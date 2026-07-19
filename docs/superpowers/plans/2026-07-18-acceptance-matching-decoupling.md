# ① 접수-매칭 분리 구현 계획 (2차 리팩토링 · 가용성)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development(권장) 또는 superpowers:executing-plans로 태스크별 실행. Steps use checkbox (`- [ ]`) syntax.

**Goal:** `POST /orders`의 접수 지연을 매칭 처리량에서 분리해 상한 시간 내 응답(빠른 접수 또는 빠른 거절)을 보장한다 — 스파이크 시 14.8초 매달림 제거.

**Architecture:** 주문은 이미 DB 트랜잭션(영속화 + 자금 홀드) 커밋 후에 엔진에 넘어간다(내구·정합 확정 상태). 그 뒤의 블로킹 `SubmitOrder`만 응답을 붙잡으므로, 엔진 핸드오프를 **바운디드**(`TrySubmitOrder`, 기본 100ms)로 바꾸고 유입 포화 시 **빠른 거절(503) + 홀드 보상(REJECTED)** 한다. `ExecutionCh` 백프레셔(A-3)·자금 홀드 동기성(overspend 방지)은 불변.

**Tech Stack:** Go, GORM, PostgreSQL, Gin, shopspring/decimal.

**스펙 문서:** `docs/superpowers/specs/2026-07-18-acceptance-matching-decoupling-design.md`

## Global Constraints

- 자금 홀드는 여전히 동기·원자적(overspend 불가) — 접수 분리는 홀드를 비동기화하지 않는다.
- `ExecutionCh` 블로킹 백프레셔(A-3 정합성 보호) 무변경. 바꾸는 건 `OrderCh` 유입 측뿐.
- 기존 `SubmitOrder`(블로킹)는 부트스트랩·리플레이 전용으로 **무변경 유지** — 그 경로엔 HTTP가 기다리지 않으므로 블로킹이 옳다. 기존 부트스트랩·outbox·리플레이·정산 통합 테스트가 무수정 그린이어야 한다.
- `orders.status`에는 기존 CHECK 제약이 없다(확인 완료) — `REJECTED` 추가에 **마이그레이션 불필요**. status CHECK 신설은 별도 위생 항목으로 범위 밖.
- 거절 주문 상태 = 신규 `REJECTED`(터미널 — 부트스트랩 `IN (PENDING,PARTIAL)`에서 자연 제외, 정산·취소 대상 아님).
- 접수 지연 실증 수치는 ⑤(23번)에서. **이 계획의 성공 기준은 "블로킹 제거 + 보상 정합 + 회귀 그린"까지.**
- 통합 테스트: 테스트 DB + DSN(포트 55432), `-v`로 SKIP 0 확인.
- 커밋은 태스크 단위, `commit-message` 스킬(author→reviewer), 한글.
- 로컬 셸: Bash 실패 시 PowerShell로, 커밋 서브에이전트엔 git diff/log를 프롬프트로 전달 + Read 검증.

## 파일 구조

- `internal/matching/engine.go` — `Engine` 인터페이스에 `TrySubmitOrder`·`IsIntakeAdmissible` 추가, `MatchingEngine` 구현.
- `internal/matching/sharded.go` — `ShardedEngine` 구현(shardFor 위임).
- `internal/matching/engine_test.go`, `sharded_test.go` — 단위 테스트.
- `internal/model/order.go` — `OrderStatusRejected` 상수.
- `internal/service/errors.go` — `ErrorKindUnavailable` + `NewUnavailableErrorf`.
- `internal/handler/order_handler.go` — `serviceErrorStatus`에 503 매핑.
- `internal/httpapi/response.go` — `CodeUnavailable` + `CodeForStatus` 503 케이스.
- `config/runtime.go`, `runtime_test.go` — `OrderAcceptanceTimeoutFromEnv`.
- `internal/service/order_service.go` — `CreateOrder` 분리 + `rejectAcceptedOrder`/`releaseInitialHold` + `AcceptanceTimeout` 필드.
- `internal/service/*_test.go` — CreateOrder 통합 테스트.
- `cmd/main.go` — `orderService.AcceptanceTimeout` 배선.
- 임의의 `matching.Engine` 페이크(있으면) — 신규 메서드 구현 추가.

---

### Task 1: 엔진 바운디드 제출 + 유입 게이트

**Files:**
- Modify: `internal/matching/engine.go`
- Modify: `internal/matching/sharded.go`
- Test: `internal/matching/engine_test.go`, `internal/matching/sharded_test.go`

**Interfaces:**
- Produces: `Engine.TrySubmitOrder(order *Order, within time.Duration) bool` — Task 4가 CreateOrder에서 바운디드 핸드오프에 사용.
- Produces: `Engine.IsIntakeAdmissible(coinSymbol string) bool` — Task 4가 DB 작업 전 입장 게이트에 사용.

- [ ] **Step 1: 실패 테스트 (engine_test.go)**

```go
func TestTrySubmitOrderReturnsTrueWhenRoom(t *testing.T) {
	me := NewMatchingEngine() // OrderCh cap 1024, 아직 Start 안 함(소비 없음)
	ok := me.TrySubmitOrder(&Order{CoinSymbol: "BTC", Side: model.OrderSideBuy, Price: decimal.NewFromInt(100), Amount: decimal.NewFromInt(1)}, 50*time.Millisecond)
	assert.True(t, ok)
	assert.Equal(t, 1, len(me.OrderCh))
}

func TestTrySubmitOrderReturnsFalseWhenFullWithinBound(t *testing.T) {
	me := NewMatchingEngine()
	for i := 0; i < cap(me.OrderCh); i++ { // 소비자 없이 가득 채움
		me.OrderCh <- &Order{CoinSymbol: "BTC"}
	}
	start := time.Now()
	ok := me.TrySubmitOrder(&Order{CoinSymbol: "BTC"}, 30*time.Millisecond)
	elapsed := time.Since(start)
	assert.False(t, ok)
	assert.GreaterOrEqual(t, elapsed, 30*time.Millisecond) // 바운드까지 기다렸다 실패
	assert.Less(t, elapsed, 500*time.Millisecond)          // 무한 블로킹 아님
}

func TestIsIntakeAdmissibleFalseAtHighWatermark(t *testing.T) {
	me := NewMatchingEngine()
	assert.True(t, me.IsIntakeAdmissible("BTC")) // 비었을 때 허용
	threshold := int(float64(cap(me.OrderCh)) * orderIntakeHighWatermarkRatio)
	for i := 0; i < threshold; i++ {
		me.OrderCh <- &Order{CoinSymbol: "BTC"}
	}
	assert.False(t, me.IsIntakeAdmissible("BTC")) // high-watermark 도달 시 거절
}
```

- [ ] **Step 2: 실패 확인** — Run: `go test ./internal/matching/... -run "TestTrySubmitOrder|TestIsIntakeAdmissible" -v` → FAIL(undefined).

- [ ] **Step 3: 구현 (engine.go)** — `Engine` 인터페이스에 두 메서드 추가 + `MatchingEngine` 구현:

```go
// orderIntakeHighWatermarkRatio: OrderCh가 이 비율 이상 차면 입장 게이트가 거절한다.
// (④가 히스테리시스·env로 정교화)
const orderIntakeHighWatermarkRatio = 0.9

type Engine interface {
	SubmitOrder(*Order)                                     // 블로킹 — 부트스트랩/리플레이 전용
	TrySubmitOrder(order *Order, within time.Duration) bool // 바운디드 — 라이브 HTTP 경로
	IsIntakeAdmissible(coinSymbol string) bool              // 유입 게이트(DB 작업 전)
	CancelOrder(CancelOrderCommand) CancelOrderResult
	RequestOrderBookSnapshot(coinSymbol string, depth int) (OrderBookSnapshot, error)
}

// TrySubmitOrder는 within 시간 안에 OrderCh에 넣지 못하면 false를 반환한다(무한
// 블로킹 없음). false일 때 주문은 채널에 들어가지 않았음이 select로 보장된다.
func (me *MatchingEngine) TrySubmitOrder(order *Order, within time.Duration) bool {
	timer := time.NewTimer(within)
	defer timer.Stop()
	select {
	case me.OrderCh <- order:
		return true
	case <-timer.C:
		return false
	}
}

// IsIntakeAdmissible는 OrderCh 점유가 high-watermark 미만이면 true. 단일 엔진이라
// coinSymbol은 무시한다(인터페이스 통일을 위해 받음 — ShardedEngine이 사용).
func (me *MatchingEngine) IsIntakeAdmissible(coinSymbol string) bool {
	return len(me.OrderCh) < int(float64(cap(me.OrderCh))*orderIntakeHighWatermarkRatio)
}
```

- [ ] **Step 4: ShardedEngine 구현 (sharded.go)** — shardFor로 위임:

```go
func (se *ShardedEngine) TrySubmitOrder(order *Order, within time.Duration) bool {
	return se.shardFor(order.CoinSymbol).TrySubmitOrder(order, within)
}

func (se *ShardedEngine) IsIntakeAdmissible(coinSymbol string) bool {
	return se.shardFor(coinSymbol).IsIntakeAdmissible(coinSymbol)
}
```

- [ ] **Step 5: ShardedEngine 테스트 (sharded_test.go)** — 같은 심볼이 같은 샤드로 라우팅되는지(게이트·제출 모두):

```go
func TestShardedEngineTrySubmitRoutesToOwningShard(t *testing.T) {
	se := NewShardedEngine(4)
	ok := se.TrySubmitOrder(&Order{CoinSymbol: "BTC", Side: model.OrderSideBuy, Price: decimal.NewFromInt(100), Amount: decimal.NewFromInt(1)}, 50*time.Millisecond)
	assert.True(t, ok)
	// 같은 심볼의 게이트/제출이 같은 샤드를 본다 — 라우팅 안정성
	assert.True(t, se.IsIntakeAdmissible("BTC"))
}
```

- [ ] **Step 6: 통과 + 회귀** — Run: `go test ./internal/matching/... -race -count=1` → PASS(신규 + 기존 무수정). 기존 `SubmitOrder`·샤딩 테스트 그린.

- [ ] **Step 7: Commit** — 초안: `feat(matching): 바운디드 주문 제출과 유입 게이트 추가 (2차 ①)`

---

### Task 2: REJECTED 상태 + 503 에러 배선

**Files:**
- Modify: `internal/model/order.go`
- Modify: `internal/service/errors.go`
- Modify: `internal/httpapi/response.go`
- Modify: `internal/handler/order_handler.go`
- Test: `internal/handler/order_handler_test.go`(있으면) 또는 신규 표 테스트

**Interfaces:**
- Produces: `model.OrderStatusRejected` — Task 4의 보상이 사용.
- Produces: `service.NewUnavailableErrorf(...)`, `service.ErrorKindUnavailable` — Task 4가 503 반환에 사용.
- Produces: `serviceErrorStatus`가 `ErrorKindUnavailable` → `503`, `CodeForStatus(503)` → `CodeUnavailable`.

- [ ] **Step 1: REJECTED 상수 추가 (order.go)**

```go
const (
	OrderStatusPending   OrderStatus = "PENDING"
	OrderStatusPartial   OrderStatus = "PARTIAL"
	OrderStatusFilled    OrderStatus = "FILLED"
	OrderStatusCancelled OrderStatus = "CANCELLED"
	OrderStatusRejected  OrderStatus = "REJECTED" // 시스템 부하로 접수 거절(유저 취소와 구분). 터미널.
)
```

- [ ] **Step 2: 실패 테스트 (errors 매핑)** — `internal/handler/order_handler_test.go`(없으면 생성)에:

```go
func TestServiceErrorStatusMapsUnavailableTo503(t *testing.T) {
	assert.Equal(t, http.StatusServiceUnavailable, serviceErrorStatus(service.NewUnavailableErrorf("saturated")))
}
```

Run: `go test ./internal/handler/... -run TestServiceErrorStatusMapsUnavailable -v` → FAIL(undefined: NewUnavailableErrorf).

- [ ] **Step 3: ErrorKindUnavailable 추가 (errors.go)**

```go
const (
	ErrorKindValidation  ErrorKind = "VALIDATION"
	ErrorKindConflict    ErrorKind = "CONFLICT"
	ErrorKindForbidden   ErrorKind = "FORBIDDEN"
	ErrorKindUnavailable ErrorKind = "UNAVAILABLE" // 일시적 과부하 — 503, 재시도 가능
)

func NewUnavailableErrorf(format string, args ...interface{}) error {
	return &DomainError{Kind: ErrorKindUnavailable, Message: fmt.Sprintf(format, args...)}
}
```

- [ ] **Step 4: 503 매핑 (order_handler.go serviceErrorStatus)** — kind switch에 케이스 추가:

```go
		case service.ErrorKindForbidden:
			return http.StatusForbidden
		case service.ErrorKindUnavailable:
			return http.StatusServiceUnavailable
```

- [ ] **Step 5: 응답 코드 (httpapi/response.go)** — `CodeUnavailable` 상수 + `CodeForStatus` 케이스:

```go
	CodeUnavailable = "SERVICE_UNAVAILABLE"
```
```go
	case http.StatusServiceUnavailable:
		return CodeUnavailable
```

- [ ] **Step 6: 통과 + 회귀** — Run: `go build ./... && go test ./internal/handler/... ./internal/httpapi/... ./internal/service/... -count=1` → PASS. 기존 매핑 테스트 무영향.

- [ ] **Step 7: REJECTED 터미널 방어 확인** — `isCancellableOrderStatus`(order_service.go)가 PENDING/PARTIAL만 허용하는지 Read로 확인 → REJECTED는 자연히 취소 불가. `validateOrderStatusForSettlement`의 default→error가 REJECTED를 정산 거부(엔진 미진입이라 도달 불가지만 방어). 코드 추적만, 필요 시 명시 케이스 추가.

- [ ] **Step 8: Commit** — 초안: `feat(order): REJECTED 상태와 503 UNAVAILABLE 에러 배선 (2차 ①)`

---

### Task 3: 접수 타임아웃 설정

**Files:**
- Modify: `config/runtime.go`, `config/runtime_test.go`

**Interfaces:**
- Produces: `config.OrderAcceptanceTimeoutFromEnv() time.Duration`(기본 100ms) — Task 4가 main에서 `orderService.AcceptanceTimeout`에 주입.

- [ ] **Step 1: 실패 테스트 (runtime_test.go)** — `OutboxBatchSize` 테스트 패턴 계승:

```go
func TestOrderAcceptanceTimeoutFromEnvDefaultsTo100ms(t *testing.T) {
	requireUnsetEnv(t, EnvGOExchangeAcceptanceTimeoutMs)
	assert.Equal(t, 100*time.Millisecond, OrderAcceptanceTimeoutFromEnv())
}

func TestOrderAcceptanceTimeoutFromEnvUsesOverride(t *testing.T) {
	t.Setenv(EnvGOExchangeAcceptanceTimeoutMs, "250")
	assert.Equal(t, 250*time.Millisecond, OrderAcceptanceTimeoutFromEnv())
}

func TestOrderAcceptanceTimeoutFromEnvFallsBackOnInvalid(t *testing.T) {
	t.Setenv(EnvGOExchangeAcceptanceTimeoutMs, "nope")
	assert.Equal(t, 100*time.Millisecond, OrderAcceptanceTimeoutFromEnv())
}
```

Run: `go test ./config/... -run TestOrderAcceptanceTimeout -v` → FAIL(undefined).

- [ ] **Step 2: 구현 (runtime.go)** — const 블록 + 함수:

```go
	EnvGOExchangeAcceptanceTimeoutMs = "GOEXCHANGE_ACCEPTANCE_TIMEOUT_MS"
```
```go
const defaultAcceptanceTimeoutMs = 100
```
```go
// OrderAcceptanceTimeoutFromEnv는 주문 접수 시 엔진 핸드오프의 바운디드 대기
// 상한이다. 일시 버스트는 흡수하되 지속 포화는 이 시간 후 503으로 거절한다.
func OrderAcceptanceTimeoutFromEnv() time.Duration {
	ms := parsePositiveIntEnv(EnvGOExchangeAcceptanceTimeoutMs, defaultAcceptanceTimeoutMs)
	return time.Duration(ms) * time.Millisecond
}
```

- [ ] **Step 3: 통과** — Run: `go test ./config/... -count=1` → PASS.

- [ ] **Step 4: Commit** — 초안: `feat(config): 주문 접수 타임아웃 GOEXCHANGE_ACCEPTANCE_TIMEOUT_MS 추가 (2차 ①)`

---

### Task 4: CreateOrder 접수-매칭 분리 + 거절 보상

**Files:**
- Modify: `internal/service/order_service.go`
- Modify: `cmd/main.go`(AcceptanceTimeout 배선)
- Test: `internal/service/order_service_integration_test.go`(또는 기존 CreateOrder 통합 테스트 파일)

**Interfaces:**
- Consumes: `Engine.TrySubmitOrder`/`IsIntakeAdmissible`(Task 1), `model.OrderStatusRejected`(Task 2), `service.NewUnavailableErrorf`(Task 2), `config.OrderAcceptanceTimeoutFromEnv`(Task 3).
- Produces: 재작성된 `CreateOrder`, 신규 `rejectAcceptedOrder`/`releaseInitialHold`, `OrderService.AcceptanceTimeout` 필드.

- [ ] **Step 1: 실패 통합 테스트 (실 DB)** — 세 시나리오:

```go
// 정상: 여유 시 200·엔진 접수·주문 PENDING (기존 동작 보존)
func TestIntegrationCreateOrderSubmitsWhenIntakeHasRoom(t *testing.T)

// 게이트 거절: 유입 포화(IsIntakeAdmissible=false)면 DB 작업 없이 503(UNAVAILABLE),
// 주문 미생성·자금 미락. 페이크 엔진으로 IsIntakeAdmissible=false 주입.
func TestIntegrationCreateOrderFastRejectsWhenIntakeSaturated(t *testing.T)

// 바운디드 거절+보상: 게이트는 통과하나 TrySubmitOrder=false(레이스)면 주문이
// 영속화·홀드된 뒤 보상으로 홀드 전액 해제 + 상태 REJECTED, 503 반환. 잔고가
// 홀드 이전으로 복원되고 원장에 OrderHold+OrderRelease 쌍이 남아 리컨실리에이션 위반 0.
func TestIntegrationCreateOrderCompensatesWhenHandoffTimesOut(t *testing.T)
```

(페이크 엔진: `IsIntakeAdmissible`/`TrySubmitOrder`를 제어하는 test double. 기존 `matching.Engine` 만족 페이크가 있으면 확장, 없으면 최소 페이크 작성 — `SubmitOrder`/`CancelOrder`/`RequestOrderBookSnapshot`도 no-op 구현.)

Run: `go test ./internal/service/... -run TestIntegrationCreateOrder -v` → FAIL.

- [ ] **Step 2: OrderService 필드 + 타임아웃 getter (order_service.go)**

```go
// (OrderService 구조체에 필드 추가)
	AcceptanceTimeout time.Duration // 0이면 defaultAcceptanceTimeout
```
```go
const defaultAcceptanceTimeout = 100 * time.Millisecond

func (s *OrderService) acceptanceTimeout() time.Duration {
	if s.AcceptanceTimeout > 0 {
		return s.AcceptanceTimeout
	}
	return defaultAcceptanceTimeout
}
```

- [ ] **Step 3: CreateOrder 재작성 (order_service.go:87)** — 게이트 → DB → 바운디드 핸드오프 → 보상:

```go
func (s *OrderService) CreateOrder(input CreateOrderInput) (*model.Order, error) {
	order, err := s.BuildOrder(input)
	if err != nil {
		return nil, err
	}

	// ① 입장 게이트: 엔진 유입이 포화면 DB 작업 전에 빠른 거절(503).
	if s.MatchingEngine != nil && !s.MatchingEngine.IsIntakeAdmissible(order.CoinSymbol) {
		return nil, NewUnavailableErrorf("order intake is saturated, please retry shortly")
	}

	if err := s.OrderRepository.DB.Transaction(func(tx *gorm.DB) error {
		orderRepo := s.OrderRepository.WithTx(tx)
		walletRepo := s.WalletRepository.WithTx(tx)
		ledgerRepo := s.LedgerRepository.WithTx(tx)
		if err := orderRepo.CreateOrder(order); err != nil {
			return err
		}
		return holdOrderAssets(walletRepo, ledgerRepo, order)
	}); err != nil {
		return nil, err
	}

	// ① 바운디드 핸드오프: 매칭 처리량에 응답이 매달리지 않게. 주문은 이미
	// 영속화+홀드로 내구·정합 확정 상태다. 바운드 내 접수 못 하면(레이스로 포화)
	// 보상으로 홀드를 풀고 REJECTED로 종결한 뒤 503.
	if s.MatchingEngine != nil {
		submitted := s.MatchingEngine.TrySubmitOrder(&matching.Order{
			ID:                order.ID,
			UserID:            order.UserID,
			CoinSymbol:        order.CoinSymbol,
			Side:              order.Side,
			Price:             order.Price,
			Amount:            order.Amount,
			QuoteAmount:       matchingQuoteAmountForOrder(order),
			CreatedAt:         order.CreatedAt,
			EnqueuedAt:        time.Now(),
			OrderType:         order.OrderType,
			FilledAmount:      order.FilledAmount,
			FilledQuoteAmount: order.FilledQuoteAmount,
		}, s.acceptanceTimeout())
		if !submitted {
			if rerr := s.rejectAcceptedOrder(order); rerr != nil {
				return nil, fmt.Errorf("order intake saturated and hold release failed for order %d: %w", order.ID, rerr)
			}
			return nil, NewUnavailableErrorf("order intake is saturated, please retry shortly")
		}
	}

	return order, nil
}
```

- [ ] **Step 4: 보상 헬퍼 (order_service.go)** — `holdOrderAssets`의 정확한 역: 초기 홀드 전액 해제 + REJECTED. 주문은 엔진 미진입이라 어떤 trade도 참조 안 함 → 동기 보상 안전.

```go
// rejectAcceptedOrder는 영속화·홀드됐으나 엔진 접수에 실패한 주문을 원상복구한다:
// 초기 홀드를 전액 해제하고 상태를 REJECTED로 종결한다(한 트랜잭션, 원장 기록 포함).
func (s *OrderService) rejectAcceptedOrder(order *model.Order) error {
	return s.OrderRepository.DB.Transaction(func(tx *gorm.DB) error {
		orderRepo := s.OrderRepository.WithTx(tx)
		walletRepo := s.WalletRepository.WithTx(tx)
		ledgerRepo := s.LedgerRepository.WithTx(tx)
		if err := releaseInitialHold(walletRepo, ledgerRepo, order); err != nil {
			return err
		}
		return orderRepo.UpdateOrderExecution(order.ID, order.FilledAmount, order.FilledQuoteAmount, model.OrderStatusRejected)
	})
}

// releaseInitialHold는 holdOrderAssets가 건 초기 홀드의 정확한 역이다(미체결 주문
// 이므로 홀드 전액). 매수=예약 KRW, 매도=예약 코인 수량.
func releaseInitialHold(walletRepo *repository.WalletRepository, ledgerRepo *repository.LedgerRepository, order *model.Order) error {
	switch order.Side {
	case model.OrderSideBuy:
		wallet, err := walletRepo.FindKRWWalletByUserIDForUpdate(order.UserID)
		if err != nil {
			return err
		}
		releaseAmount := quoteAmountWithTradingFee(order.Price.Mul(order.Amount))
		if order.OrderType == model.OrderTypeMarket {
			releaseAmount = order.QuoteAmount
		}
		update, err := releaseBuyOrderHold(wallet, releaseAmount)
		if err != nil {
			return err
		}
		if err := walletRepo.UpdateBalances(order.UserID, model.KRWAssetSymbol, update.AvailableBalance, update.LockedBalance); err != nil {
			return err
		}
		entry := ledgerEntryFromWalletUpdate(wallet, update, model.LedgerEntryTypeOrderRelease, model.LedgerReferenceTypeOrder, order.ID, "")
		return ledgerRepo.Create(&entry)
	case model.OrderSideSell:
		wallet, err := walletRepo.FindByUserIDAndCoinSymbolForUpdate(order.UserID, order.CoinSymbol)
		if err != nil {
			return err
		}
		update, err := releaseSellOrderHold(wallet, order.Amount)
		if err != nil {
			return err
		}
		if err := walletRepo.UpdateBalances(order.UserID, order.CoinSymbol, update.AvailableBalance, update.LockedBalance); err != nil {
			return err
		}
		entry := ledgerEntryFromWalletUpdate(wallet, update, model.LedgerEntryTypeOrderRelease, model.LedgerReferenceTypeOrder, order.ID, "")
		return ledgerRepo.Create(&entry)
	default:
		return NewValidationErrorf("invalid order side")
	}
}
```

- [ ] **Step 5: main.go 배선** — `orderService := service.NewOrderService(...)` 뒤에:

```go
	orderService.AcceptanceTimeout = config.OrderAcceptanceTimeoutFromEnv()
```

- [ ] **Step 6: 통과 + 회귀** — Run: `go build ./...`; `go test ./internal/service/... -run TestIntegrationCreateOrder -v` → PASS(3건). 기존 CreateOrder·정산·부트스트랩 통합 테스트 무수정 그린. `matching.Engine` 페이크에 신규 메서드 추가로 컴파일 맞춤.

- [ ] **Step 7: 전체 -race + 리컨실리에이션** — Run: `go test ./internal/matching/... ./internal/service/... ./cmd/... -race -count=1` → PASS. 보상 시나리오 후 리컨실리에이션(원장-지갑 일치) 위반 0을 통합 테스트로 확인.

- [ ] **Step 8: Commit** — 초안: `feat(order): 접수를 매칭 핸드오프에서 분리하고 포화 시 REJECTED 보상 (2차 ①)`

---

### Task 5: 전체 검증 + 완료 문서 + README

**Files:**
- Create: `docs/refactor/11_2차①_접수매칭분리_완료.md`
- Modify: `docs/refactor/README.md`(2차 ① ✅)

- [ ] **Step 1: 전체 검증** — `go build ./...` + `go vet` + `go test ./... -count=1`(통합, SKIP 0) + `go test ./internal/matching/... ./internal/service/... ./cmd/... -race -count=1` 전부 PASS.

- [ ] **Step 2: 완료 문서** — `11_2차①_접수매칭분리_완료.md`: 왜(22번 14.8초, 접수가 매칭에 묶임) / 어떻게(주문은 커밋 후 내구 확정 → 바운디드 핸드오프 + 게이트 + REJECTED 보상, ExecutionCh 백프레셔·홀드 동기성 불변) / 결과(테스트 요약, **접수 지연 실증 수치는 ⑤/23번 병기 — 이 조각은 처리량 수치를 주장하지 않음**). REJECTED 마이그레이션 불필요(status CHECK 부재) 기록.

- [ ] **Step 3: README** — 2차 리팩토링 표 ① 상태 🔨→✅, 완료 문서 링크. (②③④⑤는 예정 유지)

- [ ] **Step 4: Commit + 푸시 + CI** — author→reviewer, 푸시 후 `gh run watch` 그린.

---

## 다음 (범위 밖)

②(자금 홀드 배칭 — 천장 올리기), ③(취소 우선 경로), ④(입장 정책 정교화 — pre-DB 게이트 히스테리시스·Retry-After·취소 면제), ⑤(23번 실증). ①의 접수 지연 최종 수치는 ②·③·④와 함께 ⑤에서 측정.
