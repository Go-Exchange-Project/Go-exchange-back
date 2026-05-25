package service

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKRWTickSizeUsesPriceBands(t *testing.T) {
	tests := []struct {
		price string
		want  string
	}{
		{price: "0.00922", want: "0.00001"},
		{price: "9.99", want: "0.01"},
		{price: "10", want: "0.1"},
		{price: "100", want: "1"},
		{price: "1000", want: "5"},
		{price: "10000", want: "10"},
		{price: "100000", want: "50"},
		{price: "500000", want: "100"},
		{price: "1000000", want: "500"},
		{price: "2000000", want: "1000"},
	}

	for _, tt := range tests {
		t.Run(tt.price, func(t *testing.T) {
			got := krwTickSize(decimal.RequireFromString(tt.price))
			assert.True(t, got.Equal(decimal.RequireFromString(tt.want)))
		})
	}
}

func TestValidateLimitOrderPolicyAcceptsValidTickAndNotional(t *testing.T) {
	err := validateLimitOrderPolicy(
		decimal.RequireFromString("50000"),
		decimal.RequireFromString("0.1"),
	)

	require.NoError(t, err)
}

func TestValidateLimitOrderPolicyRejectsPriceOutsideTick(t *testing.T) {
	err := validateLimitOrderPolicy(
		decimal.RequireFromString("50001"),
		decimal.RequireFromString("1"),
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "tick size")
}

func TestValidateLimitOrderPolicyRejectsSmallNotional(t *testing.T) {
	err := validateLimitOrderPolicy(
		decimal.RequireFromString("50000"),
		decimal.RequireFromString("0.099"),
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least 5000 KRW")
}

func TestBuildOrderRejectsInvalidLimitOrderPolicy(t *testing.T) {
	tests := []struct {
		name   string
		price  string
		amount string
		want   string
	}{
		{name: "invalid tick", price: "10001", amount: "1", want: "tick size"},
		{name: "below minimum notional", price: "10000", amount: "0.499", want: "at least 5000 KRW"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			order, err := BuildOrder(CreateOrderInput{
				UserID:     1,
				CoinSymbol: "BTC",
				Side:       "BUY",
				OrderType:  "LIMIT",
				Price:      tt.price,
				Amount:     tt.amount,
			})

			require.Error(t, err)
			assert.Nil(t, order)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}
