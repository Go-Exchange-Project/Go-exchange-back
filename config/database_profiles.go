package config

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

const (
	EnvMigrationStatementTimeout      = "GOEXCHANGE_MIGRATION_STATEMENT_TIMEOUT"
	EnvReconciliationStatementTimeout = "GOEXCHANGE_RECONCILIATION_STATEMENT_TIMEOUT"
)

// 설계 §3 표의 고정값. lock_timeout·idle_in_transaction_session_timeout·커넥션
// 수는 이번 사이클에서 env로 열지 않는다 — statement_timeout 두 곳만 명시적으로
// env화됐다(설계 §3 표에 env 이름이 붙은 칸만).
const (
	defaultMigrationStatementTimeout = 10 * time.Minute
	defaultMigrationLockTimeout      = 10 * time.Second
	defaultMigrationMaxOpenConns     = 2

	defaultServiceStatementTimeout                = 15 * time.Second
	defaultServiceLockTimeout                     = 3 * time.Second
	defaultServiceIdleInTransactionSessionTimeout = 30 * time.Second

	defaultReconciliationStatementTimeout = 5 * time.Minute
	defaultReconciliationLockTimeout      = 3 * time.Second
	defaultReconciliationMaxOpenConns     = 2
)

// MigrationStatementTimeoutFromEnv는 부팅 AutoMigrate·goose 풀의 statement_timeout이다.
func MigrationStatementTimeoutFromEnv() (time.Duration, error) {
	return strictPositiveDurationEnv(EnvMigrationStatementTimeout, defaultMigrationStatementTimeout)
}

// ReconciliationStatementTimeoutFromEnv는 ReconciliationWorker 전용 풀의 statement_timeout이다.
func ReconciliationStatementTimeoutFromEnv() (time.Duration, error) {
	return strictPositiveDurationEnv(EnvReconciliationStatementTimeout, defaultReconciliationStatementTimeout)
}

// DBTimeoutProfile은 세 DB 풀(마이그레이션·서비스·검산) 공통의 시간 상한·풀 크기·
// 관측 설정을 담는다(설계 §3).
type DBTimeoutProfile struct {
	// Name은 오류 메시지와 DB 통계 collector 라벨에 쓰인다("goexchange_"+Name).
	Name string

	StatementTimeout                time.Duration
	LockTimeout                     time.Duration
	IdleInTransactionSessionTimeout time.Duration // 0 = 비활성

	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration

	// Registerer가 nil이면 DB 통계 collector를 등록하지 않는다(마이그레이션 풀).
	Registerer prometheus.Registerer
}

// MigrationDBProfile은 부팅 AutoMigrate + goose 전용 풀이다. 곧 닫히므로
// collector를 등록하지 않는다(설계 §3.1).
func MigrationDBProfile() (DBTimeoutProfile, error) {
	statementTimeout, err := MigrationStatementTimeoutFromEnv()
	if err != nil {
		return DBTimeoutProfile{}, err
	}
	return DBTimeoutProfile{
		Name:                            "migration",
		StatementTimeout:                statementTimeout,
		LockTimeout:                     defaultMigrationLockTimeout,
		IdleInTransactionSessionTimeout: 0,
		MaxOpenConns:                    defaultMigrationMaxOpenConns,
		MaxIdleConns:                    defaultMigrationMaxOpenConns,
		ConnMaxLifetime:                 0,
		Registerer:                      nil,
	}, nil
}

// ServiceDBProfile은 HTTP·엔진·정산·outbox·worker가 공유하는 기존 풀이다.
// 커넥션 수는 기존 GOEXCHANGE_DB_MAX_* env를 그대로 쓴다(설계 §3 표).
func ServiceDBProfile(registerer prometheus.Registerer) DBTimeoutProfile {
	return DBTimeoutProfile{
		Name:                            "service",
		StatementTimeout:                defaultServiceStatementTimeout,
		LockTimeout:                     defaultServiceLockTimeout,
		IdleInTransactionSessionTimeout: defaultServiceIdleInTransactionSessionTimeout,
		MaxOpenConns:                    MaxOpenConnsFromEnv(),
		MaxIdleConns:                    MaxIdleConnsFromEnv(),
		ConnMaxLifetime:                 ConnMaxLifetimeFromEnv(),
		Registerer:                      registerer,
	}
}

// ReconciliationDBProfile은 ReconciliationWorker 전용 풀이다(설계 §3).
func ReconciliationDBProfile(registerer prometheus.Registerer) (DBTimeoutProfile, error) {
	statementTimeout, err := ReconciliationStatementTimeoutFromEnv()
	if err != nil {
		return DBTimeoutProfile{}, err
	}
	return DBTimeoutProfile{
		Name:                            "reconciliation",
		StatementTimeout:                statementTimeout,
		LockTimeout:                     defaultReconciliationLockTimeout,
		IdleInTransactionSessionTimeout: 0,
		MaxOpenConns:                    defaultReconciliationMaxOpenConns,
		MaxIdleConns:                    defaultReconciliationMaxOpenConns,
		ConnMaxLifetime:                 0,
		Registerer:                      registerer,
	}, nil
}

