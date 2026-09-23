package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
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
	EnvGOExchangeSettlementConcurrency  = "GOEXCHANGE_SETTLEMENT_CONCURRENCY"
	EnvGOExchangeReconciliationInterval = "GOEXCHANGE_RECONCILIATION_INTERVAL"
	EnvGOExchangeEngineShards           = "GOEXCHANGE_ENGINE_SHARDS"
	EnvGOExchangeOutboxBatchSize        = "GOEXCHANGE_OUTBOX_BATCH_SIZE"
	EnvGOExchangeAcceptanceTimeoutMs    = "GOEXCHANGE_ACCEPTANCE_TIMEOUT_MS"
	EnvGOExchangeHoldBatchSize          = "GOEXCHANGE_HOLD_BATCH_SIZE"

	EnvGOExchangeMatchingMaxMatchesPerTurn     = "GOEXCHANGE_MATCHING_MAX_MATCHES_PER_TURN"
	EnvGOExchangeMatchingMaxConsecutiveCancels = "GOEXCHANGE_MATCHING_MAX_CONSECUTIVE_CANCELS"

	// 설계 §2 — 서비스 HTTP 서버 4종 상한 + MaxHeaderBytes.
	EnvHTTPReadHeaderTimeout = "GOEXCHANGE_HTTP_READ_HEADER_TIMEOUT"
	EnvHTTPReadTimeout       = "GOEXCHANGE_HTTP_READ_TIMEOUT"
	EnvHTTPWriteTimeout      = "GOEXCHANGE_HTTP_WRITE_TIMEOUT"
	EnvHTTPIdleTimeout       = "GOEXCHANGE_HTTP_IDLE_TIMEOUT"
	EnvHTTPMaxHeaderBytes    = "GOEXCHANGE_HTTP_MAX_HEADER_BYTES"

	// 설계 §2.2·§7 — 관리 서버(/metrics·pprof) 주소·WriteTimeout.
	EnvAdminAddr         = "GOEXCHANGE_ADMIN_ADDR"
	EnvAdminWriteTimeout = "GOEXCHANGE_ADMIN_WRITE_TIMEOUT"

	// 설계 §6.2·§6.3 — rate limit 활성화·대상별 rps/burst·프록시 신뢰 CIDR.
	EnvRateLimitEnabled       = "GOEXCHANGE_RATE_LIMIT_ENABLED"
	EnvAuthRateLimitRPS       = "GOEXCHANGE_AUTH_RATE_LIMIT_RPS"
	EnvAuthRateLimitBurst     = "GOEXCHANGE_AUTH_RATE_LIMIT_BURST"
	EnvOrderRateLimitRPS      = "GOEXCHANGE_ORDER_RATE_LIMIT_RPS"
	EnvOrderRateLimitBurst    = "GOEXCHANGE_ORDER_RATE_LIMIT_BURST"
	EnvTransferRateLimitRPS   = "GOEXCHANGE_TRANSFER_RATE_LIMIT_RPS"
	EnvTransferRateLimitBurst = "GOEXCHANGE_TRANSFER_RATE_LIMIT_BURST"
	EnvTrustedProxies         = "GOEXCHANGE_TRUSTED_PROXIES"
)

// strictPositiveEnv는 기존 parsePositiveIntEnv(database.go)와 달리 조용히
// fallback하지 않는다. quantum 값은 0이 matchSlice의 무제한 sentinel과
// 충돌하므로, 잘못된 설정으로 뜬 서버가 부하를 받는 것보다 안 뜨는 편이 낫다.
//
// 미설정(LookupEnv ok=false)만 기본값을 쓴다. 빈 문자열로 설정된 것은 셸
// 변수 오타나 치환 실패의 전형적 결과이므로 에러다 — os.Getenv로는 둘을
// 구분할 수 없어 LookupEnv를 쓴다.
func strictPositiveEnv(key string, def int) (int, error) {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return def, nil
	}
	if raw == "" {
		return 0, fmt.Errorf("%s is set but empty", key)
	}
	if raw != strings.TrimSpace(raw) {
		return 0, fmt.Errorf("%s has surrounding whitespace: %q", key, raw)
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s is not a decimal integer: %q", key, raw)
	}
	if strconv.Itoa(parsed) != raw {
		return 0, fmt.Errorf("%s is not in canonical decimal form: %q", key, raw)
	}
	if parsed < 1 {
		return 0, fmt.Errorf("%s must be >= 1, got %d", key, parsed)
	}
	return parsed, nil
}

