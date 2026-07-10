package repository

import (
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func cleanupReconciliationViolations(t *testing.T, db *gorm.DB, subjectKeys ...string) {
	t.Helper()

	if len(subjectKeys) == 0 {
		return
	}
	require.NoError(t, db.Where("subject_key IN ?", subjectKeys).Delete(&model.ReconciliationViolation{}).Error)
}

func TestIntegrationCheckLedgerWalletPageFindsNoViolationWhenLedgerMatchesWallet(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(800)
	defer cleanupRepositoryUsers(t, db, userID)

	wallet := model.Wallet{
		UserID:           userID,
		CoinSymbol:       model.KRWAssetSymbol,
		KRW:              decimal.NewFromInt(1000),
		AvailableBalance: decimal.NewFromInt(1000),
		LockedBalance:    decimal.Zero,
	}
	require.NoError(t, db.Create(&wallet).Error)
	require.NoError(t, db.Create(&model.LedgerEntry{
		UserID:                userID,
		CoinSymbol:            model.KRWAssetSymbol,
		EntryType:             model.LedgerEntryTypeDevFund,
		AvailableDelta:        decimal.NewFromInt(1000),
		LockedDelta:           decimal.Zero,
		AvailableBalanceAfter: decimal.NewFromInt(1000),
		LockedBalanceAfter:    decimal.Zero,
		ReferenceType:         model.LedgerReferenceTypeDevFund,
		ReferenceID:           0,
	}).Error)

	repo := NewReconciliationRepository(db)
	rows, err := repo.CheckLedgerWalletPage(0, 500)
	require.NoError(t, err)

	row := findLedgerWalletRow(rows, wallet.ID)
	require.NotNil(t, row, "expected a row for the seeded wallet")
	assert.True(t, row.AvailableBalance.Equal(decimal.NewFromInt(1000)))
	assert.True(t, row.LedgerAvailableSum.Equal(decimal.NewFromInt(1000)))
	assert.True(t, row.LockedBalance.Equal(row.LedgerLockedSum))
	require.True(t, row.ImpliedInitialAvailable.Valid)
	assert.True(t, row.ImpliedInitialAvailable.Decimal.IsZero(), "first entry's implied initial balance should be 0 for a wallet that started at 0")
}

func TestIntegrationCheckLedgerWalletPageFindsGapWhenWalletDivergesFromLedger(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(801)
	defer cleanupRepositoryUsers(t, db, userID)

	// 원장은 500만큼 델타를 기록했는데 지갑은 700으로 직접 조작된 상황(진짜 버그 시뮬레이션).
	wallet := model.Wallet{
		UserID:           userID,
		CoinSymbol:       model.KRWAssetSymbol,
		KRW:              decimal.NewFromInt(700),
		AvailableBalance: decimal.NewFromInt(700),
		LockedBalance:    decimal.Zero,
	}
	require.NoError(t, db.Create(&wallet).Error)
	require.NoError(t, db.Create(&model.LedgerEntry{
		UserID:                userID,
		CoinSymbol:            model.KRWAssetSymbol,
		EntryType:             model.LedgerEntryTypeDevFund,
		AvailableDelta:        decimal.NewFromInt(500),
		LockedDelta:           decimal.Zero,
		AvailableBalanceAfter: decimal.NewFromInt(500),
		LockedBalanceAfter:    decimal.Zero,
		ReferenceType:         model.LedgerReferenceTypeDevFund,
		ReferenceID:           0,
	}).Error)

	repo := NewReconciliationRepository(db)
	rows, err := repo.CheckLedgerWalletPage(0, 500)
	require.NoError(t, err)

	row := findLedgerWalletRow(rows, wallet.ID)
	require.NotNil(t, row)
	assert.True(t, row.AvailableBalance.Sub(row.LedgerAvailableSum).Equal(decimal.NewFromInt(200)), "gap should be 700-500=200")
}

func TestIntegrationCheckLedgerWalletPageReturnsNullImpliedForWalletWithNoLedgerEntries(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(802)
	defer cleanupRepositoryUsers(t, db, userID)

	wallet := model.Wallet{
		UserID:           userID,
		CoinSymbol:       model.KRWAssetSymbol,
		KRW:              decimal.NewFromInt(50),
		AvailableBalance: decimal.NewFromInt(50),
		LockedBalance:    decimal.Zero,
	}
	require.NoError(t, db.Create(&wallet).Error)

	repo := NewReconciliationRepository(db)
	rows, err := repo.CheckLedgerWalletPage(0, 500)
	require.NoError(t, err)

	row := findLedgerWalletRow(rows, wallet.ID)
	require.NotNil(t, row)
	assert.False(t, row.ImpliedInitialAvailable.Valid, "wallet with no ledger entries must report NULL, not 0, for implied initial balance")
	assert.True(t, row.LedgerAvailableSum.IsZero())
}

func TestIntegrationCheckLedgerWalletPagePaginatesByWalletID(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(803)
	defer cleanupRepositoryUsers(t, db, userID)

	wallet := model.Wallet{
		UserID:           userID,
		CoinSymbol:       model.KRWAssetSymbol,
		KRW:              decimal.NewFromInt(1),
		AvailableBalance: decimal.NewFromInt(1),
		LockedBalance:    decimal.Zero,
	}
	require.NoError(t, db.Create(&wallet).Error)

	repo := NewReconciliationRepository(db)
	// wallet.ID보다 큰 커서로 조회하면 이 지갑은 절대 나오지 않아야 한다.
	rows, err := repo.CheckLedgerWalletPage(wallet.ID, 500)
	require.NoError(t, err)
	assert.Nil(t, findLedgerWalletRow(rows, wallet.ID))
}

func findLedgerWalletRow(rows []LedgerWalletRow, walletID uint) *LedgerWalletRow {
	for i := range rows {
		if rows[i].WalletID == walletID {
			return &rows[i]
		}
	}
	return nil
}
