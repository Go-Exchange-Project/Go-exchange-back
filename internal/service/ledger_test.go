package service

import (
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
)

func TestLedgerEntryFromWalletUpdateRecordsDeltasAndBalances(t *testing.T) {
	wallet := &model.Wallet{
		UserID:           10,
		CoinSymbol:       model.KRWAssetSymbol,
		AvailableBalance: decimal.NewFromInt(1000),
		LockedBalance:    decimal.Zero,
	}
	update := WalletBalanceUpdate{
		AvailableBalance: decimal.NewFromInt(800),
		LockedBalance:    decimal.NewFromInt(200),
	}

	entry := ledgerEntryFromWalletUpdate(wallet, update, model.LedgerEntryTypeOrderHold, model.LedgerReferenceTypeOrder, 99, "")

	assert.Equal(t, uint(10), entry.UserID)
	assert.Equal(t, model.KRWAssetSymbol, entry.CoinSymbol)
	assert.Equal(t, model.LedgerEntryTypeOrderHold, entry.EntryType)
	assert.True(t, entry.AvailableDelta.Equal(decimal.NewFromInt(-200)))
	assert.True(t, entry.LockedDelta.Equal(decimal.NewFromInt(200)))
	assert.True(t, entry.AvailableBalanceAfter.Equal(decimal.NewFromInt(800)))
	assert.True(t, entry.LockedBalanceAfter.Equal(decimal.NewFromInt(200)))
	assert.Equal(t, model.LedgerReferenceTypeOrder, entry.ReferenceType)
	assert.Equal(t, uint(99), entry.ReferenceID)
}

func TestDevFundLedgerEntryRecordsAvailableCredit(t *testing.T) {
	wallet := &model.Wallet{
		UserID:           20,
		CoinSymbol:       "BTC",
		AvailableBalance: decimal.RequireFromString("0.25"),
		LockedBalance:    decimal.Zero,
	}

	entry := devFundLedgerEntry(wallet, decimal.RequireFromString("0.25"))

	assert.Equal(t, model.LedgerEntryTypeDevFund, entry.EntryType)
	assert.True(t, entry.AvailableDelta.Equal(decimal.RequireFromString("0.25")))
	assert.True(t, entry.LockedDelta.Equal(decimal.Zero))
	assert.True(t, entry.AvailableBalanceAfter.Equal(decimal.RequireFromString("0.25")))
	assert.Equal(t, model.LedgerReferenceTypeDevFund, entry.ReferenceType)
	assert.Equal(t, uint(0), entry.ReferenceID)
}
