package matching

import (
	"sync"
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

// Slice의 emitBlock은 마지막 emit(MarketOrderDone)의 블로킹까지 포함해야 한다.
// Slice를 finishOrder보다 먼저 부르면 하류 포화 시 가장 오래 막히는 지점이
// 측정에서 통째로 빠진다.
//
// 구성이 까다롭다. cap=4면 emitBackpressured 임계가 int(4*0.75)=3이므로,
// 미리 2건만 채워두면 (2 < 3) 주문은 admit되지만 trade 2건이 나가면 채널이
// 가득 차(4/4) 마지막 MarketOrderDone send가 반드시 막힌다.
// cap을 더 줄이면 임계가 0이 되어 주문이 아예 admit되지 않는다.
func TestSliceEmitBlockIncludesMarketOrderDone(t *testing.T) {
	const hold = 200 * time.Millisecond

	me := newTestEngine()
	rec := &recordedObservers{}
	rec.install(me)
	me.ExecutionCh = make(chan ExecutionEvent, 4)
	me.ExecutionCh <- ExecutionEvent{}
	me.ExecutionCh <- ExecutionEvent{}
	require.False(t, me.emitBackpressured(), "admit이 가능한 상태여야 한다")
	go func() {
		for range me.SnapshotCh {
		}
	}()

	release := make(chan struct{})
	go func() {
		<-release
		for range me.ExecutionCh {
		}
	}()

	book := me.GetOrderBook("BTC")
	book.AddOrder(testOrder(1, "BTC", model.OrderSideSell, 50000, 1))
	book.AddOrder(testOrder(2, "BTC", model.OrderSideSell, 50001, 1))

	me.Start()
	market := &Order{
		ID: 3, UserID: 3, CoinSymbol: "BTC", Side: model.OrderSideBuy,
		QuoteAmount: decimal.NewFromInt(100001),
		OrderType:   model.OrderTypeMarket, EnqueuedAt: time.Now(),
	}
	me.OrderCh <- market

	time.Sleep(hold)
	close(release)

	require.Eventually(t, func() bool {
		_, _, _, _, slices := rec.counts()
		return slices == 1
	}, 5*time.Second, 5*time.Millisecond)

	rec.mu.Lock()
	sliceBlock := rec.slices[0].emitBlock
	doneBlock := rec.emitBlocks[EmitMarketDone]
	tradeBlock := rec.emitBlocks[EmitTrade]
	rec.mu.Unlock()

	require.Greater(t, doneBlock, hold/2, "MarketOrderDone send가 막혔어야 하는 구성이다")
	require.GreaterOrEqual(t, sliceBlock, doneBlock+tradeBlock,
		"Slice가 finishOrder보다 먼저 불려 MarketOrderDone 블로킹이 빠졌다")

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
}
