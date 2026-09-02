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

// 이 파일은 조각화가 만든 **취소 의미 변화**를 고정한다(설계 §5 B′, §6 A′).
//
// 조각화 이전에는 sweep 중 취소가 아예 관측될 수 없었다. 이제는 조각 사이에
// 취소가 처리되므로 아래 두 가지가 새 상태다.
//
//  1. active sweep 중인 taker의 취소는 not-found다 — 그 주문은 book에 없다.
//     사용자는 not-found를 보지 않는다. HTTP는 이미 202를 반환했고,
//     cancel_command_worker가 DB 주문이 open인 동안 재시도한다
//     (cancel_command_worker.go의 applyNotFound → scheduleRetry).
//  2. maker는 조각 사이 취소로 체결을 피할 수 있다. 의도한 대가다.
//
// DB·outbox가 필요한 복구 의미(A-2/A-3)는 여기서 다루지 않는다.
// 엔진 goroutine이 실제로 죽고 더는 쓰지 않는다는 것만 여기서 고정한다.

// sweepFixture는 maker N개를 채우고 조각화를 최대로 만든 엔진이다.
type sweepFixture struct {
	me     *MatchingEngine
	rec    *schedRecorder
	events chan ExecutionEvent
}

func newSweepFixture(t *testing.T, makers int, victimPrice int64) *sweepFixture {
	t.Helper()
	me := newTestEngine()
	me.maxMatchesPerTurn = 1 // 조각 경계를 최대로 만들어 sweep 중간을 확실히 잡는다
	events := make(chan ExecutionEvent, 65536)
	me.ExecutionCh = events
	rec := newSchedRecorder()
	rec.install(me)
	go func() {
		for range me.SnapshotCh {
		}
	}()
	me.Start()

	for i := 0; i < makers; i++ {
		o := stopTestLimitOrder(uint(i+1), model.OrderSideSell, 50000, 1)
		o.EnqueuedAt = time.Now()
		me.OrderCh <- o
	}
	setupCount := makers
	if victimPrice > 0 {
		v := stopTestLimitOrder(uint(makers+1), model.OrderSideSell, victimPrice, 1)
		v.EnqueuedAt = time.Now()
		me.OrderCh <- v
		setupCount++
	}
	waitDoneN(t, rec.done, setupCount, "maker setup")
	return &sweepFixture{me: me, rec: rec, events: events}
}

// countEvents는 Stop()+Done() 이후에만 부른다. graceful shutdown이 이미
// ExecutionCh를 닫았으므로 여기서 다시 닫으면 panic이다.
func (f *sweepFixture) countEvents() (trades, cancelled, marketDone int) {
	for e := range f.events {
		switch {
		case e.Trade != nil:
			trades++
		case e.OrderCancelled != nil:
			cancelled++
		case e.MarketOrderDone != nil:
			marketDone++
		}
	}
	return
}

// B-1: active sweep 중인 지정가 taker의 취소는 not-found다. sweep이 끝나면
// 잔량이 book에 오르고, 재시도 취소가 그것을 제거한다. 취소 이벤트는 1회.
func TestActiveSweepCancelIsNotFoundThenRemovesRemainder(t *testing.T) {
	const makers = 200
	f := newSweepFixture(t, makers, 0)

	// maker 200개보다 큰 수량 → 잔량이 book에 남는다.
	taker := stopTestLimitOrder(1000, model.OrderSideBuy, 50000, makers+5)
	taker.EnqueuedAt = time.Now()
	f.me.OrderCh <- taker
	f.rec.waitFirstTrade(t)

	// sweep 진행 중 취소 — taker는 book에 없으므로 not-found여야 한다.
	result := f.me.CancelOrder(CancelOrderCommand{
		CoinSymbol: "BTC", OrderID: 1000,
		Side: model.OrderSideBuy, Price: decimal.NewFromInt(50000),
	})
	require.False(t, result.Removed, "active sweep 중인 taker는 book에 없다")
	require.True(t, errors.Is(result.Err, ErrCancelOrderNotFound),
		"not-found여야 worker가 durable retry로 넘어간다, got %v", result.Err)
	require.Less(t, f.rec.trades.Load(), int64(makers), "sweep 도중에 처리됐어야 한다")

	// sweep 완주 후 잔량이 book에 오른다.
	// 스냅샷 캐시는 ticker가 갱신하므로 즉시 조회하면 아직 비어 있을 수 있다.
	waitDoneN(t, f.rec.done, 1, "taker 완결")
	require.Eventually(t, func() bool {
		return bookHasPrice(t, f.me, model.OrderSideBuy, 50000)
	}, 5*time.Second, 5*time.Millisecond,
		"잔량이 book에 등록돼야 다음 retry가 제거할 수 있다")

	// worker의 다음 retry에 해당하는 재시도 취소.
	retry := f.me.CancelOrder(CancelOrderCommand{
		CoinSymbol: "BTC", OrderID: 1000,
		Side: model.OrderSideBuy, Price: decimal.NewFromInt(50000),
	})
	require.True(t, retry.Removed, "재시도가 잔량을 제거해야 한다")

	f.me.Stop()
	waitEngineDone(t, f.me)
	trades, cancelled, _ := f.countEvents()
	require.Equal(t, makers, trades)
	require.Equal(t, 1, cancelled, "OrderCancelled는 정확히 1회")
}

