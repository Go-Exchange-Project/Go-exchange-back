// package dbmigration_test인 이유: testdb가 dbmigration을 import하므로 내부 테스트
// 패키지에서 testdb를 쓰면 import cycle이 된다.
package dbmigration_test

import (
	"strings"
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

	// 008의 카탈로그 검증이 보는 조건을 그대로 단언한다. 하나라도 느슨하면
	// 검증 조건이 빠져도 이 테스트가 통과한다.
	t.Run("부분 인덱스 정의가 정확하다", func(t *testing.T) {
		var got struct {
			AccessMethod    string
			FirstColumn     string
			Indisready      bool
			Indisvalid      bool
			Indisunique     bool
			Indnkeyatts     int
			Indnatts        int
			HasNoExpression bool
			Predicate       *string
		}
		require.NoError(t, db.Raw(`
SELECT am.amname AS access_method,
       a.attname AS first_column,
       i.indisready, i.indisvalid, i.indisunique, i.indnkeyatts, i.indnatts,
       (i.indexprs IS NULL) AS has_no_expression,
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
		assert.Equal(t, 1, got.Indnatts, "INCLUDE 컬럼이 붙으면 008의 검증이 실패한다")
		assert.True(t, got.HasNoExpression, "표현식 인덱스가 아니어야 한다")
		require.NotNil(t, got.Predicate)
		assert.Equal(t, "(outcome = 'PENDING'::text)", *got.Predicate)
	})

	// gauge 조회는 이 CHECK를 신뢰한다. HTTP 검증만으로는 다른 경로의 INSERT를 못 막는다.
	t.Run("키 길이 CHECK가 공백 제외 1~128자를 강제한다", func(t *testing.T) {
		insert := func(key string) error {
			return db.Exec(`
INSERT INTO order_idempotency_keys (user_id, idempotency_key, fingerprint, fingerprint_version)
VALUES (?, ?, ?, ?)`, 999999, key, "fp", 1).Error
		}

		valid := strings.Repeat("k", 128)
		require.NoError(t, insert(valid))
		t.Cleanup(func() {
			require.NoError(t, db.Exec(
				`DELETE FROM order_idempotency_keys WHERE user_id = ?`, 999999).Error)
		})

		assert.Error(t, insert("   "), "공백만 있는 키가 통과했다")
		assert.Error(t, insert(strings.Repeat("k", 129)), "129자 키가 통과했다")

		// length()는 바이트가 아니라 문자를 센다. 서버 검증도 rune으로 세야 두 단위가 맞는다.
		multibyte := strings.Repeat("가", 128) // 384바이트
		require.NoError(t, insert(multibyte), "128자 멀티바이트 키가 거부됐다 — CHECK가 바이트를 센다")
		assert.Error(t, insert(strings.Repeat("가", 129)), "129자 멀티바이트 키가 통과했다")
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
	// 이 테스트는 goose version 8 행을 지우고 008을 실패시킨다. 다음 테스트가 우연히
	// 복구해 주기를 기대하지 않고, 여기서 008을 다시 적용해 인덱스와 version을 모두 되돌린다.
	t.Cleanup(func() {
		require.NoError(t, db.Exec(`DROP INDEX IF EXISTS order_idempotency_pending_updated_at`).Error)
		require.NoError(t, dbmigration.Up(db), "cleanup에서 008 재적용이 실패했다")

		var applied int64
		require.NoError(t, db.Raw(
			`SELECT count(*) FROM goose_db_version WHERE version_id = 8 AND is_applied`).Scan(&applied).Error)
		require.EqualValues(t, 1, applied, "cleanup 후에도 version 8이 복구되지 않았다")
	})

	require.NoError(t, db.Exec(`DELETE FROM goose_db_version WHERE version_id = 8`).Error)

	err := dbmigration.Up(db)

	require.Error(t, err, "잘못된 동명 인덱스인데 migration이 성공했다")
	var applied int64
	require.NoError(t, db.Raw(
		`SELECT count(*) FROM goose_db_version WHERE version_id = 8 AND is_applied`).Scan(&applied).Error)
	assert.Zero(t, applied, "실패했는데 version 8이 기록됐다")
}
