package service

import (
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeFundWalletInput(t *testing.T) {
	userID, coinSymbol, amount, err := normalizeFundWalletInput(FundWalletInput{
		UserID:     7,
		CoinSymbol: " btc ",
		Amount:     "0.125",
	})

	require.NoError(t, err)
	assert.Equal(t, uint(7), userID)
	assert.Equal(t, "BTC", coinSymbol)
	assert.True(t, amount.Equal(decimal.RequireFromString("0.125")))
}

func TestNormalizeFundWalletInputRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name  string
		input FundWalletInput
	}{
		{name: "missing user", input: FundWalletInput{CoinSymbol: "BTC", Amount: "1"}},
		{name: "missing coin", input: FundWalletInput{UserID: 1, Amount: "1"}},
		{name: "invalid amount", input: FundWalletInput{UserID: 1, CoinSymbol: "BTC", Amount: "abc"}},
		{name: "zero amount", input: FundWalletInput{UserID: 1, CoinSymbol: "BTC", Amount: "0"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, err := normalizeFundWalletInput(tt.input)
			require.Error(t, err)
		})
	}
}

func TestFundedWalletCreateShapeForKRW(t *testing.T) {
	wallet := model.Wallet{
		UserID:           1,
		CoinSymbol:       model.KRWAssetSymbol,
		KRW:              decimal.NewFromInt(1000),
		Quantity:         decimal.Zero,
		AvailableBalance: decimal.NewFromInt(1000),
		LockedBalance:    decimal.Zero,
	}

	assert.True(t, wallet.KRW.Equal(wallet.AvailableBalance.Add(wallet.LockedBalance)))
	assert.True(t, wallet.Quantity.Equal(decimal.Zero))
}
