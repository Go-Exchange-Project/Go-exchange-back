package matching

import (
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// parkRecorder는 Task 3 Step1 테스트들이 공유하는 관측 헬퍼다. ParkStarted는
// 원자 카운터, ParkDuration은 원자 카운터(finished)와 mutex로 보호한 duration
// 슬라이스(호출 순서대로 append)로 기록한다.
//
// 기록 순서: ParkDuration 콜백은 mutex 아래 duration을 append한 뒤에
// finished를 증가시킨다. 그래야 "finished > finishedBefore"를 본 시점에
// durations[finishedBefore]가 반드시 존재한다(설계 §9.2 테스트 7).
type parkRecorder struct {
	started   atomic.Int64
	finished  atomic.Int64
	mu        sync.Mutex
	durations []time.Duration
}

func newParkRecorder() *parkRecorder { return &parkRecorder{} }

func (r *parkRecorder) observers() EngineObservers {
	return EngineObservers{
		ParkStarted: func() { r.started.Add(1) },
		ParkDuration: func(d time.Duration) {
			r.mu.Lock()
			r.durations = append(r.durations, d)
			r.mu.Unlock()
			r.finished.Add(1)
		},
	}
}

func (r *parkRecorder) durationAt(i int) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.durations[i]
}

// 설계 §6, §9.2 테스트 9 — validateExecutionCapacity는 합산이 아니라 뺄셈으로
// 검증한다. 합산 기반 검증은 maxMatchesPerTurn·maxConsecutiveCancels가 큰
// env 값일 때 int overflow로 통과시키고 실제 send에서 다시 멈춘다.
func TestValidateExecutionCapacityBoundaries(t *testing.T) {
	tests := []struct {
		name                  string
		executionCapacity     int
		maxMatchesPerTurn     int
		maxConsecutiveCancels int
		wantErr               bool
	}{
		{
			name:                  "통과 — 경계값 정확히 maxMatchesPerTurn = cap-1-cancels",
			executionCapacity:     16,
			maxMatchesPerTurn:     7, // 16-1-8
			maxConsecutiveCancels: 8,
			wantErr:               false,
		},
		{
			name:                  "실패 — 경계값 +1",
			executionCapacity:     16,
			maxMatchesPerTurn:     8, // 16-1-8+1
			maxConsecutiveCancels: 8,
			wantErr:               true,
		},
		{
			name:                  "실패 — executionCapacity < 2 (0)",
			executionCapacity:     0,
			maxMatchesPerTurn:     1,
			maxConsecutiveCancels: 1,
			wantErr:               true,
		},
		{
			name:                  "실패 — executionCapacity < 2 (1)",
			executionCapacity:     1,
			maxMatchesPerTurn:     1,
			maxConsecutiveCancels: 1,
			wantErr:               true,
		},
		{
			name:                  "통과 — executionCapacity == 2 최소 구성",
			executionCapacity:     2,
			maxMatchesPerTurn:     1,
			maxConsecutiveCancels: 0,
			wantErr:               false,
		},
		{
			name:                  "실패 — maxConsecutiveCancels > cap-2",
			executionCapacity:     10,
			maxMatchesPerTurn:     1,
			maxConsecutiveCancels: 9, // cap-2 = 8, 9 > 8
			wantErr:               true,
		},
		{
			name:                  "통과 — maxConsecutiveCancels == cap-2 경계",
			executionCapacity:     10,
			maxMatchesPerTurn:     1,
			maxConsecutiveCancels: 8,
			wantErr:               false,
		},
		{
			name:                  "실패 — maxMatchesPerTurn = math.MaxInt (합산 overflow가 통과시키던 값)",
			executionCapacity:     16,
			maxMatchesPerTurn:     math.MaxInt,
			maxConsecutiveCancels: 1,
			wantErr:               true,
		},
		{
			name:                  "실패 — maxConsecutiveCancels = math.MaxInt (합산 overflow가 통과시키던 값)",
			executionCapacity:     16,
			maxMatchesPerTurn:     1,
			maxConsecutiveCancels: math.MaxInt,
			wantErr:               true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateExecutionCapacity(tc.executionCapacity, tc.maxMatchesPerTurn, tc.maxConsecutiveCancels)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// NewShardedEngineWithQuantum은 기존 cfg.Validate() 외에도 실제 샤드
// ExecutionCh 용량(기본 1024) 대비 조각 시작 조건을 검증해야 한다.
func TestNewShardedEngineWithQuantumRejectsInsufficientCapacity(t *testing.T) {
	// 기본 ExecutionCh cap 1024: cap-1-cancels = 1024-1-1 = 1022.
	_, err := NewShardedEngineWithQuantum(1, QuantumConfig{MaxMatchesPerTurn: 1023, MaxConsecutiveCancels: 1})
	require.Error(t, err, "경계값 +1은 error여야 한다")

	se, err := NewShardedEngineWithQuantum(1, QuantumConfig{MaxMatchesPerTurn: 1022, MaxConsecutiveCancels: 1})
	require.NoError(t, err, "경계값 정확히는 통과해야 한다")
	require.NotNil(t, se)
}

// NewMatchingEngineWithQuantum(설계 문서에는 없는 계획 결정 1 — 서비스 패키지가
// unexported quantum 필드를 바꿀 수 없어 신설)도 같은 경계로 검증한다.
func TestNewMatchingEngineWithQuantumRejectsInsufficientCapacity(t *testing.T) {
	// 기본 ExecutionCh cap 1024: cap-1-cancels = 1024-1-1 = 1022.
	_, err := NewMatchingEngineWithQuantum(QuantumConfig{MaxMatchesPerTurn: 1023, MaxConsecutiveCancels: 1})
	require.Error(t, err, "경계값 +1은 error여야 한다")

	me, err := NewMatchingEngineWithQuantum(QuantumConfig{MaxMatchesPerTurn: 1022, MaxConsecutiveCancels: 1})
	require.NoError(t, err, "경계값 정확히는 통과해야 한다")
	require.NotNil(t, me)
}

// NewMatchingEngineWithQuantum은 QuantumConfig.Validate()도 거친다(0 이하 거절).
func TestNewMatchingEngineWithQuantumRejectsInvalidQuantumConfig(t *testing.T) {
	_, err := NewMatchingEngineWithQuantum(QuantumConfig{MaxMatchesPerTurn: 0, MaxConsecutiveCancels: 1})
	require.Error(t, err)
}

// Start()는 기본 quantum(137칸 필요)과 호환되지 않는 작은 ExecutionCh가
// 끼워져 있으면 panic해야 한다 — 테스트 픽스처가 채널을 바꿔 끼운 프로그래밍
// 오류를 잡는 방어선이다.
func TestStartPanicsWhenExecutionCapacityInsufficientForQuantum(t *testing.T) {
	me := NewMatchingEngine() // 기본 quantum 128,8 → 조각 시작 137칸
	me.ExecutionCh = make(chan ExecutionEvent, 16)
	require.Panics(t, func() { me.Start() })
}

// nil ExecutionCh는 방출이 없는 엔진이므로 용량 검증에서 제외되고 panic하지
// 않는다(설계 §3.1 free()의 nil 특례와 대응).
func TestStartDoesNotPanicWithNilExecutionCh(t *testing.T) {
	me := NewMatchingEngine()
	me.ExecutionCh = nil
	require.NotPanics(t, func() { me.Start() })
	me.Stop()
	waitEngineDone(t, me)
}

// ===== Task 3: park 상태 기계와 깨우기 (설계 §3.1~§3.4, §5, §7) =====
//
// 공통: quantum (4,2) → 조각 시작 조건 4+1+2=7. 긴 ticker(1s)는 ticker
// 발화와 실제 재개 신호(capacityCh)를 구분하기 위한 것이다.

// 설계 §9.2 테스트 1(park 부분) — free < 7이면 조각을 시작하지 않고 park하며,
// free가 7이 되면(ticker fallback으로, bare MatchingEngine에는 포워더가
// 없어 capacityCh 신호가 없다) 재개해 Trade 4건 + MarketOrderDone 1건을
// 순서대로 낸다.
func TestSliceParksWhenExecutionCapacityInsufficientThenResumes(t *testing.T) {
	me := NewMatchingEngine()
	me.snapshotInterval = time.Second
	me.maxMatchesPerTurn = 4
	me.maxConsecutiveCancels = 2
	me.ExecutionCh = make(chan ExecutionEvent, 16)
	pr := newParkRecorder()
	me.Observers = pr.observers()

	book := me.GetOrderBook("BTC")
	for i := 0; i < 4; i++ {
		book.AddOrder(testOrder(uint(i+1), "BTC", model.OrderSideSell, int64(50000+i), 1))
	}

	// free = 6(< 7)이 되도록 cap 16 중 10칸을 Start() 전에 채운다.
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
		"free=6(<7)이면 조각을 시작하지 않고 park해야 한다")

	// park 중에는 슬라이스가 시작되지 않았으므로 엔진 goroutine이 아직
	// book·market·tradeSeq·ExecutionCh를 건드리지 않았다 — 테스트 goroutine이
	// 직접 읽어도 이 시점에는 race가 아니다.
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, int64(0), pr.finished.Load(), "아직 재개하지 않았다")
	require.Equal(t, 4, book.SellOrders.Len(), "오더북이 바뀌지 않아야 한다")
	require.True(t, market.FilledAmount.IsZero(), "주문 잔량이 바뀌지 않아야 한다")
	require.Equal(t, int64(0), me.tradeSeq, "tradeSeq가 바뀌지 않아야 한다")
	require.Equal(t, prefill, len(me.ExecutionCh), "park 중 채널 길이가 바뀌지 않아야 한다")

	// 채널에서 1건 빼 free = 7로 만든다.
	<-me.ExecutionCh

	require.Eventually(t, func() bool { return pr.finished.Load() == 1 }, 3*time.Second, 5*time.Millisecond,
		"free=7이 되면 재개해야 한다")

	// 남은 dummy (prefill-1)건을 먼저 비운다 — FIFO라 실제 이벤트보다 앞에 있다.
	for i := 0; i < prefill-1; i++ {
		<-me.ExecutionCh
	}

	var events [5]ExecutionEvent
	for i := range events {
		select {
		case e := <-me.ExecutionCh:
			events[i] = e
		case <-time.After(3 * time.Second):
			t.Fatalf("이벤트 %d/5개만 도착했다", i)
		}
	}
	for i := 0; i < 4; i++ {
		require.NotNil(t, events[i].Trade, "인덱스 %d: Trade여야 한다", i)
	}
	require.NotNil(t, events[4].MarketOrderDone, "마지막은 MarketOrderDone이어야 한다")

	me.Stop()
	waitEngineDone(t, me)
}

// 설계 §9.2 테스트 6 — park 중에는 busy loop가 없다. 소비·취소·stop이 전혀
// 없으므로 park 진입 이후 추가 turn은 사실상 0이어야 한다. 상한
// 2×(ticker 0~1 + stop 0 + cancel 0 + capacity wake ≤1)+1 = 5(설계 §3.4)를
// 느슨하게 확인한다.
func TestParkedEngineHasNoBusyLoop(t *testing.T) {
	me := NewMatchingEngine()
	me.snapshotInterval = time.Second
	me.maxMatchesPerTurn = 4
	me.maxConsecutiveCancels = 2
	me.ExecutionCh = make(chan ExecutionEvent, 16)

	pr := newParkRecorder()
	var turns atomic.Int64
	obs := pr.observers()
	obs.Turn = func(time.Duration) { turns.Add(1) }
	me.Observers = obs

	book := me.GetOrderBook("BTC")
	book.AddOrder(testOrder(1, "BTC", model.OrderSideSell, 50000, 100))

	// free = 6(< 7)이 되도록 10칸만 채운다. cap16을 다 채우면(free=0)
	// emitBackpressured 워터마크(75%=12)에 걸려 admission phase가 taker를
	// OrderCh에서 꺼내지도 않아 activeSweep이 생기지 않는다 — sliceBlockedNow는
	// activeSweep != nil을 전제하므로 그러면 이 테스트의 park 자체가
	// 시작되지 않는다.
	for i := 0; i < 10; i++ {
		me.ExecutionCh <- ExecutionEvent{}
	}

	me.Start()
	taker := stopTestLimitOrder(2, model.OrderSideBuy, 50000, 100)
	me.OrderCh <- taker

	require.Eventually(t, func() bool { return pr.started.Load() == 1 }, 3*time.Second, 5*time.Millisecond)

	before := turns.Load()
	time.Sleep(300 * time.Millisecond)
	after := turns.Load()
	require.LessOrEqual(t, after-before, int64(5), "park 중 busy loop로 turn이 과도하게 늘었다")

	me.Stop()
	go func() {
		for range me.ExecutionCh {
		}
	}()
	waitEngineDone(t, me)
}

// 설계 §9.2 테스트 7 — 재개 지연. ShardedEngine의 팬인이 포화돼 샤드
// 포워더가 막히고 샤드 로컬도 park한 상태에서, 팬인 소비를 재개하면 ticker
// 주기(1s)보다 훨씬 짧게(<200ms) 재개해야 한다(capacityCh 신호).
func TestShardedEngineResumesPromptlyWhenFanInDrains(t *testing.T) {
	se, err := NewShardedEngineWithQuantum(1, QuantumConfig{MaxMatchesPerTurn: 4, MaxConsecutiveCancels: 2})
	require.NoError(t, err)

	// Start() 전에 팬인·샤드 로컬 채널과 ticker를 교체한다(§2.3).
	se.ExecutionCh = make(chan ExecutionEvent, 4)
	se.shards[0].ExecutionCh = make(chan ExecutionEvent, 8)
	se.shards[0].snapshotInterval = time.Second

	pr := newParkRecorder()
	se.SetObservers(pr.observers())

	book := se.shards[0].GetOrderBook("BTC")
	for i := 0; i < 20; i++ {
		book.AddOrder(testOrder(uint(i+1), "BTC", model.OrderSideSell, int64(50000+i), 1))
	}

	se.Start()
	taker := stopTestLimitOrder(1000, model.OrderSideBuy, 99999, 20)
	se.SubmitOrder(taker)

	require.Eventually(t, func() bool {
		faninFull := len(se.ExecutionCh) == cap(se.ExecutionCh)
		localBlocked := cap(se.shards[0].ExecutionCh)-len(se.shards[0].ExecutionCh) < 7
		parkedNow := pr.started.Load() > pr.finished.Load()
		return faninFull && localBlocked && parkedNow
	}, 5*time.Second, 2*time.Millisecond, "팬인 포화 + 로컬 park 상태가 만들어지지 않았다")

	finishedBefore := pr.finished.Load()

	drained := make(chan ExecutionEvent, 4096)
	go func() {
		for e := range se.ExecutionCh {
			drained <- e
		}
	}()

	require.Eventually(t, func() bool { return pr.finished.Load() > finishedBefore },
		3*time.Second, 2*time.Millisecond, "팬인 소비 후에도 재개하지 않았다")

	d := pr.durationAt(int(finishedBefore))
	require.Less(t, d, 200*time.Millisecond,
		"capacityCh 신호로 즉시 재개해야 한다(ticker 1s보다 훨씬 짧아야 함), got %v", d)

	se.Stop()
	select {
	case <-se.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("sharded engine did not stop in time")
	}
}

// 설계 §9.2 테스트 8 — shutdown 중 park. park한 채로 Stop()해도
// ShutdownLatched가 짧은 상한 안에 정확히 1회 발생하고, 소비 없는 동안
// busy loop 없이 Done()이 열려 있다가, 소비 재개 후 sweep이 완주하면 닫힌다.
func TestShutdownWhileParkedLatchesOnceAndDrainsAfterCapacityFrees(t *testing.T) {
	me := NewMatchingEngine()
	me.snapshotInterval = time.Second
	me.maxMatchesPerTurn = 4
	me.maxConsecutiveCancels = 2
	me.ExecutionCh = make(chan ExecutionEvent, 16)

	pr := newParkRecorder()
	var turns, shutdownLatched atomic.Int64
	obs := pr.observers()
	obs.Turn = func(time.Duration) { turns.Add(1) }
	obs.ShutdownLatched = func() { shutdownLatched.Add(1) }
	me.Observers = obs

	book := me.GetOrderBook("BTC")
	book.AddOrder(testOrder(1, "BTC", model.OrderSideSell, 50000, 100))

	// free = 6(< 7) — cap을 다 채우면 emitBackpressured 워터마크에 걸려
	// taker가 admission조차 되지 않는다(위 TestParkedEngineHasNoBusyLoop와
	// 같은 이유).
	for i := 0; i < 10; i++ {
		me.ExecutionCh <- ExecutionEvent{}
	}

	me.Start()
	taker := stopTestLimitOrder(2, model.OrderSideBuy, 50000, 100)
	me.OrderCh <- taker

	require.Eventually(t, func() bool { return pr.started.Load() == 1 }, 3*time.Second, 5*time.Millisecond)

	me.Stop()
	require.Eventually(t, func() bool { return shutdownLatched.Load() == 1 }, 500*time.Millisecond, 2*time.Millisecond,
		"park 중에도 stop을 즉시 인지해야 한다")

	before := turns.Load()
	time.Sleep(300 * time.Millisecond)
	after := turns.Load()
	require.LessOrEqual(t, after-before, int64(5), "shutdown 중 park에서 busy loop가 발생했다")

	select {
	case <-me.Done():
		t.Fatal("하류가 죽어 있는데 Done()이 닫혔다")
	default:
	}

	var drainedCount atomic.Int64
	drainDone := make(chan struct{})
	go func() {
		for range me.ExecutionCh {
			drainedCount.Add(1)
		}
		close(drainDone)
	}()
	waitEngineDone(t, me)
	// doneCh가 닫혔다는 것은 엔진이 close(ExecutionCh)했다는 뜻일 뿐, 이
	// 소비자 goroutine이 버퍼에 남은 이벤트를 마지막까지 range했다는 뜻은
	// 아니다 — 둘은 별개의 완료 신호다. drainDone까지 기다려야 drainedCount가
	// 확정된다.
	select {
	case <-drainDone:
	case <-time.After(3 * time.Second):
		t.Fatal("소비자가 draining을 끝내지 못했다")
	}
	require.Equal(t, int64(1), shutdownLatched.Load(), "ShutdownLatched는 여전히 1회여야 한다")
	// dummy 10 + Trade 1건(makers 100 == taker 100, 정확히 전량 체결 — 지정가
	// taker라 MarketOrderDone은 없다).
	require.Equal(t, int64(11), drainedCount.Load(), "이벤트 개수가 일치해야 한다")
}

// latchStop 세 경로 중 나머지 둘(park select 경로는 위 shutdown-while-parked
// 테스트가 증명한다) — 4단계 논블로킹 확인과 8단계 일반 blocking select도
// 각각 ShutdownLatched를 정확히 1회만 발생시켜야 한다.
func TestLatchStopFiresOnceViaNonBlockingCheckDuringActiveSweep(t *testing.T) {
	const makers = 500
	me := newTestEngine()
	me.maxMatchesPerTurn = 1 // 조각을 여러 개로 늘려 4단계를 자주 통과시킨다
	events := make(chan ExecutionEvent, 4096)
	me.ExecutionCh = events
	var shutdownLatched atomic.Int64
	me.Observers = EngineObservers{ShutdownLatched: func() { shutdownLatched.Add(1) }}
	go func() {
		for range me.SnapshotCh {
		}
	}()
	go func() {
		for range events {
		}
	}()
	me.Start()

	for i := 0; i < makers; i++ {
		me.OrderCh <- stopTestLimitOrder(uint(i+1), model.OrderSideSell, 50000, 1)
	}
	me.OrderCh <- stopTestLimitOrder(uint(makers+1), model.OrderSideBuy, 50000, makers)

	time.Sleep(5 * time.Millisecond) // sweep이 진행 중인 상태를 만든다
	me.Stop()
	waitEngineDone(t, me)

	require.Equal(t, int64(1), shutdownLatched.Load())
}

func TestLatchStopFiresOnceViaBlockingSelectWhenIdle(t *testing.T) {
	me := newTestEngine()
	var shutdownLatched atomic.Int64
	me.Observers = EngineObservers{ShutdownLatched: func() { shutdownLatched.Add(1) }}
	go func() {
		for range me.ExecutionCh {
		}
	}()
	go func() {
		for range me.SnapshotCh {
		}
	}()
	me.Start()

	time.Sleep(5 * time.Millisecond) // idle 상태에서 8단계 blocking select에 들어가게 한다
	me.Stop()
	waitEngineDone(t, me)

	require.Equal(t, int64(1), shutdownLatched.Load())
}

// ===== Task 4: 예약 emitter (fail-fast) (설계 §8.2, §2.3, §9.2 테스트 1·10) =====

// 4차 리뷰 세부 사항 1 — 예약 예산 소진 후 추가 send는 panic한다.
func TestSendExecutionPanicsWhenReservationBudgetExhausted(t *testing.T) {
	me := NewMatchingEngine()
	me.ExecutionCh = make(chan ExecutionEvent, 8)
	me.beginReservation(2)

	require.NotPanics(t, func() { me.sendExecution(EmitTrade, ExecutionEvent{}) })
	require.NotPanics(t, func() { me.sendExecution(EmitTrade, ExecutionEvent{}) })
	require.Panics(t, func() { me.sendExecution(EmitTrade, ExecutionEvent{}) },
		"예산 2를 넘는 3번째 send는 panic해야 한다")
}

// 4차 리뷰 세부 사항 1 — 예산은 남았지만 채널이 준비되지 않은 send도
// panic한다(단일 writer 전제나 시작 조건 계산이 틀렸다는 뜻). 이벤트를
// 버리지 않는다 — panic 전에 채워둔 이벤트만 그대로 남아야 한다.
func TestSendExecutionPanicsWhenChannelNotReadyDespiteBudget(t *testing.T) {
	me := NewMatchingEngine()
	me.ExecutionCh = make(chan ExecutionEvent, 1)
	me.ExecutionCh <- ExecutionEvent{}
	me.beginReservation(1)

	require.Panics(t, func() { me.sendExecution(EmitTrade, ExecutionEvent{}) })
	require.Equal(t, 1, len(me.ExecutionCh), "이벤트를 버리지 않는다 — 채워둔 1건만 존재해야 한다")
}

// 예약 구간 밖(reservationActive == false, Match() 경로)의 send는 기존처럼
// blocking을 유지한다.
func TestSendExecutionBlocksOutsideReservation(t *testing.T) {
	me := NewMatchingEngine()
	me.ExecutionCh = make(chan ExecutionEvent, 1)
	me.ExecutionCh <- ExecutionEvent{}

	done := make(chan struct{})
	go func() {
		me.sendExecution(EmitTrade, ExecutionEvent{})
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("예약 밖 send가 즉시 끝났다 — blocking이어야 한다")
	case <-time.After(100 * time.Millisecond):
	}

	<-me.ExecutionCh // 소비하면 풀린다
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("소비 후에도 blocking send가 완료되지 않았다")
	}
}

