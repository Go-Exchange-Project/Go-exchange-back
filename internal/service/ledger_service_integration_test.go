package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/testdb"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// ledgerUserID는 테스트마다 새 사용자를 쓰게 해 잔액이 서로 섞이지 않게 한다.
// 이 저장소의 통합 테스트는 스키마를 비우지 않는다.
func ledgerUserID() uint {
	return uint(time.Now().UnixNano() % 1000000000)
}

func ledgerKey(prefix string) string {
	return fmt.Sprintf("ledger-%s-%d", prefix, time.Now().UnixNano())
}

// fundInput은 DEV_MINT에서 사용자에게 자산을 넣는 분개다. 다른 테스트의
// 준비 단계로 쓴다.
func fundInput(userID uint, asset string, amount decimal.Decimal, key string) JournalInput {
	return JournalInput{
		EventType:      model.JournalEventDevFund,
		IdempotencyKey: key,
		ReferenceType:  model.JournalReferenceDevFund,
		ReferenceID:    0,
		Postings: []PostingInput{
			{AccountType: model.AccountDevMint, Asset: asset, Amount: amount.Neg()},
			{AccountType: model.AccountUserAvailable, OwnerUserID: &userID, Asset: asset, Amount: amount},
		},
	}
}

func newLedgerFixture(t *testing.T) (*gorm.DB, *LedgerService) {
	t.Helper()
	db := testdb.OpenIntegrationDB(t)
	return db, NewLedgerService(db)
}

// userAvailableBalance는 원장 캐시에서 사용자 잔액을 읽는다.
func userAvailableBalance(t *testing.T, db *gorm.DB, userID uint, asset string) decimal.Decimal {
	t.Helper()
	balances, err := repository.NewAccountRepository(db).ListUserBalances(userID)
	require.NoError(t, err)
	for _, row := range balances {
		if row.Asset == asset {
			return row.Available
		}
	}
	return decimal.Zero
}

func countPostings(t *testing.T, db *gorm.DB, journalID uint) int64 {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(&model.Posting{}).Where("journal_id = ?", journalID).Count(&count).Error)
	return count
}

// T1. 자산별 합이 0이 아닌 분개는 거부되고 아무것도 남지 않는다.
//
// 이것이 복식부기의 근본이다. 이 검사가 뚫리면 돈이 생기거나 사라진다.
func TestLedgerRejectsUnbalancedJournal(t *testing.T) {
	db, ledger := newLedgerFixture(t)
	userID := ledgerUserID()
	key := ledgerKey("unbalanced")

	err := db.Transaction(func(tx *gorm.DB) error {
		_, _, recordErr := ledger.Record(tx, JournalInput{
			EventType:      model.JournalEventDevFund,
			IdempotencyKey: key,
			ReferenceType:  model.JournalReferenceDevFund,
			Postings: []PostingInput{
				{AccountType: model.AccountDevMint, Asset: "KRW", Amount: decimal.NewFromInt(-1000)},
				// 900만 받는다. 100이 사라진다.
				{AccountType: model.AccountUserAvailable, OwnerUserID: &userID, Asset: "KRW", Amount: decimal.NewFromInt(900)},
			},
		})
		return recordErr
	})
	require.Error(t, err, "합이 0이 아닌 분개가 통과했다")

	var journalCount int64
	require.NoError(t, db.Model(&model.JournalEntry{}).Where("idempotency_key = ?", key).Count(&journalCount).Error)
	require.Zero(t, journalCount, "거부된 분개가 남았다")
	require.True(t, userAvailableBalance(t, db, userID, "KRW").IsZero(), "거부됐는데 잔액이 변했다")
}

