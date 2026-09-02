//go:build quantumharness

package matching

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// 선택값 m128-c8의 안전 상한 확인(C1·C2·C4·C5).
//
// 이것은 **후보 간 미세 비교가 아니다.** 로컬 wall-clock으로 후보를 가르는
// 시도는 판정 불가로 종료됐다(설계 §8.4). 여기서 확인하는 것은 선택값이
// 300ms 안전 상한 안에 있는지뿐이다.
//
// 측정값이 시계 해상도(이 머신 ~645µs) 이하로 나오면 "0초"라고 단정하지 않고
// "측정 해상도 이하"로 기록한다.
//
// 빌드 태그가 있는 이유: 시간 단언이 들어 있어 부하가 걸린 CI에서 흔들릴 수
// 있다. 결정적 계약은 태그 없는 quantum_contract_test.go에 있다.

const gateSafetyLimit = 300 * time.Millisecond

func selectedConfig() QuantumConfig {
	return QuantumConfig{
		MaxMatchesPerTurn:     defaultMaxMatchesPerTurn,
		MaxConsecutiveCancels: defaultMaxConsecutiveCancels,
	}
}

// gateCollector는 p99 계산에 필요한 표본만 모은다.
type gateCollector struct {
	mu           sync.Mutex
	orderWaits   []time.Duration
	cancelWaits  []time.Duration
	snapshotGaps []time.Duration
	lastSnapshot time.Time
	measuring    bool

	firstTrade chan struct{}
	firstOnce  sync.Once
	done       chan int
	admitted   chan struct{}
	cancels    chan struct{}
}

func newGateCollector() *gateCollector {
	return &gateCollector{
		firstTrade: make(chan struct{}),
		done:       make(chan int, 1<<16),
		admitted:   make(chan struct{}, 1<<16),
		cancels:    make(chan struct{}, 1<<16),
	}
}

func (c *gateCollector) observers() EngineObservers {
	return EngineObservers{
		OrderAdmitted: func(d time.Duration) {
			c.mu.Lock()
			if c.measuring {
				c.orderWaits = append(c.orderWaits, d)
			}
			c.mu.Unlock()
			select {
			case c.admitted <- struct{}{}:
			default:
			}
		},
		OrderDone: func(n int) {
			select {
			case c.done <- n:
			default:
			}
		},
		Cancel: func(d time.Duration) {
			c.mu.Lock()
			if c.measuring {
				c.cancelWaits = append(c.cancelWaits, d)
			}
			c.mu.Unlock()
			select {
			case c.cancels <- struct{}{}:
			default:
			}
		},
		EmitBlock: func(k EmitKind, _ time.Duration) {
			if k == EmitTrade {
				c.firstOnce.Do(func() { close(c.firstTrade) })
			}
		},
	}
}

func (c *gateCollector) start() {
	c.mu.Lock()
	c.measuring = true
	c.orderWaits = nil
	c.cancelWaits = nil
	c.snapshotGaps = nil
	c.lastSnapshot = time.Now()
	c.mu.Unlock()
}

func (c *gateCollector) stop() {
	c.mu.Lock()
	if c.measuring && !c.lastSnapshot.IsZero() {
		c.snapshotGaps = append(c.snapshotGaps, time.Since(c.lastSnapshot))
	}
	c.measuring = false
	c.mu.Unlock()
}

func (c *gateCollector) p99(of []time.Duration) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(of) == 0 {
		return 0
	}
	s := append([]time.Duration{}, of...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	rank := int(math.Ceil(0.99 * float64(len(s))))
	if rank < 1 {
		rank = 1
	}
	return s[rank-1]
}

