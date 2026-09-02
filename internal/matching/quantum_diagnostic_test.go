//go:build quantumharness

package matching

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// 이 파일은 H2-5000 sweep 측정에서 나온 0ns·비정상 저값의 원인을 찾기 위한
// **일회성 진단**이다. 판정에 쓰는 측정이 아니다.
//
// 기존 하니스(quantum_harness_test.go, quantum_scenarios_test.go)는 baseline을
// 만든 코드이므로 건드리지 않는다. 여기서는 같은 시나리오를 자체 관측으로
// 다시 구성해 회차마다 "무엇을 얼마나 처리했는지"를 직접 기록한다.
//
// 알려진 측정 계약 결함 두 가지를 이 진단이 정량화한다.
//
//  1. harnessLimitOrder가 UserID를 rng.Intn(1000)+1로 정한다. 엔진에는
//     자기 주문 체결을 막는 isSelfTrade가 있으므로, taker와 UserID가 같은
//     maker는 건너뛴다. 즉 H2-5000이 정확히 5,000건을 체결한다는 보장이 없다.
//  2. OrderDone 콜백은 실제 체결 수를 넘기는데 waitDoneSignals가 그 값을
//     버린다. order_samples=1은 "같은 작업량"의 증거가 아니다.

type diagRun struct {
	Seed                int64 `json:"seed"`
	ExpectedTrades      int   `json:"expected_trades"`
	SelfExcluded        int   `json:"self_excluded"`
	OrderDoneTrades     int   `json:"order_done_trades"`
	MeasuredTradeEmits  int   `json:"measured_trade_emits"`
	TakerFilled         string `json:"taker_filled"`
	TakerRemaining      string `json:"taker_remaining"`
	FirstTradeAtNs      int64 `json:"first_trade_at_ns"`
	OrderDoneAtNs       int64 `json:"order_done_at_ns"`
	ObserverDurationNs  int64 `json:"observer_duration_ns"`
	OuterDurationNs     int64 `json:"outer_duration_ns"`
	Censored            bool  `json:"censored"`
	WorkloadMismatch    bool  `json:"workload_mismatch"`
}

// diagCollector는 harnessCollector와 달리 "무엇을 얼마나" 처리했는지를
// 직접 센다. 엔진 goroutine에서만 콜백이 불리므로 락 없이 세되, 테스트
// goroutine은 Stop()+Done() 이후에만 읽는다.
type diagCollector struct {
	tradeEmits int
	doneTrades int

	measuring    bool
	start        time.Time
	firstTradeAt time.Duration
	doneAt       time.Duration

	firstTrade chan struct{}
	firstOnce  bool
	done       chan int
	admitted   chan struct{}
	cancels    chan struct{}
}

func newDiagCollector() *diagCollector {
	return &diagCollector{
		firstTrade: make(chan struct{}),
		done:       make(chan int, 1<<16),
		admitted:   make(chan struct{}, 1<<16),
		cancels:    make(chan struct{}, 1<<16),
	}
}

func (c *diagCollector) observers() EngineObservers {
	return EngineObservers{
		OrderAdmitted: func(time.Duration) {
			select {
			case c.admitted <- struct{}{}:
			default:
			}
		},
		OrderDone: func(trades int) {
			if c.measuring {
				c.doneTrades = trades
				c.doneAt = time.Since(c.start)
			}
			select {
			case c.done <- trades:
			default:
			}
		},
		Cancel: func(time.Duration) {
			select {
			case c.cancels <- struct{}{}:
			default:
			}
		},
		EmitBlock: func(kind EmitKind, _ time.Duration) {
			if kind != EmitTrade {
				return
			}
			if c.measuring {
				c.tradeEmits++
				if !c.firstOnce {
					c.firstOnce = true
					c.firstTradeAt = time.Since(c.start)
					close(c.firstTrade)
				}
			}
		},
	}
}

