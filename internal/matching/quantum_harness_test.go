//go:build quantumharness

package matching

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// 하니스가 지켜야 할 규칙. 하나라도 깨지면 나온 숫자를 비교에 쓸 수 없다.
//
//	B1 Observers는 Start() 전에 한 번만 설정 — 재대입은 data race
//	B2 goroutine마다 자기 *rand.Rand — rand.Rand는 동시 사용 불가
//	B3 setup 주문이 전부 처리된 뒤 측정 시작 — 표본 오염 방지
//	B4 sweep은 첫 trade 관측을 시작 장벽으로 — Sleep은 시작 전/후를 잼
//	B5 완료는 producer 종료가 아니라 observer 관측으로 판정
//	B6 장벽 deadline 초과는 censored — 버리거나 성공으로 세지 않는다
//	B7 취소 ID는 중복 없는 실존 ID — 복원추출하면 not-found가 섞인다
const (
	harnessSeedBase  = 20260830
	harnessWatchdog  = 30 * time.Second
	harnessOutputDir = "../../_workspace/quantum"
)


func harnessRuns(t *testing.T) int {
	t.Helper()
	n, err := strconv.Atoi(os.Getenv("GOEXCHANGE_QUANTUM_RUNS"))
	require.NoError(t, err, "GOEXCHANGE_QUANTUM_RUNS를 지정해야 한다")
	require.Greater(t, n, 0)
	return n
}

type scenarioResult struct {
	Scenario              string `json:"scenario"`
	Seed                  int64  `json:"seed"`
	MaxMatchesPerTurn     int    `json:"max_matches_per_turn"`
	MaxConsecutiveCancels int    `json:"max_consecutive_cancels"`
	OrderWaitP50Ns        int64  `json:"order_wait_p50_ns"`
	OrderWaitP95Ns        int64  `json:"order_wait_p95_ns"`
	OrderWaitP99Ns        int64  `json:"order_wait_p99_ns"`
	OrderWaitMaxNs        int64  `json:"order_wait_max_ns"`
	OrderCensored         int    `json:"order_censored"`
	CancelWaitP99Ns       int64  `json:"cancel_wait_p99_ns"`
	CancelCensored        int    `json:"cancel_censored"`
	EmitBlockP99Ns        int64  `json:"emit_block_p99_ns"`
	SweepTotalNs          int64  `json:"sweep_total_ns"`
	SweepCensored         int    `json:"sweep_censored"`
	MaxSnapshotGapNs      int64  `json:"max_snapshot_gap_ns"`
	OrderSamples          int    `json:"order_samples"`
	CancelSamples         int    `json:"cancel_samples"`
}

// harnessCollector는 관측 콜백을 모은다. measuring이 false인 동안의 표본은
// 버린다(B3). snapshot gap도 measuring 구간만 기록한다 — setup 구간의 gap이
// 섞이면 H4가 실제보다 나빠 보인다.
type harnessCollector struct {
	mu        sync.Mutex
	measuring bool

	orderWaits  latencySamples
	cancelWaits latencySamples
	emitBlocks  latencySamples

	snapshotGaps []time.Duration
	lastSnapshot time.Time

	firstTrade   chan struct{}
	firstTradeOK bool
	doneOrders   chan int
	admitted     chan struct{}
	cancels      chan struct{}
}

func newHarnessCollector() *harnessCollector {
	return &harnessCollector{
		firstTrade: make(chan struct{}),
		doneOrders: make(chan int, 1<<16),
		admitted:   make(chan struct{}, 1<<16),
		cancels:    make(chan struct{}, 1<<16),
	}
}

// observers는 Start() 전에 한 번만 설정된다(B1).
func (c *harnessCollector) observers() EngineObservers {
	return EngineObservers{
		OrderAdmitted: func(d time.Duration) {
			c.mu.Lock()
			if c.measuring {
				c.orderWaits.observed = append(c.orderWaits.observed, d)
			}
			c.mu.Unlock()
			select {
			case c.admitted <- struct{}{}:
			default:
			}
		},
		OrderDone: func(trades int) {
			select {
			case c.doneOrders <- trades:
			default:
			}
		},
		Cancel: func(d time.Duration) {
			c.mu.Lock()
			if c.measuring {
				c.cancelWaits.observed = append(c.cancelWaits.observed, d)
			}
			c.mu.Unlock()
			select {
			case c.cancels <- struct{}{}:
			default:
			}
		},
		EmitBlock: func(kind EmitKind, d time.Duration) {
			c.mu.Lock()
			if c.measuring {
				c.emitBlocks.observed = append(c.emitBlocks.observed, d)
			}
			if kind == EmitTrade && !c.firstTradeOK {
				c.firstTradeOK = true
				close(c.firstTrade)
			}
			c.mu.Unlock()
		},
	}
}

// startMeasuring은 snapshot 상태도 초기화한다. setup 구간의 gap을 H4에
// 포함하면 안 된다.
func (c *harnessCollector) startMeasuring() {
	c.mu.Lock()
	c.measuring = true
	c.snapshotGaps = nil
	c.lastSnapshot = time.Now()
	c.mu.Unlock()
}

// closeSnapshotWindow는 마지막 스냅샷 이후 측정 종료까지의 간격을 닫아
// 기록한다. 이걸 빼면 sweep 끝 무렵의 긴 공백이 통째로 사라진다.
func (c *harnessCollector) closeSnapshotWindow() {
	c.mu.Lock()
	if c.measuring && !c.lastSnapshot.IsZero() {
		c.snapshotGaps = append(c.snapshotGaps, time.Since(c.lastSnapshot))
	}
	c.measuring = false
	c.mu.Unlock()
}