// B-2: 전량 체결되는 taker는 sweep 후 book에 없다. 재시도도 not-found이고
// 취소 이벤트는 발행되지 않는다 — DB가 terminal이므로 worker가 NOOP 처리한다.
func TestActiveSweepFullFillLeavesNothingToCancel(t *testing.T) {
	const makers = 200
	f := newSweepFixture(t, makers, 0)

	taker := stopTestLimitOrder(1000, model.OrderSideBuy, 50000, makers) // 정확히 전량
	taker.EnqueuedAt = time.Now()
	f.me.OrderCh <- taker
	f.rec.waitFirstTrade(t)

	first := f.me.CancelOrder(CancelOrderCommand{
		CoinSymbol: "BTC", OrderID: 1000,
		Side: model.OrderSideBuy, Price: decimal.NewFromInt(50000),
	})
	require.False(t, first.Removed)
	require.True(t, errors.Is(first.Err, ErrCancelOrderNotFound))

	waitDoneN(t, f.rec.done, 1, "taker 완결")
	require.False(t, bookHasPrice(t, f.me, model.OrderSideBuy, 50000), "잔량이 없어야 한다")

	retry := f.me.CancelOrder(CancelOrderCommand{
		CoinSymbol: "BTC", OrderID: 1000,
		Side: model.OrderSideBuy, Price: decimal.NewFromInt(50000),
	})
	require.False(t, retry.Removed, "전량 체결됐으므로 재시도도 not-found다")
	require.True(t, errors.Is(retry.Err, ErrCancelOrderNotFound))

	f.me.Stop()
	waitEngineDone(t, f.me)
	trades, cancelled, _ := f.countEvents()
	require.Equal(t, makers, trades)
	require.Equal(t, 0, cancelled, "취소 이벤트가 없어야 ORDER_RELEASE도 없다")
}

// B-3: 시장가 sweep도 선점되지 않는다. 취소는 not-found이고 sweep은 완주하며
// MarketOrderDone은 정확히 1회다.
func TestActiveMarketSweepCancelIsNotPreempted(t *testing.T) {
	const makers = 200
	f := newSweepFixture(t, makers, 0)

	market := &Order{
		ID: 2000, UserID: 2000, CoinSymbol: "BTC", Side: model.OrderSideBuy,
		QuoteAmount: decimal.NewFromInt(50000 * makers),
		OrderType:   model.OrderTypeMarket, EnqueuedAt: time.Now(),
	}
	f.me.OrderCh <- market
	f.rec.waitFirstTrade(t)

	result := f.me.CancelOrder(CancelOrderCommand{
		CoinSymbol: "BTC", OrderID: 2000,
		Side: model.OrderSideBuy, Price: decimal.Zero,
	})
	require.False(t, result.Removed)
	require.True(t, errors.Is(result.Err, ErrCancelOrderNotFound))
	require.Less(t, f.rec.trades.Load(), int64(makers), "취소가 sweep 도중에 처리됐다")

	waitDoneN(t, f.rec.done, 1, "market 완결")
	f.me.Stop()
	waitEngineDone(t, f.me)

	trades, cancelled, marketDone := f.countEvents()
	require.Equal(t, makers, trades, "시장가 sweep이 완주해야 한다")
	require.Equal(t, 1, marketDone, "MarketOrderDone은 정확히 1회")
	require.Equal(t, 0, cancelled)
}

