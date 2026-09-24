package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// C1: rate limit·프록시 신뢰 env — 기본값, 유효 override, 잘못된 값 거부.
func TestRateLimitEnabledFromEnvDefaultsToTrue(t *testing.T) {
	enabled, err := RateLimitEnabledFromEnv()
	require.NoError(t, err)
	assert.True(t, enabled)
}

func TestRateLimitEnabledFromEnvAcceptsRecognizedTokens(t *testing.T) {
	for raw, want := range map[string]bool{
		"true": true, "TRUE": true, "1": true, "yes": true, "on": true,
		"false": false, "FALSE": false, "0": false, "no": false, "off": false,
	} {
		t.Setenv(EnvRateLimitEnabled, raw)
		enabled, err := RateLimitEnabledFromEnv()
		require.NoError(t, err, raw)
		assert.Equal(t, want, enabled, raw)
	}
}

// 설계 §6.2 — 파싱 불가는 부팅 실패. 오타가 조용히 limiter를 끄면 안 된다.
func TestRateLimitEnabledFromEnvRejectsUnrecognizedValues(t *testing.T) {
	for _, raw := range []string{"treu", "flase", "", " ", "maybe", "2"} {
		t.Run("value="+raw, func(t *testing.T) {
			t.Setenv(EnvRateLimitEnabled, raw)
			_, err := RateLimitEnabledFromEnv()
			require.Error(t, err)
		})
	}
}

func TestAuthRateLimitRPSAndBurstFromEnvDefaults(t *testing.T) {
	rps, err := AuthRateLimitRPSFromEnv()
	require.NoError(t, err)
	assert.Equal(t, 1, rps)

	burst, err := AuthRateLimitBurstFromEnv()
	require.NoError(t, err)
	assert.Equal(t, 10, burst)
}

func TestOrderRateLimitRPSAndBurstFromEnvDefaults(t *testing.T) {
	rps, err := OrderRateLimitRPSFromEnv()
	require.NoError(t, err)
	assert.Equal(t, 20, rps)

	burst, err := OrderRateLimitBurstFromEnv()
	require.NoError(t, err)
	assert.Equal(t, 40, burst)
}

func TestTransferRateLimitRPSAndBurstFromEnvDefaults(t *testing.T) {
	rps, err := TransferRateLimitRPSFromEnv()
	require.NoError(t, err)
	assert.Equal(t, 2, rps)

	burst, err := TransferRateLimitBurstFromEnv()
	require.NoError(t, err)
	assert.Equal(t, 5, burst)
}

func TestRateLimitRPSAndBurstFromEnvOverride(t *testing.T) {
	t.Setenv(EnvAuthRateLimitRPS, "5")
	t.Setenv(EnvOrderRateLimitBurst, "100")

	rps, err := AuthRateLimitRPSFromEnv()
	require.NoError(t, err)
	assert.Equal(t, 5, rps)

	burst, err := OrderRateLimitBurstFromEnv()
	require.NoError(t, err)
	assert.Equal(t, 100, burst)
}

func TestRateLimitRPSAndBurstFromEnvRejectInvalidValues(t *testing.T) {
	cases := []struct {
		name string
		key  string
	}{
		{"auth rps", EnvAuthRateLimitRPS},
		{"auth burst", EnvAuthRateLimitBurst},
		{"order rps", EnvOrderRateLimitRPS},
		{"order burst", EnvOrderRateLimitBurst},
		{"transfer rps", EnvTransferRateLimitRPS},
		{"transfer burst", EnvTransferRateLimitBurst},
	}
	invalid := []string{"0", "-1", "", " ", "not-a-number"}

	for _, tc := range cases {
		for _, value := range invalid {
			t.Run(tc.name+"/"+value, func(t *testing.T) {
				t.Setenv(tc.key, value)
				var err error
				switch tc.key {
				case EnvAuthRateLimitRPS:
					_, err = AuthRateLimitRPSFromEnv()
				case EnvAuthRateLimitBurst:
					_, err = AuthRateLimitBurstFromEnv()
				case EnvOrderRateLimitRPS:
					_, err = OrderRateLimitRPSFromEnv()
				case EnvOrderRateLimitBurst:
					_, err = OrderRateLimitBurstFromEnv()
				case EnvTransferRateLimitRPS:
					_, err = TransferRateLimitRPSFromEnv()
				case EnvTransferRateLimitBurst:
					_, err = TransferRateLimitBurstFromEnv()
				}
				require.Error(t, err)
			})
		}
	}
}

func TestTrustedProxiesFromEnvDefaultsToNil(t *testing.T) {
	proxies, err := TrustedProxiesFromEnv()
	require.NoError(t, err)
	assert.Nil(t, proxies)
}

func TestTrustedProxiesFromEnvParsesValidCIDRList(t *testing.T) {
	t.Setenv(EnvTrustedProxies, "10.0.0.0/8, 172.16.0.0/12")

	proxies, err := TrustedProxiesFromEnv()
	require.NoError(t, err)
	assert.Equal(t, []string{"10.0.0.0/8", "172.16.0.0/12"}, proxies)
}

func TestTrustedProxiesFromEnvRejectsInvalidCIDR(t *testing.T) {
	t.Setenv(EnvTrustedProxies, "not-a-cidr")

	_, err := TrustedProxiesFromEnv()
	require.Error(t, err)
}