// applyDBProfileRuntimeParams는 프로필의 시간 상한을 pgx RuntimeParams(세션
// 시작 파라미터)로 싣는다. 문자열 결합이 아니라 pgx.ParseConfig가 이미 해석한
// cfg에 값을 더하는 방식이라 DSN이 URL이든 keyword든 동일하게 동작한다(설계 §3.1).
// time.Duration(0).Milliseconds()는 "0"이라 idle_in_transaction_session_timeout의
// "비활성" 값과 자연히 일치한다.
func applyDBProfileRuntimeParams(cfg *pgx.ConnConfig, profile DBTimeoutProfile) {
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["statement_timeout"] = strconv.FormatInt(profile.StatementTimeout.Milliseconds(), 10)
	cfg.RuntimeParams["lock_timeout"] = strconv.FormatInt(profile.LockTimeout.Milliseconds(), 10)
	cfg.RuntimeParams["idle_in_transaction_session_timeout"] = strconv.FormatInt(profile.IdleInTransactionSessionTimeout.Milliseconds(), 10)
}

// OpenDBWithProfile은 dsn(URL·keyword 양쪽 형식 가능)에 profile의 시간 상한·풀
// 설정을 적용해 연다. 연결 직후 SHOW 3종으로 실제 적용값을 검증하고, GORM open
// 또는 검증이 실패하면 이미 연 *sql.DB를 close한 뒤 그 포인터와 에러를 함께
// 돌려준다(설계 §3.2 — 호출자가 close 여부를 다시 확인할 수 있게).
func OpenDBWithProfile(dsn string, profile DBTimeoutProfile) (*gorm.DB, *sql.DB, error) {
	return openDBWithProfileExpecting(dsn, profile, profile)
}

// openDBWithProfileExpecting은 applyProfile로 연결을 열고 expectProfile로 SHOW
// 검증한다. 정상 호출(OpenDBWithProfile)은 둘이 항상 같다 — 분리한 이유는 검증
// 실패 경로(D3)를 실제 DB로 결정적으로 재현하기 위해서다.
func openDBWithProfileExpecting(dsn string, applyProfile, expectProfile DBTimeoutProfile) (*gorm.DB, *sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("parse database dsn: %w", err)
	}
	applyDBProfileRuntimeParams(cfg, applyProfile)

	sqlDB := stdlib.OpenDB(*cfg)
	sqlDB.SetMaxOpenConns(applyProfile.MaxOpenConns)
	sqlDB.SetMaxIdleConns(applyProfile.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(applyProfile.ConnMaxLifetime)

	if err := validateDBProfileRuntimeParams(sqlDB, expectProfile); err != nil {
		sqlDB.Close()
		return nil, sqlDB, err
	}

	gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{})
	if err != nil {
		sqlDB.Close()
		return nil, sqlDB, fmt.Errorf("gorm open (profile %s): %w", applyProfile.Name, err)
	}

	if applyProfile.Registerer != nil {
		if err := applyProfile.Registerer.Register(collectors.NewDBStatsCollector(sqlDB, "goexchange_"+applyProfile.Name)); err != nil {
			sqlDB.Close()
			return nil, sqlDB, fmt.Errorf("register db stats collector (profile %s): %w", applyProfile.Name, err)
		}
	}

	return gormDB, sqlDB, nil
}

// validateDBProfileRuntimeParams는 SHOW statement_timeout·lock_timeout·
// idle_in_transaction_session_timeout 세 개가 profile의 기대값과 일치하는지
// 확인한다(설계 §3.2). 하나라도 다르면 즉시 에러를 반환한다.
func validateDBProfileRuntimeParams(sqlDB *sql.DB, profile DBTimeoutProfile) error {
	checks := []struct {
		gucName string
		want    time.Duration
	}{
		{"statement_timeout", profile.StatementTimeout},
		{"lock_timeout", profile.LockTimeout},
		{"idle_in_transaction_session_timeout", profile.IdleInTransactionSessionTimeout},
	}
	for _, check := range checks {
		var raw string
		if err := sqlDB.QueryRow("SHOW " + check.gucName).Scan(&raw); err != nil {
			return fmt.Errorf("show %s (profile %s): %w", check.gucName, profile.Name, err)
		}
		got, err := parsePostgresDuration(raw)
		if err != nil {
			return fmt.Errorf("parse %s value %q (profile %s): %w", check.gucName, raw, profile.Name, err)
		}
		if got != check.want {
			return fmt.Errorf("%s mismatch (profile %s): got %s, want %s", check.gucName, profile.Name, got, check.want)
		}
	}
	return nil
}

// postgresDurationUnits는 SHOW로 돌아오는 GUC 문자열의 단위 접미사를, 오검출을
// 피하기 위해 긴 것부터("min" 앞에 "ms", 둘 다 "s"보다 먼저) 나열한다.
var postgresDurationUnits = []struct {
	suffix string
	unit   time.Duration
}{
	{"min", time.Minute},
	{"ms", time.Millisecond},
	{"s", time.Second},
	{"h", time.Hour},
	{"d", 24 * time.Hour},
}

// parsePostgresDuration은 Postgres GUC의 사람이 읽는 duration 표기("15s"·"3s"·
// "30s"·"10min"·"100ms"·"0")를 time.Duration으로 되돌린다. 우리가 쓰는 값은
// 전부 단일 단위로 정확히 나눠떨어지므로 "1min30s" 같은 복합 표기는 다루지 않는다.
func parsePostgresDuration(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "0" {
		return 0, nil
	}
	for _, u := range postgresDurationUnits {
		if numPart, ok := strings.CutSuffix(raw, u.suffix); ok {
			n, err := strconv.Atoi(numPart)
			if err != nil {
				continue
			}
			return time.Duration(n) * u.unit, nil
		}
	}
	return 0, fmt.Errorf("unrecognized postgres duration format: %q", raw)
}
