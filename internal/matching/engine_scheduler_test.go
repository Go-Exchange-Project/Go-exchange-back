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

// schedRecorder는 Start() 전에 한 번 설치되고 실행 중 교체되지 않는다.
type schedRecorder struct {
	mu     sync.Mutex
	events []string // "order" | "cancel" 순서열
	trades atomic.Int64
	done   chan int
	admit  chan struct{}
	cancel chan struct{}
	first  chan struct{}
	once   sync.Once

	// admitTradeCount는 각 admit 시점의 누적 trade 수다. 추월 여부를
	// "언제 admit됐는가"로 직접 판정하는 데 쓴다.
	admitTradeCount []int64
}

func newSchedRecorder() *schedRecorder {
	return &schedRecorder{
		done:   make(chan int, 1<<16),
		admit:  make(chan struct{}, 1<<16),
		cancel: make(chan struct{}, 1<<16),
		first:  make(chan struct{}),
	}
}

func (r *schedRecorder) install(me *MatchingEngine) {
	me.Observers = EngineObservers{
		OrderAdmitted: func(time.Duration) {
			r.mu.Lock()
			r.events = append(r.events, "order")
			r.admitTradeCount = append(r.admitTradeCount, r.trades.Load())
			r.mu.Unlock()
			select {
			case r.admit <- struct{}{}:
			default:
			}
		},
		// Slice도 progress 이벤트다(P-a). 취소 상한은 두 admit 사이가 아니라
		// 두 progress 사이에 적용된다.
		Slice: func(int, time.Duration) {
			r.mu.Lock()
			r.events = append(r.events, "slice")
			r.mu.Unlock()
		},
		OrderDone: func(n int) {
			select {
			case r.done <- n:
			default:
			}
		},
		Cancel: func(time.Duration) {
			r.mu.Lock()
			r.events = append(r.events, "cancel")
			r.mu.Unlock()
			select {
			case r.cancel <- struct{}{}:
			default:
			}
		},
		EmitBlock: func(k EmitKind, _ time.Duration) {
			if k != EmitTrade {
				return
			}
			r.trades.Add(1)
			r.once.Do(func() { close(r.first) })
		},
	}
}

func (r *schedRecorder) seq() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.events...)
}

func (r *schedRecorder) admitCounts() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64{}, r.admitTradeCount...)
}

func (r *schedRecorder) waitFirstTrade(t *testing.T) {
	t.Helper()
	select {
	case <-r.first:
	case <-time.After(20 * time.Second):
		t.Fatal("sweep이 시작되지 않았다")
	}
}

func waitN(t *testing.T, ch <-chan struct{}, n int, what string) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("%s: %d/%d만 도착했다", what, i, n)
		}
	}
}

func waitDoneN(t *testing.T, ch <-chan int, n int, what string) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("%s: %d/%d만 완결됐다", what, i, n)
		}
	}
}

// bookHasPrice는 캐시 스냅샷으로 조회한다. 엔진 goroutine이 소유한 book을
// 테스트 goroutine이 직접 읽으면 race다.
func bookHasPrice(t *testing.T, me *MatchingEngine, side model.OrderSide, price int64) bool {
	t.Helper()
	snap, err := me.RequestOrderBookSnapshot("BTC", DefaultSnapshotDepth)
	require.NoError(t, err)
	levels := snap.Asks
	if side == model.OrderSideBuy {
		levels = snap.Bids
	}
	for _, level := range levels {
		if level.Price.Equal(decimal.NewFromInt(price)) {
			return true
		}
	}
	return false
}