// reportGate는 측정값을 상한과 비교해 기록한다. 시계 해상도 이하는
// "0초"가 아니라 "측정 해상도 이하"로 남긴다.
func reportGate(t *testing.T, name string, got, limit time.Duration, samples int) {
	t.Helper()
	desc := got.String()
	if got == 0 {
		desc = "측정 해상도 이하 (< ~645µs, 0초로 단정하지 않음)"
	}
	line := fmt.Sprintf("[%s] p99=%s samples=%d limit=%v", name, desc, samples, limit)
	t.Log(line)
	fmt.Println(line)
	require.LessOrEqual(t, got, limit, "%s가 안전 상한을 넘었다", name)
}

func newGateEngine(t *testing.T, c *gateCollector) (*MatchingEngine, func()) {
	t.Helper()
	cfg := selectedConfig()
	me := NewMatchingEngine()
	me.snapshotInterval = 100 * time.Millisecond
	me.maxMatchesPerTurn = cfg.MaxMatchesPerTurn
	me.maxConsecutiveCancels = cfg.MaxConsecutiveCancels
	me.Observers = c.observers()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range me.ExecutionCh {
		}
	}()
	go func() {
		defer wg.Done()
		for range me.SnapshotCh {
			c.mu.Lock()
			if c.measuring {
				c.snapshotGaps = append(c.snapshotGaps, time.Since(c.lastSnapshot))
				c.lastSnapshot = time.Now()
			}
			c.mu.Unlock()
		}
	}()
	me.Start()
	return me, func() {
		me.Stop()
		select {
		case <-me.Done():
		case <-time.After(60 * time.Second):
			t.Fatal("engine did not stop")
		}
		wg.Wait()
	}
}

// C1 + C4: sweep이 **실제로 진행 중일 때** 취소를 넣고, 그 취소가 처리될 때까지의
// 대기와 sweep 도중 스냅샷 간격을 잰다.
//
// 순서가 핵심이다. taker 완료를 먼저 기다린 뒤 취소하면 그것은 sweep 중 취소가
// 아니다 — 이전 묶음 측정 코드가 그 실수를 했고, 그 자료로는 C1을 주장할 수 없었다.
func TestSelectedConfigC1CancelDuringSweepAndC4Snapshot(t *testing.T) {
	const makers = 5000
	c := newGateCollector()
	me, shutdown := newGateEngine(t, c)
	defer shutdown()

	for i := 0; i < makers; i++ {
		me.OrderCh <- contractOrder(uint(i+1), contractMakerUser, model.OrderSideSell, 50000, 1)
	}
	victimID := uint(makers + 1)
	me.OrderCh <- contractOrder(victimID, contractVictimUser, model.OrderSideSell, 90000, 1)
	for i := 0; i < makers+1; i++ {
		<-c.admitted
		<-c.done
	}
	c.start()

	taker := contractOrder(uint(makers+2), contractTakerUser, model.OrderSideBuy, 50000, makers)
	me.OrderCh <- taker

	// 첫 trade를 관측해야 sweep이 실제로 진행 중이다.
	select {
	case <-c.firstTrade:
	case <-time.After(30 * time.Second):
		t.Fatal("sweep이 시작되지 않았다 (censored)")
	}

	cancelDone := make(chan CancelOrderResult, 1)
	go func() {
		cancelDone <- me.CancelOrder(CancelOrderCommand{
			CoinSymbol: "BTC", OrderID: victimID,
			Side: model.OrderSideSell, Price: decimal.NewFromInt(90000),
		})
	}()

	var result CancelOrderResult
	select {
	case result = <-cancelDone:
	case <-time.After(30 * time.Second):
		t.Fatal("sweep 중 취소가 처리되지 않았다 (censored)")
	}
	require.True(t, result.Removed, "sweep 조각 사이에 취소가 처리돼야 한다")

	takerTrades := <-c.done
	c.stop()

	require.Equal(t, makers, takerTrades, "sweep이 정확히 5,000건을 체결해야 한다")
	require.True(t, taker.Amount.IsZero(), "잔량 0, got %s", taker.Amount)

	c.mu.Lock()
	cancelSamples := len(c.cancelWaits)
	var maxGap time.Duration
	for _, g := range c.snapshotGaps {
		if g > maxGap {
			maxGap = g
		}
	}
	gapSamples := len(c.snapshotGaps)
	c.mu.Unlock()

	require.Equal(t, 1, cancelSamples, "C5: 취소 표본 1건 (censored 0)")
	reportGate(t, "C1 sweep 중 취소 대기", c.p99(c.cancelWaits), gateSafetyLimit, cancelSamples)
	reportGate(t, "C4 스냅샷 최대 간격", maxGap, gateSafetyLimit, gapSamples)
}

