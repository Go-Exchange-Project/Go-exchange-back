package matching

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

type sliceSample struct {
	trades    int
	emitBlock time.Duration
}

type recordedObservers struct {
	mu         sync.Mutex
	admitted   []time.Duration
	doneTrades []int
	cancels    []time.Duration
	emits      []EmitKind
	slices     []sliceSample
	emitBlocks map[EmitKind]time.Duration
}

// install은 Start() 전에만 부른다. 실행 중 재대입은 data race다.
func (r *recordedObservers) install(me *MatchingEngine) {
	r.emitBlocks = map[EmitKind]time.Duration{}
	me.Observers = EngineObservers{
		Slice: func(n int, b time.Duration) {
			r.mu.Lock()
			r.slices = append(r.slices, sliceSample{trades: n, emitBlock: b})
			r.mu.Unlock()
		},
		OrderAdmitted: func(d time.Duration) {
			r.mu.Lock()
			r.admitted = append(r.admitted, d)
			r.mu.Unlock()
		},
		OrderDone: func(n int) {
			r.mu.Lock()
			r.doneTrades = append(r.doneTrades, n)
			r.mu.Unlock()
		},
		Cancel: func(d time.Duration) {
			r.mu.Lock()
			r.cancels = append(r.cancels, d)
			r.mu.Unlock()
		},
		EmitBlock: func(k EmitKind, d time.Duration) {
			r.mu.Lock()
			r.emits = append(r.emits, k)
			r.emitBlocks[k] += d
			r.mu.Unlock()
		},
	}
}

func (r *recordedObservers) counts() (admitted, done, cancels, emits, slices int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.admitted), len(r.doneTrades), len(r.cancels), len(r.emits), len(r.slices)
}

func drainAll(me *MatchingEngine) {
	go func() {
		for range me.ExecutionCh {
		}
	}()
	go func() {
		for range me.SnapshotCh {
		}
	}()
}

func TestObserversRecordAdmitSliceDoneAndEmit(t *testing.T) {
	me := newTestEngine()
	rec := &recordedObservers{}
	rec.install(me)
	drainAll(me)
	me.Start()

	sell := stopTestLimitOrder(1, model.OrderSideSell, 50000, 2)
	sell.EnqueuedAt = time.Now()
	me.OrderCh <- sell
	buy := stopTestLimitOrder(2, model.OrderSideBuy, 50000, 2)
	buy.EnqueuedAt = time.Now()
	me.OrderCh <- buy

	require.Eventually(t, func() bool {
		_, done, _, _, _ := rec.counts()
		return done == 2
	}, 5*time.Second, 5*time.Millisecond)

	me.Stop()
	waitEngineDone(t, me)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.Len(t, rec.admitted, 2, "admit된 주문마다 OrderAdmitted 1회")
	require.Equal(t, []int{0, 1}, rec.doneTrades, "첫 주문은 체결 0, 두 번째는 1")
	require.Len(t, rec.slices, 2, "계측 커밋도 주문당 Slice를 1회 내야 baseline이 비지 않는다")
	require.Equal(t, 0, rec.slices[0].trades)
	require.Equal(t, 1, rec.slices[1].trades)
	require.Contains(t, rec.emits, EmitTrade)
}

func TestCancelObserverUsesEnqueuedAt(t *testing.T) {
	me := newTestEngine()
	rec := &recordedObservers{}
	rec.install(me)
	drainAll(me)
	me.Start()

	resting := stopTestLimitOrder(7, model.OrderSideSell, 50000, 1)
	resting.EnqueuedAt = time.Now()
	me.OrderCh <- resting
	require.Eventually(t, func() bool {
		_, done, _, _, _ := rec.counts()
		return done == 1
	}, 5*time.Second, 5*time.Millisecond)

	result := me.CancelOrder(CancelOrderCommand{
		CoinSymbol: "BTC", OrderID: 7,
		Side: model.OrderSideSell, Price: resting.Price,
	})
	require.True(t, result.Removed)

	me.Stop()
	waitEngineDone(t, me)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	// processCancel은 EnqueuedAt이 제로면 관측을 건너뛴다. 따라서 표본이
	// 1건이라는 사실 자체가 "CancelOrder가 EnqueuedAt을 채웠다"의 증거다.
	// 값의 크기는 단언하지 않는다 — Windows 타이머 해상도(~15ms)에서
	// time.Since가 정확히 0으로 나올 수 있다.
	require.Len(t, rec.cancels, 1, "CancelOrder가 EnqueuedAt을 채워야 관측된다")
	require.Contains(t, rec.emits, EmitCancelled)
}

