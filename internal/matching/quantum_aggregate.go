package matching

import (
	"fmt"
	"sort"
	"time"
)

// InfNs는 censored 때문에 분위수가 정의되지 않음을 뜻한다.
// JSON에 Infinity를 쓸 수 없어 -1로 표현한다.
const InfNs int64 = -1

// RunFile은 하니스가 쓴 JSON 배열의 원소 하나다. 필드명이 하니스의
// scenarioResult와 어긋나면 0으로 읽혀 조용히 통과하므로, ValidateRunSet이
// 설정값 일치를 확인해 그 경우를 잡는다.
type RunFile struct {
	Scenario              string `json:"scenario"`
	Seed                  int64  `json:"seed"`
	MaxMatchesPerTurn     int    `json:"max_matches_per_turn"`
	MaxConsecutiveCancels int    `json:"max_consecutive_cancels"`
	OrderWaitP99Ns        int64  `json:"order_wait_p99_ns"`
	OrderCensored         int    `json:"order_censored"`
	CancelWaitP99Ns       int64  `json:"cancel_wait_p99_ns"`
	CancelCensored        int    `json:"cancel_censored"`
	SweepTotalNs          int64  `json:"sweep_total_ns"`
	SweepCensored         int    `json:"sweep_censored"`
	MaxSnapshotGapNs      int64  `json:"max_snapshot_gap_ns"`
}

// requiredScenarios는 판정에 반드시 있어야 하는 다섯이다.
// H4가 빠지면 max gap이 0이 되어 C4를 조용히 통과하므로 에러여야 한다.
var requiredScenarios = []string{"H0", "H1", "H2-1", "H2-5000", "H4"}

// ValidateRunSet은 집계 전에 정확히 한 번 부른다. censored 조기 반환보다
// **먼저** 수행해야 한다 — censored를 이유로 일찍 빠져나가면 run-set이
// 깨진 것을 영영 못 본다.
//
// checkConfig가 false면 설정값 일치 검사를 건너뛴다(baseline은 기본값으로 돈다).
func ValidateRunSet(runs []RunFile, want int, cfg QuantumConfig, checkConfig bool) error {
	for _, scenario := range requiredScenarios {
		sel := pickRuns(runs, scenario)
		if len(sel) != want {
			return fmt.Errorf("%s: %d runs, want %d", scenario, len(sel), want)
		}
		seen := make(map[int64]bool, len(sel))
		for _, r := range sel {
			if seen[r.Seed] {
				return fmt.Errorf("%s: duplicate seed %d", scenario, r.Seed)
			}
			seen[r.Seed] = true
			if !checkConfig {
				continue
			}
			if r.MaxMatchesPerTurn != cfg.MaxMatchesPerTurn ||
				r.MaxConsecutiveCancels != cfg.MaxConsecutiveCancels {
				return fmt.Errorf("%s seed %d: config m=%d c=%d, want m=%d c=%d",
					scenario, r.Seed, r.MaxMatchesPerTurn, r.MaxConsecutiveCancels,
					cfg.MaxMatchesPerTurn, cfg.MaxConsecutiveCancels)
			}
		}
	}
	return nil
}

func pickRuns(runs []RunFile, scenario string) []RunFile {
	var out []RunFile
	for _, r := range runs {
		if r.Scenario == scenario {
			out = append(out, r)
		}
	}
	return out
}