func (c *harnessCollector) censor(order, cancel int) {
	c.mu.Lock()
	c.orderWaits.censored += order
	c.cancelWaits.censored += cancel
	c.mu.Unlock()
}

// waitSignals는 n건의 관측을 기다린다(B5·B6). 도착 수와 성공 여부를 돌려준다.
func waitSignals(ch <-chan struct{}, n int, deadline time.Duration) (int, bool) {
	timeout := time.After(deadline)
	for got := 0; got < n; got++ {
		select {
		case <-ch:
		case <-timeout:
			return got, false
		}
	}
	return n, true
}

func waitDoneSignals(ch <-chan int, n int, deadline time.Duration) bool {
	timeout := time.After(deadline)
	for got := 0; got < n; got++ {
		select {
		case <-ch:
		case <-timeout:
			return false
		}
	}
	return true
}

func (c *harnessCollector) waitFirstTrade(deadline time.Duration) bool {
	select {
	case <-c.firstTrade:
		return true
	case <-time.After(deadline):
		return false
	}
}

func (c *harnessCollector) result(scenario string, seed int64, cfg QuantumConfig,
	sweepTotal time.Duration, sweepCensored int) scenarioResult {
	c.mu.Lock()
	defer c.mu.Unlock()

	orderP99 := InfNs
	if !c.orderWaits.p99Infinite() {
		orderP99 = int64(c.orderWaits.percentile(0.99))
	}
	cancelP99 := InfNs
	if !c.cancelWaits.p99Infinite() {
		cancelP99 = int64(c.cancelWaits.percentile(0.99))
	}
	sweepNs := int64(sweepTotal)
	if sweepCensored > 0 {
		sweepNs = InfNs
	}
	var maxGap time.Duration
	for _, g := range c.snapshotGaps {
		if g > maxGap {
			maxGap = g
		}
	}
	return scenarioResult{
		Scenario:              scenario,
		Seed:                  seed,
		MaxMatchesPerTurn:     cfg.MaxMatchesPerTurn,
		MaxConsecutiveCancels: cfg.MaxConsecutiveCancels,
		OrderWaitP50Ns:        int64(c.orderWaits.percentile(0.50)),
		OrderWaitP95Ns:        int64(c.orderWaits.percentile(0.95)),
		OrderWaitP99Ns:        orderP99,
		OrderWaitMaxNs:        int64(c.orderWaits.percentile(1.0)),
		OrderCensored:         c.orderWaits.censored,
		CancelWaitP99Ns:       cancelP99,
		CancelCensored:        c.cancelWaits.censored,
		EmitBlockP99Ns:        int64(c.emitBlocks.percentile(0.99)),
		SweepTotalNs:          sweepNs,
		SweepCensored:         sweepCensored,
		MaxSnapshotGapNs:      int64(maxGap),
		OrderSamples:          len(c.orderWaits.observed),
		CancelSamples:         len(c.cancelWaits.observed),
	}
}

// writeResults는 GOEXCHANGE_QUANTUM_OUTDIR 하위에 쓴다. baseline과 후보
// 산출물이 같은 디렉터리에 섞이면 baseline이 덮인다.
func writeResults(t *testing.T, name string, results []scenarioResult) {
	t.Helper()
	sub := os.Getenv("GOEXCHANGE_QUANTUM_OUTDIR")
	require.NotEmpty(t, sub, "GOEXCHANGE_QUANTUM_OUTDIR을 지정해야 한다")
	dir := filepath.Join(harnessOutputDir, sub)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, name+".json")
	require.NoFileExists(t, path, "같은 산출물을 덮어쓰려 한다 — 디렉터리를 확인하라")
	data, err := json.MarshalIndent(results, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))
	t.Logf("wrote %s (%d runs)", path, len(results))
}

func harnessQuantum(me *MatchingEngine) QuantumConfig {
	cfg := QuantumConfig{
		MaxMatchesPerTurn:     me.maxMatchesPerTurn,
		MaxConsecutiveCancels: me.maxConsecutiveCancels,
	}
	if v, err := strconv.Atoi(os.Getenv("GOEXCHANGE_MATCHING_MAX_MATCHES_PER_TURN")); err == nil && v > 0 {
		cfg.MaxMatchesPerTurn = v
	}
	if v, err := strconv.Atoi(os.Getenv("GOEXCHANGE_MATCHING_MAX_CONSECUTIVE_CANCELS")); err == nil && v > 0 {
		cfg.MaxConsecutiveCancels = v
	}
	return cfg
}

func harnessEngine(t *testing.T, c *harnessCollector) (*MatchingEngine, QuantumConfig, func()) {
	t.Helper()
	me := NewMatchingEngine()
	me.snapshotInterval = 100 * time.Millisecond
	cfg := harnessQuantum(me)
	me.maxMatchesPerTurn = cfg.MaxMatchesPerTurn
	me.maxConsecutiveCancels = cfg.MaxConsecutiveCancels
	me.Observers = c.observers() // B1

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
	return me, cfg, func() {
		me.Stop()
		select {
		case <-me.Done():
		case <-time.After(60 * time.Second):
			t.Fatal("harness engine did not stop")
		}
		wg.Wait()
	}
}

func harnessLimitOrder(rng *rand.Rand, id uint, side model.OrderSide, price int64, amount int64) *Order {
	return &Order{
		ID:         id,
		UserID:     uint(rng.Intn(1000) + 1),
		CoinSymbol: "BTC",
		Side:       side,
		Price:      decimal.NewFromInt(price),
		Amount:     decimal.NewFromInt(amount),
		OrderType:  model.OrderTypeLimit,
		CreatedAt:  time.Now(),
		EnqueuedAt: time.Now(),
	}
}
