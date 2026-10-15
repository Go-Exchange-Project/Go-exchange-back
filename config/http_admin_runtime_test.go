package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// C1(Task 6): 서비스 HTTP 서버 4종 상한 + MaxHeaderBytes, 관리 서버 주소·WriteTimeout.
// 0·음수·공백·비정수는 에러다(strictPositiveDurationEnv/strictPositiveEnv와 같은 계약).

func TestHTTPReadHeaderTimeoutFromEnv(t *testing.T) {
	t.Run("unset uses design default 5s", func(t *testing.T) {
		requireUnsetEnv(t, EnvHTTPReadHeaderTimeout)
		got, err := HTTPReadHeaderTimeoutFromEnv()
		require.NoError(t, err)
		require.Equal(t, 5*time.Second, got)
	})
	t.Run("valid override applies", func(t *testing.T) {
		t.Setenv(EnvHTTPReadHeaderTimeout, "3s")
		got, err := HTTPReadHeaderTimeoutFromEnv()
		require.NoError(t, err)
		require.Equal(t, 3*time.Second, got)
	})
	for _, tc := range []struct{ name, value string }{
		{"empty string", ""}, {"zero", "0s"}, {"negative", "-1s"}, {"not a duration", "abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvHTTPReadHeaderTimeout, tc.value)
			_, err := HTTPReadHeaderTimeoutFromEnv()
			require.Error(t, err)
		})
	}
}

func TestHTTPReadTimeoutFromEnv(t *testing.T) {
	requireUnsetEnv(t, EnvHTTPReadTimeout)
	got, err := HTTPReadTimeoutFromEnv()
	require.NoError(t, err)
	require.Equal(t, 15*time.Second, got)

	t.Setenv(EnvHTTPReadTimeout, "0s")
	_, err = HTTPReadTimeoutFromEnv()
	require.Error(t, err)
}

func TestHTTPWriteTimeoutFromEnv(t *testing.T) {
	requireUnsetEnv(t, EnvHTTPWriteTimeout)
	got, err := HTTPWriteTimeoutFromEnv()
	require.NoError(t, err)
	require.Equal(t, 20*time.Second, got)

	t.Setenv(EnvHTTPWriteTimeout, "not-a-duration")
	_, err = HTTPWriteTimeoutFromEnv()
	require.Error(t, err)
}

func TestHTTPIdleTimeoutFromEnv(t *testing.T) {
	requireUnsetEnv(t, EnvHTTPIdleTimeout)
	got, err := HTTPIdleTimeoutFromEnv()
	require.NoError(t, err)
	require.Equal(t, 60*time.Second, got)

	t.Setenv(EnvHTTPIdleTimeout, "-5s")
	_, err = HTTPIdleTimeoutFromEnv()
	require.Error(t, err)
}

func TestHTTPMaxHeaderBytesFromEnv(t *testing.T) {
	requireUnsetEnv(t, EnvHTTPMaxHeaderBytes)
	got, err := HTTPMaxHeaderBytesFromEnv()
	require.NoError(t, err)
	require.Equal(t, 1<<20, got)

	t.Setenv(EnvHTTPMaxHeaderBytes, "65536")
	got, err = HTTPMaxHeaderBytesFromEnv()
	require.NoError(t, err)
	require.Equal(t, 65536, got)

	t.Setenv(EnvHTTPMaxHeaderBytes, "0")
	_, err = HTTPMaxHeaderBytesFromEnv()
	require.Error(t, err)
}

func TestAdminAddrFromEnv(t *testing.T) {
	requireUnsetEnv(t, EnvAdminAddr)
	require.Equal(t, "127.0.0.1:9101", AdminAddrFromEnv())

	t.Setenv(EnvAdminAddr, "0.0.0.0:9101")
	require.Equal(t, "0.0.0.0:9101", AdminAddrFromEnv())
}

func TestAdminWriteTimeoutFromEnv(t *testing.T) {
	requireUnsetEnv(t, EnvAdminWriteTimeout)
	got, err := AdminWriteTimeoutFromEnv()
	require.NoError(t, err)
	require.Equal(t, 120*time.Second, got)

	t.Setenv(EnvAdminWriteTimeout, "0s")
	_, err = AdminWriteTimeoutFromEnv()
	require.Error(t, err)
}
