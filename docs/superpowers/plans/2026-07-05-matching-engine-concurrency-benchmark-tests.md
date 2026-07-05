# 매칭엔진 동시성/레이스 검증 및 성능 벤치마크 테스트 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `internal/matching` 패키지에 동시성/레이스 테스트 3종과 성능 벤치마크 3종을 새 파일 2개로 추가한다.

**Architecture:** 매칭엔진은 `Start()`의 단일 컨슈머 고루틴이 `OrderCh`/`CancelCh`/`SnapshotReq`를 순차 처리하므로 매칭 로직 자체는 이미 직렬화되어 있다. 동시성 테스트는 여러 프로듀서 고루틴이 `OrderCh`에 동시 제출하는 실사용 패턴을 스트레스 테스트하고, 벤치마크는 채널 오버헤드를 배제하기 위해 `me.Match()`를 직접 호출한다.

**Tech Stack:** Go 1.25.7, `testing` 표준 패키지, `github.com/stretchr/testify` (assert), `github.com/shopspring/decimal`, 기존 `internal/matching` 패키지의 `Order`/`MatchingEngine`/`OrderBook`/`PriceLevel`.

## Global Constraints

- Go 버전: 1.25.7 (go.mod에 명시된 그대로)
- `github.com/stretchr/testify v1.11.1`는 이미 go.mod에 있음 — 추가 의존성 설치 불필요
- 기존 `internal/matching/engine_test.go`는 절대 수정하지 않는다
- 새 파일은 정확히 2개만 만든다: `internal/matching/engine_concurrency_test.go`, `internal/matching/engine_bench_test.go`
- 두 새 파일 모두 `package matching`(내부 테스트 패키지, `_test` 접미사 없음)이며, `engine_test.go`에 이미 정의된 헬퍼(`testOrder`, `testUserOrder`, `submitAndWaitSnapshot`, `requireNextTrade`, `requireNextExecutionEvent`, `assertNoTrade`, `requireCancelSnapshot`)를 그대로 재사용한다 — 재정의하지 않는다
- 별도의 `testutil` 패키지 추출, Makefile/CI 신규 추가는 범위 밖이다

---

### Task 1: 동시성 테스트 파일 생성 + 안전장치 헬퍼 + Test 1

**Files:**
- Create: `internal/matching/engine_concurrency_test.go`

**Interfaces:**
- Consumes: `NewMatchingEngine()`, `(*MatchingEngine).Start()`, `(*MatchingEngine).GetOrderBook(string) *OrderBook`, `testOrder(id uint, symbol string, side model.OrderSide, price int64, amount int64) *Order` (engine_test.go에 정의됨), `submitAndWaitSnapshot(t *testing.T, me *MatchingEngine, order *Order) OrderBookSnapshot` (engine_test.go에 정의됨), `(*btree.BTreeG[*PriceLevel]).Ascend(func(*PriceLevel) bool)`, `(*deque.Deque[*Order]).Len() int`, `(*deque.Deque[*Order]).At(int) *Order`
- Produces: `waitWithTimeout(t *testing.T, wg *sync.WaitGroup, timeout time.Duration)` — Task 2, 3에서 재사용

- [ ] **Step 1: 파일 작성**

`internal/matching/engine_concurrency_test.go`:

```go
package matching

import (
	"sync"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
)

func waitWithTimeout(t *testing.T, wg *sync.WaitGroup, timeout time.Duration) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal("timed out waiting for goroutines to finish")
	}
}

func TestConcurrentOrderSubmission_NoRaceAndConsistentState(t *testing.T) {
	me := NewMatchingEngine()
	me.Start()

	const numGoroutines = 50
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(i int) {
			defer wg.Done()
			order := testOrder(uint(i+1), "BTC", model.OrderSideBuy, int64(50000+i), 1)
			submitAndWaitSnapshot(t, me, order)
		}(i)
	}

	waitWithTimeout(t, &wg, 5*time.Second)

	totalQty := decimal.Zero
	orderCount := 0
	me.GetOrderBook("BTC").BuyOrders.Ascend(func(level *PriceLevel) bool {
		for j := 0; j < level.Orders.Len(); j++ {
			totalQty = totalQty.Add(level.Orders.At(j).Amount)
			orderCount++
		}
		return true
	})

	assert.Equal(t, numGoroutines, orderCount)
	assert.True(t, totalQty.Equal(decimal.NewFromInt(numGoroutines)))
}
```

