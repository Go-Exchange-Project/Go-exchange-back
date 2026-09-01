package matching

import "fmt"

// 로컬 탐색으로 확정할 때까지의 임시 개발값이다. production 값은
// _workspace/quantum/selection.md의 근거와 함께 확정한다.
const (
	defaultMaxMatchesPerTurn     = 64
	defaultMaxConsecutiveCancels = 32
)

// QuantumConfig는 엔진 스케줄러의 두 상한이다. 0은 matchSlice의 무제한
// sentinel과 충돌하므로 반드시 1 이상이어야 한다.
type QuantumConfig struct {
	MaxMatchesPerTurn     int
	MaxConsecutiveCancels int
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
