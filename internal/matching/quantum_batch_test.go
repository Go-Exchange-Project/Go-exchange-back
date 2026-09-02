//go:build quantumharness

package matching

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// v2 묶음 측정. 단일 sweep C3를 폐기한 이유와 계약은 설계 §8.4~§8.6에 있다.
//
// 요점 세 가지.
//
//  1. 대조군과 후보를 **같은 실행 파일 안에서** 비교한다. 이전 계측 전용 SHA와
//     직접 비교하지 않는다. 대조군은 경계를 사실상 만나지 않는 테스트 전용
//     설정이며, quantum yield가 0인지 단언해 그 전제를 관측으로 뒷받침한다.
//  2. 한 번의 기록값은 단일 sweep이 아니라 여러 sweep의 **평균**이다.
//     단일 sweep 측정은 5회 중앙값 오차가 ±9~11%로 C3의 ±5%를 구분하지 못했다.
//  3. 작업량을 고정하고 매 묶음마다 검사한다. 어긋나면 censored가 아니라
//     measurement_invalid다 — 측정이 성립하지 않았다는 뜻이므로 run-set 전체를
//     실패시킨다.

// measurementSchemaVersion은 결과 형식 버전이다. 집계기는 이 버전만 받는다.
// 이전 baseline/explore/confirm 산출물이 새 선택에 섞이는 경로를 막는다.
const measurementSchemaVersion = 2

// 대조군: 경계를 사실상 만나지 않는 테스트 전용 설정.
const controlQuantumBound = 1_000_000

// 묶음 측정 고정 파라미터.
const (
	batchSweepTrades = 5000 // sweep당 정확히 이만큼 체결한다
	batchWarmupSweep = 8    // 기록하지 않는다
	batchMakerPrice  = 50000
	batchVictimBase  = 90000
)

// maker·taker·victim이 서로 겹치지 않는 고정 UserID.
// 자기 주문 제외(isSelfTrade)를 0으로 만들어 작업량을 정확히 고정한다.
const (
	batchMakerUser  uint = 800001
	batchTakerUser  uint = 800002
	batchVictimUser uint = 800003
)

type batchResult struct {
	MeasurementSchemaVersion int    `json:"measurement_schema_version"`
	Scenario                 string `json:"scenario"`
	Label                    string `json:"label"` // "control" | "candidate"
	Seed                     int64  `json:"seed"`
	MaxMatchesPerTurn        int    `json:"max_matches_per_turn"`
	MaxConsecutiveCancels    int    `json:"max_consecutive_cancels"`

	SweepBatchSize    int   `json:"sweep_batch_size"`
	SweepBatchTotalNs int64 `json:"sweep_batch_total_ns"`
	SweepBatchMeanNs  int64 `json:"sweep_batch_mean_ns"`

	ExpectedTradesTotal     int    `json:"expected_trades_total"`
	OrderDoneTradesTotal    int    `json:"order_done_trades_total"`
	MeasuredTradeEmitsTotal int    `json:"measured_trade_emits_total"`
	RemainingAmountTotal    string `json:"remaining_amount_total"`

	QuantumYields int64 `json:"quantum_yields"`

	Censored           bool   `json:"censored"`
	MeasurementInvalid bool   `json:"measurement_invalid"`
	InvalidReason      string `json:"invalid_reason"`
}

// batchCollector는 측정 구간의 작업량과 yield를 센다. 콜백은 엔진
// goroutine에서만 불리므로 락 없이 세되, 테스트 goroutine은 배리어 채널로만
// 동기화하고 카운터는 엔진 정지 후에 읽는다.
type batchCollector struct {
	measuring   bool
	tradeEmits  int
	doneTrades  int
	yields      int64
	cancelCount int64

	done     chan int
	admitted chan struct{}
	cancels  chan struct{}
}

func newBatchCollector() *batchCollector {
	return &batchCollector{
		done:     make(chan int, 1<<20),
		admitted: make(chan struct{}, 1<<20),
		cancels:  make(chan struct{}, 1<<20),
	}
}

