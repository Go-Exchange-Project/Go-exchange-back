package dbmigration

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// openIsolatedSchemaDB는 goose.UpTo로 지정한 버전까지만 적용된 전용 임시
// 스키마를 연다. TestOrderIdempotencyMigrationFailsOnWrongSameNamedConstraint·
// TestOrderIdempotencyMigrationFailsOnWrongSameNamedIndex(버전 8)와 migration
// 011의 Down 안전성 테스트(버전 10·11)가 함께 쓴다.
//
// 이 테스트들은 공통적으로 goose_db_version에서 특정 버전 행을 지우거나
// DownTo로 되돌린 뒤 재적용해 실패·복구를 관찰한다. 공유 testdb 스키마에서
// 이걸 하면 그보다 높은 버전이 이미 적용돼 있어 goose가 "그 버전보다 낮은
// 미적용 버전은 채울 수 없다"는 가드에 걸려 항상 실패하거나(버전 8의 경우),
// DownTo가 공유 스키마의 실제 운영 상태를 되돌려 뒤의 다른 테스트까지 깨뜨린다.
// 그래서 테스트마다 새 스키마를 만들어 격리한다. 공유 스키마의 goose_db_version은
// 절대 건드리지 않는다.
func openIsolatedSchemaDB(t *testing.T, version int64) *gorm.DB {
	t.Helper()

	dsn := os.Getenv("GOEXCHANGE_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("GOEXCHANGE_TEST_DATABASE_DSN is not set; skipping Postgres integration test")
	}

	schema := fmt.Sprintf("test_isolated_v%d_%d", version, time.Now().UnixNano())

	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	adminSQLDB, err := admin.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, adminSQLDB.Close()) })

	// cleanup은 스키마를 만들기 전에 등록한다 — 그 뒤 어디서 실패해도 임시
	// 스키마가 공유 테스트 DB에 남지 않는다.
	t.Cleanup(func() {
		require.NoError(t, admin.Exec(fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, schema)).Error)
	})
	require.NoError(t, admin.Exec(fmt.Sprintf(`CREATE SCHEMA %s`, schema)).Error)

	// search_path는 pgx가 인식하는 libpq 키가 아니라서 그대로 런타임
	// 파라미터로 넘어가 연결마다(풀의 모든 물리 연결에) 적용된다 — 세션별
	// SET이 아니라 연결 시작 시 서버가 설정하는 값이라 커넥션 풀과 무관하다.
	scoped, err := gorm.Open(postgres.Open(dsn+" search_path="+schema), &gorm.Config{})
	require.NoError(t, err)
	scopedSQLDB, err := scoped.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, scopedSQLDB.Close()) })

	// testdb.OpenIntegrationDB와 같은 AutoMigrate 목록을 이 임시 스키마에도
	// 적용한다 — 다르면 이 스키마의 시작 상태가 실제 통합 테스트 스키마와 갈린다.
	require.NoError(t, scoped.AutoMigrate(&model.User{}, &model.Order{}, &model.Trade{}, &model.FailedSettlement{}, &model.FailedMarketCompletion{}, &model.FailedOrderCancellation{}, &model.ReconciliationViolation{}, &model.TradeOutboxEvent{},
		&model.Account{}, &model.AccountBalance{}, &model.JournalEntry{}, &model.Posting{}, &model.TransferRequest{}, &model.TransferStatusEvent{}, &model.UserAssetStat{}))

	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.UpTo(scopedSQLDB, migrationsDir(), version))
	assertGooseVersionRecorded(t, scoped, version, true)

	return scoped
}

