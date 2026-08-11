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
// 실제로 만들어진 인덱스가 계획과 같은지는 카탈로그로만 확인할 수 있다 —
// CONCURRENTLY가 중단되면 SQL은 그대로인데 indisvalid=false인 인덱스가 남는다.
func TestTradesBuyOrderIDIndexIntegration(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)

	var got struct {
		IndexSchema   string
		IndexName     string
		TableSchema   string
		TableName     string
		AccessMethod  string
		FirstColumn   string
		Indisready    bool
		Indisvalid    bool
		Indisunique   bool
		Indnkeyatts   int
		Indnatts      int
		NoExpressions bool
		NoPredicate   bool
		Definition    string
	}

	require.NoError(t, db.Raw(`
SELECT index_ns.nspname                AS index_schema,
       index_rel.relname               AS index_name,
       table_ns.nspname                AS table_schema,
       table_rel.relname               AS table_name,
       access_method.amname            AS access_method,
       column_meta.attname             AS first_column,
       index_meta.indisready           AS indisready,
       index_meta.indisvalid           AS indisvalid,
       index_meta.indisunique          AS indisunique,
       index_meta.indnkeyatts          AS indnkeyatts,
       index_meta.indnatts             AS indnatts,
       (index_meta.indexprs IS NULL)   AS no_expressions,
       (index_meta.indpred IS NULL)    AS no_predicate,
       pg_get_indexdef(index_rel.oid)  AS definition
FROM pg_class index_rel
JOIN pg_namespace index_ns ON index_ns.oid = index_rel.relnamespace
JOIN pg_index index_meta ON index_meta.indexrelid = index_rel.oid
JOIN pg_class table_rel ON table_rel.oid = index_meta.indrelid
JOIN pg_namespace table_ns ON table_ns.oid = table_rel.relnamespace
JOIN pg_am access_method ON access_method.oid = index_rel.relam
JOIN pg_attribute column_meta
  ON column_meta.attrelid = table_rel.oid
 AND column_meta.attnum = index_meta.indkey[0]
WHERE index_ns.nspname = current_schema()
  AND index_rel.relname = 'idx_trades_buy_order_id'`).Scan(&got).Error)

	require.Equal(t, "idx_trades_buy_order_id", got.IndexName,
		"migration 적용 후에도 인덱스가 없다 — goose version과 Up 로그를 먼저 확인한다")

	assert.Equal(t, "public", got.IndexSchema)
	assert.Equal(t, "public", got.TableSchema)
	assert.Equal(t, "trades", got.TableName)
	assert.Equal(t, "btree", got.AccessMethod)
	assert.Equal(t, "buy_order_id", got.FirstColumn)

	// indisvalid=false는 중단된 concurrent build의 잔해다. 이름만 보고 성공으로
	// 판단하면 안 되는 이유이고, migration이 RAISE EXCEPTION으로 막는 대상이다.
	assert.True(t, got.Indisready, "indisready=false — 중단된 concurrent build 잔해")
	assert.True(t, got.Indisvalid, "indisvalid=false — 인덱스가 플래너에 쓰이지 않는다")

	assert.False(t, got.Indisunique)
	assert.Equal(t, 1, got.Indnkeyatts)
	assert.Equal(t, 1, got.Indnatts)
	assert.True(t, got.NoExpressions)
	assert.True(t, got.NoPredicate)
	assert.Contains(t, got.Definition, "ON public.trades USING btree (buy_order_id)")
}
