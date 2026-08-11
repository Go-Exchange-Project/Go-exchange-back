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

func TestSQLDBFromGORMRejectsNil(t *testing.T) {
	sqlDB, err := sqlDBFromGORM(nil)

	require.Error(t, err)
	assert.Nil(t, sqlDB)
}