이 테스트는 모두 매수(Buy) 주문만 사용한다 — 매도 주문이 전혀 없으므로 어떤 가격을 넣어도 체결이 발생하지 않고, 그래서 `TradeCh`/`ExecutionCh`를 드레인할 필요가 없다. 각 고루틴이 서로 다른 가격(`50000+i`)에 주문을 넣으므로 오더북에 정확히 `numGoroutines`개의 주문이 남아야 하고, 총 수량은 `numGoroutines`와 같아야 한다.

- [ ] **Step 2: 레이스 검출과 함께 실행**

Run: `cd internal/matching && go test -race -run TestConcurrentOrderSubmission -v .`

Expected: `PASS`, "WARNING: DATA RACE" 문구 없음

- [ ] **Step 3: 커밋**

```bash
git add internal/matching/engine_concurrency_test.go
git commit -m "test: add concurrent order submission race test for matching engine"
```

---

### Task 2: 멀티 심볼 동시 접근 테스트 추가

**Files:**
- Modify: `internal/matching/engine_concurrency_test.go` (Task 1에서 만든 파일에 함수 추가)

**Interfaces:**
- Consumes: Task 1의 `waitWithTimeout`, `testOrder`, `submitAndWaitSnapshot`, `(*MatchingEngine).GetOrderBook`
- Produces: (없음, 최종 테스트 함수)

- [ ] **Step 1: import에 `sync/atomic` 추가하고 테스트 함수 추가**

`internal/matching/engine_concurrency_test.go`의 import 블록을 다음으로 교체:

```go
import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
)
```

파일 끝에 다음 함수 추가:

```go
func TestConcurrentMultiSymbolAccess_NoRace(t *testing.T) {
	me := NewMatchingEngine()
	me.Start()

	symbols := []string{"BTC", "ETH", "AVAX", "SOL", "DOGE"}
	const ordersPerSymbol = 20
	var wg sync.WaitGroup
	wg.Add(len(symbols) * ordersPerSymbol)

	var nextOrderID uint32
	for _, symbol := range symbols {
		for i := 0; i < ordersPerSymbol; i++ {
			go func(symbol string, i int) {
				defer wg.Done()
				id := atomic.AddUint32(&nextOrderID, 1)
				order := testOrder(uint(id), symbol, model.OrderSideBuy, int64(1000+i), 1)
				submitAndWaitSnapshot(t, me, order)
			}(symbol, i)
		}
	}

	waitWithTimeout(t, &wg, 5*time.Second)

	for _, symbol := range symbols {
		book := me.GetOrderBook(symbol)
		count := 0
		book.BuyOrders.Ascend(func(level *PriceLevel) bool {
			for j := 0; j < level.Orders.Len(); j++ {
				if level.Orders.At(j).CoinSymbol != symbol {
					t.Fatalf("order from symbol %s found in %s book", level.Orders.At(j).CoinSymbol, symbol)
				}
				count++
			}
			return true
		})
		assert.Equal(t, ordersPerSymbol, count, "symbol %s order count mismatch", symbol)
	}
}
```

이 테스트도 매수 주문만 사용해 체결이 없도록 하고, 서로 다른 심볼로 동시에 제출된 주문이 각자의 오더북에만 들어갔는지 확인한다. `atomic.AddUint32`로 고루틴 간 주문 ID 충돌을 막는다.

- [ ] **Step 2: 레이스 검출과 함께 실행**

Run: `cd internal/matching && go test -race -run TestConcurrentMultiSymbolAccess -v .`

Expected: `PASS`, 데이터 레이스 경고 없음

- [ ] **Step 3: 커밋**

```bash
git add internal/matching/engine_concurrency_test.go
git commit -m "test: add concurrent multi-symbol access race test for matching engine"
```

---

### Task 3: 멀티 엔진 인스턴스 격리 테스트 추가

**Files:**
- Modify: `internal/matching/engine_concurrency_test.go`

**Interfaces:**
- Consumes: Task 1의 `waitWithTimeout`, `testOrder`, `submitAndWaitSnapshot`, `requireNextTrade`, `assertNoTrade` (engine_test.go에 정의됨), `model.Trade.SellOrderID`, `model.Trade.BuyOrderID`, `model.Trade.EngineSequence`
- Produces: (없음, 최종 테스트 함수)

- [ ] **Step 1: 테스트 함수 추가**

`internal/matching/engine_concurrency_test.go` 파일 끝에 추가:

