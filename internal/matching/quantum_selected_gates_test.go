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

// newGateEngine은 엔진을 **구성만** 하고 Start하지 않는다. 호출자가 준비를
// 끝낸 뒤 직접 me.Start()를 부른다 — C2는 채널을 미리 채워야 하므로 시작
// 시점을 통제해야 한다.
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
	me.Start()

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
//
// **엔진을 Start하기 전에 모든 입력을 적재한다.** 이것이 홍수를 결정적으로
// 만드는 유일한 방법이다. producer goroutine을 먼저 띄우고 관측 신호를
// 기다리는 방식은 "취소 N건이 관측됐다"만 증명할 뿐, probe가 들어가는 순간
// CancelCh에 취소가 남아 있다는 것은 증명하지 못한다 — producer가 잠시 밀리면
// probe가 홍수 없이 처리될 수 있다.
//
// 준비 순서:
//   1. resting 주문을 오더북에 직접 넣는다 (엔진 시작 전이므로 안전하다)
//   2. 테스트 전용 CancelCh(용량 >= 15,000)에 취소를 전부 넣는다
//   3. OrderCh에 probe 500건을 넣는다
//   4. 측정을 켜고 Start한다
func TestSelectedConfigC2OrderWaitUnderCancelFlood(t *testing.T) {
	const resting = 20000
	const probes = 500
	const cancels = 15000

	c := newGateCollector()
	me, shutdown := newGateEngine(t, c)
	defer shutdown()

	// 1) resting 주문은 오더북에 직접 넣는다. 엔진이 아직 돌지 않으므로
	//    book을 테스트 goroutine이 만져도 race가 아니다. 이렇게 하면 setup이
	//    observer 표본을 만들지 않아 측정 구간이 probe·취소로만 채워진다.
	book := me.GetOrderBook("BTC")
	for i := 0; i < resting; i++ {
		book.AddOrder(contractOrder(uint(i+1), contractMakerUser, model.OrderSideSell, int64(60000+i), 1))
	}

	// 2) 취소를 전부 미리 넣는다. 기본 CancelCh는 1024라 15,000건을 담지
	//    못하므로 테스트 전용으로 교체한다. 중복 없는 실존 ID만 쓴다 —
	//    복원추출하면 not-found가 섞여 취소 처리 비용이 실제와 달라진다.
	me.CancelCh = make(chan CancelOrderCommand, cancels)
	for i := 0; i < cancels; i++ {
		me.CancelCh <- CancelOrderCommand{
			CoinSymbol: "BTC", OrderID: uint(i + 1),
			Side: model.OrderSideSell, Price: decimal.NewFromInt(int64(60000 + i)),
			EnqueuedAt: time.Now(),
		}
	}
	require.Equal(t, cancels, len(me.CancelCh), "Start 전에 취소 backlog가 가득 차 있어야 한다")

	// 3) probe도 미리 넣는다. 어떤 것과도 체결되지 않는 가격이므로
	//    재는 것은 체결이 아니라 큐 대기다. OrderCh 기본 용량 1024 >= 500.
	for i := 0; i < probes; i++ {
		me.OrderCh <- contractOrder(uint(resting+i+1), contractTakerUser, model.OrderSideBuy, 100, 1)
	}
	require.Equal(t, probes, len(me.OrderCh))

	// 4) 이 시점에 두 큐가 모두 가득 차 있다. 이제 측정을 켜고 시작한다.
	c.start()
	me.Start()

	deadline := time.After(60 * time.Second)
	for i := 0; i < probes; i++ {
		select {
		case <-c.admitted:
		case <-deadline:
			t.Fatalf("C5: probe 주문 %d/%d만 admit됐다 (censored)", i, probes)
		}
	}
	// Cancel observer는 handleCancel보다 먼저 불리므로 "처리 완료"가 아니라
	// **queue-wait 표본 수집 완료**를 뜻한다.
	waitCancelSignals(t, c.cancels, cancels, "취소 queue-wait 표본 수집")
	c.stop()

	c.mu.Lock()
	orderSamples := len(c.orderWaits)
	cancelSamples := len(c.cancelWaits)
	c.mu.Unlock()

	require.Equal(t, probes, orderSamples, "C5: order wait 표본이 정확히 %d개여야 한다", probes)
	require.Equal(t, cancels, cancelSamples, "C5: cancel wait 표본이 정확히 %d개여야 한다", cancels)
	reportGate(t, "C2 취소 홍수 중 주문 대기", c.p99(c.orderWaits), gateSafetyLimit, orderSamples)
}

// waitCancelSignals는 Cancel observer 신호 n개를 기다린다. 이 신호는
// processCancel 진입 시점에 발생하므로 "취소 처리 완료"가 아니라
// **queue-wait 표본이 n개 수집됐다**는 뜻이다.
func waitCancelSignals(t *testing.T, ch <-chan struct{}, n int, what string) {
	t.Helper()
	deadline := time.After(60 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("%s: %d/%d만 수집됐다 (censored)", what, i, n)
		}
	}
}
