package matching

import "fmt"

// 사전 등록된 선택 규칙으로 확정한 값이다(설계 §9.4).
//
//	1. maxMatchesPerTurn이 큰 값 우선  → 격자 최대 128
//	2. 같으면 maxConsecutiveCancels가 작은 값 우선 → 격자 최소 8
//
// **"가장 빠른 값"이라는 뜻이 아니다.** 격자 중 추가 yield가 가장 적고
// (5,000 체결당 39회) progress 전 연속 취소 상한이 가장 작다는 뜻이다.
// 로컬 wall-clock 처리량은 판정하지 못했다 — 설계 §8.4 참고.
const (
	defaultMaxMatchesPerTurn     = 128
	defaultMaxConsecutiveCancels = 8
)

// QuantumConfig는 엔진 스케줄러의 두 상한이다. 0은 matchSlice의 무제한
// sentinel과 충돌하므로 반드시 1 이상이어야 한다.
type QuantumConfig struct {
	MaxMatchesPerTurn     int
	MaxConsecutiveCancels int
}

// ExpectedSlices는 trades건을 maxMatchesPerTurn 단위로 나눌 때의 조각 수다.
// maxMatchesPerTurn <= 0은 무제한 sentinel이므로 항상 1조각이다.
//
// 이 값은 **처리량 손실률이 아니다.** sweep 하나를 몇 조각으로 나눴는지,
// 그래서 스케줄러 제어점으로 몇 번 더 돌아왔는지만 뜻한다.
// 실제 처리량은 이 수로 증명되지 않는다.
func ExpectedSlices(trades, maxMatchesPerTurn int) int {
	if maxMatchesPerTurn <= 0 || trades <= maxMatchesPerTurn {
		return 1
	}
	slices := trades / maxMatchesPerTurn
	if trades%maxMatchesPerTurn != 0 {
		slices++
	}
	return slices
}

// ExpectedYields는 예산 소진으로 양보한 횟수다. 마지막 조각은 완료로
// 끝나므로 조각 수보다 하나 적다.
func ExpectedYields(trades, maxMatchesPerTurn int) int {
	return ExpectedSlices(trades, maxMatchesPerTurn) - 1
}

func (c QuantumConfig) Validate() error {
	if c.MaxMatchesPerTurn < 1 {
		return fmt.Errorf("maxMatchesPerTurn must be >= 1, got %d", c.MaxMatchesPerTurn)
	}
	if c.MaxConsecutiveCancels < 1 {
		return fmt.Errorf("maxConsecutiveCancels must be >= 1, got %d", c.MaxConsecutiveCancels)
	}
	return nil
}
