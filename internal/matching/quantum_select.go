package matching

import (
	"errors"
	"sort"
	"time"
)

// snapshotFreshnessLimit는 C4의 상한이자 C1·C2 상한의 바닥이다.
// 이 파이프라인이 이미 감수하는 최소 관측 지연 단위가 스냅샷 코얼레싱
// 주기(100ms)이므로, 그보다 촘촘한 요구는 시스템의 다른 부분과 정합하지 않는다.
const snapshotFreshnessLimit = 300 * time.Millisecond

// throughputAllowance는 C3의 허용 배수다.
const throughputAllowance = 1.05

// BaselineStats는 보존된 baseline JSON에서 유도한 기준값이다.
// 탐색 단계는 이 값을 다시 측정하지 않는다.
type BaselineStats struct {
	H2SmallCancelP99  time.Duration // H2-1 cancel p99 중앙값
	H0OrderP99        time.Duration // H0 order p99 중앙값
	H2LargeSweepTotal time.Duration // H2-5000 sweep 총 시간 중앙값
}

type CandidateResult struct {
	Config            QuantumConfig
	H2LargeCancelP99  time.Duration
	H1OrderP99        time.Duration
	H0OrderP99        time.Duration
	H2LargeSweepTotal time.Duration
	MaxSnapshotGap    time.Duration
	Censored          int
	SemanticTestsPass bool

	// H1OrderP99Raw는 회차별 초과 보고(ExceedingRuns)에 쓰는 원자료다.
	// 판정에는 쓰지 않는다.
	H1OrderP99Raw []RunFile
}

// StarvationRatio는 H1/H0 주문 대기 비율이다. 사전 등록된 게이트가 아니라
// **report-only** 지표다 — C1·C2의 300ms 바닥이 현재 워크로드에서 비구속적이라
// 개선을 게이트로는 보여줄 수 없기 때문이다. 게이트를 새로 만들지 않는다.
func (c CandidateResult) StarvationRatio() float64 {
	if c.H0OrderP99 <= 0 {
		return 0
	}
	return float64(c.H1OrderP99) / float64(c.H0OrderP99)
}

type QuantumChoice struct {
	Config QuantumConfig
	Result CandidateResult
}

// ErrNoCandidatePassed는 통과 조합이 0개일 때 반환된다. 이 경우 임계값을
// 완화하거나 격자를 넓히지 않는다 — quantum만으로 해결되지 않는다는
// 증거이므로 중단한다.
var ErrNoCandidatePassed = errors.New("no candidate passed the pre-registered gates")

func upperBound(controlP99 time.Duration) time.Duration {
	if bound := controlP99 * 3; bound > snapshotFreshnessLimit {
		return bound
	}
	return snapshotFreshnessLimit
}

// FailedGate는 후보가 처음 위반한 게이트 이름을 돌려준다. 통과하면 빈 문자열.
func (c CandidateResult) FailedGate(base BaselineStats) string {
	switch {
	case c.Censored > 0:
		return "C5(censored)"
	case !c.SemanticTestsPass:
		return "C6(semantic)"
	case c.H2LargeCancelP99 > upperBound(base.H2SmallCancelP99):
		return "C1(sweep-cancel-latency)"
	case c.H1OrderP99 > upperBound(base.H0OrderP99):
		return "C2(flood-order-latency)"
	case float64(c.H2LargeSweepTotal) > float64(base.H2LargeSweepTotal)*throughputAllowance:
		return "C3(throughput)"
	case c.MaxSnapshotGap > snapshotFreshnessLimit:
		return "C4(snapshot-freshness)"
	}
	return ""
}

// RankCandidates는 통과 후보를 사전 등록된 선택 규칙 순으로 정렬해 돌려준다.
//
// 규칙: (1) MaxMatchesPerTurn 최대 → (2) 동률이면 MaxConsecutiveCancels
// 최소 → (3) 그래도 동률이면 입력 순서상 먼저.
//
// 1차 탐색에서 상위 2개를 고르는 데도 이 함수를 쓴다 — 상위 선정과 최종
// 선택이 같은 규칙이어야 탐색이 결과를 편향시키지 않는다.
func RankCandidates(candidates []CandidateResult, base BaselineStats) []CandidateResult {
	order := make(map[QuantumConfig]int, len(candidates))
	passed := make([]CandidateResult, 0, len(candidates))
	for i, c := range candidates {
		if _, seen := order[c.Config]; !seen {
			order[c.Config] = i
		}
		if c.FailedGate(base) == "" {
			passed = append(passed, c)
		}
	}
	sort.SliceStable(passed, func(i, j int) bool {
		a, b := passed[i].Config, passed[j].Config
		if a.MaxMatchesPerTurn != b.MaxMatchesPerTurn {
			return a.MaxMatchesPerTurn > b.MaxMatchesPerTurn
		}
		if a.MaxConsecutiveCancels != b.MaxConsecutiveCancels {
			return a.MaxConsecutiveCancels < b.MaxConsecutiveCancels
		}
		return order[a] < order[b]
	})
	return passed
}

// SelectQuantum은 통과 후보 중 1등을 고른다. 부작용이 없다 — 측정 없이
// 판정 로직을 검증할 수 있어야 튜닝 단계에서 규칙을 슬쩍 바꾸는 일이 없다.
func SelectQuantum(candidates []CandidateResult, base BaselineStats) (QuantumChoice, error) {
	ranked := RankCandidates(candidates, base)
	if len(ranked) == 0 {
		return QuantumChoice{}, ErrNoCandidatePassed
	}
	return QuantumChoice{Config: ranked[0].Config, Result: ranked[0]}, nil
}