```go
func TestMultipleEngineInstances_Isolated(t *testing.T) {
	const numEngines = 5
	engines := make([]*MatchingEngine, numEngines)
	for i := range engines {
		engines[i] = NewMatchingEngine()
		engines[i].Start()
	}

	var wg sync.WaitGroup
	wg.Add(numEngines)
	for i, me := range engines {
		go func(i int, me *MatchingEngine) {
			defer wg.Done()
			submitAndWaitSnapshot(t, me, testOrder(uint(i+1), "BTC", model.OrderSideSell, 50000, 1))
			submitAndWaitSnapshot(t, me, testOrder(uint(i+100), "BTC", model.OrderSideBuy, 50000, 1))
		}(i, me)
	}
	waitWithTimeout(t, &wg, 5*time.Second)

	for i, me := range engines {
		trade := requireNextTrade(t, me)
		assert.Equal(t, uint(i+1), trade.SellOrderID, "engine %d trade sell order id mismatch", i)
		assert.Equal(t, uint(i+100), trade.BuyOrderID, "engine %d trade buy order id mismatch", i)
		assert.Equal(t, int64(1), trade.EngineSequence, "engine %d should have independent trade sequence starting at 1", i)
		assertNoTrade(t, me)
		assert.Equal(t, 0, me.GetOrderBook("BTC").BuyOrders.Len())
		assert.Equal(t, 0, me.GetOrderBook("BTC").SellOrders.Len())
	}
}
```

각 엔진마다 매도 주문(가격 50000) 후 같은 가격의 매수 주문을 제출해 체결시킨다. 모든 엔진이 동시에 고루틴에서 구동되지만, `EngineSequence`가 각 엔진에서 독립적으로 1부터 시작하는지, 오더북이 서로 섞이지 않았는지 확인한다.

- [ ] **Step 2: 레이스 검출과 함께 전체 동시성 테스트 실행**

Run: `cd internal/matching && go test -race -run "TestConcurrent|TestMultipleEngineInstances" -v .`

Expected: 3개 테스트 모두 `PASS`, 데이터 레이스 경고 없음

- [ ] **Step 3: 커밋**

```bash
git add internal/matching/engine_concurrency_test.go
git commit -m "test: add multi-engine isolation race test for matching engine"
```

---

### Task 4: 벤치마크 파일 생성 + 드레인 헬퍼 + 즉시 체결 벤치마크

**Files:**
- Create: `internal/matching/engine_bench_test.go`

**Interfaces:**
- Consumes: `NewMatchingEngine()`, `(*MatchingEngine).Match(*Order)`, `(*MatchingEngine).TradeCh`, `(*MatchingEngine).ExecutionCh`, `testOrder` (engine_test.go)
- Produces: `drainEngineEvents(me *MatchingEngine, done <-chan struct{})` — Task 5, 6에서 재사용

- [ ] **Step 1: 파일 작성**

`internal/matching/engine_bench_test.go`:

```go
package matching

import (
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
)

func drainEngineEvents(me *MatchingEngine, done <-chan struct{}) {
	for {
		select {
		case <-me.TradeCh:
		case <-me.ExecutionCh:
		case <-done:
			return
		}
	}
}

func BenchmarkMatch_ImmediateCross(b *testing.B) {
	me := NewMatchingEngine()
	me.Match(testOrder(1, "BTC", model.OrderSideSell, 50000, int64(b.N)+1))

	done := make(chan struct{})
	go drainEngineEvents(me, done)
	defer close(done)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		me.Match(testOrder(uint(i+2), "BTC", model.OrderSideBuy, 50000, 1))
	}
}
```

`me.Match()`를 직접 호출해 채널 오버헤드를 배제한다. 셋업에서 `b.N+1` 수량의 매도 주문 하나를 미리 넣어 벤치마크 루프 내내 소진되지 않을 유동성을 확보하고, 루프마다 수량 1짜리 매수 주문을 보내 즉시 체결시킨다. `TradeCh`/`ExecutionCh`는 백그라운드 고루틴이 계속 비워주므로 버퍼가 가득 차 블로킹되지 않는다.

- [ ] **Step 2: 실행**

Run: `cd internal/matching && go test -bench=BenchmarkMatch_ImmediateCross -benchmem -run=^$ .`

Expected: 에러 없이 완료, `ns/op`, `B/op`, `allocs/op` 수치가 출력됨

- [ ] **Step 3: 커밋**

```bash
git add internal/matching/engine_bench_test.go
git commit -m "test: add immediate-cross matching benchmark"
```

---

### Task 5: 오더북 깊이별 벤치마크 추가

**Files:**
- Modify: `internal/matching/engine_bench_test.go`

**Interfaces:**
- Consumes: Task 4의 `drainEngineEvents`, `testOrder`
- Produces: (없음, 최종 벤치마크 함수)

