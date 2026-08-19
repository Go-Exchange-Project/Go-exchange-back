package dbmigration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrationsDirContainsGooseMigration(t *testing.T) {
	path := migrationsDir()

	info, err := os.Stat(filepath.Join(path, "001_constraints.sql"))

	require.NoError(t, err)
	assert.False(t, info.IsDir())
}

func TestMigrationsDirUsesEnvOverride(t *testing.T) {
	t.Setenv(EnvMigrationsDir, "/app/migrations")

	assert.Equal(t, "/app/migrations", migrationsDir())
}

// 006은 운영 테이블에 온라인으로 인덱스를 만든다. CONCURRENTLY는 트랜잭션 밖에서만
// 돌고, 중단되면 같은 이름의 invalid 인덱스를 남긴다. 그래서 (1) NO TRANSACTION,
// (2) IF NOT EXISTS, (3) 같은 Up 안의 카탈로그 검증과 RAISE EXCEPTION이 한 세트다.
// 셋 중 하나라도 빠지면 "이름만 있는 invalid 인덱스"가 goose version 6으로 기록된다.
func TestTradesBuyOrderIDIndexMigrationIsConcurrentAndValidated(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(migrationsDir(), "006_trades_buy_order_id_index.sql"))
	require.NoError(t, err)
	sql := string(raw)

	assert.True(t, strings.HasPrefix(sql, "-- +goose NO TRANSACTION\n"))
	assert.Contains(t, sql, "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_trades_buy_order_id")
	assert.Contains(t, sql, "ON trades (buy_order_id)")
	assert.Contains(t, sql, "indisready")
	assert.Contains(t, sql, "indisvalid")
	assert.Contains(t, sql, "RAISE EXCEPTION")
	assert.Contains(t, sql, "DROP INDEX CONCURRENTLY IF EXISTS idx_trades_buy_order_id")
}

// cancel_commands 스키마는 007이 단독으로 소유한다(AutoMigrate 대상이 아니다).
// IF NOT EXISTS와 조건부 ADD CONSTRAINT는 재실행·부분 적용 상태에 대한 방어다.
// UNIQUE는 부분 인덱스가 아니다 — PENDING만 막으면 command가 PROCESSED이고 정산이
// 아직 안 끝난 창에서 두 번째 command가 생겨 ORDER_RELEASE가 두 번 날 수 있다.
func TestCancelCommandsMigrationDeclaresDurableContract(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(migrationsDir(), "007_cancel_commands.sql"))
	require.NoError(t, err)
	sql := string(raw)

	assert.Contains(t, sql, "CREATE TABLE IF NOT EXISTS cancel_commands")

	// price가 없으면 worker가 matching.CancelOrderCommand를 복원할 수 없다.
	assert.Contains(t, sql, "price")
	assert.Contains(t, sql, "NUMERIC")

	assert.Contains(t, sql, "cancel_commands_order_unique")
	assert.Contains(t, sql, "UNIQUE (order_id)")
	assert.NotContains(t, sql, "UNIQUE INDEX cancel_commands_order_unique",
		"UNIQUE를 부분 인덱스로 만들면 PROCESSED 이후 창이 다시 열린다")

	assert.Contains(t, sql, "cancel_commands_status_check")
	assert.Contains(t, sql, "'PENDING'")
	assert.Contains(t, sql, "'PROCESSED'")
	assert.Contains(t, sql, "'NOOP'")

	assert.Contains(t, sql, "CREATE INDEX IF NOT EXISTS cancel_commands_pending")
	assert.Contains(t, sql, "WHERE status = 'PENDING'")
}

func TestSQLDBFromGORMRejectsNil(t *testing.T) {
	sqlDB, err := sqlDBFromGORM(nil)

	require.Error(t, err)
	assert.Nil(t, sqlDB)
}
