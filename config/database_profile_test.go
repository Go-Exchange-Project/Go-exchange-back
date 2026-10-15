package config

import (
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// C1: 새 env(GOEXCHANGE_MIGRATION_STATEMENT_TIMEOUT·GOEXCHANGE_RECONCILIATION_STATEMENT_TIMEOUT)의
// 기본값·파싱. 0·음수·공백·파싱 불가는 에러다(strictPositiveDurationEnv와 같은 계약).
func TestMigrationStatementTimeoutFromEnv(t *testing.T) {
	t.Run("unset uses design default 10m", func(t *testing.T) {
		require.NoError(t, os.Unsetenv(EnvMigrationStatementTimeout))
		got, err := MigrationStatementTimeoutFromEnv()
		require.NoError(t, err)
		require.Equal(t, 10*time.Minute, got)
	})

	t.Run("valid override applies", func(t *testing.T) {
		t.Setenv(EnvMigrationStatementTimeout, "20m")
		got, err := MigrationStatementTimeoutFromEnv()
		require.NoError(t, err)
		require.Equal(t, 20*time.Minute, got)
	})

	for _, tc := range []struct{ name, value string }{
		{"empty string", ""},
		{"zero", "0s"},
		{"negative", "-1s"},
		{"leading space", " 10m"},
		{"not a duration", "abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvMigrationStatementTimeout, tc.value)
			_, err := MigrationStatementTimeoutFromEnv()
			require.Error(t, err, "%q는 에러여야 한다", tc.value)
		})
	}
}

func TestReconciliationStatementTimeoutFromEnv(t *testing.T) {
	t.Run("unset uses design default 5m", func(t *testing.T) {
		require.NoError(t, os.Unsetenv(EnvReconciliationStatementTimeout))
		got, err := ReconciliationStatementTimeoutFromEnv()
		require.NoError(t, err)
		require.Equal(t, 5*time.Minute, got)
	})

	t.Run("valid override applies", func(t *testing.T) {
		t.Setenv(EnvReconciliationStatementTimeout, "1m")
		got, err := ReconciliationStatementTimeoutFromEnv()
		require.NoError(t, err)
		require.Equal(t, time.Minute, got)
	})

	for _, tc := range []struct{ name, value string }{
		{"empty string", ""},
		{"zero", "0s"},
		{"negative", "-1s"},
		{"trailing space", "5m "},
		{"not a duration", "xyz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvReconciliationStatementTimeout, tc.value)
			_, err := ReconciliationStatementTimeoutFromEnv()
			require.Error(t, err, "%q는 에러여야 한다", tc.value)
		})
	}
}

// C2: 세 DB 프로필이 각자 기대 RuntimeParams를 만든다. URL DSN·keyword DSN
// 양쪽에서 검증한다 — pgx.ParseConfig가 형식을 가리지 않고 같은 결과를 내야
// 문자열 결합이 아니라 런타임 파라미터로 적용한 의미가 있다.
func TestDBProfileRuntimeParamsAppliedToBothDSNFormats(t *testing.T) {
	profile := DBTimeoutProfile{
		Name:                            "test",
		StatementTimeout:                15 * time.Second,
		LockTimeout:                     3 * time.Second,
		IdleInTransactionSessionTimeout: 30 * time.Second,
	}

	dsns := []string{
		"postgres://appuser:secret@db.internal:5432/goexchange?sslmode=disable",
		"host=db.internal user=appuser password=secret dbname=goexchange port=5432 sslmode=disable",
	}
	for _, dsn := range dsns {
		t.Run(dsn, func(t *testing.T) {
			cfg, err := pgx.ParseConfig(dsn)
			require.NoError(t, err)
			applyDBProfileRuntimeParams(cfg, profile)

			require.Equal(t, "15000", cfg.RuntimeParams["statement_timeout"])
			require.Equal(t, "3000", cfg.RuntimeParams["lock_timeout"])
			require.Equal(t, "30000", cfg.RuntimeParams["idle_in_transaction_session_timeout"])
			// DSN 자체의 접속 정보는 그대로 파싱돼야 한다(런타임 파라미터만 추가).
			require.Equal(t, "db.internal", cfg.Host)
			require.Equal(t, "appuser", cfg.User)
			require.Equal(t, "goexchange", cfg.Database)
		})
	}
}

func TestDBProfileRuntimeParamsDisablesIdleTimeoutWhenZero(t *testing.T) {
	profile := DBTimeoutProfile{
		Name:                            "migration",
		StatementTimeout:                10 * time.Minute,
		LockTimeout:                     10 * time.Second,
		IdleInTransactionSessionTimeout: 0,
	}
	cfg, err := pgx.ParseConfig("host=db.internal user=u dbname=d port=5432 sslmode=disable")
	require.NoError(t, err)
	applyDBProfileRuntimeParams(cfg, profile)

	require.Equal(t, "600000", cfg.RuntimeParams["statement_timeout"])
	require.Equal(t, "10000", cfg.RuntimeParams["lock_timeout"])
	require.Equal(t, "0", cfg.RuntimeParams["idle_in_transaction_session_timeout"])
}

