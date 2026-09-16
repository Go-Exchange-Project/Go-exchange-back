package matching

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// ===== Task 5: cancel phase와 응답 계약 (설계 §4.1, §4.2, §9.2 테스트 2·3·4·5·11) =====
//
// 공통 quantum (4,2) → 조각 시작 조건 7.

// 설계 §9.2 테스트 2 — 취소 공간 0. active sweep 없이 채널을 가득 채운
// 상태에서는 free==0이라 취소가 즉시 거절된다. 공간이 생기면 재시도로
// 제거된다.
func TestCancelBackpressuredWhenNoExecutionCapacity(t *testing.T) {
	me := NewMatchingEngine()
	me.maxMatchesPerTurn = 4
	me.maxConsecutiveCancels = 2
	me.ExecutionCh = make(chan ExecutionEvent, 16)

	var backpressured atomic.Int64
	me.Observers = EngineObservers{CancelBackpressured: func() { backpressured.Add(1) }}

	book := me.GetOrderBook("BTC")
	book.AddOrder(testOrder(1, "BTC", model.OrderSideSell, 50000, 1))

	for i := 0; i < 16; i++ { // free = 0, active sweep 없음
		me.ExecutionCh <- ExecutionEvent{}
	}

	me.Start()

	start := time.Now()
	result := me.CancelOrder(CancelOrderCommand{
		CoinSymbol: "BTC", OrderID: 1, Side: model.OrderSideSell, Price: decimal.NewFromInt(50000),
	})
	elapsed := time.Since(start)
	require.Less(t, elapsed, 500*time.Millisecond, "짧은 상한 안에 거절돼야 한다")
	require.True(t, errors.Is(result.Err, ErrCancelOrderBackpressured))
	require.False(t, result.Removed)
	require.Equal(t, int64(1), backpressured.Load())
	require.Equal(t, 1, book.SellOrders.Len(), "주문이 book에 남아 있어야 한다")
	require.Equal(t, 16, len(me.ExecutionCh), "이벤트가 추가되지 않아야 한다")

	<-me.ExecutionCh // 1건 소비 → free = 1

	retry := me.CancelOrder(CancelOrderCommand{
		CoinSymbol: "BTC", OrderID: 1, Side: model.OrderSideSell, Price: decimal.NewFromInt(50000),
	})
	require.True(t, retry.Removed, "공간이 생기면 재시도로 제거돼야 한다")

	me.Stop()

	var cancelledCount, drained int
	drainDone := make(chan struct{})
	go func() {
		for e := range me.ExecutionCh {
			drained++
			if e.OrderCancelled != nil {
				cancelledCount++
			}
		}
		close(drainDone)
	}()
	waitEngineDone(t, me)
	<-drainDone
	require.Equal(t, 1, cancelledCount, "OrderCancelled는 정확히 1회여야 한다")
	require.Equal(t, 16, drained)
}