// C2: 취소 홍수 중 신규 주문의 큐 대기가 안전 상한 안에 있다.
func TestSelectedConfigC2OrderWaitUnderCancelFlood(t *testing.T) {
	const resting = 20000
	const probes = 500
	const cancels = 15000

	c := newGateCollector()
	me, shutdown := newGateEngine(t, c)
	defer shutdown()

	for i := 0; i < resting; i++ {
		me.OrderCh <- contractOrder(uint(i+1), contractMakerUser, model.OrderSideSell, int64(60000+i), 1)
	}
	for i := 0; i < resting; i++ {
		<-c.admitted
		<-c.done
	}
	c.start()

	// 취소 producer만 먼저 시작한다. probe와 동시에 시작하면 backlog가
	// 형성되기 전에 probe가 admit될 수 있고, 그러면 "취소 홍수 중"을 잰 것이
	// 아니다.
	//
	// 중복 없는 실존 ID만 취소한다. 복원추출하면 not-found가 섞여 취소 처리
	// 비용이 실제와 달라진다.
	go func() {
		for i := 0; i < cancels; i++ {
			me.CancelCh <- CancelOrderCommand{
				CoinSymbol: "BTC", OrderID: uint(i + 1),
				Side: model.OrderSideSell, Price: decimal.NewFromInt(int64(60000 + i)),
				EnqueuedAt: time.Now(),
			}
		}
	}()

	// 장벽: 취소가 실제로 처리되고 있음을 관측한 뒤에 probe를 보낸다.
	// 채널 전송 완료(flood.Wait)는 처리 완료가 아니므로 판정 근거로 쓰지 않는다.
	const floodBarrier = 2000
	waitCancelSignals(t, c.cancels, floodBarrier, "취소 홍수 형성 장벽")

	go func() {
		for i := 0; i < probes; i++ {
			// 어떤 것과도 체결되지 않는 가격 — 재는 것은 큐 대기다.
			me.OrderCh <- contractOrder(uint(resting+i+1), contractTakerUser, model.OrderSideBuy, 100, 1)
		}
	}()

	deadline := time.After(60 * time.Second)
	for i := 0; i < probes; i++ {
		select {
		case <-c.admitted:
		case <-deadline:
			t.Fatalf("C5: probe 주문 %d/%d만 admit됐다 (censored)", i, probes)
		}
	}

	// 측정을 끝내기 전에 취소 15,000건이 **전부 처리**됐는지 확인한다.
	waitCancelSignals(t, c.cancels, cancels-floodBarrier, "취소 처리 완료")
	c.stop()

	c.mu.Lock()
	orderSamples := len(c.orderWaits)
	cancelSamples := len(c.cancelWaits)
	c.mu.Unlock()

	require.Equal(t, probes, orderSamples, "C5: order wait 표본이 정확히 %d개여야 한다", probes)
	require.Equal(t, cancels, cancelSamples, "C5: cancel wait 표본이 정확히 %d개여야 한다", cancels)
	reportGate(t, "C2 취소 홍수 중 주문 대기", c.p99(c.orderWaits), gateSafetyLimit, orderSamples)
}

// waitCancelSignals는 Cancel observer 신호 n개를 기다린다.
func waitCancelSignals(t *testing.T, ch <-chan struct{}, n int, what string) {
	t.Helper()
	deadline := time.After(60 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("%s: %d/%d만 처리됐다 (censored)", what, i, n)
		}
	}
}
