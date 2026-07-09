package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseBoolEnv(t *testing.T) {
	trueValues := []string{"1", "t", "true", "TRUE", "y", "yes", "on", " on "}
	for _, value := range trueValues {
		t.Run(value, func(t *testing.T) {
			assert.True(t, parseBoolEnv(value))
		})
	}

	falseValues := []string{"", "0", "false", "no", "off", "anything"}
	for _, value := range falseValues {
		t.Run(value, func(t *testing.T) {
			assert.False(t, parseBoolEnv(value))
		})
	}
}

func TestDevToolsEnabledFromEnv(t *testing.T) {
	t.Setenv(EnvGOExchangeEnableDevTools, "true")

	assert.True(t, DevToolsEnabledFromEnv())
}

func TestUpbitEnabledFromEnvDefaultsToEnabled(t *testing.T) {
	oldValue, hadValue := os.LookupEnv(EnvGOExchangeEnableUpbit)
	requireUnsetEnv(t, EnvGOExchangeEnableUpbit)
	t.Cleanup(func() {
		if hadValue {
			_ = os.Setenv(EnvGOExchangeEnableUpbit, oldValue)
			return
		}
		_ = os.Unsetenv(EnvGOExchangeEnableUpbit)
	})

	assert.True(t, UpbitEnabledFromEnv())
}

func TestUpbitEnabledFromEnv(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		expected bool
	}{
		{name: "enabled", value: "true", expected: true},
		{name: "disabled", value: "false", expected: false},
		{name: "zero", value: "0", expected: false},
		{name: "one", value: "1", expected: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvGOExchangeEnableUpbit, tt.value)
			assert.Equal(t, tt.expected, UpbitEnabledFromEnv())
		})
	}
}

func TestCORSAllowedOriginsFromEnvDefaultsToLocalOrigins(t *testing.T) {
	oldValue, hadValue := os.LookupEnv(EnvGOExchangeCORSOrigins)
	requireUnsetEnv(t, EnvGOExchangeCORSOrigins)
	t.Cleanup(func() {
		if hadValue {
			_ = os.Setenv(EnvGOExchangeCORSOrigins, oldValue)
			return
		}
		_ = os.Unsetenv(EnvGOExchangeCORSOrigins)
	})

	assert.Equal(t, []string{
		"http://localhost:3000",
		"http://127.0.0.1:3000",
	}, CORSAllowedOriginsFromEnv())
}

func TestCORSAllowedOriginsFromEnvParsesCommaSeparatedList(t *testing.T) {
	t.Setenv(EnvGOExchangeCORSOrigins, " http://localhost:3000, http://192.168.219.100:3000 ,,")

	assert.Equal(t, []string{
		"http://localhost:3000",
		"http://192.168.219.100:3000",
	}, CORSAllowedOriginsFromEnv())
}

func requireUnsetEnv(t *testing.T, key string) {
	t.Helper()
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset env %s: %v", key, err)
	}
}

func TestPprofEnabledFromEnv(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		expected bool
	}{
		{name: "enabled", value: "true", expected: true},
		{name: "disabled", value: "false", expected: false},
		{name: "unset", value: "", expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvGOExchangeEnablePprof, tt.value)
			assert.Equal(t, tt.expected, PprofEnabledFromEnv())
		})
	}
}

func TestSettlementWorkersFromEnvDefault(t *testing.T) {
	requireUnsetEnv(t, EnvGOExchangeSettlementWorkers)

	assert.Equal(t, 10, SettlementWorkersFromEnv())
}

func TestSettlementWorkersFromEnvCustomValue(t *testing.T) {
	t.Setenv(EnvGOExchangeSettlementWorkers, "3")

	assert.Equal(t, 3, SettlementWorkersFromEnv())
}

func TestSettlementWorkersFromEnvInvalidValueFallsBackToDefault(t *testing.T) {
	t.Setenv(EnvGOExchangeSettlementWorkers, "not-a-number")

	assert.Equal(t, 10, SettlementWorkersFromEnv())
}
