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