func (c *batchCollector) observers() EngineObservers {
	return EngineObservers{
		OrderAdmitted: func(time.Duration) {
			select {
			case c.admitted <- struct{}{}:
			default:
			}
		},
		OrderDone: func(trades int) {
			if c.measuring {
				c.doneTrades += trades
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
			if kind == EmitTrade && c.measuring {
				c.tradeEmits++
			}
		},
		Yield: func() {
			if c.measuring {
				atomic.AddInt64(&c.yields, 1)
			}
		},
	}
}

func batchOrder(id uint, user uint, side model.OrderSide, price int64, amount int64) *Order {
	return &Order{
		ID:         id,
		UserID:     user,
		CoinSymbol: "BTC",
		Side:       side,
		Price:      decimal.NewFromInt(price),
		Amount:     decimal.NewFromInt(amount),
		OrderType:  model.OrderTypeLimit,
		CreatedAt:  time.Now(),
		EnqueuedAt: time.Now(),
	}
}

// runSweepBatch는 batchSize회의 sweep을 연속 실행해 평균 소요 시간을 낸다.
//
// 준비 주문(워밍업분 포함)은 타이머 시작 **전에** 모두 넣고 완료 장벽을
// 통과한다. 각 sweep은 taker 완료와 victim 취소 완료를 모두 확인한 뒤 다음으로
// 넘어간다.
func runSweepBatch(t *testing.T, label string, seed int64, cfg QuantumConfig, batchSize int) batchResult {
	t.Helper()

	total := batchWarmupSweep + batchSize
	c := newBatchCollector()

	me := NewMatchingEngine()
	me.snapshotInterval = 100 * time.Millisecond
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

	// 준비: 전 sweep분 maker와 victim을 타이머 시작 전에 모두 적재한다.
	// maker는 한 가격대에 쌓는다 — deque의 front 제거가 O(1)이라 깊이가
	// 체결 비용을 바꾸지 않는다.
	orderID := uint(1)
	for i := 0; i < total*batchSweepTrades; i++ {
		me.OrderCh <- batchOrder(orderID, batchMakerUser, model.OrderSideSell, batchMakerPrice, 1)
		orderID++
	}
	victimIDs := make([]uint, total)
	victimPrices := make([]int64, total)
	for i := 0; i < total; i++ {
		victimIDs[i] = orderID
		victimPrices[i] = int64(batchVictimBase + i)
		me.OrderCh <- batchOrder(orderID, batchVictimUser, model.OrderSideSell, victimPrices[i], 1)
		orderID++
	}
	setupCount := total*batchSweepTrades + total
	censored := false
	if _, ok := waitSignals(c.admitted, setupCount, batchWatchdog); !ok {
		censored = true
	}
	if !waitDoneSignals(c.done, setupCount, batchWatchdog) {
		censored = true
	}

	takers := make([]*Order, total)
	runSweep := func(i int) bool {
		taker := batchOrder(orderID, batchTakerUser, model.OrderSideBuy, batchMakerPrice, batchSweepTrades)
		orderID++
		takers[i] = taker
		me.OrderCh <- taker
		if !waitDoneSignals(c.done, 1, batchWatchdog) {
			return false
		}
		me.CancelCh <- CancelOrderCommand{
			CoinSymbol: "BTC", OrderID: victimIDs[i],
			Side: model.OrderSideSell, Price: decimal.NewFromInt(victimPrices[i]),
			EnqueuedAt: time.Now(),
		}
		if _, ok := waitSignals(c.cancels, 1, batchWatchdog); !ok {
			return false
		}
		return true
	}

	// 워밍업 — 기록하지 않는다.
	for i := 0; i < batchWarmupSweep && !censored; i++ {
		if !runSweep(i) {
			censored = true
		}
	}

	// 측정 구간.
	c.measuring = true
	batchStart := time.Now()
	for i := batchWarmupSweep; i < total && !censored; i++ {
		if !runSweep(i) {
			censored = true
		}
	}
	batchTotal := time.Since(batchStart)
	c.measuring = false

	me.Stop()
	select {
	case <-me.Done():
	case <-time.After(120 * time.Second):
		t.Fatal("batch engine did not stop")
	}
	<-execDone

	remaining := decimal.Zero
	for i := batchWarmupSweep; i < total; i++ {
		if takers[i] != nil {
			remaining = remaining.Add(takers[i].Amount)
		}
	}

	expected := batchSize * batchSweepTrades
	mean := int64(0)
	if batchSize > 0 {
		mean = int64(batchTotal) / int64(batchSize)
	}

	res := batchResult{
		MeasurementSchemaVersion: measurementSchemaVersion,
		Scenario:                 "H2-5000-batch",
		Label:                    label,
		Seed:                     seed,
		MaxMatchesPerTurn:        cfg.MaxMatchesPerTurn,
		MaxConsecutiveCancels:    cfg.MaxConsecutiveCancels,
		SweepBatchSize:           batchSize,
		SweepBatchTotalNs:        int64(batchTotal),
		SweepBatchMeanNs:         mean,
		ExpectedTradesTotal:      expected,
		OrderDoneTradesTotal:     c.doneTrades,
		MeasuredTradeEmitsTotal:  c.tradeEmits,
		RemainingAmountTotal:     remaining.String(),
		QuantumYields:            atomic.LoadInt64(&c.yields),
		Censored:                 censored,
	}
	res.MeasurementInvalid, res.InvalidReason = validateBatch(res)
	return res
}

const batchWatchdog = 120 * time.Second

// validateBatch는 작업량·필드·시간값 계약을 확인한다.
// censored(watchdog 초과)와 합치지 않는다 — 원인이 다르고 대응도 다르다.
func validateBatch(r batchResult) (bool, string) {
	switch {
	case r.MeasurementSchemaVersion != measurementSchemaVersion:
		return true, fmt.Sprintf("schema version %d, want %d", r.MeasurementSchemaVersion, measurementSchemaVersion)
	case r.SweepBatchSize <= 0:
		return true, "sweep_batch_size <= 0"
	case r.ExpectedTradesTotal != r.SweepBatchSize*batchSweepTrades:
		return true, fmt.Sprintf("expected_trades_total=%d, want %d", r.ExpectedTradesTotal, r.SweepBatchSize*batchSweepTrades)
	case r.OrderDoneTradesTotal != r.ExpectedTradesTotal:
		return true, fmt.Sprintf("order_done_trades_total=%d, want %d", r.OrderDoneTradesTotal, r.ExpectedTradesTotal)
	case r.MeasuredTradeEmitsTotal != r.ExpectedTradesTotal:
		return true, fmt.Sprintf("measured_trade_emits_total=%d, want %d", r.MeasuredTradeEmitsTotal, r.ExpectedTradesTotal)
	case r.RemainingAmountTotal != "0":
		return true, "remaining_amount_total != 0: " + r.RemainingAmountTotal
	case r.SweepBatchTotalNs <= 0:
		return true, "sweep_batch_total_ns <= 0"
	case r.SweepBatchMeanNs <= 0:
		return true, "sweep_batch_mean_ns <= 0"
	}
	return false, ""
}

func controlConfig() QuantumConfig {
	return QuantumConfig{MaxMatchesPerTurn: controlQuantumBound, MaxConsecutiveCancels: controlQuantumBound}
}

func writeBatch(t *testing.T, subdir, name string, results []batchResult) {
	t.Helper()
	dir := filepath.Join(harnessOutputDir, subdir)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, name+".json")
	require.NoFileExists(t, path, "같은 산출물을 덮어쓰려 한다 — 디렉터리를 확인하라")
	data, err := json.MarshalIndent(results, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))
	t.Logf("wrote %s (%d records)", path, len(results))
}