// strictPositiveDurationEnv는 strictPositiveEnv와 같은 계약을 duration 값에
// 적용한다. 미설정(LookupEnv ok=false)만 기본값을 쓰고, 그 외 이상값(빈 문자열·
// 앞뒤 공백·0 이하·파싱 불가)은 에러다 — DB 시간 상한은 조용한 fallback보다
// 안 뜨는 편이 낫다(quantum 값과 같은 이유, strictPositiveEnv 주석 참고).
func strictPositiveDurationEnv(key string, def time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return def, nil
	}
	if raw == "" {
		return 0, fmt.Errorf("%s is set but empty", key)
	}
	if raw != strings.TrimSpace(raw) {
		return 0, fmt.Errorf("%s has surrounding whitespace: %q", key, raw)
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s is not a valid duration: %q", key, raw)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s must be > 0, got %s", key, parsed)
	}
	return parsed, nil
}

// MatchingQuantumFromEnv는 매칭 스케줄러의 두 상한을 strict 파싱한다.
// matching 타입을 반환하지 않는 것은 config → matching 의존을 만들지
// 않기 위해서다. main이 두 값으로 matching.QuantumConfig를 구성한다.
//
// 기본값은 matching 패키지의 상수와 같아야 한다. 어긋나면 테스트와
// 프로덕션이 다른 값으로 돈다.
func MatchingQuantumFromEnv() (maxMatchesPerTurn int, maxConsecutiveCancels int, err error) {
	maxMatchesPerTurn, err = strictPositiveEnv(EnvGOExchangeMatchingMaxMatchesPerTurn, defaultMatchingMaxMatchesPerTurn)
	if err != nil {
		return 0, 0, err
	}
	maxConsecutiveCancels, err = strictPositiveEnv(EnvGOExchangeMatchingMaxConsecutiveCancels, defaultMatchingMaxConsecutiveCancels)
	if err != nil {
		return 0, 0, err
	}
	return maxMatchesPerTurn, maxConsecutiveCancels, nil
}

// 사전 등록된 결정적 순위로 확정한 기본값이다(설계 §9.1).
// 로컬 wall-clock 최적값이 아니다 — 그 판정은 측정 정밀도 부족으로 종료됐다.
// internal/matching/quantum_config.go의 값과 반드시 같아야 한다.
const (
	defaultMatchingMaxMatchesPerTurn     = 128
	defaultMatchingMaxConsecutiveCancels = 8
)

// 설계 §2의 운영 시작점 — 정확성 경계가 아니다.
const (
	defaultHTTPReadHeaderTimeout = 5 * time.Second
	defaultHTTPReadTimeout       = 15 * time.Second
	defaultHTTPWriteTimeout      = 20 * time.Second
	defaultHTTPIdleTimeout       = 60 * time.Second
	defaultHTTPMaxHeaderBytes    = 1 << 20

	defaultAdminAddr         = "127.0.0.1:9101"
	defaultAdminWriteTimeout = 120 * time.Second
)

// 설계 §6.2 — 대상별 rate limit 기본값(부하 프로필에서 env로 조정).
const (
	defaultAuthRateLimitRPS       = 1
	defaultAuthRateLimitBurst     = 10
	defaultOrderRateLimitRPS      = 20
	defaultOrderRateLimitBurst    = 40
	defaultTransferRateLimitRPS   = 2
	defaultTransferRateLimitBurst = 5
)

func HTTPReadHeaderTimeoutFromEnv() (time.Duration, error) {
	return strictPositiveDurationEnv(EnvHTTPReadHeaderTimeout, defaultHTTPReadHeaderTimeout)
}

func HTTPReadTimeoutFromEnv() (time.Duration, error) {
	return strictPositiveDurationEnv(EnvHTTPReadTimeout, defaultHTTPReadTimeout)
}

func HTTPWriteTimeoutFromEnv() (time.Duration, error) {
	return strictPositiveDurationEnv(EnvHTTPWriteTimeout, defaultHTTPWriteTimeout)
}

func HTTPIdleTimeoutFromEnv() (time.Duration, error) {
	return strictPositiveDurationEnv(EnvHTTPIdleTimeout, defaultHTTPIdleTimeout)
}