// T2. 같은 멱등성 키는 한 번만 기록된다.
//
// ②가 이 순서의 핵심이다. 잔액 검사가 멱등 판정보다 앞에 있으면, 이미 처리된
// 요청이 지금 잔액 때문에 실패한다. 그러면 외부가 알림을 계속 재전송하고
// 우리는 계속 실패를 돌려주는 고리에 들어간다.
func TestLedgerRecordIsIdempotent(t *testing.T) {
	db, ledger := newLedgerFixture(t)
	userID := ledgerUserID()
	key := ledgerKey("idempotent")
	amount := decimal.NewFromInt(1000)

	var firstID uint
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		entry, created, err := ledger.Record(tx, fundInput(userID, "KRW", amount, key))
		if err != nil {
			return err
		}
		require.True(t, created, "첫 기록인데 created가 false다")
		firstID = entry.ID
		return nil
	}))

	t.Run("두 번째 기록은 기존 분개를 돌려준다", func(t *testing.T) {
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			entry, created, err := ledger.Record(tx, fundInput(userID, "KRW", amount, key))
			if err != nil {
				return err
			}
			require.False(t, created, "중복인데 created가 true다")
			require.Equal(t, firstID, entry.ID, "다른 분개가 돌아왔다")
			return nil
		}))

		require.Equal(t, int64(2), countPostings(t, db, firstID), "전기가 두 벌 생겼다")
		require.True(t, userAvailableBalance(t, db, userID, "KRW").Equal(amount), "잔액이 두 번 늘었다")
	})

	t.Run("잔액을 다 써도 재시도가 성공한다", func(t *testing.T) {
		// 받은 돈을 전부 외부로 내보내 available을 0으로 만든다. 지금 같은
		// 분개를 새로 만들려 하면 음수 검사에 걸릴 상태다.
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			_, _, err := ledger.Record(tx, JournalInput{
				EventType:      model.JournalEventWithdrawal,
				IdempotencyKey: ledgerKey("drain"),
				ReferenceType:  model.JournalReferenceTransfer,
				Postings: []PostingInput{
					{AccountType: model.AccountUserAvailable, OwnerUserID: &userID, Asset: "KRW", Amount: amount.Neg()},
					{AccountType: model.AccountExternalBank, Asset: "KRW", Amount: amount},
				},
			})
			return err
		}))
		require.True(t, userAvailableBalance(t, db, userID, "KRW").IsZero())

		// 멱등 판정이 잔액 검사보다 먼저이므로, 이미 기록된 사건은 현재 잔액과
		// 무관하게 기존 분개를 돌려준다.
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			entry, created, err := ledger.Record(tx, fundInput(userID, "KRW", amount, key))
			if err != nil {
				return err
			}
			require.False(t, created)
			require.Equal(t, firstID, entry.ID)
			return nil
		}), "이미 처리된 요청이 현재 잔액 때문에 실패했다")

		require.True(t, userAvailableBalance(t, db, userID, "KRW").IsZero(), "재시도가 잔액을 다시 늘렸다")
	})

	// 같은 키로 다른 내용을 보내는 것은 둘 중 하나가 잘못된 요청이라는 뜻이다.
	// 이것을 성공으로 돌려주면 호출자는 자기가 요청한 금액이 기록됐다고 믿고
	// 상태를 바꾼다 — 실제로는 다른 금액이 기록돼 있는데.
	t.Run("같은 키에 다른 금액은 거부된다", func(t *testing.T) {
		before := userAvailableBalance(t, db, userID, "KRW")

		err := db.Transaction(func(tx *gorm.DB) error {
			_, _, recordErr := ledger.Record(tx,
				fundInput(userID, "KRW", amount.Add(decimal.NewFromInt(1)), key))
			return recordErr
		})
		require.Error(t, err, "같은 키에 다른 금액이 통과했다")

		var journalCount int64
		require.NoError(t, db.Model(&model.JournalEntry{}).
			Where("idempotency_key = ?", key).Count(&journalCount).Error)
		require.Equal(t, int64(1), journalCount, "분개가 더 생겼다")
		require.Equal(t, int64(2), countPostings(t, db, firstID), "전기가 더 생겼다")
		require.True(t, userAvailableBalance(t, db, userID, "KRW").Equal(before), "잔액이 변했다")
	})

	// 검산 4종을 여기에 붙이는 이유: 이 시점의 DB에는 지급·출금 분개가 실제로
	// 들어 있어 검사가 의미 있는 데이터를 본다. 별도 테스트 함수를 만들면 같은
	// 사실을 두 번 보게 되고, 전 사건을 덮는 T4까지 검산 쿼리가 한 번도 실행되지
	// 않는 상태로 남는다.
	t.Run("검산 4종이 위반 0건이다", func(t *testing.T) {
		recon := repository.NewLedgerReconciliationRepository(db)

		unbalanced, err := recon.CheckUnbalancedJournals(0, 100)
		require.NoError(t, err)
		require.Empty(t, unbalanced, "자산별 합이 0이 아닌 분개가 있다")

		drift, err := recon.CheckBalanceCacheDrift(0, 100)
		require.NoError(t, err)
		require.Empty(t, drift, "잔액 캐시가 전기 합과 어긋난다")

		totals, err := recon.CheckAssetTotals()
		require.NoError(t, err)
		require.Empty(t, totals, "자산 전체 합이 0이 아니다")

		negative, err := recon.CheckNegativeAccounts(0, 100)
		require.NoError(t, err)
		require.Empty(t, negative, "음수가 되면 안 되는 계정이 음수다")
	})
}