// 계약 1: 취소 flood 중 주문이 유한 횟수 안에 admit된다.
//
// 상한은 "두 admit 사이"가 아니라 **"두 progress 사이"** 에 적용된다.
// cancelsSinceProgress는 주문 admit(P-b)뿐 아니라 조각 실행(P-a)에서도
// 리셋되기 때문이다 — 조각이 도는 동안에는 취소가 아무것도 굶기지 않는다.
// admit 기준으로만 세면 설계보다 좁은 계약을 검증하게 되어 거짓 실패가 난다.
//
// **첫 주문 이전 구간부터 검사한다** — 그 구간을 빼면 무제한 드레인도 통과한다.
func TestCancelsBetweenProgressAreBounded(t *testing.T) {
	const limit = 4
	me := newTestEngine()
	me.maxConsecutiveCancels = limit
	rec := newSchedRecorder()
	rec.install(me)
	drainAll(me)

	// 취소를 먼저 전부 큐에 넣고 그 다음 주문을 넣는다. 무제한 드레인이면
	// 200건이 한 번에 처리된 뒤에야 첫 주문이 admit된다.
	for i := 0; i < 200; i++ {
		me.CancelCh <- CancelOrderCommand{
			CoinSymbol: "BTC", OrderID: uint(9000 + i),
			Side: model.OrderSideSell, Price: decimal.NewFromInt(70000),
			EnqueuedAt: time.Now(),
		}
	}
	for i := 0; i < 20; i++ {
		o := stopTestLimitOrder(uint(i+1), model.OrderSideSell, int64(50000+i), 1)
		o.EnqueuedAt = time.Now()
		me.OrderCh <- o
	}
	me.Start()
	waitN(t, rec.admit, 20, "주문 admit")

	seq := rec.seq()

	// 첫 주문이 취소 limit개 안에 admit돼야 한다. 무제한 드레인이면 200건을
	// 다 처리한 뒤에야 첫 주문이 온다.
	firstOrder := -1
	lastOrder := -1
	for i, e := range seq {
		if e == "order" {
			if firstOrder < 0 {
				firstOrder = i
			}
			lastOrder = i
		}
	}
	require.GreaterOrEqual(t, firstOrder, 0, "주문이 하나도 admit되지 않았다")
	require.LessOrEqual(t, firstOrder, limit,
		"첫 주문이 취소 %d건 뒤에야 admit됐다 — 상한이 작동하지 않았다", firstOrder)

	// 마지막 주문 이후 구간은 검사하지 않는다. 대기 주문이 없으면 취소가
	// 아무것도 굶기지 않으므로 상한 없이 처리되는 것이 옳다(P-c).
	run := 0
	for i, e := range seq[:lastOrder+1] {
		if e == "cancel" {
			run++
			require.LessOrEqual(t, run, limit,
				"인덱스 %d: progress 사이 연속 취소 %d개가 상한 %d을 넘었다\nseq=%v",
				i, run, limit, seq[:lastOrder+1])
			continue
		}
		run = 0 // "order"(P-b) 또는 "slice"(P-a) — 둘 다 progress다
	}

	me.Stop()
	waitEngineDone(t, me)
}

// 계약 2: 대형 sweep 조각 사이에 취소가 처리된다.
func TestCancelDuringLargeSweepIsProcessed(t *testing.T) {
	const makers = 5000
	me := newTestEngine()
	me.maxMatchesPerTurn = 8
	rec := newSchedRecorder()
	rec.install(me)
	drainAll(me)
	me.Start()

	for i := 0; i < makers; i++ {
		o := stopTestLimitOrder(uint(i+1), model.OrderSideSell, 50000, 1)
		o.EnqueuedAt = time.Now()
		me.OrderCh <- o
	}
	victim := stopTestLimitOrder(90001, model.OrderSideSell, 90000, 1)
	victim.EnqueuedAt = time.Now()
	me.OrderCh <- victim
	waitDoneN(t, rec.done, makers+1, "setup")

	taker := stopTestLimitOrder(90002, model.OrderSideBuy, 50000, makers)
	taker.EnqueuedAt = time.Now()
	me.OrderCh <- taker
	rec.waitFirstTrade(t)

	result := me.CancelOrder(CancelOrderCommand{
		CoinSymbol: "BTC", OrderID: 90001,
		Side: model.OrderSideSell, Price: decimal.NewFromInt(90000),
	})
	require.True(t, result.Removed, "sweep 진행 중에도 취소가 처리돼야 한다")
	require.Less(t, rec.trades.Load(), int64(makers),
		"취소가 sweep 완료 후에 처리됐다 — 조각 사이에 끼어들지 못했다")

	me.Stop()
	waitEngineDone(t, me)
}