// runDiagSweep은 runSweepScenario와 같은 구성을 재현하되, 회차마다 실제
// 작업량과 두 종류의 소요 시간을 함께 기록한다.
//
// fixedUsers가 true면 maker·taker·victim의 UserID를 서로 겹치지 않는 고정값으로
// 준다 — 자기 주문 제외를 0으로 만들어 작업량을 5,000으로 고정하기 위한 수정안이다.
func runDiagSweep(t *testing.T, seed int64, makerCount int, fixedUsers bool) diagRun {
	t.Helper()
	setupRNG := rand.New(rand.NewSource(seed))
	takerRNG := rand.New(rand.NewSource(seed + 1))

	c := newDiagCollector()
	me := NewMatchingEngine()
	me.snapshotInterval = 100 * time.Millisecond
	cfg := harnessQuantum(me)
	me.maxMatchesPerTurn = cfg.MaxMatchesPerTurn
	me.maxConsecutiveCancels = cfg.MaxConsecutiveCancels
	me.Observers = c.observers()

	execDone := make(chan struct{})
	go func() {
		for range me.ExecutionCh {
		}
		close(execDone)
	}()
	go func() {
		for range me.SnapshotCh {
		}
	}()
	me.Start()

	// maker 구성. 고정 UserID면 자기 주문 제외가 0이 된다.
	makerUsers := make([]uint, makerCount)
	for i := 0; i < makerCount; i++ {
		o := harnessLimitOrder(setupRNG, uint(i+1), model.OrderSideSell, 50000, 1)
		if fixedUsers {
			o.UserID = diagMakerUser
		}
		makerUsers[i] = o.UserID
		me.OrderCh <- o
	}
	victimID := uint(makerCount + 1)
	victim := harnessLimitOrder(setupRNG, victimID, model.OrderSideSell, 90000, 1)
	if fixedUsers {
		victim.UserID = diagVictimUser
	}
	me.OrderCh <- victim

	_, ok := waitSignals(c.admitted, makerCount+1, harnessWatchdog)
	require.True(t, ok, "maker setup 미완료")
	require.True(t, waitDoneSignals(c.done, makerCount+1, harnessWatchdog), "maker setup 미완결")

	taker := harnessLimitOrder(takerRNG, uint(makerCount+2), model.OrderSideBuy, 50000, int64(makerCount))
	if fixedUsers {
		taker.UserID = diagTakerUser
	}
	// 현재 무작위 UserID 구성에서 자기 주문으로 제외될 maker 수.
	selfExcluded := 0
	for _, u := range makerUsers {
		if u == taker.UserID {
			selfExcluded++
		}
	}

	c.measuring = true
	c.start = time.Now()
	outerStart := time.Now()
	me.OrderCh <- taker

	censored := false
	select {
	case <-c.firstTrade:
	case <-time.After(harnessWatchdog):
		censored = true
	}
	if !censored {
		go func() {
			me.CancelCh <- CancelOrderCommand{
				CoinSymbol: "BTC", OrderID: victimID,
				Side: model.OrderSideSell, Price: decimal.NewFromInt(90000),
				EnqueuedAt: time.Now(),
			}
		}()
		if _, ok := waitSignals(c.cancels, 1, harnessWatchdog); !ok {
			censored = true
		}
		if !waitDoneSignals(c.done, 1, harnessWatchdog) {
			censored = true
		}
	}
	outer := time.Since(outerStart)

	me.Stop()
	select {
	case <-me.Done():
	case <-time.After(60 * time.Second):
		t.Fatal("engine did not stop")
	}
	<-execDone

	expected := makerCount - selfExcluded
	return diagRun{
		Seed:               seed,
		ExpectedTrades:     expected,
		SelfExcluded:       selfExcluded,
		OrderDoneTrades:    c.doneTrades,
		MeasuredTradeEmits: c.tradeEmits,
		TakerFilled:        taker.FilledAmount.String(),
		TakerRemaining:     taker.Amount.String(),
		FirstTradeAtNs:     int64(c.firstTradeAt),
		OrderDoneAtNs:      int64(c.doneAt),
		ObserverDurationNs: int64(c.doneAt),
		OuterDurationNs:    int64(outer),
		Censored:           censored,
		WorkloadMismatch:   c.doneTrades != expected || c.tradeEmits != expected,
	}
}

