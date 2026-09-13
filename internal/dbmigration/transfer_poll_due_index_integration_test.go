// package dbmigration_test인 이유: testdb가 dbmigration을 import하므로 내부 테스트
// 패키지에서 testdb를 쓰면 import cycle이 된다.
package dbmigration_test

import (
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 정적 문자열 검사(runner_test.go)는 011이 무엇을 쓰려 했는지만 본다. 실제로
// 만들어진 인덱스가 계획과 같은지는 카탈로그로만 확인할 수 있다 — CONCURRENTLY가
// 중단되면 SQL은 그대로인데 indisvalid=false인 인덱스가 남는다.
//
// pg_get_indexdef 전체 문자열을 정확 일치로 단언하지 않는다 — 테스트 DB는
// postgres:16, docker-compose.prod.yml은 postgres:18이라 렌더링이 다를 수
// 있다. 구조 필드(pg_index)로 검증하고, 문자열은 실패 메시지에만 쓴다.
func TestTransferPollDueIndexIntegration(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)

	var got struct {
		IndexSchema   string
		IndexName     string
		TableSchema   string
		TableName     string
		AccessMethod  string
		FirstColumn   string
		FirstOption   int
		SecondColumn  string
		SecondOption  int
		Indisready    bool
		Indisvalid    bool
		Indisunique   bool
		Indnkeyatts   int
		Indnatts      int
		NoExpressions bool
		Predicate     *string
		Definition    string
	}

	require.NoError(t, db.Raw(`
SELECT index_ns.nspname                AS index_schema,
       index_rel.relname               AS index_name,
       table_ns.nspname                AS table_schema,
       table_rel.relname               AS table_name,
       access_method.amname            AS access_method,
       first_column.attname            AS first_column,
       index_meta.indoption[0]         AS first_option,
       second_column.attname           AS second_column,
       index_meta.indoption[1]         AS second_option,
       index_meta.indisready           AS indisready,
       index_meta.indisvalid           AS indisvalid,
       index_meta.indisunique          AS indisunique,
       index_meta.indnkeyatts          AS indnkeyatts,
       index_meta.indnatts             AS indnatts,
       (index_meta.indexprs IS NULL)   AS no_expressions,
       pg_get_expr(index_meta.indpred, index_meta.indrelid) AS predicate,
       pg_get_indexdef(index_rel.oid)  AS definition
FROM pg_class index_rel
JOIN pg_namespace index_ns ON index_ns.oid = index_rel.relnamespace
JOIN pg_index index_meta ON index_meta.indexrelid = index_rel.oid
JOIN pg_class table_rel ON table_rel.oid = index_meta.indrelid
JOIN pg_namespace table_ns ON table_ns.oid = table_rel.relnamespace
JOIN pg_am access_method ON access_method.oid = index_rel.relam
JOIN pg_attribute first_column
  ON first_column.attrelid = table_rel.oid
 AND first_column.attnum = index_meta.indkey[0]
JOIN pg_attribute second_column
  ON second_column.attrelid = table_rel.oid
 AND second_column.attnum = index_meta.indkey[1]
WHERE index_ns.nspname = current_schema()
  AND index_rel.relname = 'transfer_requests_due_poll_idx'`).Scan(&got).Error)

	require.Equal(t, "transfer_requests_due_poll_idx", got.IndexName,
		"migration 적용 후에도 인덱스가 없다 — goose version과 011 Up 로그를 먼저 확인한다")

	assert.Equal(t, "public", got.IndexSchema)
	assert.Equal(t, "public", got.TableSchema)
	assert.Equal(t, "transfer_requests", got.TableName)
	assert.Equal(t, "btree", got.AccessMethod)

	// indisvalid=false는 중단된 concurrent build의 잔해다. 이름만 보고 성공으로
	// 판단하면 안 되는 이유이고, migration이 RAISE EXCEPTION으로 막는 대상이다.
	assert.True(t, got.Indisready, "indisready=false — 중단된 concurrent build 잔해")
	assert.True(t, got.Indisvalid, "indisvalid=false — 인덱스가 플래너에 쓰이지 않는다")
	assert.False(t, got.Indisunique)

	assert.Equal(t, 2, got.Indnkeyatts)
	assert.Equal(t, 2, got.Indnatts)

	assert.Equal(t, "next_check_at", got.FirstColumn)
	// indoption 비트: 0=DESC, 1=NULLS FIRST. 2는 ASC(비트 0 없음)+NULLS FIRST다.
	// PostgreSQL의 ASC 기본값은 NULLS LAST이므로, 이 비트가 없으면 인덱스가
	// DueForCheck의 ORDER BY next_check_at ASC NULLS FIRST를 만들지 못한다.
	assert.Equal(t, 2, got.FirstOption, "next_check_at이 ASC NULLS FIRST가 아니다: definition=%s", got.Definition)

	assert.Equal(t, "id", got.SecondColumn)
	assert.Equal(t, 0, got.SecondOption, "id가 평범한 ASC가 아니다: definition=%s", got.Definition)

	assert.True(t, got.NoExpressions)
	require.NotNil(t, got.Predicate)
	assert.Contains(t, *got.Predicate, "RECEIVED")
	assert.Contains(t, *got.Predicate, "PROCESSING")

	// 옛 인덱스는 검증 뒤에 지워진다 — 남아 있으면 DueForCheck에 쓰이지 않으면서
	// 쓰기 비용만 남는다.
	var oldIndexCount int64
	require.NoError(t, db.Raw(`
SELECT count(*) FROM pg_class
WHERE relname = 'transfer_requests_next_check_at_idx'
  AND relnamespace = current_schema()::regnamespace`).Scan(&oldIndexCount).Error)
	assert.Zero(t, oldIndexCount, "옛 transfer_requests_next_check_at_idx가 남아 있다")
}
