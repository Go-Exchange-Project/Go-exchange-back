package service

import (
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyTradeFillAccumulatesFilledAmount(t *testing.T) {
	order := &model.Order{
		ID:           1,
		Amount:       decimal.NewFromInt(10),
		FilledAmount: decimal.NewFromInt(3),
	}

	filledAmount, status, err := applyTradeFill(order, decimal.NewFromInt(4))

	require.NoError(t, err)
	assert.Equal(t, decimal.NewFromInt(7), filledAmount)
	assert.Equal(t, model.OrderStatusPartial, status)
}

func TestApplyTradeFillReturnsFilledWhenAmountComplete(t *testing.T) {
	order := &model.Order{
		ID:           1,
		Amount:       decimal.NewFromInt(10),
		FilledAmount: decimal.NewFromInt(7),
	}

	filledAmount, status, err := applyTradeFill(order, decimal.NewFromInt(3))

	require.NoError(t, err)
	assert.Equal(t, decimal.NewFromInt(10), filledAmount)
	assert.Equal(t, model.OrderStatusFilled, status)
}

func TestApplyTradeFillRejectsOverfill(t *testing.T) {
	order := &model.Order{
		ID:           42,
		Amount:       decimal.NewFromInt(10),
		FilledAmount: decimal.NewFromInt(9),
	}

	_, _, err := applyTradeFill(order, decimal.NewFromInt(2))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "order 42")
	assert.Contains(t, err.Error(), "exceeds order amount")
}

func TestValidateOrderStatusForSettlementAllowsOpenStatuses(t *testing.T) {
	for _, status := range []model.OrderStatus{model.OrderStatusPending, model.OrderStatusPartial} {
		t.Run(string(status), func(t *testing.T) {
			order := &model.Order{ID: 1, Status: status}

			require.NoError(t, validateOrderStatusForSettlement(order, "buy"))
		})
	}
}

func TestValidateOrderStatusForSettlementRejectsClosedAndUnknownStatuses(t *testing.T) {
	tests := []struct {
		name       string
		status     model.OrderStatus
		wantErrMsg string
	}{
		{name: "cancelled", status: model.OrderStatusCancelled, wantErrMsg: "cannot be settled"},
		{name: "filled", status: model.OrderStatusFilled, wantErrMsg: "cannot receive additional settlement"},
		{name: "unknown", status: model.OrderStatus("BROKEN"), wantErrMsg: "unknown status"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			order := &model.Order{ID: 2, Status: tt.status}

			err := validateOrderStatusForSettlement(order, "sell")

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErrMsg)
		})
	}
}

func TestSettlementParticipantsUsesOrderUserIDs(t *testing.T) {
	buyOrder := &model.Order{
		ID:     1,
		UserID: 10,
		Side:   model.OrderSideBuy,
	}
	sellOrder := &model.Order{
		ID:     2,
		UserID: 20,
		Side:   model.OrderSideSell,
	}

	participants, err := settlementParticipants(buyOrder, sellOrder)

	require.NoError(t, err)
	assert.Equal(t, uint(10), participants.BuyerUserID)
	assert.Equal(t, uint(20), participants.SellerUserID)
}

func TestTradeQuoteAmount(t *testing.T) {
	trade := &model.Trade{
		Price:    decimal.RequireFromString("50000.25"),
		Quantity: decimal.RequireFromString("0.125"),
	}

	assert.Equal(t, decimal.RequireFromString("6250.03125"), tradeQuoteAmount(trade))
}

func TestReservedQuoteAmountUsesBuyOrderLimitPrice(t *testing.T) {
	buyOrder := &model.Order{
		Price: decimal.NewFromInt(60000),
	}

	assert.Equal(t, decimal.NewFromInt(300000), reservedQuoteAmount(buyOrder, decimal.NewFromInt(5)))
}

func TestDeterministicTradeIdempotencyKeyIsStableForSamePayload(t *testing.T) {
	first := &model.Trade{
		CoinSymbol:  "btc",
		Price:       decimal.RequireFromString("90000.0"),
		Quantity:    decimal.RequireFromString("0.25"),
		TradedAt:    time.Now(),
		BuyOrderID:  10,
		SellOrderID: 20,
	}
	second := &model.Trade{
		CoinSymbol:  " BTC ",
		Price:       decimal.RequireFromString("90000"),
		Quantity:    decimal.RequireFromString("0.250"),
		TradedAt:    time.Now().Add(time.Minute),
		BuyOrderID:  10,
		SellOrderID: 20,
	}

	assert.Equal(t, deterministicTradeIdempotencyKey(first), deterministicTradeIdempotencyKey(second))
}

func TestDeterministicTradeIdempotencyKeyChangesWithPayload(t *testing.T) {
	first := &model.Trade{
		CoinSymbol:  "BTC",
		Price:       decimal.NewFromInt(90),
		Quantity:    decimal.NewFromInt(5),
		BuyOrderID:  10,
		SellOrderID: 20,
	}
	second := &model.Trade{
		CoinSymbol:  "BTC",
		Price:       decimal.NewFromInt(91),
		Quantity:    decimal.NewFromInt(5),
		BuyOrderID:  10,
		SellOrderID: 20,
	}

	assert.NotEqual(t, deterministicTradeIdempotencyKey(first), deterministicTradeIdempotencyKey(second))
}

func TestValidateIdempotentTradePayloadRejectsDifferentPayload(t *testing.T) {
	existing := &model.Trade{
		IdempotencyKey: "shared-key",
		CoinSymbol:     "BTC",
		Price:          decimal.NewFromInt(90),
		Quantity:       decimal.NewFromInt(5),
		BuyOrderID:     10,
		SellOrderID:    20,
	}
	incoming := &model.Trade{
		IdempotencyKey: "shared-key",
		CoinSymbol:     "BTC",
		Price:          decimal.NewFromInt(91),
		Quantity:       decimal.NewFromInt(5),
		BuyOrderID:     10,
		SellOrderID:    20,
	}

	err := validateIdempotentTradePayload(existing, incoming)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "idempotency key conflict")
}