// 설계 §9.2 테스트 3 — parked 중 한도 소진. free는 충분히 남아 있지만
// maxConsecutiveCancels(=2)를 넘는 취소는 "한도" 사유로 거절된다.
func TestParkedCancelExhaustsQuotaThenRejectsUntilResume(t *testing.T) {
	me := NewMatchingEngine()
	me.snapshotInterval = time.Second
	me.maxMatchesPerTurn = 4
	me.maxConsecutiveCancels = 2 // C = 2
	me.ExecutionCh = make(chan ExecutionEvent, 16)

	pr := newParkRecorder()
	var backpressured atomic.Int64
	obs := pr.observers()
	obs.CancelBackpressured = func() { backpressured.Add(1) }
	me.Observers = obs

	book := me.GetOrderBook("BTC")
	for i := 0; i < 4; i++ {
		book.AddOrder(testOrder(uint(i+1), "BTC", model.OrderSideSell, int64(50000+i), 1))
	}
	// victim은 매수 쪽에 둔다 — 시장가 매수 sweep은 매도 쪽만 소비하므로
	// 건드리지 않는다.
	victim1 := testOrder(101, "BTC", model.OrderSideBuy, 10, 1)
	victim2 := testOrder(102, "BTC", model.OrderSideBuy, 11, 1)
	victim3 := testOrder(103, "BTC", model.OrderSideBuy, 12, 1)
	book.AddOrder(victim1)
	book.AddOrder(victim2)
	book.AddOrder(victim3)

	// free = 6(< 7이라 park하지만, C=2건을 처리해도 6-2=4 >= 1이라 거절
	// 사유가 "free==0"이 아니라 "한도"임을 보장한다).
	const prefill = 10
	for i := 0; i < prefill; i++ {
		me.ExecutionCh <- ExecutionEvent{}
	}

	me.Start()
	market := &Order{
		ID: 100, CoinSymbol: "BTC", Side: model.OrderSideBuy,
		QuoteAmount: decimal.NewFromInt(50000 + 50001 + 50002 + 50003),
		OrderType:   model.OrderTypeMarket, EnqueuedAt: time.Now(),
	}
	me.OrderCh <- market

	require.Eventually(t, func() bool { return pr.started.Load() == 1 }, 3*time.Second, 5*time.Millisecond,
		"park해야 한다")

	// 첫 C(=2)건은 처리된다.
	r1 := me.CancelOrder(CancelOrderCommand{CoinSymbol: "BTC", OrderID: 101, Side: model.OrderSideBuy, Price: decimal.NewFromInt(10)})
	require.True(t, r1.Removed, "첫 번째 취소는 한도 안이라 처리돼야 한다")
	r2 := me.CancelOrder(CancelOrderCommand{CoinSymbol: "BTC", OrderID: 102, Side: model.OrderSideBuy, Price: decimal.NewFromInt(11)})
	require.True(t, r2.Removed, "두 번째 취소도 한도 안이라 처리돼야 한다")

	lenBeforeReject := len(me.ExecutionCh)

	// C+1번째부터는 한도 소진으로 거절된다.
	r3 := me.CancelOrder(CancelOrderCommand{CoinSymbol: "BTC", OrderID: 103, Side: model.OrderSideBuy, Price: decimal.NewFromInt(12)})
	require.True(t, errors.Is(r3.Err, ErrCancelOrderBackpressured), "세 번째는 한도 소진으로 거절돼야 한다")
	require.False(t, r3.Removed)
	require.Equal(t, int64(1), backpressured.Load())
	require.Equal(t, lenBeforeReject, len(me.ExecutionCh), "거절은 이벤트를 추가하지 않는다")
	// bookHasPrice는 스냅샷 캐시를 읽는다. ticker가 1s(긴 ticker)라 즉시
	// 조회하면 아직 캐시가 갱신되지 않았을 수 있으므로 Eventually로 확인한다.
	require.Eventually(t, func() bool { return bookHasPrice(t, me, model.OrderSideBuy, 12) },
		3*time.Second, 20*time.Millisecond, "거절된 주문은 book에 남아 있어야 한다")

	// 아직 소비하지 않았으므로 재개하지 않았다.
	require.Equal(t, int64(0), pr.finished.Load())

	// 소비 재개 → sweep 완주.
	go func() {
		for range me.ExecutionCh {
		}
	}()
	require.Eventually(t, func() bool { return pr.finished.Load() == 1 }, 3*time.Second, 5*time.Millisecond,
		"재개해야 한다")

	// 거절됐던 취소를 다시 보내면 제거된다(재개 뒤 admission이 매 turn
	// cancelsSinceProgress를 초기화하므로 한도가 다시 열린다).
	require.Eventually(t, func() bool {
		retry := me.CancelOrder(CancelOrderCommand{CoinSymbol: "BTC", OrderID: 103, Side: model.OrderSideBuy, Price: decimal.NewFromInt(12)})
		return retry.Removed
	}, 5*time.Second, 10*time.Millisecond, "재시도하면 제거돼야 한다")

	me.Stop()
	waitEngineDone(t, me)
}

