// package dbmigration_test인 이유: testdb가 dbmigration을 import하므로 내부 테스트
// 패키지에서 testdb를 쓰면 import cycle이 된다(007 테스트와 같은 이유).
package dbmigration_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/testdb"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 009가 실제로 건 제약을 카탈로그가 아니라 동작으로 확인한다. 제약 정의 문자열을
// 비교하면 Postgres 버전마다 표현이 달라 깨지고, 무엇보다 "이 제약이 실제로 막는가"를
// 증명하지 못한다.
//
// 표는 AutoMigrate가 만들고 제약은 009가 거는 구조라, 여기서 보이는 거부 동작은
// 전부 009의 결과다.
func TestLedgerSchemaIntegration(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)

	t.Run("허용값 밖 transfer status는 거부된다", func(t *testing.T) {
		err := insertTransferRequest(db, transferRow{
			status:    "PROCESSNG", // 오타. 이런 값이 들어가면 어느 분기에도 걸리지 않는다
			direction: "DEPOSIT",
			key:       uniqueKey("status"),
		})
		require.Error(t, err, "허용값 밖 status가 통과했다")
	})

	t.Run("허용값 밖 event outcome은 거부된다", func(t *testing.T) {
		requestID := mustInsertDeposit(t, db, uniqueKey("outcome-parent"))

		err := db.Exec(`
			INSERT INTO transfer_status_events (transfer_request_id, source, event_key, outcome, received_at)
			VALUES (?, 'CALLBACK', ?, 'MAYBE', now())`,
			requestID, uniqueKey("outcome")).Error
		require.Error(t, err, "허용값 밖 outcome이 통과했다")
	})

	t.Run("입금은 hold_journal_id를 가질 수 없다", func(t *testing.T) {
		journalID := mustInsertJournal(t, db, uniqueKey("deposit-hold"))

		err := insertTransferRequest(db, transferRow{
			status:    "RECEIVED",
			direction: "DEPOSIT",
			key:       uniqueKey("deposit-hold"),
			holdID:    &journalID,
		})
		require.Error(t, err, "입금에 잠금 분개가 붙었다")
	})

	t.Run("같은 event_key 두 번째 INSERT는 0행이다", func(t *testing.T) {
		requestID := mustInsertDeposit(t, db, uniqueKey("dup-parent"))
		eventKey := uniqueKey("dup-event")

		insert := func() int64 {
			result := db.Exec(`
				INSERT INTO transfer_status_events (transfer_request_id, source, event_key, outcome, received_at)
				VALUES (?, 'CALLBACK', ?, 'SUCCESS', now())
				ON CONFLICT (event_key) DO NOTHING`,
				requestID, eventKey)
			require.NoError(t, result.Error)
			return result.RowsAffected
		}

		require.Equal(t, int64(1), insert(), "처음 보는 사건이 기록되지 않았다")
		// 유니크 위반을 일으키지 않고 0행으로 알려준다. 위반을 일으키면 트랜잭션
		// 전체가 abort돼 같은 트랜잭션에서 복구할 수 없다.
		require.Equal(t, int64(0), insert(), "중복 사건이 두 번 기록됐다")
	})

	t.Run("금액 0인 전기는 거부된다", func(t *testing.T) {
		journalID := mustInsertJournal(t, db, uniqueKey("zero-posting"))
		accountID := mustInsertSystemAccount(t, db, "FEE_INCOME", uniqueAsset("ZRO"))

		err := db.Exec(`
			INSERT INTO postings (journal_id, account_id, asset, amount, created_at)
			VALUES (?, ?, 'KRW', 0, now())`, journalID, accountID).Error
		require.Error(t, err, "0원 전기가 통과했다")
	})

	// 즉시 CHECK였다면 아래 첫 경우가 INSERT 시점에 막힌다. 두 경우가 함께
	// 통과·실패해야 "출금을 만들 수 있으면서 잠금 없는 출금은 못 만든다"가 성립한다.
	t.Run("출금 잠금은 커밋 시점에만 검사된다", func(t *testing.T) {
		t.Run("트랜잭션 안에서 나중에 채우면 통과한다", func(t *testing.T) {
			err := db.Transaction(func(tx *gorm.DB) error {
				var requestID uint
				if err := tx.Raw(`
					INSERT INTO transfer_requests
						(user_id, direction, rail, asset, amount, fee_amount, fee_asset,
						 status, client_request_key, check_attempts, review_reason,
						 failure_reason, created_at, updated_at)
					VALUES (1, 'WITHDRAWAL', 'BANK', 'KRW', 1000, 0, 'KRW',
						 'RECEIVED', ?, 0, '', '', now(), now())
					RETURNING id`, uniqueKey("deferred-ok")).Scan(&requestID).Error; err != nil {
					return err
				}

				journalID := mustInsertJournalTx(tx, uniqueKey("deferred-ok-journal"))
				return tx.Exec(`UPDATE transfer_requests SET hold_journal_id = ? WHERE id = ?`,
					journalID, requestID).Error
			})
			require.NoError(t, err, "출금 요청을 만들 수 없다 — 즉시 CHECK가 남아 있다")
		})

		t.Run("채우지 않고 커밋하면 실패한다", func(t *testing.T) {
			err := db.Transaction(func(tx *gorm.DB) error {
				return tx.Exec(`
					INSERT INTO transfer_requests
						(user_id, direction, rail, asset, amount, fee_amount, fee_asset,
						 status, client_request_key, check_attempts, review_reason,
						 failure_reason, created_at, updated_at)
					VALUES (1, 'WITHDRAWAL', 'BANK', 'KRW', 1000, 0, 'KRW',
						 'RECEIVED', ?, 0, '', '', now(), now())`,
					uniqueKey("deferred-bad")).Error
			})
			require.Error(t, err, "잠금 없는 출금이 커밋됐다")
		})
	})

	t.Run("external_ref는 접수 중에만 비어 있을 수 있다", func(t *testing.T) {
		t.Run("RECEIVED는 NULL이 여러 건이어도 된다", func(t *testing.T) {
			require.NoError(t, insertTransferRequest(db, transferRow{
				status: "RECEIVED", direction: "DEPOSIT", key: uniqueKey("null-ref-1"),
			}))
			// 부분 유니크 인덱스가 아니면 두 번째 NULL에서 막힌다.
			require.NoError(t, insertTransferRequest(db, transferRow{
				status: "RECEIVED", direction: "DEPOSIT", key: uniqueKey("null-ref-2"),
			}))
		})

		t.Run("PROCESSING은 external_ref가 있어야 한다", func(t *testing.T) {
			err := insertTransferRequest(db, transferRow{
				status: "PROCESSING", direction: "DEPOSIT", key: uniqueKey("no-ref"),
			})
			require.Error(t, err, "외부 거래번호 없이 제출 상태가 됐다")
		})
	})
}

