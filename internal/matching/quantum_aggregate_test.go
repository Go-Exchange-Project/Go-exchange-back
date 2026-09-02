package matching

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fullRunSet은 다섯 시나리오 × want회를 채운 최소 run-set이다.
func fullRunSet(want int, cfg QuantumConfig) []RunFile {
	var out []RunFile
	seed := int64(1)
	for _, s := range requiredScenarios {
		for i := 0; i < want; i++ {
			out = append(out, RunFile{
				Scenario: s, Seed: seed,
				MaxMatchesPerTurn:     cfg.MaxMatchesPerTurn,
				MaxConsecutiveCancels: cfg.MaxConsecutiveCancels,
			})
			seed++
		}
	}
	return out
}

func setScenario(runs []RunFile, scenario string, set func(*RunFile, int)) {
	i := 0
	for idx := range runs {
		if runs[idx].Scenario == scenario {
			set(&runs[idx], i)
			i++
		}
	}
}

func TestValidateRunSetCatchesStructuralProblems(t *testing.T) {
	cfg := QuantumConfig{MaxMatchesPerTurn: 32, MaxConsecutiveCancels: 8}
	require.NoError(t, ValidateRunSet(fullRunSet(3, cfg), 3, cfg, true))

	t.Run("회차 부족", func(t *testing.T) {
		require.Error(t, ValidateRunSet(fullRunSet(3, cfg)[:14], 3, cfg, true))
	})

	// H4가 빠지면 max gap이 0이 되어 C4를 조용히 통과한다.
	t.Run("H4 누락", func(t *testing.T) {
		var noH4 []RunFile
		for _, r := range fullRunSet(3, cfg) {
			if r.Scenario != "H4" {
				noH4 = append(noH4, r)
			}
		}
		require.ErrorContains(t, ValidateRunSet(noH4, 3, cfg, true), "H4")
	})

	t.Run("중복 seed", func(t *testing.T) {
		dup := fullRunSet(3, cfg)
		dup[1].Seed = dup[0].Seed
		require.ErrorContains(t, ValidateRunSet(dup, 3, cfg, true), "duplicate seed")
	})

	// 디렉터리가 말하는 후보와 JSON의 설정이 다르면 짝이 틀린 것이다.
	t.Run("설정 불일치", func(t *testing.T) {
		mismatch := fullRunSet(3, cfg)
		mismatch[5].MaxMatchesPerTurn = 999
		require.ErrorContains(t, ValidateRunSet(mismatch, 3, cfg, true), "want m=32")
	})

	t.Run("baseline은 설정 검사를 건너뛴다", func(t *testing.T) {
		require.NoError(t, ValidateRunSet(fullRunSet(3, QuantumConfig{}), 3, QuantumConfig{}, false))
	})
}

func TestAggregateBaselineTakesMedian(t *testing.T) {
	runs := fullRunSet(3, QuantumConfig{})
	values := [][]int64{{5, 1, 3}, {30, 10, 20}, {300, 100, 200}}
	setScenario(runs, "H0", func(r *RunFile, i int) { r.OrderWaitP99Ns = values[0][i] })
	setScenario(runs, "H2-1", func(r *RunFile, i int) { r.CancelWaitP99Ns = values[1][i] })
	setScenario(runs, "H2-5000", func(r *RunFile, i int) { r.SweepTotalNs = values[2][i] })

	base, err := AggregateBaseline(runs, 3)
	require.NoError(t, err)
	require.Equal(t, time.Duration(3), base.H0OrderP99)
	require.Equal(t, time.Duration(20), base.H2SmallCancelP99)
	require.Equal(t, time.Duration(200), base.H2LargeSweepTotal)
}