- [ ] **Step 1: import에 `fmt` 추가하고 벤치마크 함수 추가**

`internal/matching/engine_bench_test.go`의 import 블록을 다음으로 교체:

```go
import (
	"fmt"
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
)
```

파일 끝에 추가:

```go
func BenchmarkOrderBookDepth(b *testing.B) {
	depths := []int{100, 1000, 10000}
	for _, depth := range depths {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			me := NewMatchingEngine()
			for i := 0; i < depth; i++ {
				me.Match(testOrder(uint(i+1), "BTC", model.OrderSideSell, int64(50000+i), 1))
			}

			done := make(chan struct{})
			go drainEngineEvents(me, done)
			defer close(done)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				orderID := uint(100000 + i*2)
				me.Match(testOrder(orderID, "BTC", model.OrderSideBuy, 50000, 1))
				me.Match(testOrder(orderID+1, "BTC", model.OrderSideSell, 50000, 1))
			}
		})
	}
}
```

깊이(100/1,000/10,000)만큼 매도 주문을 가격 오름차순으로 미리 채운다. 루프마다 최우선가(50000)에 매수 주문을 보내 그 레벨을 소진시키고, 곧바로 같은 가격에 매도 주문을 다시 채워 넣어 오더북 깊이를 거의 일정하게 유지한다. 이렇게 하면 BTree 탐색 비용이 깊이에 따라 어떻게 변하는지를 측정할 수 있다.

- [ ] **Step 2: 실행**

Run: `cd internal/matching && go test -bench=BenchmarkOrderBookDepth -benchmem -run=^$ .`

Expected: `depth=100`, `depth=1000`, `depth=10000` 서브벤치마크 3개 모두 에러 없이 완료되고 각각 `ns/op`/`allocs/op` 출력

- [ ] **Step 3: 커밋**

```bash
git add internal/matching/engine_bench_test.go
git commit -m "test: add orderbook-depth scaling benchmark"
```

---

### Task 6: 대량 체결(벽 주문) 벤치마크 추가

**Files:**
- Modify: `internal/matching/engine_bench_test.go`

**Interfaces:**
- Consumes: Task 4의 `drainEngineEvents`, `testOrder`
- Produces: (없음, 최종 벤치마크 함수)

- [ ] **Step 1: 벤치마크 함수 추가**

`internal/matching/engine_bench_test.go` 파일 끝에 추가:

```go
func BenchmarkBulkFill(b *testing.B) {
	const wallDepth = 100

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		me := NewMatchingEngine()
		for lvl := 0; lvl < wallDepth; lvl++ {
			me.Match(testOrder(uint(lvl+1), "BTC", model.OrderSideSell, int64(50000+lvl), 1))
		}

		localDone := make(chan struct{})
		go drainEngineEvents(me, localDone)
		b.StartTimer()

		me.Match(testOrder(uint(wallDepth+1), "BTC", model.OrderSideBuy, int64(50000+wallDepth-1), int64(wallDepth)))

		b.StopTimer()
		close(localDone)
	}
}
```

매 반복마다 `b.StopTimer()`로 100단계 매도 벽(가격 50000~50099)을 재구성한 뒤, `b.StartTimer()`를 켜고 그 벽 전체를 정확히 쓸어담는 수량(100)의 매수 주문 하나를 보낸다. 셋업 비용은 타이머 밖에서 이루어지므로 측정값은 순수하게 "벽 전체를 체결하는 데 걸리는 시간"만 반영한다.

- [ ] **Step 2: 전체 벤치마크 실행**

Run: `cd internal/matching && go test -bench=. -benchmem -run=^$ .`

Expected: `BenchmarkMatch_ImmediateCross`, `BenchmarkOrderBookDepth/depth=100`, `BenchmarkOrderBookDepth/depth=1000`, `BenchmarkOrderBookDepth/depth=10000`, `BenchmarkBulkFill` 5개 모두 에러 없이 완료

- [ ] **Step 3: 전체 동시성 테스트 + 벤치마크 + 기존 테스트 스위트 전체 실행 (회귀 확인)**

Run: `cd internal/matching && go test -race -v . && go test -bench=. -benchmem -run=^$ .`

Expected: 기존 `engine_test.go`의 모든 테스트 + 새 동시성 테스트 3종 모두 `PASS`, 벤치마크 5종 모두 정상 출력. `engine_test.go`는 변경되지 않았으므로 기존 테스트는 그대로 통과해야 한다.

- [ ] **Step 4: 커밋**

```bash
git add internal/matching/engine_bench_test.go
git commit -m "test: add bulk-fill benchmark for matching engine"
```
