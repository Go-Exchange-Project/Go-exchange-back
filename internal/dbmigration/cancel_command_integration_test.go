// package dbmigration_test인 이유: testdb가 dbmigration을 import하므로 내부 테스트
// 패키지에서 testdb를 쓰면 import cycle이 된다.
package dbmigration_test

import (
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 정적 문자열 검사(runner_test.go)는 migration이 무엇을 쓰려 했는지만 본다.
// 007이 실제로 만든 테이블과 제약이 계획과 같은지는 카탈로그로만 확인할 수 있다.
// 이 테이블은 AutoMigrate 대상이 아니므로 여기 보이는 것은 전부 007의 결과다.
func TestCancelCommandsIntegration(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)

	t.Run("price는 NOT NULL numeric이다", func(t *testing.T) {
		var got struct {
			DataType   string
			IsNullable string
		}

		require.NoError(t, db.Raw(`
SELECT data_type, is_nullable
FROM information_schema.columns
WHERE table_schema = current_schema()
  AND table_name = 'cancel_commands'
  AND column_name = 'price'`).Scan(&got).Error)

		// price가 없으면 worker가 엔진 명령을 복원할 수 없다.
		require.Equal(t, "numeric", got.DataType,
			"price 컬럼이 없거나 numeric이 아니다 — goose version과 007 Up 로그를 먼저 확인한다")
		assert.Equal(t, "NO", got.IsNullable)
	})

	t.Run("order_id UNIQUE는 부분 인덱스가 아니다", func(t *testing.T) {
		var got struct {
			ConType    string
			Definition string
			IndPred    *string
			IndIsUnique bool
			IndNKeyAtts int
		}

		require.NoError(t, db.Raw(`
SELECT constraint_meta.contype                          AS con_type,
       pg_get_constraintdef(constraint_meta.oid)        AS definition,
       pg_get_expr(index_meta.indpred, index_meta.indrelid) AS ind_pred,
       index_meta.indisunique                           AS ind_is_unique,
       index_meta.indnkeyatts                           AS ind_n_key_atts
FROM pg_constraint constraint_meta
JOIN pg_class table_rel ON table_rel.oid = constraint_meta.conrelid
JOIN pg_namespace table_ns ON table_ns.oid = table_rel.relnamespace
JOIN pg_index index_meta ON index_meta.indexrelid = constraint_meta.conindid
WHERE table_ns.nspname = current_schema()
  AND table_rel.relname = 'cancel_commands'
  AND constraint_meta.conname = 'cancel_commands_order_unique'`).Scan(&got).Error)

		require.Equal(t, "u", got.ConType,
			"cancel_commands_order_unique가 UNIQUE 제약이 아니다")
		assert.Equal(t, "UNIQUE (order_id)", got.Definition)
		assert.True(t, got.IndIsUnique)
		assert.Equal(t, 1, got.IndNKeyAtts)

		// 부분 UNIQUE는 command가 PROCESSED이고 정산 전인 창을 다시 연다.
		assert.Nil(t, got.IndPred, "UNIQUE가 부분 인덱스다 — 중복 command 창이 열린다")
	})

	t.Run("status CHECK는 세 상태만 허용한다", func(t *testing.T) {
		var definition string

		require.NoError(t, db.Raw(`
SELECT pg_get_constraintdef(constraint_meta.oid)
FROM pg_constraint constraint_meta
JOIN pg_class table_rel ON table_rel.oid = constraint_meta.conrelid
JOIN pg_namespace table_ns ON table_ns.oid = table_rel.relnamespace
WHERE table_ns.nspname = current_schema()
  AND table_rel.relname = 'cancel_commands'
  AND constraint_meta.conname = 'cancel_commands_status_check'`).Scan(&definition).Error)

		require.NotEmpty(t, definition, "cancel_commands_status_check가 없다")
		for _, status := range []string{"PENDING", "PROCESSED", "NOOP"} {
			assert.Contains(t, definition, status)
		}
	})

	t.Run("PENDING 스캔 인덱스만 부분이다", func(t *testing.T) {
		var predicate *string

		require.NoError(t, db.Raw(`
SELECT pg_get_expr(index_meta.indpred, index_meta.indrelid)
FROM pg_class index_rel
JOIN pg_namespace index_ns ON index_ns.oid = index_rel.relnamespace
JOIN pg_index index_meta ON index_meta.indexrelid = index_rel.oid
WHERE index_ns.nspname = current_schema()
  AND index_rel.relname = 'cancel_commands_pending'`).Scan(&predicate).Error)

		require.NotNil(t, predicate,
			"cancel_commands_pending이 없거나 부분 인덱스가 아니다")
		assert.Contains(t, *predicate, "PENDING")
	})
}