// medianNs는 회차 값의 중앙값이다. InfNs가 하나라도 있으면 에러다 —
// 분위수가 정의되지 않은 회차를 섞어 중앙값을 내면 그 숫자는 거짓이다.
func medianNs(runs []RunFile, scenario string, get func(RunFile) int64) (time.Duration, error) {
	sel := pickRuns(runs, scenario)
	values := make([]int64, 0, len(sel))
	for i, r := range sel {
		v := get(r)
		if v == InfNs {
			return 0, fmt.Errorf("%s run %d: censored (quantile undefined)", scenario, i)
		}
		values = append(values, v)
	}
	if len(values) == 0 {
		return 0, fmt.Errorf("%s: no runs", scenario)
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return time.Duration(values[len(values)/2]), nil
}

func maxNs(runs []RunFile, scenario string, get func(RunFile) int64) time.Duration {
	var out int64
	for _, r := range pickRuns(runs, scenario) {
		if v := get(r); v > out {
			out = v
		}
	}
	return time.Duration(out)
}

func sumCensored(runs []RunFile) int {
	total := 0
	for _, r := range runs {
		total += r.OrderCensored + r.CancelCensored + r.SweepCensored
	}
	return total
}

// AggregateBaseline은 보존된 baseline JSON에서 C1·C2·C3의 기준값을 유도한다.
func AggregateBaseline(runs []RunFile, want int) (BaselineStats, error) {
	if err := ValidateRunSet(runs, want, QuantumConfig{}, false); err != nil {
		return BaselineStats{}, fmt.Errorf("baseline run-set invalid: %w", err)
	}
	h0, err := medianNs(runs, "H0", func(r RunFile) int64 { return r.OrderWaitP99Ns })
	if err != nil {
		return BaselineStats{}, err
	}
	small, err := medianNs(runs, "H2-1", func(r RunFile) int64 { return r.CancelWaitP99Ns })
	if err != nil {
		return BaselineStats{}, err
	}
	large, err := medianNs(runs, "H2-5000", func(r RunFile) int64 { return r.SweepTotalNs })
	if err != nil {
		return BaselineStats{}, err
	}
	return BaselineStats{H0OrderP99: h0, H2SmallCancelP99: small, H2LargeSweepTotal: large}, nil
}

// AggregateCandidate는 후보 조합 하나의 JSON을 판정 입력으로 바꾼다.
func AggregateCandidate(cfg QuantumConfig, runs []RunFile, want int, semanticPass bool) (CandidateResult, error) {
	if err := ValidateRunSet(runs, want, cfg, true); err != nil {
		return CandidateResult{}, fmt.Errorf("candidate run-set invalid: %w", err)
	}
	out := CandidateResult{
		Config:            cfg,
		Censored:          sumCensored(runs),
		MaxSnapshotGap:    maxNs(runs, "H4", func(r RunFile) int64 { return r.MaxSnapshotGapNs }),
		SemanticTestsPass: semanticPass,
		H1OrderP99Raw:     runs,
	}
	if out.Censored > 0 {
		// C5에서 어차피 탈락한다. 분위수는 정의되지 않을 수 있으므로 건너뛴다.
		return out, nil
	}
	var err error
	if out.H1OrderP99, err = medianNs(runs, "H1", func(r RunFile) int64 { return r.OrderWaitP99Ns }); err != nil {
		return CandidateResult{}, err
	}
	if out.H0OrderP99, err = medianNs(runs, "H0", func(r RunFile) int64 { return r.OrderWaitP99Ns }); err != nil {
		return CandidateResult{}, err
	}
	if out.H2LargeCancelP99, err = medianNs(runs, "H2-5000", func(r RunFile) int64 { return r.CancelWaitP99Ns }); err != nil {
		return CandidateResult{}, err
	}
	if out.H2LargeSweepTotal, err = medianNs(runs, "H2-5000", func(r RunFile) int64 { return r.SweepTotalNs }); err != nil {
		return CandidateResult{}, err
	}
	return out, nil
}

// ExceedingRuns는 상한을 넘은 회차의 인덱스를 돌려준다. 중앙값이 통과해도
// 개별 초과는 보고서의 "한계" 절에 그대로 적는다.
func ExceedingRuns(runs []RunFile, scenario string, get func(RunFile) int64, limitNs int64) []int {
	var out []int
	for i, r := range pickRuns(runs, scenario) {
		if v := get(r); v == InfNs || v > limitNs {
			out = append(out, i)
		}
	}
	return out
}