// 계약 3: active sweep 중 뒤 주문이 추월하지 않는다.
// later 주문이 **admit된 순간의 trade count**를 보고 판정한다.
// 최종 총수만 보면 추월해도 통과한다.
func TestNewOrderDoesNotOvertakeActiveSweep(t *testing.T) {
	const makers = 200
	me := newTestEngine()
	me.maxMatchesPerTurn = 1
	rec := newSchedRecorder()
	rec.install(me)
	drainAll(me)
	me.Start()

	for i := 0; i < makers; i++ {
		o := stopTestLimitOrder(uint(i+1), model.OrderSideSell, 50000, 1)
		o.EnqueuedAt = time.Now()
		me.OrderCh <- o
	}
	waitDoneN(t, rec.done, makers, "setup")
	admitsBefore := len(rec.admitCounts())

	taker := stopTestLimitOrder(1000, model.OrderSideBuy, 50000, makers)
	taker.EnqueuedAt = time.Now()
	me.OrderCh <- taker
	rec.waitFirstTrade(t)

	later := stopTestLimitOrder(1001, model.OrderSideSell, 80000, 1)
	later.EnqueuedAt = time.Now()
	me.OrderCh <- later

	waitDoneN(t, rec.done, 2, "taker+later 완결")
	me.Stop()
	waitEngineDone(t, me)

	counts := rec.admitCounts()
	require.Len(t, counts, admitsBefore+2, "taker와 later가 admit돼야 한다")
	laterAdmitTrades := counts[admitsBefore+1]
	require.GreaterOrEqual(t, laterAdmitTrades, int64(makers),
		"later 주문이 trade %d개 시점에 admit됐다 — sweep %d건 완료 전에 추월했다",
		laterAdmitTrades, makers)
}

// 계약 4: backpressure는 신규 OrderCh 유입만 막고, 이미 latch된 주문은 막지 않는다.
//
// latch된 주문이 book에 오르는 것을 먼저 확인해 결정론을 만든 뒤, 취소로
// watermark를 넘겨 신규 유입이 막히는지 본다. select 운에 맡기지 않는다.
func TestBackpressureBlocksIntakeNotLatchedOrder(t *testing.T) {
	me := newTestEngine()
	me.ExecutionCh = make(chan ExecutionEvent, 16) // watermark = 12
	me.maxConsecutiveCancels = 8
	rec := newSchedRecorder()
	rec.install(me)
	go func() {
		for range me.SnapshotCh {
		}
	}()
	me.Start()

	// 1) 취소 대상 resting 주문 4건.
	for i := 0; i < 4; i++ {
		o := stopTestLimitOrder(uint(i+1), model.OrderSideSell, int64(50000+i), 1)
		o.EnqueuedAt = time.Now()
		me.OrderCh <- o
	}
	waitDoneN(t, rec.done, 4, "resting setup")

	// 2) watermark 바로 아래(11건)까지 채운다. 아직 게이트는 열려 있다.
	for i := 0; i < 11; i++ {
		me.ExecutionCh <- ExecutionEvent{}
	}
	require.False(t, me.emitBackpressured(), "아직 게이트가 열려 있어야 한다")

	// 3) 체결 0건 지정가 probe. emit이 없어야 올바른 구현도 send에 막히지 않는다.
	probe := stopTestLimitOrder(100, model.OrderSideSell, 90000, 1)
	probe.EnqueuedAt = time.Now()
	me.OrderCh <- probe
	require.Eventually(t, func() bool {
		return bookHasPrice(t, me, model.OrderSideSell, 90000)
	}, 10*time.Second, 5*time.Millisecond, "latch된 주문이 처리되지 않았다")

	// 4) 취소 4건으로 watermark를 넘긴다(11+4=15 >= 12).
	for i := 0; i < 4; i++ {
		me.CancelCh <- CancelOrderCommand{
			CoinSymbol: "BTC", OrderID: uint(i + 1),
			Side: model.OrderSideSell, Price: decimal.NewFromInt(int64(50000 + i)),
			EnqueuedAt: time.Now(),
		}
	}
	waitN(t, rec.cancel, 4, "취소 처리")
	require.True(t, me.emitBackpressured(), "취소 emit으로 watermark를 넘겼어야 한다")

	// 5) backpressure 상태에서 신규 주문은 admit되지 않는다.
	admitsBefore := len(rec.admitCounts())
	blocked := stopTestLimitOrder(101, model.OrderSideSell, 95000, 1)
	blocked.EnqueuedAt = time.Now()
	me.OrderCh <- blocked
	time.Sleep(200 * time.Millisecond)
	require.Equal(t, admitsBefore, len(rec.admitCounts()),
		"backpressure 중에 신규 주문이 admit됐다")

	// 6) 소비자를 붙여 해소하면 그 주문이 처리된다 — 유실이 아니라 지연이다.
	go func() {
		for range me.ExecutionCh {
		}
	}()
	require.Eventually(t, func() bool {
		return bookHasPrice(t, me, model.OrderSideSell, 95000)
	}, 10*time.Second, 5*time.Millisecond, "backpressure 해소 후에도 주문이 유실됐다")

	me.Stop()
	waitEngineDone(t, me)
}

