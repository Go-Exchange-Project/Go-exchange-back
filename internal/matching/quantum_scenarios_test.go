//go:build quantumharness

package matching

import (
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

const (
	// 취소를 중복 없이 뽑으려면 resting 주문이 취소 수 이상이어야 한다(B7).
	floodRestingOrders = 20000
	floodProbeOrders   = 500
	floodCancels       = 15000
)

// runFloodScenario는 H0(cancelCount=0)과 H1의 공통 골격이다.
// 두 시나리오는 주문 수·producer 수·consumer 속도가 반드시 같고 취소 수만
// 다르다 — C2의 상한이 H0에서 유도되므로 다른 파라미터가 다르면 비교가
// 성립하지 않는다.
func runFloodScenario(t *testing.T, seed int64, cancelCount int) scenarioResult {
	require.LessOrEqual(t, cancelCount, floodRestingOrders,
		"중복 없는 실존 ID를 쓰려면 resting 주문이 취소 수 이상이어야 한다")

	setupRNG := rand.New(rand.NewSource(seed)) // B2: goroutine별 RNG
	cancelRNG := rand.New(rand.NewSource(seed + 1))
	probeRNG := rand.New(rand.NewSource(seed + 2))

	c := newHarnessCollector()
	me, cfg, shutdown := harnessEngine(t, c)
	defer shutdown()

	for i := 0; i < floodRestingOrders; i++ {
		me.OrderCh <- harnessLimitOrder(setupRNG, uint(i+1), model.OrderSideSell, int64(60000+i), 1)
	}
	// B3: setup이 전부 처리된 뒤에 측정을 켠다.
	_, ok := waitSignals(c.admitted, floodRestingOrders, harnessWatchdog)
	require.True(t, ok, "setup 주문이 admit되지 않았다")
	require.True(t, waitDoneSignals(c.doneOrders, floodRestingOrders, harnessWatchdog),
		"setup 주문이 완결되지 않았다")
	c.startMeasuring()

	// 취소는 CancelCh에 직접 넣는다 — 동기 CancelOrder는 응답을 기다리므로
	// 한 goroutine에서 부르면 큐 깊이가 1을 넘지 않아 flood가 되지 않는다.
	// B7: 셔플로 중복 없는 실존 ID만 뽑는다.
	var flood sync.WaitGroup
	if cancelCount > 0 {
		perm := cancelRNG.Perm(floodRestingOrders)[:cancelCount]
		flood.Add(1)
		go func() {
			defer flood.Done()
			for _, idx := range perm {
				me.CancelCh <- CancelOrderCommand{
					CoinSymbol: "BTC",
					OrderID:    uint(idx + 1),
					Side:       model.OrderSideSell,
					Price:      decimal.NewFromInt(int64(60000 + idx)),
					EnqueuedAt: time.Now(),
				}
			}
		}()
	}

	start := time.Now()
	go func() {
		for i := 0; i < floodProbeOrders; i++ {
			// 어떤 것과도 체결되지 않는 가격 — 측정 대상은 큐 대기다.
			me.OrderCh <- harnessLimitOrder(probeRNG, uint(floodRestingOrders+i+1),
				model.OrderSideBuy, 100, 1)
		}
	}()

	// B5·B6: producer 종료가 아니라 observer 관측을 기다린다.
	got, ok := waitSignals(c.admitted, floodProbeOrders, harnessWatchdog)
	elapsed := time.Since(start)
	if !ok {
		c.censor(floodProbeOrders-got, 0)
		t.Logf("seed=%d cancels=%d: probe %d/%d censored",
			seed, cancelCount, floodProbeOrders-got, floodProbeOrders)
	}
	if cancelCount > 0 {
		if n, ok := waitSignals(c.cancels, cancelCount, harnessWatchdog); !ok {
			c.censor(0, cancelCount-n)
			t.Logf("seed=%d: 취소 %d/%d censored", seed, cancelCount-n, cancelCount)
		}
	}
	flood.Wait()
	c.closeSnapshotWindow()

	name := "H1"
	if cancelCount == 0 {
		name = "H0"
	}
	return c.result(name, seed, cfg, elapsed, 0)
}

func TestHarnessH0NoCancelControl(t *testing.T) {
	runs := harnessRuns(t)
	results := make([]scenarioResult, 0, runs)
	for i := 0; i < runs; i++ {
		results = append(results, runFloodScenario(t, harnessSeedBase+int64(i)*100, 0))
	}
	writeResults(t, "H0", results)
}

func TestHarnessH1CancelFlood(t *testing.T) {
	runs := harnessRuns(t)
	results := make([]scenarioResult, 0, runs)
	for i := 0; i < runs; i++ {
		results = append(results, runFloodScenario(t, harnessSeedBase+int64(i)*100, floodCancels))
	}
	writeResults(t, "H1", results)
}

// runSweepScenario는 makerCount개 maker를 쓸어가는 taker를 넣고, sweep이
// 실제로 시작된 뒤(B4) 취소 1건을 던져 그 취소가 언제 처리되는지 잰다.
//
// SweepTotalNs는 taker의 OrderDone까지의 시간이다(B5). 첫 취소 응답까지가
// 아니다.
func runSweepScenario(t *testing.T, scenario string, seed int64, makerCount int) scenarioResult {
	setupRNG := rand.New(rand.NewSource(seed))
	takerRNG := rand.New(rand.NewSource(seed + 1))

	c := newHarnessCollector()
	me, cfg, shutdown := harnessEngine(t, c)
	defer shutdown()

	for i := 0; i < makerCount; i++ {
		me.OrderCh <- harnessLimitOrder(setupRNG, uint(i+1), model.OrderSideSell, 50000, 1)
	}
	// sweep이 닿지 않는 가격의 취소 대상.
	victimID := uint(makerCount + 1)
	me.OrderCh <- harnessLimitOrder(setupRNG, victimID, model.OrderSideSell, 90000, 1)

	_, ok := waitSignals(c.admitted, makerCount+1, harnessWatchdog)
	require.True(t, ok, "maker setup 미완료")
	require.True(t, waitDoneSignals(c.doneOrders, makerCount+1, harnessWatchdog), "maker setup 미완결")
	c.startMeasuring()

	start := time.Now()
	me.OrderCh <- harnessLimitOrder(takerRNG, uint(makerCount+2), model.OrderSideBuy, 50000, int64(makerCount))

	// B4: 첫 trade를 관측해야 sweep이 실제로 진행 중이다.
	if !c.waitFirstTrade(harnessWatchdog) {
		c.closeSnapshotWindow()
		t.Logf("seed=%d makers=%d: sweep이 시작되지 않았다", seed, makerCount)
		return c.result(scenario, seed, cfg, 0, 1)
	}

	go func() {
		me.CancelCh <- CancelOrderCommand{
			CoinSymbol: "BTC", OrderID: victimID,
			Side: model.OrderSideSell, Price: decimal.NewFromInt(90000),
			EnqueuedAt: time.Now(),
		}
	}()

	// cancel observer 1건을 deadline 안에 확인한 뒤 결과를 만든다.
	if _, ok := waitSignals(c.cancels, 1, harnessWatchdog); !ok {
		c.censor(0, 1)
		t.Logf("seed=%d makers=%d: sweep 중 취소가 censored", seed, makerCount)
	}

	// B5: taker의 OrderDone이 sweep 완료다.
	sweepCensored := 0
	if !waitDoneSignals(c.doneOrders, 1, harnessWatchdog) {
		sweepCensored = 1
		t.Logf("seed=%d makers=%d: sweep이 완료되지 않았다", seed, makerCount)
	}
	elapsed := time.Since(start)
	c.closeSnapshotWindow()

	return c.result(scenario, seed, cfg, elapsed, sweepCensored)
}

func TestHarnessH2Small(t *testing.T) {
	runs := harnessRuns(t)
	results := make([]scenarioResult, 0, runs)
	for i := 0; i < runs; i++ {
		results = append(results, runSweepScenario(t, "H2-1", harnessSeedBase+int64(i)*100+1, 1))
	}
	writeResults(t, "H2-1", results)
}

func TestHarnessH2Large(t *testing.T) {
	runs := harnessRuns(t)
	results := make([]scenarioResult, 0, runs)
	for i := 0; i < runs; i++ {
		results = append(results, runSweepScenario(t, "H2-5000", harnessSeedBase+int64(i)*100+2, 5000))
	}
	writeResults(t, "H2-5000", results)
}

// H4는 H2-5000과 같은 부하이고 보는 것만 다르다(스냅샷 간격).
// 시드를 달리해 H2-5000과 독립 표본으로 만든다.
func TestHarnessH4SnapshotFreshness(t *testing.T) {
	runs := harnessRuns(t)
	results := make([]scenarioResult, 0, runs)
	for i := 0; i < runs; i++ {
		results = append(results, runSweepScenario(t, "H4", harnessSeedBase+int64(i)*100+3, 5000))
	}
	writeResults(t, "H4", results)
}
