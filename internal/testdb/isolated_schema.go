package testdb

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/dbmigration"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// OpenIsolatedSchemaDB는 OpenIntegrationDB와 같은 스키마를 매 호출마다 새
// 임시 스키마 안에 만든다. 공유 스키마를 쓰는 다른 테스트가 남긴 행(정리하지
// 않는 fixture, 실제 시계로 예약된 조회 일정 등)에 흔들리면 안 되는 테스트가
// 쓴다 — 예를 들어 DueForCheck 전역 스캔의 LIMIT·정렬 자체가 검증 대상인
// 테스트는 공유 스키마의 남은 행이 배치를 나눠 가지면 조용히 다른 것을
// 검증하게 된다.
//
// t.Cleanup(DROP SCHEMA ... CASCADE)은 스키마를 만들기 전에 등록한다 — 그 뒤
// 어디서 실패해도 임시 스키마가 공유 테스트 DB에 남지 않는다.
func OpenIsolatedSchemaDB(t testing.TB) *gorm.DB {
	t.Helper()

	dsn := os.Getenv("GOEXCHANGE_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("GOEXCHANGE_TEST_DATABASE_DSN is not set; skipping Postgres integration test")
	}

	schema := fmt.Sprintf("test_isolated_%d", time.Now().UnixNano())

	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	fatalIfErr(t, err)
	adminSQLDB, err := admin.DB()
	fatalIfErr(t, err)
	t.Cleanup(func() { fatalIfErr(t, adminSQLDB.Close()) })

	t.Cleanup(func() {
		fatalIfErr(t, admin.Exec(fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, schema)).Error)
	})
	fatalIfErr(t, admin.Exec(fmt.Sprintf(`CREATE SCHEMA %s`, schema)).Error)

	// search_path는 pgx가 libpq 표준 키로 인식하지 않는 값이라 그대로 런타임
	// 파라미터로 넘어가 연결마다(풀의 모든 물리 연결에) 적용된다 — 세션별
	// SET이 아니라 연결 시작 시 서버가 설정하는 값이라 커넥션 풀과 무관하다.
	scoped, err := gorm.Open(postgres.Open(dsn+" search_path="+schema), &gorm.Config{})
	fatalIfErr(t, err)
	scopedSQLDB, err := scoped.DB()
	fatalIfErr(t, err)
	t.Cleanup(func() { fatalIfErr(t, scopedSQLDB.Close()) })

	// OpenIntegrationDB와 같은 AutoMigrate 목록 + 전체 goose 마이그레이션.
	// transfer_requests 관련 제약·트리거는 009에 있으므로 UpTo(8)이 아니라
	// 전체를 올려야 실제 통합 테스트 스키마와 같은 상태가 된다.
	fatalIfErr(t, scoped.AutoMigrate(&model.User{}, &model.Order{}, &model.Trade{}, &model.FailedSettlement{}, &model.FailedMarketCompletion{}, &model.FailedOrderCancellation{}, &model.ReconciliationViolation{}, &model.TradeOutboxEvent{},
		&model.Account{}, &model.AccountBalance{}, &model.JournalEntry{}, &model.Posting{}, &model.TransferRequest{}, &model.TransferStatusEvent{}, &model.UserAssetStat{}))
	fatalIfErr(t, dbmigration.Up(scoped))

	assertIsolatedSchemaReady(t, scoped)

	return scoped
}

// assertIsolatedSchemaReady는 이 스키마 안에 goose_db_version과
// transfer_requests가 실제로 생겼는지 information_schema로 직접 확인한다 —
// AutoMigrate·goose 호출이 조용히 다른 스키마(search_path 설정이 안 먹은
// 경우)에 떨어지면 이후 단언이 "0건"을 격리 덕분이 아니라 테이블이 아예
// 없어서 통과하는 사고가 생긴다.
func assertIsolatedSchemaReady(t testing.TB, db *gorm.DB) {
	t.Helper()

	for _, table := range []string{"goose_db_version", "transfer_requests"} {
		var exists bool
		fatalIfErr(t, db.Raw(`
SELECT EXISTS (
    SELECT 1 FROM information_schema.tables
    WHERE table_schema = current_schema() AND table_name = ?
)`, table).Scan(&exists).Error)
		if !exists {
			t.Fatalf("isolated schema is missing table %q after migration", table)
		}
	}
}