// 즉시 완료 경로는 OrderAdmitted만 낸다. 매칭을 수행하지 않아 셀 체결이
// 없으므로, OrderDone(0)/Slice(0)를 내면 executions_per_order 분포가 오염된다.
func TestImmediateCompletionEmitsOnlyOrderAdmitted(t *testing.T) {
	me := newTestEngine()
	rec := &recordedObservers{}
	rec.install(me)
	drainAll(me)
	me.Start()

	zero := stopTestLimitOrder(1, model.OrderSideSell, 50000, 1)
	zero.Amount = decimal.Zero
	zero.EnqueuedAt = time.Now()
	me.OrderCh <- zero

	require.Eventually(t, func() bool {
		admitted, _, _, _, _ := rec.counts()
		return admitted == 1
	}, 5*time.Second, 5*time.Millisecond)

	me.Stop()
	waitEngineDone(t, me)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.Empty(t, rec.doneTrades, "즉시 완료 주문은 OrderDone을 내지 않는다")
	require.Empty(t, rec.slices, "즉시 완료 주문은 Slice를 내지 않는다")
}

// 마지막 조각 전 park가 관측되고, 재개된 마지막 조각이 Trade와
// MarketOrderDone을 순서대로 낸다(설계 §9.1 — 원래 이름은
// TestSliceEmitBlockIncludesMarketOrderDone였고 blocking-send 시간을
// 측정했으나, park 도입 후 조각 시작 자체가 용량을 미리 확인하므로 조각
// 내부 send는 더 이상 유의미하게 블로킹되지 않는다. 이제는 "용량 부족 시
// 마지막 조각 전에 park한다"와 "재개된 조각이 순서를 지킨다"를 확인한다).
//
// quantum (1,1) → 조각 시작 조건 1+1+1=3. 매도 maker 2건, 시장가 매수 1건
// (정확히 2건 체결)이라 maxMatchesPerTurn=1은 필연적으로 조각을 2개로
// 나눈다 — 첫 조각(Trade 1건)은 free 3으로 시작 가능하고, 마지막 조각
// (Trade 1건 + MarketOrderDone 1건)은 free 2라 park해야 한다.
func TestParkBeforeFinalSliceIsObservedAndSliceEmitsMarketOrderDone(t *testing.T) {
	me := newTestEngine()
	me.maxMatchesPerTurn = 1
	me.maxConsecutiveCancels = 1
	me.ExecutionCh = make(chan ExecutionEvent, 4)

	var started, finished atomic.Int64
	me.Observers = EngineObservers{
		ParkStarted:  func() { started.Add(1) },
		ParkDuration: func(time.Duration) { finished.Add(1) },
	}

	// free = 3이 되도록 Start() 전에 dummy 1건만 채운다(§2.3: Start() 뒤
	// 외부 write 금지). dummy 2건(원래 구성, free=2)은 첫 조각 전부터 park해
	// 테스트 의도(마지막 조각 앞에서만 park)가 달라지므로 쓰지 않는다.
	me.ExecutionCh <- ExecutionEvent{}

	go func() {
		for range me.SnapshotCh {
		}
	}()

	book := me.GetOrderBook("BTC")
	book.AddOrder(testOrder(1, "BTC", model.OrderSideSell, 50000, 1))
	book.AddOrder(testOrder(2, "BTC", model.OrderSideSell, 50001, 1))

	me.Start()
	market := &Order{
		ID: 3, UserID: 3, CoinSymbol: "BTC", Side: model.OrderSideBuy,
		QuoteAmount: decimal.NewFromInt(50000 + 50001),
		OrderType:   model.OrderTypeMarket, EnqueuedAt: time.Now(),
	}
	me.OrderCh <- market

	require.Eventually(t, func() bool { return started.Load() == 1 }, 3*time.Second, 2*time.Millisecond,
		"마지막 조각 전에 park해야 한다")
	require.Equal(t, int64(0), finished.Load(), "아직 재개하지 않았다")
	require.Equal(t, 2, len(me.ExecutionCh), "채널 길이는 dummy 1 + 첫 조각 Trade 1 = 2여야 한다")

	// 소비 재개: dummy 1건을 비워 free를 3으로 만든다.
	<-me.ExecutionCh

	require.Eventually(t, func() bool { return finished.Load() == 1 }, 3*time.Second, 2*time.Millisecond,
		"free=3이 되면 재개해야 한다")

	firstTrade := requireNextExecutionEvent(t, me) // 첫 조각의 Trade(이미 채널에 있던 것)
	require.NotNil(t, firstTrade.Trade)
	lastTrade := requireNextExecutionEvent(t, me) // 마지막 조각의 Trade
	require.NotNil(t, lastTrade.Trade)
	done := requireNextExecutionEvent(t, me)
	require.NotNil(t, done.MarketOrderDone, "Trade 다음에 MarketOrderDone이 순서대로 나와야 한다")

	me.Stop()
	waitEngineDone(t, me)
}

func TestNilObserversAreSafe(t *testing.T) {
	me := newTestEngine()
	drainAll(me)
	me.Start()
	order := stopTestLimitOrder(1, model.OrderSideSell, 50000, 1)
	me.OrderCh <- order
	me.Stop()
	waitEngineDone(t, me)

	// 설계 §8.1 신규 4콜백도 제로값(nil)이면 안전해야 한다.
	var o EngineObservers
	require.NotPanics(t, func() {
		o.parkStarted()
		o.parkDuration(0)
		o.cancelBackpressured()
		o.shutdownLatched()
	})
}
