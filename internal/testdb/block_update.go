package testdb

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
)

var blockedUpdateSeq atomic.Uint64

// BlockUpdate는 table의 특정 UPDATE만 실패시키는 트리거를 건다. when에는 트리거의
// WHEN 절을 그대로 넣는다(예: "NEW.status = 'REJECTED' AND NEW.user_id = 7").
//
// OrderService의 저장소 필드는 인터페이스가 아니라 concrete pointer라, 실패를 주입하려면
// 운영 코드를 추상화해야 한다. 테스트 하나 때문에 운영 경로를 바꾸는 대신 DB 쪽에서 막는다.
func BlockUpdate(t testing.TB, db *gorm.DB, table string, when string) {
	t.Helper()

	// 함수도 영구 객체다. 이름을 고정하면 다음 실행의 CREATE FUNCTION이 충돌하고,
	// DROP TRIGGER만 하면 함수가 공유 DB에 계속 쌓인다.
	suffix := fmt.Sprintf("%d_%d", time.Now().UnixNano(), blockedUpdateSeq.Add(1))
	fn := "fail_" + table + "_" + suffix
	trg := fn + "_trg"

	if err := db.Exec(fmt.Sprintf(`
CREATE FUNCTION %s() RETURNS trigger AS $$
BEGIN RAISE EXCEPTION 'injected failure'; END $$ LANGUAGE plpgsql`, fn)).Error; err != nil {
		t.Fatal(err)
	}

	// 함수 생성 직후에 등록한다. 트리거 생성이 실패하면 아래에서 테스트가 중단되므로,
	// 뒤에 등록하면 함수가 공유 DB에 그대로 남는다.
	t.Cleanup(func() {
		if err := db.Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON %s`, trg, table)).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn)).Error; err != nil {
			t.Fatal(err)
		}
	})

	if err := db.Exec(fmt.Sprintf(`
CREATE TRIGGER %s BEFORE UPDATE ON %s
FOR EACH ROW WHEN (%s)
EXECUTE FUNCTION %s()`, trg, table, when, fn)).Error; err != nil {
		t.Fatal(err)
	}
}