// 계약 5: shutdown이 이미 접수된 주문과 취소를 모두 처리하고 출력 채널을 닫는다.
//
// 소비자가 watermark를 해소한다 — 소비자가 없으면 올바른 구현도 blocking
// send에서 멈춰 판별력이 없다. 게이트 오적용은 deadlock이 아니라 **주문 유실**로
// 나타난다: drain의 OrderCh 수신이 "비었음"으로 관측되는 것이 아니라
// 건너뛰어져, 주문을 남긴 채 drain 완료로 판정된다.
func TestStopDrainsQueuedOrderAndCancel(t *testing.T) {
	me := newTestEngine()
	me.ExecutionCh = make(chan ExecutionEvent, 16)
	rec := newSchedRecorder()
	rec.install(me)
	release := make(chan struct{})
	execDone := make(chan struct{})
	go func() {
		<-release
		for range me.ExecutionCh {
		}
		close(execDone)
	}()
	// 소비자가 range하는 채널을 테스트가 직접 읽으면 경쟁한다.
	// close 여부는 range 종료 신호로 판정한다.
	snapDone := make(chan struct{})
	go func() {
		for range me.SnapshotCh {
		}
		close(snapDone)
	}()

	me.Start()
	resting := stopTestLimitOrder(1, model.OrderSideSell, 50000, 1)
	resting.EnqueuedAt = time.Now()
	me.OrderCh <- resting
	waitDoneN(t, rec.done, 1, "resting setup")

	for i := 0; i < 13; i++ {
		me.ExecutionCh <- ExecutionEvent{}
	}
	require.True(t, me.emitBackpressured())

	// Stop() 전에 주문과 취소를 둘 다 큐에 넣는다.
	queued := stopTestLimitOrder(2, model.OrderSideSell, 90000, 1)
	queued.EnqueuedAt = time.Now()
	me.OrderCh <- queued
	me.CancelCh <- CancelOrderCommand{
		CoinSymbol: "BTC", OrderID: 1,
		Side: model.OrderSideSell, Price: decimal.NewFromInt(50000),
		EnqueuedAt: time.Now(),
	}

	me.Stop()
	close(release)
	waitEngineDone(t, me)

	select {
	case <-execDone:
	case <-time.After(20 * time.Second):
		t.Fatal("ExecutionCh가 닫히지 않았다")
	}
	select {
	case <-snapDone:
	case <-time.After(20 * time.Second):
		t.Fatal("SnapshotCh가 닫히지 않았다")
	}

	require.True(t, bookHasPrice(t, me, model.OrderSideSell, 90000),
		"drain 중 OrderCh에 게이트가 걸려 주문이 유실됐다")
	require.False(t, bookHasPrice(t, me, model.OrderSideSell, 50000),
		"drain 중 취소가 처리되지 않았다")
}

// shutdown은 진행 중 sweep을 선점하지 않는다. 시장가면 MarketOrderDone까지 낸다.
func TestStopFinishesActiveMarketSweep(t *testing.T) {
	const makers = 100
	me := newTestEngine()
	me.maxMatchesPerTurn = 1
	events := make(chan ExecutionEvent, 4096)
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
	waitDoneN(t, rec.done, makers, "setup")

	market := &Order{
		ID: 2000, UserID: 2000, CoinSymbol: "BTC", Side: model.OrderSideBuy,
		QuoteAmount: decimal.NewFromInt(50000 * makers),
		OrderType:   model.OrderTypeMarket, EnqueuedAt: time.Now(),
	}
	me.OrderCh <- market
	rec.waitFirstTrade(t)

	me.Stop()
	waitEngineDone(t, me)

	doneCount, tradeCount := 0, 0
	for e := range events {
		if e.MarketOrderDone != nil {
			doneCount++
		}
		if e.Trade != nil {
			tradeCount++
		}
	}
	require.Equal(t, 1, doneCount, "MarketOrderDone 정확히 1회")
	require.Equal(t, makers, tradeCount, "shutdown이 sweep을 선점해 체결이 잘렸다")
}
