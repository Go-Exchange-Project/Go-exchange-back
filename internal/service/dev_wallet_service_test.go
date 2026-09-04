package service

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeFundWalletInput(t *testing.T) {
	userID, coinSymbol, amount, requestKey, err := normalizeFundWalletInput(FundWalletInput{
		UserID:     7,
		CoinSymbol: " btc ",
		Amount:     "0.125",
		RequestKey: " req-1 ",
	})

	require.NoError(t, err)
	assert.Equal(t, uint(7), userID)
	assert.Equal(t, "BTC", coinSymbol)
	assert.True(t, amount.Equal(decimal.RequireFromString("0.125")))
	assert.Equal(t, "req-1", requestKey)
}

func TestNormalizeFundWalletInputRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name  string
		input FundWalletInput
	}{
		{name: "missing user", input: FundWalletInput{CoinSymbol: "BTC", Amount: "1", RequestKey: "k"}},
		{name: "missing coin", input: FundWalletInput{UserID: 1, Amount: "1", RequestKey: "k"}},
		{name: "invalid amount", input: FundWalletInput{UserID: 1, CoinSymbol: "BTC", Amount: "abc", RequestKey: "k"}},
		{name: "zero amount", input: FundWalletInput{UserID: 1, CoinSymbol: "BTC", Amount: "0", RequestKey: "k"}},
		// 키가 없으면 재시도를 구분할 수 없다. 서버가 대신 만들어 주지 않는다.
		{name: "missing request key", input: FundWalletInput{UserID: 1, CoinSymbol: "BTC", Amount: "1"}},
		{name: "blank request key", input: FundWalletInput{UserID: 1, CoinSymbol: "BTC", Amount: "1", RequestKey: "   "}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, _, err := normalizeFundWalletInput(tt.input)
			require.Error(t, err)
		})
	}
}