// T3. 잔액 갱신 전에 롤백되면 분개·전기·캐시가 모두 없다.
//
// Record는 호출자의 트랜잭션 안에서 돌고 스스로 커밋하지 않는다. 호출자가
// 오류를 내면 분개만 남고 전기가 없는 상태가 커밋되지 않아야 한다.
func TestLedgerRollsBackJournalPostingAndBalance(t *testing.T) {
	db, ledger := newLedgerFixture(t)
	userID := ledgerUserID()
	key := ledgerKey("rollback")

	sentinel := fmt.Errorf("의도적 롤백")
	err := db.Transaction(func(tx *gorm.DB) error {
		if _, _, recordErr := ledger.Record(tx, fundInput(userID, "KRW", decimal.NewFromInt(5000), key)); recordErr != nil {
			return recordErr
		}
		// 여기까지는 분개·전기·잔액이 모두 이 트랜잭션 안에 있다. 시간이나
		// 실행 순서에 기대지 않고 명시적으로 롤백시킨다.
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)

	var journal model.JournalEntry
	findErr := db.Where("idempotency_key = ?", key).First(&journal).Error
	require.ErrorIs(t, findErr, gorm.ErrRecordNotFound, "롤백됐는데 분개가 남았다")
	require.True(t, userAvailableBalance(t, db, userID, "KRW").IsZero(), "롤백됐는데 잔액이 변했다")
}

// T9. 사용자 계정을 음수로 만드는 전기는 거부된다. 외부 계정은 허용된다.
//
// 사용자 잔액이 음수가 된다는 것은 없는 돈을 썼다는 뜻이다. 반대로 외부 계정이
// 음수인 것은 정상이다 — "바깥에서 안으로 들어왔다"는 뜻이지 빚이 아니다.
func TestLedgerRejectsNegativeUserAccount(t *testing.T) {
	db, ledger := newLedgerFixture(t)
	userID := ledgerUserID()

	t.Run("사용자 계정 음수는 거부된다", func(t *testing.T) {
		key := ledgerKey("negative")
		err := db.Transaction(func(tx *gorm.DB) error {
			_, _, recordErr := ledger.Record(tx, JournalInput{
				EventType:      model.JournalEventWithdrawal,
				IdempotencyKey: key,
				ReferenceType:  model.JournalReferenceTransfer,
				Postings: []PostingInput{
					{AccountType: model.AccountUserAvailable, OwnerUserID: &userID, Asset: "KRW", Amount: decimal.NewFromInt(-1)},
					{AccountType: model.AccountExternalBank, Asset: "KRW", Amount: decimal.NewFromInt(1)},
				},
			})
			return recordErr
		})
		require.Error(t, err, "잔액 0인 사용자가 1원을 냈다")

		var count int64
		require.NoError(t, db.Model(&model.JournalEntry{}).Where("idempotency_key = ?", key).Count(&count).Error)
		require.Zero(t, count, "거부된 분개가 남았다")
	})

	t.Run("외부 계정 음수는 허용된다", func(t *testing.T) {
		// fundInput의 DEV_MINT가 음수가 되는 분개다. 이것이 막히면 아무도
		// 자산을 받을 수 없다.
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			_, _, err := ledger.Record(tx, fundInput(userID, "KRW", decimal.NewFromInt(100), ledgerKey("mint")))
			return err
		}))
		require.True(t, userAvailableBalance(t, db, userID, "KRW").Equal(decimal.NewFromInt(100)))
	})
}