// runSlice는 budget 소진(done=false) 조기 반환 경로에서도 예약을 해제해야
// 한다 — 그러지 않으면 다음 작업의 send가 남은 예약 상태로 잘못 판정된다.
func TestRunSliceClearsReservationOnBudgetExhaustedReturn(t *testing.T) {
	me := NewMatchingEngine()
	me.maxMatchesPerTurn = 1
	me.ExecutionCh = make(chan ExecutionEvent, 16)
	book := me.GetOrderBook("BTC")
	book.AddOrder(testOrder(1, "BTC", model.OrderSideSell, 50000, 1))
	book.AddOrder(testOrder(2, "BTC", model.OrderSideSell, 50001, 1))
	taker := testOrder(3, "BTC", model.OrderSideBuy, 60000, 2) // 2 trades 필요, budget 1
	me.activeSweep = &activeSweep{order: taker, book: book}

	me.runSlice() // budget 소진(done=false) 경로

	require.False(t, me.reservationActive, "budget 소진 반환 경로도 예약을 해제해야 한다")
}

// runSlice는 완료(done=true, finishOrder 실행) 반환 경로에서도 예약을
// 해제해야 한다.
func TestRunSliceClearsReservationOnCompletion(t *testing.T) {
	me := NewMatchingEngine()
	me.maxMatchesPerTurn = 4
	me.ExecutionCh = make(chan ExecutionEvent, 16)
	book := me.GetOrderBook("BTC")
	book.AddOrder(testOrder(1, "BTC", model.OrderSideSell, 50000, 1))
	taker := testOrder(2, "BTC", model.OrderSideBuy, 60000, 1)
	me.activeSweep = &activeSweep{order: taker, book: book}

	me.runSlice() // 완료(done=true) 경로

	require.False(t, me.reservationActive, "완료 반환 경로도 예약을 해제해야 한다")
}

