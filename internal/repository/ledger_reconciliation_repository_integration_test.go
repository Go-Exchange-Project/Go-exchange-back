package repository

import (
	"fmt"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// TestIntegrationCheckUnbalancedJournalsReturnsAllAssetRowsAtPageBoundary는 한 분개가
// 자산 2종의 불균형 전기를 가질 때, journal 단위 페이지 경계에 걸려도 두 자산 행이
// 모두 돌아오는지 고정한다.
//
// LIMIT을 (journal_id, asset) 행에 직접 걸던 예전 코드였다면, pageSize=1 호출은 이
// 분개의 두 자산 행 중 하나만 돌려줬을 것이다 — ORDER BY가 journal_id까지만 있어
// 어느 쪽이 잘리는지도 실행마다 달랐다.
func TestIntegrationCheckUnbalancedJournalsReturnsAllAssetRowsAtPageBoundary(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewLedgerReconciliationRepository(db)

	// 기존 이력과 무관하게 이 픽스처만 보도록, 지금까지의 최댓값을 커서 기준으로 잡는다.
	var baseline uint
	require.NoError(t, db.Model(&model.JournalEntry{}).
		Select("COALESCE(MAX(id), 0)").Scan(&baseline).Error)

	assetA := fmt.Sprintf("RCA%d", time.Now().UnixNano()%1_000_000)
	assetB := fmt.Sprintf("RCB%d", time.Now().UnixNano()%1_000_000)

	// 이 테스트가 만드는 모든 것(계정 2개·잔액 캐시 2행·분개 2개·전기 4행)이 이
	// 픽스처가 영구히 위반으로 남기는 대상이다. 첫 생성보다 먼저 등록해야, 그 뒤
	// 어느 단계에서 실패해도 이미 만들어진 부분이 걸린다. 참조 방향대로
	// postings → journal_entries → account_balances → accounts 순서로 지운다.
	var journal1, journal2 uint
	t.Cleanup(func() {
		require.NoError(t, db.Where("journal_id IN ?", []uint{journal1, journal2}).Delete(&model.Posting{}).Error)
		require.NoError(t, db.Where("id IN ?", []uint{journal1, journal2}).Delete(&model.JournalEntry{}).Error)

		var accountIDs []uint
		require.NoError(t, db.Model(&model.Account{}).
			Where("asset IN ?", []string{assetA, assetB}).
			Pluck("id", &accountIDs).Error)
		if len(accountIDs) > 0 {
			require.NoError(t, db.Where("account_id IN ?", accountIDs).Delete(&model.AccountBalance{}).Error)
			require.NoError(t, db.Where("id IN ?", accountIDs).Delete(&model.Account{}).Error)
		}
	})

	accountRepo := NewAccountRepository(db)
	accounts, err := accountRepo.EnsureAccounts([]AccountSpec{
		{AccountType: model.AccountDevMint, Asset: assetA},
		{AccountType: model.AccountDevMint, Asset: assetB},
	})
	require.NoError(t, err)
	require.Len(t, accounts, 2)

	accountID := map[string]uint{}
	for _, account := range accounts {
		accountID[account.Asset] = account.ID
	}

	// journal1은 자산 2종에서 짝 없는 단일 전기만 가져 둘 다 불균형이다. journal2는
	// 정반대 금액을 실어, 이 픽스처가 검사 3(자산 전체 합)을 영구히 오염시키지
	// 않도록 전역 합계는 0으로 되돌린다.
	base := time.Now().UnixNano()
	journal1 = createTestJournalEntry(t, db, fmt.Sprintf("recon-unbalanced-%d-1", base))
	journal2 = createTestJournalEntry(t, db, fmt.Sprintf("recon-unbalanced-%d-2", base))

	require.NoError(t, db.Create(&[]model.Posting{
		{JournalID: journal1, AccountID: accountID[assetA], Asset: assetA, Amount: decimal.NewFromInt(100)},
		{JournalID: journal1, AccountID: accountID[assetB], Asset: assetB, Amount: decimal.NewFromInt(50)},
	}).Error)

	require.NoError(t, db.Create(&[]model.Posting{
		{JournalID: journal2, AccountID: accountID[assetA], Asset: assetA, Amount: decimal.NewFromInt(-100)},
		{JournalID: journal2, AccountID: accountID[assetB], Asset: assetB, Amount: decimal.NewFromInt(-50)},
	}).Error)

	page, err := repo.CheckUnbalancedJournals(baseline, 1)
	require.NoError(t, err)
	require.Len(t, page, 2, "경계에 걸린 분개의 두 자산 행이 모두 나와야 한다")
	for _, row := range page {
		assert.Equal(t, journal1, row.JournalID)
	}
	assert.ElementsMatch(t, []string{assetA, assetB}, []string{page[0].Asset, page[1].Asset})

	next, err := repo.CheckUnbalancedJournals(page[len(page)-1].JournalID, 1)
	require.NoError(t, err)
	require.Len(t, next, 2, "커서가 페이지의 마지막 분개 ID로 전진해 다음 분개를 돌려줘야 한다")
	for _, row := range next {
		assert.Equal(t, journal2, row.JournalID)
	}
}

func createTestJournalEntry(t *testing.T, db *gorm.DB, idempotencyKey string) uint {
	t.Helper()
	entry := model.JournalEntry{
		EventType:      model.JournalEventDevFund,
		IdempotencyKey: idempotencyKey,
		ReferenceType:  model.JournalReferenceDevFund,
		ReferenceID:    1,
	}
	require.NoError(t, db.Create(&entry).Error)
	return entry.ID
}
