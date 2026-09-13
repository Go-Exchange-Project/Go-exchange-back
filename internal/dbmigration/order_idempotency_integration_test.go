// package dbmigration_test인 이유: testdb가 dbmigration을 import하므로 내부 테스트
// 패키지에서 testdb를 쓰면 import cycle이 된다.
package dbmigration_test

import (
	"strings"
	"testing"

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

	// 커밋 시점 outcome은 PENDING으로 확정된다. NULL을 허용하면 Go 모델의 값 타입과
	// 어긋나 GORM이 빈 문자열을 넣는 경로가 생긴다.
	t.Run("outcome은 NOT NULL이고 기본값이 PENDING이다", func(t *testing.T) {
		var got struct {
			IsNullable    string
			ColumnDefault *string
		}
		require.NoError(t, db.Raw(`
SELECT is_nullable, column_default
FROM information_schema.columns
WHERE table_schema = current_schema()
  AND table_name = 'order_idempotency_keys'
  AND column_name = 'outcome'`).Scan(&got).Error)

		assert.Equal(t, "NO", got.IsNullable)
		require.NotNil(t, got.ColumnDefault)
		assert.Contains(t, *got.ColumnDefault, "'PENDING'")
	})

	t.Run("goose version이 8이다", func(t *testing.T) {
		var version int64
		require.NoError(t, db.Raw(
			`SELECT max(version_id) FROM goose_db_version WHERE is_applied`).Scan(&version).Error)
		assert.GreaterOrEqual(t, version, int64(8))
	})
}

// TestOrderIdempotencyMigrationFailsOnWrongSameNamedConstraint과
// TestOrderIdempotencyMigrationFailsOnWrongSameNamedIndex는
// order_idempotency_migration_isolation_test.go(package dbmigration)에 있다.
// 이 파일이 아닌 이유: 이 두 테스트는 goose_db_version의 버전 8 행을 지운 뒤
// 008을 다시 적용해야 하는데, 이 파일이 쓰는 공유 testdb 스키마는 9·10이 이미
// 적용돼 있어 goose가 그 재적용을 거부한다 — 격리된 임시 스키마가 필요하고,
// 그 스키마 안에서는 goose.UpTo(..., 8)을 이 패키지 안에서 직접 불러야 해서
// (dbmigration 패키지 밖에서는 비공개 migrationsDir()을 쓸 수 없다) 내부 테스트
// 패키지(package dbmigration)에 둔다.
