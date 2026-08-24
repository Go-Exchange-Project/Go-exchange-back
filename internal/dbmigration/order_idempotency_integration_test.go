// package dbmigration_test인 이유: testdb가 dbmigration을 import하므로 내부 테스트
// 패키지에서 testdb를 쓰면 import cycle이 된다.
package dbmigration_test

import (
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/dbmigration"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOrderIdempotencyKeysIntegration(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)

	t.Run("UNIQUE는 (user_id, idempotency_key)다", func(t *testing.T) {
		var definition string
		require.NoError(t, db.Raw(`
SELECT pg_get_constraintdef(c.oid)
FROM pg_constraint c
JOIN pg_class t ON t.oid = c.conrelid
WHERE t.relname = 'order_idempotency_keys'
  AND c.conname = 'order_idempotency_keys_user_key_unique'`).Scan(&definition).Error)
		require.NotEmpty(t, definition)
		assert.Equal(t, "UNIQUE (user_id, idempotency_key)", definition)
	})

	t.Run("부분 인덱스 정의가 정확하다", func(t *testing.T) {
		var got struct {
			AccessMethod string
			FirstColumn  string
			Indisready   bool
			Indisvalid   bool
			Indisunique  bool
			Indnkeyatts  int
			Predicate    *string
		}
		require.NoError(t, db.Raw(`
SELECT am.amname AS access_method,
       a.attname AS first_column,
       i.indisready, i.indisvalid, i.indisunique, i.indnkeyatts,
       pg_get_expr(i.indpred, i.indrelid) AS predicate
FROM pg_class c
JOIN pg_index i ON i.indexrelid = c.oid
JOIN pg_am am ON am.oid = c.relam
JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
WHERE c.relname = 'order_idempotency_pending_updated_at'`).Scan(&got).Error)

		require.Equal(t, "btree", got.AccessMethod, "인덱스가 없다 — goose version과 008 Up 로그를 먼저 확인한다")
		assert.Equal(t, "updated_at", got.FirstColumn)
		assert.True(t, got.Indisready)
		assert.True(t, got.Indisvalid)
		assert.False(t, got.Indisunique)
		assert.Equal(t, 1, got.Indnkeyatts)
		require.NotNil(t, got.Predicate)
		assert.Contains(t, *got.Predicate, "PENDING")
	})

	t.Run("goose version이 8이다", func(t *testing.T) {
		var version int64
		require.NoError(t, db.Raw(
			`SELECT max(version_id) FROM goose_db_version WHERE is_applied`).Scan(&version).Error)
		assert.GreaterOrEqual(t, version, int64(8))
	})
}

// 같은 이름의 잘못된 인덱스가 있으면 migration이 실패하고 version 8이 기록되지 않아야
// 한다. IF NOT EXISTS만으로는 조용히 통과한다.
func TestOrderIdempotencyMigrationFailsOnWrongSameNamedIndex(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)

	require.NoError(t, db.Exec(`DROP INDEX IF EXISTS order_idempotency_pending_updated_at`).Error)
	// predicate 없는 전체 인덱스를 같은 이름으로 만든다.
	require.NoError(t, db.Exec(
		`CREATE INDEX order_idempotency_pending_updated_at ON order_idempotency_keys (updated_at)`).Error)
	t.Cleanup(func() {
		require.NoError(t, db.Exec(`DROP INDEX IF EXISTS order_idempotency_pending_updated_at`).Error)
		require.NoError(t, db.Exec(
			`CREATE INDEX IF NOT EXISTS order_idempotency_pending_updated_at
			 ON order_idempotency_keys (updated_at) WHERE outcome = 'PENDING'`).Error)
	})

	require.NoError(t, db.Exec(`DELETE FROM goose_db_version WHERE version_id = 8`).Error)

	err := dbmigration.Up(db)

	require.Error(t, err, "잘못된 동명 인덱스인데 migration이 성공했다")
	var applied int64
	require.NoError(t, db.Raw(
		`SELECT count(*) FROM goose_db_version WHERE version_id = 8 AND is_applied`).Scan(&applied).Error)
	assert.Zero(t, applied, "실패했는데 version 8이 기록됐다")
}