// baseline 분위수가 +Inf면 상한을 유도할 수 없다.
func TestAggregateBaselineRejectsInfiniteMedian(t *testing.T) {
	runs := fullRunSet(3, QuantumConfig{})
	setScenario(runs, "H0", func(r *RunFile, _ int) {
		r.OrderWaitP99Ns = InfNs
		r.OrderCensored = 1
	})
	_, err := AggregateBaseline(runs, 3)
	require.ErrorContains(t, err, "H0")
}

// run-set 검증이 censored 조기 반환보다 먼저다. censored를 이유로 일찍
// 빠져나가면 run-set이 깨진 것을 영영 못 본다.
func TestAggregateCandidateValidatesBeforeCensoredShortcut(t *testing.T) {
	cfg := QuantumConfig{MaxMatchesPerTurn: 32, MaxConsecutiveCancels: 8}
	runs := fullRunSet(3, cfg)
	runs[0].OrderCensored = 1
	runs[1].MaxMatchesPerTurn = 9
	_, err := AggregateCandidate(cfg, runs, 3, true)
	require.ErrorContains(t, err, "run-set invalid")
}

func TestAggregateCandidateSumsCensoredAndTakesMaxGap(t *testing.T) {
	cfg := QuantumConfig{MaxMatchesPerTurn: 32, MaxConsecutiveCancels: 8}
	runs := fullRunSet(3, cfg)
	setScenario(runs, "H1", func(r *RunFile, i int) {
		r.OrderWaitP99Ns = 20
		r.OrderCensored = i // 0+1+2 = 3
	})
	setScenario(runs, "H4", func(r *RunFile, i int) { r.MaxSnapshotGapNs = int64(50 + 20*i) })

	got, err := AggregateCandidate(cfg, runs, 3, true)
	require.NoError(t, err)
	require.Equal(t, 3, got.Censored, "censored는 회차 합계")
	require.Equal(t, time.Duration(90), got.MaxSnapshotGap, "snapshot gap은 H4의 최댓값")
}

func TestAggregateCandidateComputesMedians(t *testing.T) {
	cfg := QuantumConfig{MaxMatchesPerTurn: 32, MaxConsecutiveCancels: 8}
	runs := fullRunSet(3, cfg)
	setScenario(runs, "H0", func(r *RunFile, i int) { r.OrderWaitP99Ns = []int64{1, 2, 3}[i] })
	setScenario(runs, "H1", func(r *RunFile, i int) { r.OrderWaitP99Ns = []int64{40, 20, 30}[i] })
	setScenario(runs, "H2-5000", func(r *RunFile, i int) {
		r.CancelWaitP99Ns = []int64{7, 5, 6}[i]
		r.SweepTotalNs = []int64{700, 500, 600}[i]
	})

	got, err := AggregateCandidate(cfg, runs, 3, true)
	require.NoError(t, err)
	require.Equal(t, time.Duration(2), got.H0OrderP99)
	require.Equal(t, time.Duration(30), got.H1OrderP99)
	require.Equal(t, time.Duration(6), got.H2LargeCancelP99)
	require.Equal(t, time.Duration(600), got.H2LargeSweepTotal)
	require.InDelta(t, 15.0, got.StarvationRatio(), 1e-9, "H1/H0 report-only 비율")
}

func TestExceedingRunsReportsPerRunOverruns(t *testing.T) {
	runs := fullRunSet(3, QuantumConfig{})
	setScenario(runs, "H1", func(r *RunFile, i int) { r.OrderWaitP99Ns = []int64{100, 500, 100}[i] })
	over := ExceedingRuns(runs, "H1", func(r RunFile) int64 { return r.OrderWaitP99Ns }, 200)
	require.Equal(t, []int{1}, over)

	// censored 회차도 초과로 센다.
	setScenario(runs, "H1", func(r *RunFile, i int) { r.OrderWaitP99Ns = []int64{100, InfNs, 100}[i] })
	require.Equal(t, []int{1}, ExceedingRuns(runs, "H1", func(r RunFile) int64 { return r.OrderWaitP99Ns }, 200))
}
