package config

import (
	"os"
	"runtime"
	"strings"
	"time"
)

const (
	EnvGOExchangeEnableDevTools         = "GOEXCHANGE_ENABLE_DEV_TOOLS"
	EnvGOExchangeDevToolsToken          = "GOEXCHANGE_DEV_TOOLS_TOKEN"
	EnvGOExchangeEnableUpbit            = "GOEXCHANGE_ENABLE_UPBIT"
	EnvGOExchangeCORSOrigins            = "GOEXCHANGE_CORS_ALLOWED_ORIGINS"
	EnvGOExchangeEnablePprof            = "GOEXCHANGE_ENABLE_PPROF"
	EnvGOExchangeSettlementWorkers      = "GOEXCHANGE_SETTLEMENT_WORKERS"
	EnvGOExchangeReconciliationInterval = "GOEXCHANGE_RECONCILIATION_INTERVAL"
	EnvGOExchangeEngineShards           = "GOEXCHANGE_ENGINE_SHARDS"
)

const defaultSettlementWorkers = 10
const defaultReconciliationIntervalSeconds = 3600

var defaultCORSAllowedOrigins = []string{
	"http://localhost:3000",
	"http://127.0.0.1:3000",
}

func DevToolsEnabledFromEnv() bool {
	return parseBoolEnv(os.Getenv(EnvGOExchangeEnableDevTools))
}

func DevToolsTokenFromEnv() string {
	return strings.TrimSpace(os.Getenv(EnvGOExchangeDevToolsToken))
}

func UpbitEnabledFromEnv() bool {
	value, ok := os.LookupEnv(EnvGOExchangeEnableUpbit)
	if !ok {
		return true
	}
	return parseBoolEnv(value)
}

func PprofEnabledFromEnv() bool {
	return parseBoolEnv(os.Getenv(EnvGOExchangeEnablePprof))
}

func CORSAllowedOriginsFromEnv() []string {
	if origins := strings.TrimSpace(os.Getenv(EnvGOExchangeCORSOrigins)); origins != "" {
		return parseCommaSeparatedList(origins)
	}
	return append([]string(nil), defaultCORSAllowedOrigins...)
}

func parseCommaSeparatedList(raw string) []string {
	parts := strings.Split(raw, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value == "" {
			continue
		}
		values = append(values, value)
	}
	return values
}

func parseBoolEnv(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	default:
		return false
	}
}

func SettlementWorkersFromEnv() int {
	return parsePositiveIntEnv(EnvGOExchangeSettlementWorkers, defaultSettlementWorkers)
}

func ReconciliationIntervalFromEnv() time.Duration {
	seconds := parsePositiveIntEnv(EnvGOExchangeReconciliationInterval, defaultReconciliationIntervalSeconds)
	return time.Duration(seconds) * time.Second
}

// EngineShardsFromEnv은 매칭 엔진 샤드 수를 반환한다. 매칭은 CPU가 아니라
// 직렬화가 병목이므로(20번 벤치마크) 기본값은 코어 수다.
func EngineShardsFromEnv() int {
	return parsePositiveIntEnv(EnvGOExchangeEngineShards, runtime.NumCPU())
}