// 설계 §9.2 테스트 4 — 응답 계약.
// (a) CancelCh에 버퍼 없는 ResponseCh를 가진 command를 넣고 읽지 않아도,
//     이어지는 정상 취소·조각 처리는 막히지 않는다(엔진은 논블로킹으로
//     응답하므로 계약을 어긴 테스트만 응답을 못 받는다).
func TestCancelChDirectInjectionWithUnbufferedResponseChDoesNotBlockEngine(t *testing.T) {
	me := newTestEngine()
	drainAll(me)
	me.Start()

	book := me.GetOrderBook("BTC")
	book.AddOrder(testOrder(1, "BTC", model.OrderSideSell, 50000, 1))

	unbuffered := make(chan CancelOrderResult) // 버퍼 0 — 계약 위반(테스트 전용 경로에서만 가능)
	me.CancelCh <- CancelOrderCommand{
		CoinSymbol: "BTC", OrderID: 999, Side: model.OrderSideSell, Price: decimal.NewFromInt(1),
		ResponseCh: unbuffered,
	}

	// 이어서 넣은 정상 취소가 여전히 처리돼야 한다.
	result := me.CancelOrder(CancelOrderCommand{
		CoinSymbol: "BTC", OrderID: 1, Side: model.OrderSideSell, Price: decimal.NewFromInt(50000),
	})
	require.True(t, result.Removed, "엔진이 멈추지 않고 다음 취소를 처리해야 한다")

	// 조각(admission)도 계속 처리된다.
	probe := testOrder(2, "BTC", model.OrderSideBuy, 60000, 1)
	me.OrderCh <- probe
	require.Eventually(t, func() bool { return bookHasPrice(t, me, model.OrderSideBuy, 60000) }, 3*time.Second, 5*time.Millisecond,
		"조각도 계속 처리돼야 한다")

	me.Stop()
	waitEngineDone(t, me)
}

// (b) public CancelOrder에 버퍼 없는 ResponseCh를 넘겨도 결과를 정상
// 반환한다 — 엔진이 내부적으로 버퍼 1 채널을 새로 만들어 쓰기 때문이다.
func TestPublicCancelOrderIgnoresCallerSuppliedResponseCh(t *testing.T) {
	me := newTestEngine()
	drainAll(me)
	me.Start()

	book := me.GetOrderBook("BTC")
	book.AddOrder(testOrder(1, "BTC", model.OrderSideBuy, 50000, 1))

	unbuffered := make(chan CancelOrderResult) // 버퍼 0
	result := me.CancelOrder(CancelOrderCommand{
		CoinSymbol: "BTC", OrderID: 1, Side: model.OrderSideBuy, Price: decimal.NewFromInt(50000),
		ResponseCh: unbuffered,
	})
	require.True(t, result.Removed, "버퍼 없는 ResponseCh를 넘겨도 정상 반환해야 한다")

	me.Stop()
	waitEngineDone(t, me)
}

// 설계 §9.2 테스트 5 — 성공 응답 시점. 성공 응답을 받은 순간 해당
// OrderCancelled가 이미 채널에 있다(소비자 없음). 현재(고침 전)는 응답을
// 이벤트보다 먼저 보내 이 계약이 깨진다.
func TestCancelSuccessResponseArrivesAfterEventIsEnqueued(t *testing.T) {
	me := NewMatchingEngine() // 소비자 없음, 기본 quantum·cap(1024)이라 여유가 충분하다
	book := me.GetOrderBook("BTC")
	book.AddOrder(testOrder(1, "BTC", model.OrderSideSell, 50000, 1))
	me.Start()

	result := me.CancelOrder(CancelOrderCommand{
		CoinSymbol: "BTC", OrderID: 1, Side: model.OrderSideSell, Price: decimal.NewFromInt(50000),
	})
	require.True(t, result.Removed)
	require.Equal(t, 1, len(me.ExecutionCh), "성공 응답을 받은 순간 이벤트가 이미 채널에 있어야 한다")

	me.Stop()
	go func() {
		for range me.ExecutionCh {
		}
	}()
	waitEngineDone(t, me)
}

