package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// main()에서 SetObservers 호출이 지워지면 계측이 조용히 사라진다.
// 런타임 테스트로는 잡히지 않으므로 소스 계약으로 고정한다.
func TestMainWiresMatchingEngineObservers(t *testing.T) {
	source, err := os.ReadFile("main.go")
	require.NoError(t, err)
	require.True(t,
		strings.Contains(string(source), "me.SetObservers(metrics.NewMatchingEngineObservers())"),
		"main.go가 매칭 엔진 옵저버를 배선해야 한다")
}

// 잘못된 quantum 설정으로 뜬 서버가 부하를 받는 것보다 안 뜨는 편이 낫다.
// 기동 실패 경로와 적용값 로그는 런타임 테스트로 잡히지 않으므로 소스
// 계약으로 고정한다 — GCP preflight가 그 로그로 적용값을 확인한다.
func TestMainFailsFastOnInvalidQuantumConfig(t *testing.T) {
	source, err := os.ReadFile("main.go")
	require.NoError(t, err)
	text := string(source)
	require.Contains(t, text, "config.MatchingQuantumFromEnv()")
	require.Contains(t, text, "matching.NewShardedEngineWithQuantum(")
	require.Contains(t, text, `log.Fatal("matching quantum config invalid: "`)
	require.Contains(t, text, "maxMatchesPerTurn=%d maxConsecutiveCancels=%d")
}
