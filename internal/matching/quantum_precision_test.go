//go:build quantumharness

package matching

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func durationOf(ns int64) time.Duration { return time.Duration(ns) }

// 정밀도 시험(설계 §8.6). 전체 격자 전에 측정 도구가 C3의 5% 차이를 구분할 수
// 있는지 먼저 확인한다. 통과하기 전에는 후보를 선택하지 않는다.
//
//	GOEXCHANGE_QUANTUM_BATCH=64 GOEXCHANGE_QUANTUM_OUTDIR=precision/b64 \
//	  go test -tags quantumharness ./internal/matching -run TestPrecisionTrial

// precisionCandidate는 가장 자주 양보하는 후보다.
var precisionCandidate = QuantumConfig{MaxMatchesPerTurn: 16, MaxConsecutiveCancels: 8}

const precisionPairs = 5

func batchSizeFromEnv(t *testing.T) int {
	t.Helper()
	n, err := strconv.Atoi(os.Getenv("GOEXCHANGE_QUANTUM_BATCH"))
	require.NoError(t, err, "GOEXCHANGE_QUANTUM_BATCH를 지정해야 한다 (64 또는 128)")
	require.Greater(t, n, 0)
	return n
}

func outdirFromEnv(t *testing.T) string {
	t.Helper()
	sub := os.Getenv("GOEXCHANGE_QUANTUM_OUTDIR")
	require.NotEmpty(t, sub, "GOEXCHANGE_QUANTUM_OUTDIR을 지정해야 한다")
	return sub
}

// runPair는 대조군과 후보를 한 쌍으로 측정한다. 순서를 번갈아 바꿔
// 실행 순서 효과가 한쪽에만 쌓이지 않게 한다.
func runPair(t *testing.T, seed int64, cfg QuantumConfig, batchSize int, controlFirst bool) (control, candidate batchResult) {
	t.Helper()
	if controlFirst {
		control = runSweepBatch(t, "control", seed, controlConfig(), batchSize)
		candidate = runSweepBatch(t, "candidate", seed, cfg, batchSize)
		return
	}
	candidate = runSweepBatch(t, "candidate", seed, cfg, batchSize)
	control = runSweepBatch(t, "control", seed, controlConfig(), batchSize)
	return
}

// requireValidPair는 쌍이 판정에 쓸 수 있는지 확인한다.
// 대조군은 경계를 만나지 않아야 하므로 yield가 0이어야 한다.
func requireValidPair(t *testing.T, control, candidate batchResult) {
	t.Helper()
	require.False(t, control.MeasurementInvalid, "control measurement_invalid: %s", control.InvalidReason)
	require.False(t, candidate.MeasurementInvalid, "candidate measurement_invalid: %s", candidate.InvalidReason)
	require.False(t, control.Censored, "control censored")
	require.False(t, candidate.Censored, "candidate censored")
	require.Zero(t, control.QuantumYields,
		"대조군이 quantum 경계를 만났다 — 대조군 측정 실패 (yields=%d)", control.QuantumYields)
}

func TestPrecisionTrial(t *testing.T) {
	batchSize := batchSizeFromEnv(t)
	outdir := outdirFromEnv(t)

	records := make([]batchResult, 0, precisionPairs*2)
	ratios := make([]float64, 0, precisionPairs)

	for pair := 1; pair <= precisionPairs; pair++ {
		seed := harnessSeedBase + int64(pair)*1000
		controlFirst := pair%2 == 1 // 홀수 쌍: 대조군 → 후보
		control, candidate := runPair(t, seed, precisionCandidate, batchSize, controlFirst)
		requireValidPair(t, control, candidate)

		ratio := float64(candidate.SweepBatchMeanNs) / float64(control.SweepBatchMeanNs)
		ratios = append(ratios, ratio)
		records = append(records, control, candidate)

		t.Logf("pair%d control_first=%v control_mean=%v candidate_mean=%v ratio=%.4f yields(candidate)=%d",
			pair, controlFirst,
			durationOf(control.SweepBatchMeanNs), durationOf(candidate.SweepBatchMeanNs),
			ratio, candidate.QuantumYields)
	}

	writeBatch(t, outdir, "precision-m16-c8", records)

	med := medianFloat(ratios)
	maxDev := 0.0
	for _, r := range ratios {
		if d := absFloat(r-med) / med; d > maxDev {
			maxDev = d
		}
	}
	summary := fmt.Sprintf("[precision batch=%d] ratios=%v median=%.4f max_deviation=%.2f%% (기준 2.5%%)",
		batchSize, formatRatios(ratios), med, maxDev*100)
	t.Log(summary)
	fmt.Println(summary)

	require.LessOrEqual(t, maxDev, 0.025,
		"정밀도 FAIL: 최대 편차 %.2f%%가 2.5%%를 넘는다. batch=%d에서는 C3의 5%% 차이를 구분할 수 없다",
		maxDev*100, batchSize)
}

func medianFloat(v []float64) float64 {
	s := append([]float64{}, v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func formatRatios(v []float64) string {
	out := "["
	for i, r := range v {
		if i > 0 {
			out += " "
		}
		out += fmt.Sprintf("%.4f", r)
	}
	return out + "]"
}
