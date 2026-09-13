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

// 011은 006과 같은 관용구를 쓴다(NO TRANSACTION, CONCURRENTLY, indisvalid 검증) —
// poller가 5초마다 도는 운영 테이블에 온라인으로 인덱스를 바꾼다. 009의
// transfer_requests_next_check_at_idx는 PROCESSING만 포함하고 키도 하나뿐이라
// DueForCheck(RECEIVED도 포함, next_check_at·id 두 열 정렬)를 받치지 못해서
// 교체한다 — 옛 인덱스는 검증이 끝난 뒤에만 지운다.
// Up·Down 둘 다 같은 규율(CONCURRENTLY 중단 시 잔해를 검증으로 잡은 뒤에만
// 상대 인덱스를 지운다)을 따라야 한다. 파일 전체에 Contains만 걸면 Down이
// 검증 없이 곧장 DROP해도(잔해를 "이미 있음"으로 넘기고 유효한 인덱스 없이
// goose version만 되돌리는 실패) 이름이 파일 어딘가에 있다는 이유로 통과한다
// — 그래서 Up 섹션과 Down 섹션을 나누고, 각 섹션 안에서 RAISE EXCEPTION이
// 상대 인덱스 DROP보다 먼저 오는지(=검증이 DROP을 실제로 막을 수 있는 위치인지)
// 까지 확인한다.
func TestTransferPollDueIndexMigrationIsConcurrentAndValidated(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(migrationsDir(), "011_transfer_poll_due_index.sql"))
	require.NoError(t, err)
	sql := string(raw)

	assert.True(t, strings.HasPrefix(sql, "-- +goose NO TRANSACTION\n"))

	downMarker := "-- +goose Down"
	downIndex := strings.Index(sql, downMarker)
	require.Greater(t, downIndex, 0, "-- +goose Down을 찾을 수 없다")
	upSection := sql[:downIndex]
	downSection := sql[downIndex:]

	// Up: 새 인덱스를 만들고, 검증한 뒤에만 옛 인덱스를 지운다.
	assert.Contains(t, upSection, "CREATE INDEX CONCURRENTLY IF NOT EXISTS transfer_requests_due_poll_idx")
	assert.Contains(t, upSection, "ON transfer_requests (next_check_at ASC NULLS FIRST, id ASC)")
	assert.Contains(t, upSection, "WHERE status IN ('RECEIVED', 'PROCESSING')")
	assert.Contains(t, upSection, "indisready")
	assert.Contains(t, upSection, "indisvalid")
	assert.Contains(t, upSection, "RAISE EXCEPTION")
	assert.Contains(t, upSection, "DROP INDEX CONCURRENTLY IF EXISTS transfer_requests_next_check_at_idx")
	assert.Less(t,
		strings.Index(upSection, "RAISE EXCEPTION"),
		strings.Index(upSection, "DROP INDEX CONCURRENTLY IF EXISTS transfer_requests_next_check_at_idx"),
		"Up의 RAISE EXCEPTION이 옛 인덱스 DROP보다 뒤에 있다 — 검증이 DROP을 막지 못한다")

	// Down: 옛 인덱스를 되살리고, 검증한 뒤에만 새 인덱스를 지운다 — Up과
	// 정확히 대칭이다. 그러지 않으면 옛 인덱스 재생성이 CONCURRENTLY 중단으로
	// invalid 잔해를 남겼을 때 새 인덱스가 무검증으로 지워져 유효한 조회
	// 인덱스가 하나도 남지 않는다.
	assert.Contains(t, downSection, "CREATE INDEX CONCURRENTLY IF NOT EXISTS transfer_requests_next_check_at_idx")
	assert.Contains(t, downSection, "indisready")
	assert.Contains(t, downSection, "indisvalid")
	assert.Contains(t, downSection, "RAISE EXCEPTION")
	assert.Contains(t, downSection, "DROP INDEX CONCURRENTLY IF EXISTS transfer_requests_due_poll_idx")
	assert.Less(t,
		strings.Index(downSection, "RAISE EXCEPTION"),
		strings.Index(downSection, "DROP INDEX CONCURRENTLY IF EXISTS transfer_requests_due_poll_idx"),
		"Down의 RAISE EXCEPTION이 새 인덱스 DROP보다 뒤에 있다 — 검증이 DROP을 막지 못한다")
}

func TestSQLDBFromGORMRejectsNil(t *testing.T) {
	sqlDB, err := sqlDBFromGORM(nil)

	require.Error(t, err)
	assert.Nil(t, sqlDB)
}

// 008은 gauge 조회용 부분 인덱스를 만든다. IF NOT EXISTS는 "같은 이름의 다른 인덱스"도
// 조용히 통과시키므로(006에서 확인한 구멍), 같은 Up 안의 카탈로그 검증이 한 세트다.
func TestOrderIdempotencyMigrationDeclaresContract(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(migrationsDir(), "008_order_idempotency_keys.sql"))
	require.NoError(t, err)
	sql := string(raw)

	assert.Contains(t, sql, "CREATE TABLE IF NOT EXISTS order_idempotency_keys")
	assert.Contains(t, sql, "order_idempotency_keys_user_key_unique")
	assert.Contains(t, sql, "UNIQUE (user_id, idempotency_key)")
	assert.Contains(t, sql, "fingerprint_version")
	assert.Contains(t, sql, "'PENDING','ACCEPTED','REJECTED','UNKNOWN'")
	assert.Contains(t, sql, "NOT NULL DEFAULT 'PENDING'")

	// 제약도 conname 존재만으로는 부족하다 — 실제 정의를 확인해야 한다.
	assert.Contains(t, sql, "pg_get_constraintdef")

	assert.Contains(t, sql, "CREATE INDEX IF NOT EXISTS order_idempotency_pending_updated_at")
	assert.Contains(t, sql, "WHERE outcome = 'PENDING'")

	// 카탈로그 방어 — 셋이 한 세트다.
	assert.Contains(t, sql, "indisready")
	assert.Contains(t, sql, "indisvalid")
	assert.Contains(t, sql, "RAISE EXCEPTION")
}