// 설계 §9.2 테스트 10 — Match() 계약. 소비자가 동시에 읽는 작은(cap 1)
// 채널에서도 panic 없이 대형 sweep을 끝내고, 방출 개수·순서가 기존과 같다
// (예약 구간 밖 blocking send가 여전히 동작함을 증명한다).
func TestMatchStillBlocksOutsideReservationAndPreservesOrderAndCount(t *testing.T) {
	const makers = 500
	me := NewMatchingEngine()
	me.ExecutionCh = make(chan ExecutionEvent, 1)

	var mu sync.Mutex
	var events []ExecutionEvent
	done := make(chan struct{})
	go func() {
		for e := range me.ExecutionCh {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		}
		close(done)
	}()

	book := me.GetOrderBook("BTC")
	for i := 0; i < makers; i++ {
		book.AddOrder(testOrder(uint(i+1), "BTC", model.OrderSideSell, int64(50000+i), 1))
	}
	taker := testOrder(uint(makers+1), "BTC", model.OrderSideBuy, 999999999, makers)

	require.NotPanics(t, func() { me.Match(taker) })

	close(me.ExecutionCh)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("소비자가 끝나지 않았다")
	}

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, events, makers, "방출 개수가 기존과 같아야 한다")
	for i, e := range events {
		require.NotNil(t, e.Trade, "인덱스 %d: Trade여야 한다(순서 보존)", i)
		require.Equal(t, int64(i+1), e.Trade.EngineSequence, "순서가 보존돼야 한다")
	}
}
