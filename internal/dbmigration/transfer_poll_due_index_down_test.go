package dbmigration

import (
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// indexExistsInCurrentSchema는 이름만으로 존재 여부를 본다 — 상세 조회 전에
// "아예 없다"와 "있는데 정의가 다르다"를 구분해야 실패 메시지가 정확하다.
func indexExistsInCurrentSchema(t *testing.T, db *gorm.DB, name string) bool {
	t.Helper()

	var exists bool
	require.NoError(t, db.Raw(`
SELECT EXISTS (
    SELECT 1 FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = current_schema() AND c.relname = ?
)`, name).Scan(&exists).Error)
	return exists
}

// assertDuePollIndexValid는 011이 만드는 transfer_requests_due_poll_idx(next_check_at,
// id 2열, RECEIVED·PROCESSING predicate)가 유효한 상태로 있는지 본다 —
// TestTransferPollDueIndexIntegration과 같은 구조 필드를 본다.
func assertDuePollIndexValid(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.True(t, indexExistsInCurrentSchema(t, db, "transfer_requests_due_poll_idx"),
		"transfer_requests_due_poll_idx가 없다")

	var got struct {
		AccessMethod string
		FirstColumn  string
		FirstOption  int
		SecondColumn string
		SecondOption int
		Indisready   bool
		Indisvalid   bool
		Indisunique  bool
		Indnkeyatts  int
		Indnatts     int
		Predicate    *string
	}
	require.NoError(t, db.Raw(`
SELECT access_method.amname AS access_method,
       first_column.attname AS first_column,
       index_meta.indoption[0] AS first_option,
       second_column.attname AS second_column,
       index_meta.indoption[1] AS second_option,
       index_meta.indisready AS indisready,
       index_meta.indisvalid AS indisvalid,
       index_meta.indisunique AS indisunique,
       index_meta.indnkeyatts AS indnkeyatts,
       index_meta.indnatts AS indnatts,
       pg_get_expr(index_meta.indpred, index_meta.indrelid) AS predicate
FROM pg_class index_rel
JOIN pg_namespace index_ns ON index_ns.oid = index_rel.relnamespace
JOIN pg_index index_meta ON index_meta.indexrelid = index_rel.oid
JOIN pg_class table_rel ON table_rel.oid = index_meta.indrelid
JOIN pg_am access_method ON access_method.oid = index_rel.relam
JOIN pg_attribute first_column
  ON first_column.attrelid = table_rel.oid AND first_column.attnum = index_meta.indkey[0]
JOIN pg_attribute second_column
  ON second_column.attrelid = table_rel.oid AND second_column.attnum = index_meta.indkey[1]
WHERE index_ns.nspname = current_schema()
  AND index_rel.relname = 'transfer_requests_due_poll_idx'`).Scan(&got).Error)

	assert.Equal(t, "btree", got.AccessMethod)
	assert.True(t, got.Indisready, "indisready=false — 중단된 concurrent build 잔해")
	assert.True(t, got.Indisvalid, "indisvalid=false — 인덱스가 플래너에 쓰이지 않는다")
	assert.False(t, got.Indisunique)
	assert.Equal(t, 2, got.Indnkeyatts)
	assert.Equal(t, 2, got.Indnatts)
	assert.Equal(t, "next_check_at", got.FirstColumn)
	assert.Equal(t, 2, got.FirstOption, "next_check_at이 ASC NULLS FIRST가 아니다")
	assert.Equal(t, "id", got.SecondColumn)
	assert.Equal(t, 0, got.SecondOption)
	require.NotNil(t, got.Predicate)
	assert.Contains(t, *got.Predicate, "RECEIVED")
	assert.Contains(t, *got.Predicate, "PROCESSING")
}

// assertOldNextCheckAtIndexValid는 009가 만들던 원래 transfer_requests_next_check_at_idx
// (next_check_at 1열, PROCESSING만) 정의로 되돌아갔는지 본다.
func assertOldNextCheckAtIndexValid(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.True(t, indexExistsInCurrentSchema(t, db, "transfer_requests_next_check_at_idx"),
		"transfer_requests_next_check_at_idx가 없다")

	var got struct {
		AccessMethod string
		FirstColumn  string
		FirstOption  int
		Indisready   bool
		Indisvalid   bool
		Indisunique  bool
		Indnkeyatts  int
		Indnatts     int
		Predicate    *string
	}
	require.NoError(t, db.Raw(`
SELECT access_method.amname AS access_method,
       first_column.attname AS first_column,
       index_meta.indoption[0] AS first_option,
       index_meta.indisready AS indisready,
       index_meta.indisvalid AS indisvalid,
       index_meta.indisunique AS indisunique,
       index_meta.indnkeyatts AS indnkeyatts,
       index_meta.indnatts AS indnatts,
       pg_get_expr(index_meta.indpred, index_meta.indrelid) AS predicate
FROM pg_class index_rel
JOIN pg_namespace index_ns ON index_ns.oid = index_rel.relnamespace
JOIN pg_index index_meta ON index_meta.indexrelid = index_rel.oid
JOIN pg_class table_rel ON table_rel.oid = index_meta.indrelid
JOIN pg_am access_method ON access_method.oid = index_rel.relam
JOIN pg_attribute first_column
  ON first_column.attrelid = table_rel.oid AND first_column.attnum = index_meta.indkey[0]
WHERE index_ns.nspname = current_schema()
  AND index_rel.relname = 'transfer_requests_next_check_at_idx'`).Scan(&got).Error)

	assert.Equal(t, "btree", got.AccessMethod)
	assert.True(t, got.Indisready, "indisready=false — 중단된 concurrent build 잔해")
	assert.True(t, got.Indisvalid, "indisvalid=false — 인덱스가 플래너에 쓰이지 않는다")
	assert.False(t, got.Indisunique)
	assert.Equal(t, 1, got.Indnkeyatts)
	assert.Equal(t, 1, got.Indnatts)
	assert.Equal(t, "next_check_at", got.FirstColumn)
	assert.Equal(t, 0, got.FirstOption, "next_check_at이 평범한 ASC(NULLS LAST)가 아니다")
	require.NotNil(t, got.Predicate)
	assert.Equal(t, "((status)::text = 'PROCESSING'::text)", *got.Predicate)
}

// 정상 경로: version 11에서 DownTo(10)은 옛 인덱스를 되살리고 새 인덱스를
// 지운다. 이어서 UpTo(11)은 그 반대다. 두 방향 모두 011의 검증 블록을 실제로
// 통과해야 한다.
func TestTransferPollDueIndexDownRestoresOldIndexThenUpRestoresNew(t *testing.T) {
	db := openIsolatedSchemaDB(t, 11)
	sqlDB, err := db.DB()
	require.NoError(t, err)

	assertDuePollIndexValid(t, db)
	assert.False(t, indexExistsInCurrentSchema(t, db, "transfer_requests_next_check_at_idx"))

	require.NoError(t, goose.DownTo(sqlDB, migrationsDir(), 10))

	version, err := goose.GetDBVersion(sqlDB)
	require.NoError(t, err)
	assert.EqualValues(t, 10, version)
	assertOldNextCheckAtIndexValid(t, db)
	assert.False(t, indexExistsInCurrentSchema(t, db, "transfer_requests_due_poll_idx"))

	require.NoError(t, goose.UpTo(sqlDB, migrationsDir(), 11))

	version, err = goose.GetDBVersion(sqlDB)
	require.NoError(t, err)
	assert.EqualValues(t, 11, version)
	assertDuePollIndexValid(t, db)
	assert.False(t, indexExistsInCurrentSchema(t, db, "transfer_requests_next_check_at_idx"))
}

// 008 테스트와 같은 기법이다: 같은 이름의 잘못된 정의를 미리 만들어 둔다 —
// CONCURRENTLY 생성이 중단돼 남기는 indisvalid=false 잔해와 같은 경로(IF NOT
// EXISTS가 재생성을 건너뛴다)를 pg_index를 직접 손대지 않고 결정적으로
// 재현한다. Down의 검증이 이걸 잡아 실패해야 하고, 그 실패 때문에 새 인덱스
// (transfer_requests_due_poll_idx)를 지우는 마지막 문장에 도달하지 않아야
// 한다 — 유효한 조회 인덱스가 하나도 없는 상태를 막는 것이 이 항목의 목적이다.
func TestTransferPollDueIndexDownPreservesNewIndexWhenOldIndexValidationFails(t *testing.T) {
	db := openIsolatedSchemaDB(t, 11)
	sqlDB, err := db.DB()
	require.NoError(t, err)

	require.NoError(t, db.Exec(
		`CREATE INDEX transfer_requests_next_check_at_idx ON transfer_requests (id)`).Error)

	migrateErr := goose.DownTo(sqlDB, migrationsDir(), 10)
	require.Error(t, migrateErr, "잘못된 동명 옛 인덱스인데 Down이 성공했다")

	version, err := goose.GetDBVersion(sqlDB)
	require.NoError(t, err)
	assert.EqualValues(t, 11, version, "Down이 실패했는데 goose version이 내려갔다")

	assertDuePollIndexValid(t, db)
}
