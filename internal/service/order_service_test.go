package service

import (
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildOrderSetsUserIDAndDefaultOrderType(t *testing.T) {
	order, err := BuildOrder(CreateOrderInput{
		UserID:     7,
		CoinSymbol: "btc",
		Side:       "buy",
		Price:      "50000.25",
		Amount:     "0.125",
	})

	require.NoError(t, err)
	assert.Equal(t, uint(7), order.UserID)
	assert.Equal(t, "BTC", order.CoinSymbol)
	assert.Equal(t, model.OrderSideBuy, order.Side)
	assert.Equal(t, model.OrderTypeLimit, order.OrderType)
	assert.Equal(t, model.OrderStatusPending, order.Status)
	assert.True(t, order.FilledAmount.Equal(decimal.Zero))
}

func TestBuildOrderParsesDecimalStringsExactly(t *testing.T) {
	order, err := BuildOrder(CreateOrderInput{
		UserID:     1,
		CoinSymbol: "ETH",
		Side:       "SELL",
		OrderType:  "LIMIT",
		Price:      "12345.67890123",
		Amount:     "0.00000001",
	})

	require.NoError(t, err)
	assert.Equal(t, decimal.RequireFromString("12345.67890123"), order.Price)
	assert.Equal(t, decimal.RequireFromString("0.00000001"), order.Amount)
}

func TestBuildOrderRejectsInvalidInputs(t *testing.T) {
	tests := []struct {
		name  string
		input CreateOrderInput
	}{
		{
			name: "invalid side",
			input: CreateOrderInput{
				UserID:     1,
				CoinSymbol: "BTC",
				Side:       "HOLD",
				OrderType:  "LIMIT",
				Price:      "1",
				Amount:     "1",
			},
		},
		{
			name: "unsupported market order",
			input: CreateOrderInput{
				UserID:     1,
				CoinSymbol: "BTC",
				Side:       "BUY",
				OrderType:  "MARKET",
				Price:      "1",
				Amount:     "1",
			},
		},
		{
			name: "invalid order type",
			input: CreateOrderInput{
				UserID:     1,
				CoinSymbol: "BTC",
				Side:       "BUY",
				OrderType:  "STOP",
				Price:      "1",
				Amount:     "1",
			},
		},
		{
			name: "zero price",
			input: CreateOrderInput{
				UserID:     1,
				CoinSymbol: "BTC",
				Side:       "BUY",
				OrderType:  "LIMIT",
				Price:      "0",
				Amount:     "1",
			},
		},
		{
			name: "negative amount",
			input: CreateOrderInput{
				UserID:     1,
				CoinSymbol: "BTC",
				Side:       "BUY",
				OrderType:  "LIMIT",
				Price:      "1",
				Amount:     "-1",
			},
		},
		{
			name: "missing user",
			input: CreateOrderInput{
				CoinSymbol: "BTC",
				Side:       "BUY",
				OrderType:  "LIMIT",
				Price:      "1",
				Amount:     "1",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			order, err := BuildOrder(tt.input)
			require.Error(t, err)
			assert.Nil(t, order)
		})
	}
}

func TestParseOrderStatus(t *testing.T) {
	status, err := parseOrderStatus(" partial ")

	require.NoError(t, err)
	assert.Equal(t, model.OrderStatusPartial, status)

	_, err = parseOrderStatus("BROKEN")
	require.Error(t, err)
}

func TestNormalizeQueryLimit(t *testing.T) {
	assert.Equal(t, DefaultQueryLimit, normalizeQueryLimit(0))
	assert.Equal(t, 25, normalizeQueryLimit(25))
	assert.Equal(t, MaxQueryLimit, normalizeQueryLimit(MaxQueryLimit+1))
}

func TestRemainingOrderQuantityUsesAmountMinusFilledAmount(t *testing.T) {
	order := &model.Order{
		Amount:       decimal.RequireFromString("10.5"),
		FilledAmount: decimal.RequireFromString("3.25"),
	}

	remaining, err := remainingOrderQuantity(order)

	require.NoError(t, err)
	assert.True(t, remaining.Equal(decimal.RequireFromString("7.25")))
}

func TestRemainingOrderQuantityRejectsNoRemainingQuantity(t *testing.T) {
	order := &model.Order{
		Amount:       decimal.NewFromInt(5),
		FilledAmount: decimal.NewFromInt(5),
	}

	_, err := remainingOrderQuantity(order)

	require.Error(t, err)
}
