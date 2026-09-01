package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStrictPositiveEnvContract(t *testing.T) {
	const key = "GOEXCHANGE_TEST_STRICT_INT"

	t.Run("unset uses default", func(t *testing.T) {
		require.NoError(t, os.Unsetenv(key))
		got, err := strictPositiveEnv(key, 7)
		require.NoError(t, err)
		require.Equal(t, 7, got, "미설정은 기본값을 쓰겠다는 명시적 선택이다")
	})

	for _, tc := range []struct{ name, value string }{
		{"empty string", ""},
		{"leading space", " 3"},
		{"trailing space", "3 "},
		{"plus sign", "+3"},
		{"leading zero", "03"},
		{"zero", "0"},
		{"negative", "-1"},
		{"not a number", "abc"},
		{"overflow", "99999999999999999999"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(key, tc.value)
			_, err := strictPositiveEnv(key, 7)
			require.Error(t, err, "%q는 에러여야 한다 (기본값 fallback 금지)", tc.value)
		})
	}

	t.Run("valid", func(t *testing.T) {
		t.Setenv(key, "3")
		got, err := strictPositiveEnv(key, 7)
		require.NoError(t, err)
		require.Equal(t, 3, got)
	})
}

func TestMatchingQuantumFromEnv(t *testing.T) {
	t.Run("both unset", func(t *testing.T) {
		require.NoError(t, os.Unsetenv(EnvGOExchangeMatchingMaxMatchesPerTurn))
		require.NoError(t, os.Unsetenv(EnvGOExchangeMatchingMaxConsecutiveCancels))
		matches, cancels, err := MatchingQuantumFromEnv()
		require.NoError(t, err)
		require.Equal(t, defaultMatchingMaxMatchesPerTurn, matches)
		require.Equal(t, defaultMatchingMaxConsecutiveCancels, cancels)
	})

	t.Run("override applies", func(t *testing.T) {
		t.Setenv(EnvGOExchangeMatchingMaxMatchesPerTurn, "16")
		t.Setenv(EnvGOExchangeMatchingMaxConsecutiveCancels, "8")
		matches, cancels, err := MatchingQuantumFromEnv()
		require.NoError(t, err)
		require.Equal(t, 16, matches)
		require.Equal(t, 8, cancels)
	})

	t.Run("one invalid fails", func(t *testing.T) {
		t.Setenv(EnvGOExchangeMatchingMaxMatchesPerTurn, "0")
		_, _, err := MatchingQuantumFromEnv()
		require.Error(t, err)
	})
}

// 기존 파서의 동작을 바꾸지 않았는지 고정한다. 5곳이 이 조용한 fallback에
// 의존하고 있고, 그쪽은 의도된 설계다.
func TestParsePositiveIntEnvStillFallsBackSilently(t *testing.T) {
	const key = "GOEXCHANGE_TEST_LEGACY_INT"
	t.Setenv(key, "0")
	require.Equal(t, 9, parsePositiveIntEnv(key, 9), "기존 파서는 조용한 fallback을 유지한다")
	t.Setenv(key, " 3")
	require.Equal(t, 3, parsePositiveIntEnv(key, 9), "기존 파서는 TrimSpace를 유지한다")
}