// 설계 §9.2 테스트 11 — 교차 샤드. 샤드 A가 팬인 포화로 park해도, 로컬
// 칸이 남아 있는 샤드 B의 취소는 계속 성공한다. B 로컬도 소진되면(free 0)
// B 취소도 짧은 상한 안에 거절되지만, 어느 엔진도 panic하지 않는다.
func TestCrossShardCancelBackpressureIsIsolatedPerShard(t *testing.T) {
	se, err := NewShardedEngineWithQuantum(2, QuantumConfig{MaxMatchesPerTurn: 4, MaxConsecutiveCancels: 2})
	require.NoError(t, err)

	const symA, symB = "AAA", "BBB"
	shardA := se.shardFor(symA)
	shardB := se.shardFor(symB)
	require.NotSame(t, shardA, shardB, "두 심볼이 다른 샤드에 배정돼야 한다 — 테스트 전제")

	// Start() 전에 팬인·샤드 로컬 채널과 ticker를 교체한다(§2.3).
	se.ExecutionCh = make(chan ExecutionEvent, 4)
	shardA.ExecutionCh = make(chan ExecutionEvent, 8)
	shardB.ExecutionCh = make(chan ExecutionEvent, 8)
	shardA.snapshotInterval = time.Second
	shardB.snapshotInterval = time.Second

	prA := newParkRecorder()
	var backpressuredB atomic.Int64
	shardA.Observers = prA.observers()
	shardB.Observers = EngineObservers{CancelBackpressured: func() { backpressuredB.Add(1) }}

	bookA := shardA.GetOrderBook(symA)
	for i := 0; i < 20; i++ {
		bookA.AddOrder(testOrder(uint(i+1), symA, model.OrderSideSell, int64(50000+i), 1))
	}
	bookB := shardB.GetOrderBook(symB)
	bTargets := make([]*Order, 0, 20)
	for i := 0; i < 20; i++ {
		o := testOrder(uint(1000+i), symB, model.OrderSideSell, int64(60000+i), 1)
		bookB.AddOrder(o)
		bTargets = append(bTargets, o)
	}

	se.Start()
	takerA := &Order{
		ID: 9000, CoinSymbol: symA, Side: model.OrderSideBuy, OrderType: model.OrderTypeLimit,
		Price: decimal.NewFromInt(99999), Amount: decimal.NewFromInt(20),
	}
	se.SubmitOrder(takerA)

	require.Eventually(t, func() bool {
		return len(se.ExecutionCh) == cap(se.ExecutionCh) && prA.started.Load() > prA.finished.Load()
	}, 5*time.Second, 2*time.Millisecond, "샤드 A가 팬인 포화 + park 상태여야 한다")

	// 샤드 B: active sweep이 없으므로 admission phase가 매 turn
	// cancelsSinceProgress를 초기화한다 — 성공 취소를 반복해 로컬을 채운다.
	filled := 0
	for _, o := range bTargets {
		r := se.CancelOrder(CancelOrderCommand{CoinSymbol: symB, OrderID: o.ID, Side: model.OrderSideSell, Price: o.Price})
		if !r.Removed {
			break
		}
		filled++
		if len(shardB.ExecutionCh) == cap(shardB.ExecutionCh) {
			break
		}
	}
	require.Greater(t, filled, 0, "적어도 한 건은 성공했어야 한다")
	require.Equal(t, cap(shardB.ExecutionCh), len(shardB.ExecutionCh), "샤드 B 로컬이 가득 차야 한다")
	require.Less(t, filled, len(bTargets), "전부 성공하면 free==0 거절을 관측할 대상이 없다")

	// 그다음 B 취소는 free 0으로 거절된다.
	nextTarget := bTargets[filled]
	start := time.Now()
	rejected := se.CancelOrder(CancelOrderCommand{CoinSymbol: symB, OrderID: nextTarget.ID, Side: model.OrderSideSell, Price: nextTarget.Price})
	require.Less(t, time.Since(start), 500*time.Millisecond)
	require.True(t, errors.Is(rejected.Err, ErrCancelOrderBackpressured))
	require.Greater(t, backpressuredB.Load(), int64(0))

	// 팬인 소비 재개 → 두 샤드 모두 완주.
	go func() {
		for range se.ExecutionCh {
		}
	}()
	require.Eventually(t, func() bool { return prA.finished.Load() >= 1 }, 5*time.Second, 5*time.Millisecond,
		"샤드 A가 재개해야 한다")

	require.Eventually(t, func() bool {
		retry := se.CancelOrder(CancelOrderCommand{CoinSymbol: symB, OrderID: nextTarget.ID, Side: model.OrderSideSell, Price: nextTarget.Price})
		return retry.Removed
	}, 5*time.Second, 10*time.Millisecond, "재개 후 거절됐던 B 취소가 제거돼야 한다")

	se.Stop()
	select {
	case <-se.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("sharded engine did not stop in time")
	}
}