type transferRow struct {
	status    string
	direction string
	key       string
	holdID    *uint
}

func insertTransferRequest(db *gorm.DB, row transferRow) error {
	return db.Exec(`
		INSERT INTO transfer_requests
			(user_id, direction, rail, asset, amount, fee_amount, fee_asset,
			 status, client_request_key, hold_journal_id, check_attempts,
			 review_reason, failure_reason, created_at, updated_at)
		VALUES (1, ?, 'BANK', 'KRW', 1000, 0, 'KRW', ?, ?, ?, 0, '', '', now(), now())`,
		row.direction, row.status, row.key, row.holdID).Error
}

func mustInsertDeposit(t *testing.T, db *gorm.DB, key string) uint {
	t.Helper()
	var id uint
	require.NoError(t, db.Raw(`
		INSERT INTO transfer_requests
			(user_id, direction, rail, asset, amount, fee_amount, fee_asset,
			 status, client_request_key, check_attempts, review_reason,
			 failure_reason, created_at, updated_at)
		VALUES (1, 'DEPOSIT', 'BANK', 'KRW', 1000, 0, 'KRW', 'RECEIVED', ?, 0, '', '', now(), now())
		RETURNING id`, key).Scan(&id).Error)
	return id
}

func mustInsertJournal(t *testing.T, db *gorm.DB, key string) uint {
	t.Helper()
	return mustInsertJournalTx(db, key)
}

func mustInsertJournalTx(db *gorm.DB, key string) uint {
	var id uint
	if err := db.Raw(`
		INSERT INTO journal_entries (event_type, idempotency_key, reference_type, reference_id, created_at)
		VALUES ('DEV_FUND', ?, 'DEV_FUND', 0, now())
		RETURNING id`, key).Scan(&id).Error; err != nil {
		panic(err)
	}
	return id
}

func mustInsertSystemAccount(t *testing.T, db *gorm.DB, accountType string, asset string) uint {
	t.Helper()
	var id uint
	require.NoError(t, db.Raw(`
		INSERT INTO accounts (account_type, asset, allows_negative, created_at)
		VALUES (?, ?, false, now())
		RETURNING id`, accountType, asset).Scan(&id).Error)
	return id
}

// uniqueKey는 같은 DB를 반복해서 쓰는 통합 테스트에서 유니크 제약끼리 부딪히지
// 않게 한다. 테스트마다 스키마를 비우지 않는 것이 이 저장소의 방식이다.
func uniqueKey(prefix string) string {
	return fmt.Sprintf("ledger-schema-%s-%d", prefix, time.Now().UnixNano())
}

func uniqueAsset(prefix string) string {
	return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano()%1000000)
}
