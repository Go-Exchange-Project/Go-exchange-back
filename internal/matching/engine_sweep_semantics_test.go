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
//
// **"sweep 진행 중"을 실행 속도로 만들지 않는다.** 첫 trade를 본 뒤 취소를
// 보내는 방식은 테스트 goroutine이 엔진보다 느리면 sweep이 이미 끝나 있다.
// 대신 ExecutionCh를 작게 잡고 소비자를 붙이지 않아 **엔진이 emit에서 막히게**
// 한 뒤 취소를 큐에 넣고 소비자를 푼다. 엔진은 막힌 send가 풀리기 전에는
// cancel phase에 도달할 수 없으므로, 취소는 반드시 sweep 도중에 처리된다.

// sweepEmitCap은 sweep이 도중에 멈추도록 만드는 ExecutionCh 용량이다.
// maker 수보다 훨씬 작아야 "막힌 시점 = sweep 도중"이 보장된다.
const sweepEmitCap = 16

type sweepFixture struct {
	me      *MatchingEngine
	rec     *schedRecorder
	events  chan ExecutionEvent
	release chan struct{}
	drained chan struct{}

	trades     atomic.Int64
	cancelled  atomic.Int64
	marketDone atomic.Int64
}

// newSweepFixture는 maker N개를 채우고 조각화를 최대로 만든 엔진을 만든다.
// ExecutionCh 소비자는 releaseEmits()를 부를 때까지 시작하지 않는다.
func newSweepFixture(t *testing.T, makers int) *sweepFixture {
	t.Helper()
	me := newTestEngine()
	me.maxMatchesPerTurn = 1 // 조각 경계를 최대로 만든다
	events := make(chan ExecutionEvent, sweepEmitCap)
	me.ExecutionCh = events
	rec := newSchedRecorder()
	rec.install(me)

	f := &sweepFixture{
		me:      me,
		rec:     rec,
		events:  events,
		release: make(chan struct{}),
		drained: make(chan struct{}),
	}
	go func() {
		<-f.release
		for e := range f.events {
			switch {
			case e.Trade != nil:
				f.trades.Add(1)
			case e.OrderCancelled != nil:
				f.cancelled.Add(1)
			case e.MarketOrderDone != nil:
				f.marketDone.Add(1)
			}
		}
		close(f.drained)
	}()
	go func() {
		for range me.SnapshotCh {
		}
	}()
	me.Start()

	// setup 주문은 서로 체결되지 않으므로 emit이 없다. 소비자 없이도 막히지 않는다.
	for i := 0; i < makers; i++ {
		o := stopTestLimitOrder(uint(i+1), model.OrderSideSell, 50000, 1)
		o.EnqueuedAt = time.Now()
		me.OrderCh <- o
	}
	waitDoneN(t, rec.done, makers, "maker setup")
	require.Empty(t, events, "setup은 emit을 만들지 않아야 한다")
	return f
}

// waitEmitSaturated는 엔진이 emit에서 막힐 때까지 기다린다. ExecutionCh가
// 가득 찼다는 것은 이미 sweepEmitCap건을 체결했고 다음 send가 막힌다는 뜻이다.
// maker 수가 그보다 크므로 이 시점은 반드시 sweep 도중이다.
func (f *sweepFixture) waitEmitSaturated(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool {
		return len(f.events) == cap(f.events)
	}, 20*time.Second, time.Millisecond, "엔진이 sweep 도중에 막히지 않았다")
}

// submitCancel은 취소를 큐에 넣기만 한다. 엔진이 막혀 있는 동안 호출하면
// 소비자를 풀기 전에 큐에 들어가 있음이 보장된다.
func (f *sweepFixture) submitCancel(orderID uint, side model.OrderSide, price int64) chan CancelOrderResult {
	resp := make(chan CancelOrderResult, 1)
	f.me.CancelCh <- CancelOrderCommand{
		CoinSymbol: "BTC", OrderID: orderID, Side: side,
		Price:      decimal.NewFromInt(price),
		EnqueuedAt: time.Now(),
		ResponseCh: resp,
	}
	return resp
}

