package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

// testProfileDSN은 D1~D5의 실제 PostgreSQL 통합 테스트용 DSN을 돌려준다.
// 다른 통합 테스트(internal/testdb.OpenIntegrationDB)와 같은 env를 쓰지만,
// 여기서는 전체 스키마 마이그레이트가 필요 없는 가벼운 프로필 연결만 쓴다.
func testProfileDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("GOEXCHANGE_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("GOEXCHANGE_TEST_DATABASE_DSN is not set; skipping Postgres integration test")
	}
	return dsn
}

func requirePGCode(t *testing.T, err error, code string) {
	t.Helper()
	require.Error(t, err)
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "expected *pgconn.PgError, got %T: %v", err, err)
	require.Equal(t, code, pgErr.Code)
}

// D1: statement_timeout이 짧은 프로필에서 pg_sleep이 57014로 끝나고, 같은
// *sql.Conn에서 다음 평범한 쿼리가 성공한다 — 연결 재사용 계약 자체를 증명한다
// (새 연결로 우연히 통과하는 것이 아니다).
func TestStatementTimeoutCancelsSlowQueryAndConnectionIsReusable(t *testing.T) {
	dsn := testProfileDSN(t)
	profile := DBTimeoutProfile{
		Name:                            "test-statement-timeout",
		StatementTimeout:                100 * time.Millisecond,
		LockTimeout:                     2 * time.Second,
		IdleInTransactionSessionTimeout: 0,
		MaxOpenConns:                    2,
		MaxIdleConns:                    2,
	}
	_, sqlDB, err := OpenDBWithProfile(dsn, profile)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	ctx := context.Background()
	conn, err := sqlDB.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()

	_, err = conn.ExecContext(ctx, "SELECT pg_sleep(1)")
	requirePGCode(t, err, "57014")

	var one int
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT 1").Scan(&one),
		"같은 커넥션에서 다음 쿼리가 정상 동작해야 한다(풀에서 새 연결을 받은 게 아니다)")
	require.Equal(t, 1, one)
}

// D2: lock_timeout이 짧은 프로필에서, 연결 두 개를 분리해 한쪽이 행을 잠그고
// 다른 쪽 FOR UPDATE가 55P03으로 끝난다.
func TestLockTimeoutOnContendedRowReturns55P03(t *testing.T) {
	dsn := testProfileDSN(t)
	profile := DBTimeoutProfile{
		Name:                            "test-lock-timeout",
		StatementTimeout:                5 * time.Second,
		LockTimeout:                     100 * time.Millisecond,
		IdleInTransactionSessionTimeout: 0,
		MaxOpenConns:                    4,
		MaxIdleConns:                    4,
	}
	_, sqlDB, err := OpenDBWithProfile(dsn, profile)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	ctx := context.Background()
	tableName := fmt.Sprintf("db_profile_lock_test_%d", time.Now().UnixNano())
	_, err = sqlDB.ExecContext(ctx, "CREATE TABLE "+tableName+" (id int PRIMARY KEY, v int)")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = sqlDB.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+tableName) })
	_, err = sqlDB.ExecContext(ctx, "INSERT INTO "+tableName+" (id, v) VALUES (1, 1)")
	require.NoError(t, err)

	lockerConn, err := sqlDB.Conn(ctx)
	require.NoError(t, err)
	defer lockerConn.Close()
	lockerTx, err := lockerConn.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = lockerTx.Rollback() }()
	_, err = lockerTx.ExecContext(ctx, "SELECT * FROM "+tableName+" WHERE id = 1 FOR UPDATE")
	require.NoError(t, err)

	waiterConn, err := sqlDB.Conn(ctx)
	require.NoError(t, err)
	defer waiterConn.Close()
	waiterTx, err := waiterConn.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = waiterTx.Rollback() }()
	_, err = waiterTx.ExecContext(ctx, "SELECT * FROM "+tableName+" WHERE id = 1 FOR UPDATE")
	requirePGCode(t, err, "55P03")
}

// D3: SHOW 3종 중 하나라도 기대와 다르면 초기화가 실패하고, 열었던 *sql.DB가
// 닫힌다(Ping이 오류를 반환한다).
func TestOpenDBWithProfileFailsWhenAnyShowValueMismatches(t *testing.T) {
	dsn := testProfileDSN(t)
	applyProfile := DBTimeoutProfile{
		Name:                            "test-apply",
		StatementTimeout:                111 * time.Millisecond,
		LockTimeout:                     222 * time.Millisecond,
		IdleInTransactionSessionTimeout: 0,
		MaxOpenConns:                    2,
		MaxIdleConns:                    2,
	}
	expectProfile := applyProfile
	expectProfile.LockTimeout = 333 * time.Millisecond // 실제 적용값과 다르게 기대해 불일치를 만든다.

	gormDB, sqlDB, err := openDBWithProfileExpecting(dsn, applyProfile, expectProfile)
	require.Error(t, err)
	require.ErrorContains(t, err, "lock_timeout")
	require.Nil(t, gormDB)
	require.NotNil(t, sqlDB, "닫혔더라도 호출자가 확인할 수 있도록 *sql.DB는 돌려준다")
	require.Error(t, sqlDB.Ping(), "검증 실패 시 *sql.DB가 close돼 있어야 한다")
}

// D4: 서비스 풀과 검산 풀의 SHOW statement_timeout 값이 서로 다르다.
func TestServiceAndReconciliationPoolsHaveDifferentStatementTimeout(t *testing.T) {
	dsn := testProfileDSN(t)

	serviceProfile, err := ServiceDBProfile(nil)
	require.NoError(t, err)
	serviceProfile.MaxOpenConns, serviceProfile.MaxIdleConns = 2, 2
	_, serviceDB, err := OpenDBWithProfile(dsn, serviceProfile)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serviceDB.Close() })

	requireUnsetEnv(t, EnvReconciliationStatementTimeout)
	reconciliationProfile, err := ReconciliationDBProfile(nil)
	require.NoError(t, err)
	_, reconciliationDB, err := OpenDBWithProfile(dsn, reconciliationProfile)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reconciliationDB.Close() })

	var serviceTimeout, reconciliationTimeout string
	require.NoError(t, serviceDB.QueryRow("SHOW statement_timeout").Scan(&serviceTimeout))
	require.NoError(t, reconciliationDB.QueryRow("SHOW statement_timeout").Scan(&reconciliationTimeout))
	require.NotEqual(t, serviceTimeout, reconciliationTimeout)
}

// D5: 마이그레이션이 끝난 뒤 그 풀의 *sql.DB가 닫혀 있다 — Ping이 오류를
// 반환하는 것으로 판정한다(정확한 오류 문자열은 Go 버전에 의존하므로 비교하지 않는다).
func TestMigrationDBProfileClosedAfterUse(t *testing.T) {
	dsn := testProfileDSN(t)
	requireUnsetEnv(t, EnvMigrationStatementTimeout)
	profile, err := MigrationDBProfile()
	require.NoError(t, err)

	_, sqlDB, err := OpenDBWithProfile(dsn, profile)
	require.NoError(t, err)

	require.NoError(t, sqlDB.Ping(), "닫기 전에는 정상 동작해야 한다")
	require.NoError(t, sqlDB.Close())
	require.Error(t, sqlDB.Ping(), "닫힌 뒤에는 Ping이 오류를 반환해야 한다")
}