func HTTPMaxHeaderBytesFromEnv() (int, error) {
	return strictPositiveEnv(EnvHTTPMaxHeaderBytes, defaultHTTPMaxHeaderBytes)
}

// AdminAddrFromEnv은 문자열이라 strict 파싱 대상이 아니다(strictPositiveEnv 계열은
// 숫자·duration 전용) — 미설정만 기본값을 쓰고, 그 외에는 트림한 값을 그대로 쓴다.
func AdminAddrFromEnv() string {
	return envOrDefault(EnvAdminAddr, defaultAdminAddr)
}

func AdminWriteTimeoutFromEnv() (time.Duration, error) {
	return strictPositiveDurationEnv(EnvAdminWriteTimeout, defaultAdminWriteTimeout)
}

// RateLimitEnabledFromEnv은 기본 켜짐이다 — 끄면 운영 미들웨어 경로를 우회한
// 다른 시스템을 측정하게 되므로(설계 §6.4), 부하 테스트에서도 끄지 않는다.
func RateLimitEnabledFromEnv() bool {
	value, ok := os.LookupEnv(EnvRateLimitEnabled)
	if !ok {
		return true
	}
	return parseBoolEnv(value)
}

func AuthRateLimitRPSFromEnv() (int, error) {
	return strictPositiveEnv(EnvAuthRateLimitRPS, defaultAuthRateLimitRPS)
}

func AuthRateLimitBurstFromEnv() (int, error) {
	return strictPositiveEnv(EnvAuthRateLimitBurst, defaultAuthRateLimitBurst)
}

func OrderRateLimitRPSFromEnv() (int, error) {
	return strictPositiveEnv(EnvOrderRateLimitRPS, defaultOrderRateLimitRPS)
}

func OrderRateLimitBurstFromEnv() (int, error) {
	return strictPositiveEnv(EnvOrderRateLimitBurst, defaultOrderRateLimitBurst)
}

func TransferRateLimitRPSFromEnv() (int, error) {
	return strictPositiveEnv(EnvTransferRateLimitRPS, defaultTransferRateLimitRPS)
}

func TransferRateLimitBurstFromEnv() (int, error) {
	return strictPositiveEnv(EnvTransferRateLimitBurst, defaultTransferRateLimitBurst)
}

// TrustedProxiesFromEnv은 콤마 구분 CIDR 목록을 반환한다(설계 §6.3). 미설정이면
// nil — gin의 SetTrustedProxies(nil)과 짝을 이뤄 RemoteAddr만 신뢰한다. 잘못된
// CIDR은 에러이며, 호출자가 부팅 실패로 이어야 한다.
func TrustedProxiesFromEnv() ([]string, error) {
	raw, ok := os.LookupEnv(EnvTrustedProxies)
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	proxies := make([]string, 0, len(parts))
	for _, part := range parts {
		cidr := strings.TrimSpace(part)
		if cidr == "" {
			return nil, fmt.Errorf("%s contains an empty entry", EnvTrustedProxies)
		}
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return nil, fmt.Errorf("%s contains invalid CIDR %q: %w", EnvTrustedProxies, cidr, err)
		}
		proxies = append(proxies, cidr)
	}
	return proxies, nil
}

const defaultSettlementWorkers = 10
const defaultSettlementConcurrency = 4
const defaultReconciliationIntervalSeconds = 3600
const defaultOutboxBatchSize = 512
const defaultAcceptanceTimeoutMs = 100
const defaultHoldBatchSize = 64

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

// EnvGOExchangeSettlementConcurrency는 전역 정산 worker pool 크기(동시 정산 트랜잭션 수)다.
// 기존 EnvGOExchangeSettlementWorkers(해시 파티션 수)와는 다른 축이다 — 파티션 수는 순서
// 보존 단위, 이 값은 DB 동시성 상한. DB 풀(GOEXCHANGE_DB_MAX_OPEN_CONNS, 기본 25)을 주문
// 홀드·아웃박스·리컨실리에이션과 공유하므로 보수적 기본값에서 시작한다.
func SettlementConcurrencyFromEnv() int {
	return parsePositiveIntEnv(EnvGOExchangeSettlementConcurrency, defaultSettlementConcurrency)
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

// HoldBatchSizeFromEnv는 홀드 코디네이터의 그룹커밋 배치 상한을 반환한다.
func HoldBatchSizeFromEnv() int {
	return parsePositiveIntEnv(EnvGOExchangeHoldBatchSize, defaultHoldBatchSize)
}
