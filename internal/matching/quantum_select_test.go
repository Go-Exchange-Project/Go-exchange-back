package matching

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testBaseline() BaselineStats {
	return BaselineStats{
		H2SmallCancelP99:  10 * time.Millisecond,
		H0OrderP99:        10 * time.Millisecond,
		H2LargeSweepTotal: 1000 * time.Millisecond,
	}
}

func passingCandidate(matches, cancels int) CandidateResult {
	return CandidateResult{
		Config:            QuantumConfig{MaxMatchesPerTurn: matches, MaxConsecutiveCancels: cancels},
		H2LargeCancelP99:  20 * time.Millisecond,
		H1OrderP99:        20 * time.Millisecond,
		H0OrderP99:        10 * time.Millisecond,
		H2LargeSweepTotal: 1020 * time.Millisecond,
		MaxSnapshotGap:    100 * time.Millisecond,
		SemanticTestsPass: true,
	}
}

// 규칙 1(matches 최대) → 2(cancels 최소) → 3(입력 순서).
func TestSelectQuantumAppliesRankingRules(t *testing.T) {
	got, err := SelectQuantum([]CandidateResult{
		passingCandidate(16, 8), passingCandidate(128, 32),
		passingCandidate(128, 8), passingCandidate(64, 8),
	}, testBaseline())
	require.NoError(t, err)
	require.Equal(t, QuantumConfig{MaxMatchesPerTurn: 128, MaxConsecutiveCancels: 8}, got.Config)

	// 규칙 3: 두 규칙으로도 동률이면 먼저 온 것. 결정론적이어야 한다.
	first := passingCandidate(64, 16)
	first.H1OrderP99 = 19 * time.Millisecond
	ranked := RankCandidates([]CandidateResult{first, passingCandidate(64, 16)}, testBaseline())
	require.Len(t, ranked, 2)
	require.Equal(t, 19*time.Millisecond, ranked[0].H1OrderP99, "입력 순서상 먼저 온 것")

	// 상한 바닥: control p99가 마이크로초여도 300ms 안이면 통과한다.
	tiny := BaselineStats{
		H2SmallCancelP99: time.Microsecond, H0OrderP99: time.Microsecond,
		H2LargeSweepTotal: time.Second,
	}
	c := passingCandidate(64, 8)
	c.H2LargeCancelP99, c.H1OrderP99 = 250*time.Millisecond, 250*time.Millisecond
	_, err = SelectQuantum([]CandidateResult{c}, tiny)
	require.NoError(t, err, "300ms 바닥 안이면 통과해야 한다")
}

func TestSelectQuantumRejectsCensoredAndSemanticFailure(t *testing.T) {
	censored := passingCandidate(128, 8)
	censored.Censored = 1
	semantic := passingCandidate(128, 32)
	semantic.SemanticTestsPass = false

	require.Equal(t, "C5(censored)", censored.FailedGate(testBaseline()))
	require.Equal(t, "C6(semantic)", semantic.FailedGate(testBaseline()))

	got, err := SelectQuantum([]CandidateResult{censored, semantic, passingCandidate(16, 8)}, testBaseline())
	require.NoError(t, err)
	require.Equal(t, 16, got.Config.MaxMatchesPerTurn, "censored·semantic 실패는 탈락")
}

func TestSelectQuantumErrorsWhenNonePass(t *testing.T) {
	slow := passingCandidate(64, 8)
	slow.H2LargeCancelP99 = 10 * time.Second
	require.Equal(t, "C1(sweep-cancel-latency)", slow.FailedGate(testBaseline()))

	_, err := SelectQuantum([]CandidateResult{slow}, testBaseline())
	require.ErrorIs(t, err, ErrNoCandidatePassed,
		"통과 조합이 0개면 에러다 — 임계값을 완화하지 않는다")
}