// maker·taker·victim이 서로 겹치지 않는 고정 UserID. 자기 주문 제외를 0으로
// 만들어 H2-5000이 정확히 makerCount건을 처리하게 한다.
const (
	diagMakerUser  uint = 900001
	diagTakerUser  uint = 900002
	diagVictimUser uint = 900003
)

func writeDiag(t *testing.T, name string, runs []diagRun) {
	t.Helper()
	dir := filepath.Join(harnessOutputDir, "diagnostic")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, name+".json")
	data, err := json.MarshalIndent(runs, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))

	// 사람이 바로 읽을 요약도 함께 남긴다.
	for _, r := range runs {
		t.Logf("seed=%d expected=%d self_excl=%d done_trades=%d emits=%d filled=%s remain=%s first=%v done=%v outer=%v mismatch=%v censored=%v",
			r.Seed, r.ExpectedTrades, r.SelfExcluded, r.OrderDoneTrades, r.MeasuredTradeEmits,
			r.TakerFilled, r.TakerRemaining,
			time.Duration(r.FirstTradeAtNs), time.Duration(r.ObserverDurationNs),
			time.Duration(r.OuterDurationNs), r.WorkloadMismatch, r.Censored)
	}
	t.Logf("wrote %s", path)
}

// 현재(무작위 UserID) 구성 재현 — 0ns가 나오는 원인을 관측한다.
func TestDiagH2LargeCurrent(t *testing.T) {
	runs := harnessRuns(t)
	out := make([]diagRun, 0, runs)
	for i := 0; i < runs; i++ {
		out = append(out, runDiagSweep(t, harnessSeedBase+int64(i)*100+2, 5000, false))
	}
	writeDiag(t, "h2-5000-current", out)
	summarizeDiag(t, "current", out)
}

// 수정안(고정 UserID) — 작업량이 5,000으로 고정되는지, 저값이 사라지는지 본다.
func TestDiagH2LargeFixedUsers(t *testing.T) {
	runs := harnessRuns(t)
	out := make([]diagRun, 0, runs)
	for i := 0; i < runs; i++ {
		out = append(out, runDiagSweep(t, harnessSeedBase+int64(i)*100+2, 5000, true))
	}
	writeDiag(t, "h2-5000-fixed-users", out)
	summarizeDiag(t, "fixed-users", out)
}

func summarizeDiag(t *testing.T, label string, runs []diagRun) {
	t.Helper()
	mismatch, zeroOuter, zeroObserver := 0, 0, 0
	minOuter, maxOuter := int64(1<<62), int64(0)
	for _, r := range runs {
		if r.WorkloadMismatch {
			mismatch++
		}
		if r.OuterDurationNs == 0 {
			zeroOuter++
		}
		if r.ObserverDurationNs == 0 {
			zeroObserver++
		}
		if r.OuterDurationNs < minOuter {
			minOuter = r.OuterDurationNs
		}
		if r.OuterDurationNs > maxOuter {
			maxOuter = r.OuterDurationNs
		}
	}
	t.Logf("[%s] runs=%d workload_mismatch=%d zero_outer=%d zero_observer=%d outer_range=[%v..%v]",
		label, len(runs), mismatch, zeroOuter, zeroObserver,
		time.Duration(minOuter), time.Duration(maxOuter))
	fmt.Printf("[%s] runs=%d workload_mismatch=%d zero_outer=%d zero_observer=%d outer_range=[%v..%v]\n",
		label, len(runs), mismatch, zeroOuter, zeroObserver,
		time.Duration(minOuter), time.Duration(maxOuter))
}