func (f *sweepFixture) releaseEmits() { close(f.release) }

func (f *sweepFixture) awaitCancel(t *testing.T, resp chan CancelOrderResult) CancelOrderResult {
	t.Helper()
	select {
	case r := <-resp:
		return r
	case <-time.After(20 * time.Second):
		t.Fatal("취소 응답이 오지 않았다")
		return CancelOrderResult{}
	}
}

// counts는 Stop() 이후에 부른다. 소비자가 채널 close까지 읽고 끝난 뒤에야
// 값이 확정된다.
func (f *sweepFixture) counts(t *testing.T) (trades, cancelled, marketDone int) {
	t.Helper()
	select {
	case <-f.drained:
	case <-time.After(20 * time.Second):
		t.Fatal("ExecutionCh 소비자가 끝나지 않았다")
	}
	return int(f.trades.Load()), int(f.cancelled.Load()), int(f.marketDone.Load())
}

// B-1: active sweep 중인 지정가 taker의 취소는 not-found다. sweep이 끝나면
// 잔량이 book에 오르고, 재시도 취소가 그것을 제거한다. 취소 이벤트는 1회.
func TestActiveSweepCancelIsNotFoundThenRemovesRemainder(t *testing.T) {
	const makers = 200
	f := newSweepFixture(t, makers)

	// maker 200개보다 큰 수량 → 잔량이 book에 남는다.
	taker := stopTestLimitOrder(1000, model.OrderSideBuy, 50000, makers+5)
	taker.EnqueuedAt = time.Now()
	f.me.OrderCh <- taker
	f.waitEmitSaturated(t)

	// 엔진이 막혀 있는 지금 취소를 큐에 넣는다. 엔진은 막힌 send가 풀리기
	// 전에는 cancel phase에 도달할 수 없으므로 sweep 도중 처리가 보장된다.
	resp := f.submitCancel(1000, model.OrderSideBuy, 50000)
	f.releaseEmits()

	result := f.awaitCancel(t, resp)
	require.False(t, result.Removed, "active sweep 중인 taker는 book에 없다")
	require.True(t, errors.Is(result.Err, ErrCancelOrderNotFound),
		"not-found여야 worker가 durable retry로 넘어간다, got %v", result.Err)

	// sweep 완주 후 잔량이 book에 오른다.
	// 스냅샷 캐시는 ticker가 갱신하므로 즉시 조회하면 아직 비어 있을 수 있다.
	waitDoneN(t, f.rec.done, 1, "taker 완결")
	require.Eventually(t, func() bool {
		return bookHasPrice(t, f.me, model.OrderSideBuy, 50000)
	}, 20*time.Second, 5*time.Millisecond,
		"잔량이 book에 등록돼야 다음 retry가 제거할 수 있다")

	// worker의 다음 retry에 해당하는 재시도 취소.
	retry := f.awaitCancel(t, f.submitCancel(1000, model.OrderSideBuy, 50000))
	require.True(t, retry.Removed, "재시도가 잔량을 제거해야 한다")

	f.me.Stop()
	waitEngineDone(t, f.me)
	trades, cancelled, _ := f.counts(t)
	require.Equal(t, makers, trades)
	require.Equal(t, 1, cancelled, "OrderCancelled는 정확히 1회")
}