// assertGooseVersionRecorded는 이 임시 스키마 안의 goose_db_version을
// information_schema로 직접 확인한다("이 스키마에 그 테이블이 있다"를 가정하지
// 않는다) — 호출한 테스트의 계약("이 버전이 기록돼야/기록되지 않아야 한다")을
// 그대로 관찰하기 위해서다.
func assertGooseVersionRecorded(t *testing.T, db *gorm.DB, version int64, want bool) {
	t.Helper()

	var tableExists bool
	require.NoError(t, db.Raw(`
SELECT EXISTS (
    SELECT 1 FROM information_schema.tables
    WHERE table_schema = current_schema() AND table_name = 'goose_db_version'
)`).Scan(&tableExists).Error)
	require.True(t, tableExists, "이 임시 스키마에 goose_db_version이 없다")

	var applied int64
	require.NoError(t, db.Raw(
		`SELECT count(*) FROM goose_db_version WHERE version_id = ? AND is_applied`, version).Scan(&applied).Error)
	if want {
		assert.EqualValues(t, 1, applied, "적용 후에도 이 스키마에 version %d이 기록되지 않았다", version)
	} else {
		assert.Zero(t, applied, "실패했는데 이 스키마에 version %d이 기록됐다", version)
	}
}

// 제약은 conname 존재만 보고 조건부로 만든다. 같은 이름의 잘못된 제약이 이미 있으면
// 이름만으로 통과하므로, 실제 정의 검증이 없으면 틀린 스키마가 version 8로 기록된다.
func TestOrderIdempotencyMigrationFailsOnWrongSameNamedConstraint(t *testing.T) {
	wrong := map[string]struct{ name, definition string }{
		"UNIQUE 범위가 전역이다": {
			"order_idempotency_keys_user_key_unique", "UNIQUE (idempotency_key)"},
		"키 길이 상한이 다르다": {
			"order_idempotency_keys_key_length",
			"CHECK (length(btrim(idempotency_key)) BETWEEN 1 AND 1280)"},
		"outcome 목록이 다르다": {
			"order_idempotency_keys_outcome_check",
			"CHECK (outcome IN ('PENDING','ACCEPTED','REJECTED'))"},
	}

	for name, tc := range wrong {
		t.Run(name, func(t *testing.T) {
			db := openIsolatedSchemaDB(t, 8)

			require.NoError(t, db.Exec(
				`ALTER TABLE order_idempotency_keys DROP CONSTRAINT `+tc.name).Error)
			require.NoError(t, db.Exec(
				`ALTER TABLE order_idempotency_keys ADD CONSTRAINT `+tc.name+` `+tc.definition).Error)

			require.NoError(t, db.Exec(`DELETE FROM goose_db_version WHERE version_id = 8`).Error)

			sqlDB, err := db.DB()
			require.NoError(t, err)
			migrateErr := goose.UpTo(sqlDB, migrationsDir(), 8)

			require.Error(t, migrateErr, "잘못된 동명 제약인데 migration이 성공했다")
			assertGooseVersionRecorded(t, db, 8, false)
		})
	}
}

// 같은 이름의 잘못된 인덱스가 있으면 migration이 실패하고 version 8이 기록되지 않아야
// 한다. IF NOT EXISTS만으로는 조용히 통과한다.
func TestOrderIdempotencyMigrationFailsOnWrongSameNamedIndex(t *testing.T) {
	db := openIsolatedSchemaDB(t, 8)

	require.NoError(t, db.Exec(`DROP INDEX IF EXISTS order_idempotency_pending_updated_at`).Error)
	// predicate 없는 전체 인덱스를 같은 이름으로 만든다.
	require.NoError(t, db.Exec(
		`CREATE INDEX order_idempotency_pending_updated_at ON order_idempotency_keys (updated_at)`).Error)

	require.NoError(t, db.Exec(`DELETE FROM goose_db_version WHERE version_id = 8`).Error)

	sqlDB, err := db.DB()
	require.NoError(t, err)
	migrateErr := goose.UpTo(sqlDB, migrationsDir(), 8)

	require.Error(t, migrateErr, "잘못된 동명 인덱스인데 migration이 성공했다")
	assertGooseVersionRecorded(t, db, 8, false)
}