func TestMigrationDBProfileMatchesDesignDefaults(t *testing.T) {
	require.NoError(t, os.Unsetenv(EnvMigrationStatementTimeout))
	profile, err := MigrationDBProfile()
	require.NoError(t, err)
	require.Equal(t, 10*time.Minute, profile.StatementTimeout)
	require.Equal(t, 10*time.Second, profile.LockTimeout)
	require.Equal(t, time.Duration(0), profile.IdleInTransactionSessionTimeout)
	require.Equal(t, 2, profile.MaxOpenConns)
	require.Nil(t, profile.Registerer, "마이그레이션 풀은 collector를 등록하지 않는다")
}

func TestServiceDBProfileMatchesDesignDefaults(t *testing.T) {
	require.NoError(t, os.Unsetenv(EnvDBMaxOpenConns))
	require.NoError(t, os.Unsetenv(EnvDBMaxIdleConns))
	require.NoError(t, os.Unsetenv(EnvDBConnMaxLifetime))
	require.NoError(t, os.Unsetenv(EnvDBStatementTimeout))
	require.NoError(t, os.Unsetenv(EnvDBLockTimeout))
	require.NoError(t, os.Unsetenv(EnvDBIdleTxTimeout))

	profile, err := ServiceDBProfile(nil)
	require.NoError(t, err)
	require.Equal(t, 15*time.Second, profile.StatementTimeout)
	require.Equal(t, 3*time.Second, profile.LockTimeout)
	require.Equal(t, 30*time.Second, profile.IdleInTransactionSessionTimeout)
	require.Equal(t, 25, profile.MaxOpenConns)
}

// Task 6 리뷰 복구 항목(설계 §3): 서비스 풀의 세 시간 상한이 env로 열려 있다.
func TestServiceDBProfileTimeoutsFromEnv(t *testing.T) {
	requireUnsetEnv(t, EnvDBStatementTimeout)
	requireUnsetEnv(t, EnvDBLockTimeout)
	requireUnsetEnv(t, EnvDBIdleTxTimeout)

	t.Setenv(EnvDBStatementTimeout, "20s")
	t.Setenv(EnvDBLockTimeout, "5s")
	t.Setenv(EnvDBIdleTxTimeout, "45s")

	profile, err := ServiceDBProfile(nil)
	require.NoError(t, err)
	require.Equal(t, 20*time.Second, profile.StatementTimeout)
	require.Equal(t, 5*time.Second, profile.LockTimeout)
	require.Equal(t, 45*time.Second, profile.IdleInTransactionSessionTimeout)
}

func TestServiceDBProfileTimeoutsRejectInvalidEnv(t *testing.T) {
	for _, key := range []string{EnvDBStatementTimeout, EnvDBLockTimeout, EnvDBIdleTxTimeout} {
		t.Run(key, func(t *testing.T) {
			requireUnsetEnv(t, EnvDBStatementTimeout)
			requireUnsetEnv(t, EnvDBLockTimeout)
			requireUnsetEnv(t, EnvDBIdleTxTimeout)
			t.Setenv(key, "0s")

			_, err := ServiceDBProfile(nil)
			require.Error(t, err)
		})
	}
}

func TestReconciliationDBProfileMatchesDesignDefaults(t *testing.T) {
	require.NoError(t, os.Unsetenv(EnvReconciliationStatementTimeout))
	profile, err := ReconciliationDBProfile(nil)
	require.NoError(t, err)
	require.Equal(t, 5*time.Minute, profile.StatementTimeout)
	require.Equal(t, 3*time.Second, profile.LockTimeout)
	require.Equal(t, time.Duration(0), profile.IdleInTransactionSessionTimeout)
	require.Equal(t, 2, profile.MaxOpenConns)
}

// 세 프로필의 statement_timeout이 서로 다른 값이어야 한다 — 실수로 같은 값을
// 복사했는지 확인하는 대조군.
func TestThreeDBProfilesHaveDistinctStatementTimeouts(t *testing.T) {
	require.NoError(t, os.Unsetenv(EnvMigrationStatementTimeout))
	require.NoError(t, os.Unsetenv(EnvReconciliationStatementTimeout))

	migration, err := MigrationDBProfile()
	require.NoError(t, err)
	service, err := ServiceDBProfile(nil)
	require.NoError(t, err)
	reconciliation, err := ReconciliationDBProfile(nil)
	require.NoError(t, err)

	require.NotEqual(t, migration.StatementTimeout, service.StatementTimeout)
	require.NotEqual(t, service.StatementTimeout, reconciliation.StatementTimeout)
	require.NotEqual(t, migration.StatementTimeout, reconciliation.StatementTimeout)
}
