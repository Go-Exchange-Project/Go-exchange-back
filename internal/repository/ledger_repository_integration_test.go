package repository

import (
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegrationLedgerRepositoryCreatesAndListsByUser(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(700)
	otherUserID := repositoryTestUserID(701)
	defer cleanupRepositoryUsers(t, db, userID, otherUserID)

	repo := NewLedgerRepository(db)
	require.NoError(t, repo.CreateMany([]model.LedgerEntry{
		{
			UserID:                userID,
			CoinSymbol:            model.KRWAssetSymbol,
			EntryType:             model.LedgerEntryTypeDevFund,
			AvailableDelta:        decimal.NewFromInt(1000),
			LockedDelta:           decimal.Zero,
			AvailableBalanceAfter: decimal.NewFromInt(1000),
			LockedBalanceAfter:    decimal.Zero,
			ReferenceType:         model.LedgerReferenceTypeDevFund,
			ReferenceID:           0,
		},
		{
			UserID:                otherUserID,
			CoinSymbol:            model.KRWAssetSymbol,
			EntryType:             model.LedgerEntryTypeDevFund,
			AvailableDelta:        decimal.NewFromInt(1),
			LockedDelta:           decimal.Zero,
			AvailableBalanceAfter: decimal.NewFromInt(1),
			LockedBalanceAfter:    decimal.Zero,
			ReferenceType:         model.LedgerReferenceTypeDevFund,
			ReferenceID:           0,
		},
	}))

	entries, err := repo.ListByUserID(userID, 10)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, model.LedgerEntryTypeDevFund, entries[0].EntryType)
	assert.True(t, entries[0].AvailableDelta.Equal(decimal.NewFromInt(1000)))
}

func TestIntegrationLedgerConstraintsRejectInvalidRows(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(702)
	defer cleanupRepositoryUsers(t, db, userID)

	base := model.LedgerEntry{
		UserID:                userID,
		CoinSymbol:            model.KRWAssetSymbol,
		EntryType:             model.LedgerEntryTypeDevFund,
		AvailableDelta:        decimal.NewFromInt(1),
		LockedDelta:           decimal.Zero,
		AvailableBalanceAfter: decimal.NewFromInt(1),
		LockedBalanceAfter:    decimal.Zero,
		ReferenceType:         model.LedgerReferenceTypeDevFund,
		ReferenceID:           0,
	}

	invalidType := base
	invalidType.EntryType = "BAD"
	require.Error(t, db.Create(&invalidType).Error)

	zeroDelta := base
	zeroDelta.AvailableDelta = decimal.Zero
	zeroDelta.LockedDelta = decimal.Zero
	require.Error(t, db.Create(&zeroDelta).Error)

	negativeAfter := base
	negativeAfter.AvailableBalanceAfter = decimal.NewFromInt(-1)
	require.Error(t, db.Create(&negativeAfter).Error)
}
