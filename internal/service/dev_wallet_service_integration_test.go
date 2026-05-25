package service

import (
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegrationDevFundWalletCreatesAndIncrementsKRW(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(900)
	defer cleanupServiceUsers(t, db, userID)

	service := NewDevWalletService(db)

	wallet, err := service.FundWallet(FundWalletInput{
		UserID:     userID,
		CoinSymbol: model.KRWAssetSymbol,
		Amount:     "1000",
	})
	require.NoError(t, err)
	assert.True(t, wallet.AvailableBalance.Equal(decimal.NewFromInt(1000)))
	assert.True(t, wallet.LockedBalance.Equal(decimal.Zero))
	assert.True(t, wallet.KRW.Equal(decimal.NewFromInt(1000)))

	wallet, err = service.FundWallet(FundWalletInput{
		UserID:     userID,
		CoinSymbol: model.KRWAssetSymbol,
		Amount:     "25.5",
	})
	require.NoError(t, err)
	assert.True(t, wallet.AvailableBalance.Equal(decimal.RequireFromString("1025.5")))
	assert.True(t, wallet.KRW.Equal(decimal.RequireFromString("1025.5")))

	var count int64
	require.NoError(t, db.Model(&model.Wallet{}).Where("user_id = ? AND coin_symbol = ?", userID, model.KRWAssetSymbol).Count(&count).Error)
	assert.Equal(t, int64(1), count)

	entries := requireLedgerEntries(t, db, userID, model.LedgerEntryTypeDevFund, model.LedgerReferenceTypeDevFund, 0)
	require.Len(t, entries, 2)
	assertLedgerDelta(t, entries[0], model.KRWAssetSymbol, "25.5", "0", "1025.5", "0")
	assertLedgerDelta(t, entries[1], model.KRWAssetSymbol, "1000", "0", "1000", "0")
}

func TestIntegrationDevFundWalletCreatesCoinWallet(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(901)
	defer cleanupServiceUsers(t, db, userID)

	service := NewDevWalletService(db)

	wallet, err := service.FundWallet(FundWalletInput{
		UserID:     userID,
		CoinSymbol: "btc",
		Amount:     "0.25",
	})

	require.NoError(t, err)
	assert.Equal(t, "BTC", wallet.CoinSymbol)
	assert.True(t, wallet.AvailableBalance.Equal(decimal.RequireFromString("0.25")))
	assert.True(t, wallet.LockedBalance.Equal(decimal.Zero))
	assert.True(t, wallet.Quantity.Equal(decimal.RequireFromString("0.25")))
	assert.True(t, wallet.KRW.Equal(decimal.Zero))
	entries := requireLedgerEntries(t, db, userID, model.LedgerEntryTypeDevFund, model.LedgerReferenceTypeDevFund, 0)
	require.Len(t, entries, 1)
	assertLedgerDelta(t, entries[0], "BTC", "0.25", "0", "0.25", "0")
}