// B-4: maker는 조각 사이 취소로 체결을 피할 수 있다. 의도한 대가다.
// 조각화 이전에는 sweep이 원자적이라 불가능했다.
func TestMakerCancelledBetweenSlicesEscapesFill(t *testing.T) {
	const makers = 200
	// victim은 sweep이 닿는 가격(50000)에 두되, ID로 구분한다.
	f := newSweepFixture(t, makers, 0)
	victimID := uint(makers + 1)
	victim := stopTestLimitOrder(victimID, model.OrderSideSell, 50000, 1)
	victim.EnqueuedAt = time.Now()
	f.me.OrderCh <- victim
	waitDoneN(t, f.rec.done, 1, "victim setup")

	// maker 201개 전부를 쓸어가는 taker.
	taker := stopTestLimitOrder(1000, model.OrderSideBuy, 50000, makers+1)
	taker.EnqueuedAt = time.Now()
	f.me.OrderCh <- taker
	f.rec.waitFirstTrade(t)

	// sweep 도중 victim 취소. 아직 체결되지 않았다면 제거된다.
	result := f.me.CancelOrder(CancelOrderCommand{
		CoinSymbol: "BTC", OrderID: victimID,
		Side: model.OrderSideSell, Price: decimal.NewFromInt(50000),
	})
	require.True(t, result.Removed, "조각 사이에 maker를 취소할 수 있어야 한다")

	waitDoneN(t, f.rec.done, 1, "taker 완결")
	f.me.Stop()
	waitEngineDone(t, f.me)

	trades, cancelled, _ := f.countEvents()
	require.Equal(t, 1, cancelled, "취소 이벤트 1회")
	// victim이 체결을 피했으므로 체결 수가 maker 총수보다 적다.
	require.Equal(t, makers, trades,
		"취소된 maker는 체결되지 않아야 한다 — 체결 회피가 의도한 대가다")
}

// crash hook이 실제로 엔진을 죽이는지 먼저 고정한다. 이것이 틀리면
// 크래시 복구 테스트의 통과가 아무것도 뜻하지 않는다.
//
// "Stop() 없이 참조를 버린다"는 goroutine을 죽이지 않는다. 버려진 엔진은
// 계속 ExecutionCh에 쓰고, 그러면 "크래시 후"가 아니라 두 엔진이 동시에 쓴
// 결과를 재게 된다.
func TestCrashHookStopsEngineWithoutClosingChannels(t *testing.T) {
	const makers = 200
	me := newTestEngine()
	me.maxMatchesPerTurn = 1
	events := make(chan ExecutionEvent, 65536)
	me.ExecutionCh = events
	rec := newSchedRecorder()
	rec.install(me)
	go func() {
		for range me.SnapshotCh {
		}
	}()

	// Start 전에 설치하고, arm은 atomic으로 한다 — 실행 중 함수 필드를
	// 교체하면 data race다.
	var armed atomic.Bool
	var remaining atomic.Int64
	me.crashHook = func() bool {
		if !armed.Load() {
			return false
		}
		return remaining.Add(-1) <= 0
	}
	me.Start()

	for i := 0; i < makers; i++ {
		o := stopTestLimitOrder(uint(i+1), model.OrderSideSell, 50000, 1)
		o.EnqueuedAt = time.Now()
		me.OrderCh <- o
	}
	waitDoneN(t, rec.done, makers, "setup")

	// setup이 끝난 뒤에 arm한다. setup 조각에 걸리면 taker sweep을 못 잰다.
	remaining.Store(5)
	armed.Store(true)

	taker := stopTestLimitOrder(1000, model.OrderSideBuy, 50000, makers)
	taker.EnqueuedAt = time.Now()
	me.OrderCh <- taker

	select {
	case <-me.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("crash hook이 엔진을 멈추지 않았다")
	}

	// 크래시는 sweep 중간이어야 한다.
	got := rec.trades.Load()
	require.Greater(t, got, int64(0), "체결 전에 크래시했다")
	require.Less(t, got, int64(makers), "sweep이 끝난 뒤 크래시했다")

	// 죽은 엔진이 더는 쓰지 않는다.
	before := len(events)
	time.Sleep(200 * time.Millisecond)
	require.Equal(t, before, len(events), "죽은 엔진이 계속 쓰고 있다")

	// 크래시는 채널을 닫지 않는다 — graceful shutdown이 아니다.
	select {
	case _, ok := <-me.SnapshotCh:
		require.True(t, ok, "크래시가 SnapshotCh를 닫았다")
	default:
	}
	events <- ExecutionEvent{} // 닫혔다면 panic한다
}
