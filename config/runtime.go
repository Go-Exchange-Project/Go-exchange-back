package config

import (
	"os"
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
	EnvGOExchangeOutboxBatchSize        = "GOEXCHANGE_OUTBOX_BATCH_SIZE"
	EnvGOExchangeAcceptanceTimeoutMs    = "GOEXCHANGE_ACCEPTANCE_TIMEOUT_MS"
)

const defaultSettlementWorkers = 10
const defaultReconciliationIntervalSeconds = 3600
const defaultOutboxBatchSize = 512
const defaultAcceptanceTimeoutMs = 100

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

// EngineShardsFromEnv은 매칭 엔진 샤드 수를 반환한다. 기본값 1 — 21번
// 벤치마크에서 병목이 다운스트림(outbox→DB)일 때 샤드 N개는 주문 버퍼
// N×1024로 깊어져 p95만 악화(+57%)됨을 실측했다. 매칭이 실제 병목이 되면
// 이 환경변수로 확대한다.
func EngineShardsFromEnv() int {
	return parsePositiveIntEnv(EnvGOExchangeEngineShards, 1)
}

// OutboxBatchSizeFromEnv는 outbox writer의 그룹커밋 배치 상한을 반환한다.
// 21번 벤치마크에서 상한 64가 포화(평균 54.4건/flush, ≈66ms/flush)돼 write-ahead
// 관문이 파이프라인 전체를 캡했다 — 기본 512로 왕복·fsync 횟수를 1/8로 줄인다.
func OutboxBatchSizeFromEnv() int {
	return parsePositiveIntEnv(EnvGOExchangeOutboxBatchSize, defaultOutboxBatchSize)
}

// OrderAcceptanceTimeoutFromEnv는 주문 접수 시 엔진 핸드오프의 바운디드 대기
// 상한이다. 일시 버스트는 흡수하되 지속 포화는 이 시간 후 503으로 거절한다.
func OrderAcceptanceTimeoutFromEnv() time.Duration {
	ms := parsePositiveIntEnv(EnvGOExchangeAcceptanceTimeoutMs, defaultAcceptanceTimeoutMs)
	return time.Duration(ms) * time.Millisecond
}
