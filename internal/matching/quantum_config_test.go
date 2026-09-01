package matching

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestQuantumConfigValidate(t *testing.T) {
	require.NoError(t, QuantumConfig{MaxMatchesPerTurn: 1, MaxConsecutiveCancels: 1}.Validate())
	require.Error(t, QuantumConfig{MaxMatchesPerTurn: 0, MaxConsecutiveCancels: 1}.Validate())
	require.Error(t, QuantumConfig{MaxMatchesPerTurn: 1, MaxConsecutiveCancels: 0}.Validate())
	require.Error(t, QuantumConfig{MaxMatchesPerTurn: -1, MaxConsecutiveCancels: 1}.Validate())
}

func TestNewShardedEngineWithQuantumInjectsEveryShard(t *testing.T) {
	se, err := NewShardedEngineWithQuantum(4, QuantumConfig{MaxMatchesPerTurn: 17, MaxConsecutiveCancels: 5})
	require.NoError(t, err)
	require.Len(t, se.shards, 4)
	for i, shard := range se.shards {
		require.Equal(t, 17, shard.maxMatchesPerTurn, "shard %d", i)
		require.Equal(t, 5, shard.maxConsecutiveCancels, "shard %d", i)
	}

	_, err = NewShardedEngineWithQuantum(2, QuantumConfig{MaxMatchesPerTurn: 0, MaxConsecutiveCancels: 4})
	require.Error(t, err, "검증되지 않은 설정으로 엔진을 만들면 안 된다")
}

// 무인자 생성자는 테스트용 기본값을 유지한다. 약 30곳의 기존 호출부가
// 이것에 의존한다.
func TestNewMatchingEngineKeepsDevelopmentDefaults(t *testing.T) {
	me := NewMatchingEngine()
	require.Equal(t, defaultMaxMatchesPerTurn, me.maxMatchesPerTurn)
	require.Equal(t, defaultMaxConsecutiveCancels, me.maxConsecutiveCancels)
	require.GreaterOrEqual(t, me.maxMatchesPerTurn, 1, "0은 무제한 sentinel과 충돌한다")
	require.GreaterOrEqual(t, me.maxConsecutiveCancels, 1)
}