// B-2: 전량 체결되는 taker는 sweep 후 book에 없다. 재시도도 not-found이고
// 취소 이벤트는 발행되지 않는다 — DB가 terminal이므로 worker가 NOOP 처리한다.
func TestActiveSweepFullFillLeavesNothingToCancel(t *testing.T) {
	const makers = 200
	f := newSweepFixture(t, makers)

	taker := stopTestLimitOrder(1000, model.OrderSideBuy, 50000, makers) // 정확히 전량
	taker.EnqueuedAt = time.Now()
	f.me.OrderCh <- taker
	f.waitEmitSaturated(t)

	resp := f.submitCancel(1000, model.OrderSideBuy, 50000)
	f.releaseEmits()

	first := f.awaitCancel(t, resp)
	require.False(t, first.Removed)
	require.True(t, errors.Is(first.Err, ErrCancelOrderNotFound))

	waitDoneN(t, f.rec.done, 1, "taker 완결")
	require.False(t, bookHasPrice(t, f.me, model.OrderSideBuy, 50000), "잔량이 없어야 한다")

	retry := f.awaitCancel(t, f.submitCancel(1000, model.OrderSideBuy, 50000))
	require.False(t, retry.Removed, "전량 체결됐으므로 재시도도 not-found다")
	require.True(t, errors.Is(retry.Err, ErrCancelOrderNotFound))

	f.me.Stop()
	waitEngineDone(t, f.me)
	trades, cancelled, _ := f.counts(t)
	require.Equal(t, makers, trades)
	require.Equal(t, 0, cancelled, "취소 이벤트가 없어야 ORDER_RELEASE도 없다")
}

// B-3: 시장가 sweep도 선점되지 않는다. 취소는 not-found이고 sweep은 완주하며
// MarketOrderDone은 정확히 1회다.
func TestActiveMarketSweepCancelIsNotPreempted(t *testing.T) {
	const makers = 200
	f := newSweepFixture(t, makers)

	market := &Order{
		ID: 2000, UserID: 2000, CoinSymbol: "BTC", Side: model.OrderSideBuy,
		QuoteAmount: decimal.NewFromInt(50000 * makers),
		OrderType:   model.OrderTypeMarket, EnqueuedAt: time.Now(),
	}
	f.me.OrderCh <- market
	f.waitEmitSaturated(t)

	resp := f.submitCancel(2000, model.OrderSideBuy, 0)
	f.releaseEmits()

	result := f.awaitCancel(t, resp)
	require.False(t, result.Removed)
	require.True(t, errors.Is(result.Err, ErrCancelOrderNotFound))

	waitDoneN(t, f.rec.done, 1, "market 완결")
	f.me.Stop()
	waitEngineDone(t, f.me)

	trades, cancelled, marketDone := f.counts(t)
	require.Equal(t, makers, trades, "시장가 sweep이 완주해야 한다")
	require.Equal(t, 1, marketDone, "MarketOrderDone은 정확히 1회")
	require.Equal(t, 0, cancelled)
}

// B-4: maker는 조각 사이 취소로 체결을 피할 수 있다. 의도한 대가다.
// 조각화 이전에는 sweep이 원자적이라 불가능했다.
func TestMakerCancelledBetweenSlicesEscapesFill(t *testing.T) {
	const makers = 200
	f := newSweepFixture(t, makers)

	// victim은 같은 가격대의 **맨 뒤**에 붙는다(deque는 FIFO). sweep이
	// sweepEmitCap건만 진행한 시점에는 아직 체결되지 않았음이 보장된다.
	victimID := uint(makers + 1)
	victim := stopTestLimitOrder(victimID, model.OrderSideSell, 50000, 1)
	victim.EnqueuedAt = time.Now()
	f.me.OrderCh <- victim
	waitDoneN(t, f.rec.done, 1, "victim setup")

	// maker 201개 전부를 쓸어가는 taker.
	taker := stopTestLimitOrder(1000, model.OrderSideBuy, 50000, makers+1)
	taker.EnqueuedAt = time.Now()
	f.me.OrderCh <- taker
	f.waitEmitSaturated(t)

	resp := f.submitCancel(victimID, model.OrderSideSell, 50000)
	f.releaseEmits()

	result := f.awaitCancel(t, resp)
	require.True(t, result.Removed, "조각 사이에 maker를 취소할 수 있어야 한다")

	waitDoneN(t, f.rec.done, 1, "taker 완결")
	f.me.Stop()
	waitEngineDone(t, f.me)

	trades, cancelled, _ := f.counts(t)
	require.Equal(t, 1, cancelled, "취소 이벤트 1회")
	// victim이 체결을 피했으므로 체결 수가 maker 총수(201)보다 하나 적다.
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
