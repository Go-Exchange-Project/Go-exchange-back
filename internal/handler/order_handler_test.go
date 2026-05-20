package handler

import (
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/auth"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
)

func TestAuthenticatedUserIDReadsGinContext(t *testing.T) {
	c := &gin.Context{}
	c.Set(auth.UserIDContextKey, uint(42))

	userID, ok := authenticatedUserID(c)

	assert.True(t, ok)
	assert.Equal(t, uint(42), userID)
}

func TestAuthenticatedUserIDRejectsMissingContext(t *testing.T) {
	c := &gin.Context{}

	userID, ok := authenticatedUserID(c)

	assert.False(t, ok)
	assert.Equal(t, uint(0), userID)
}

func TestOrderResponseUsesDecimalStringsAndRemaining(t *testing.T) {
	order := model.Order{
		ID:           7,
		CoinSymbol:   "BTC",
		Side:         model.OrderSideBuy,
		OrderType:    model.OrderTypeLimit,
		Status:       model.OrderStatusPartial,
		Price:        decimal.RequireFromString("100.25"),
		Amount:       decimal.RequireFromString("3.5"),
		FilledAmount: decimal.RequireFromString("1.25"),
		CreatedAt:    time.Unix(1000, 0),
	}

	response := orderResponse(order)

	assert.Equal(t, "100.25", response.Price)
	assert.Equal(t, "3.5", response.Amount)
	assert.Equal(t, "1.25", response.FilledAmount)
	assert.Equal(t, "2.25", response.Remaining)
}

func TestWalletResponseUsesAvailableLockedAndTotalStrings(t *testing.T) {
	wallet := model.Wallet{
		ID:               1,
		CoinSymbol:       "BTC",
		AvailableBalance: decimal.RequireFromString("1.5"),
		LockedBalance:    decimal.RequireFromString("0.25"),
		AvgBuyPrice:      decimal.RequireFromString("90000"),
	}

	response := walletResponse(wallet)

	assert.Equal(t, "1.5", response.AvailableBalance)
	assert.Equal(t, "0.25", response.LockedBalance)
	assert.Equal(t, "1.75", response.TotalBalance)
	assert.Equal(t, "90000", response.AvgBuyPrice)
}

func TestTradeResponseUsesUserSideAndDecimalStrings(t *testing.T) {
	trade := repository.UserTrade{
		ID:          2,
		CoinSymbol:  "ETH",
		Side:        model.OrderSideSell,
		Price:       decimal.RequireFromString("2000.5"),
		Quantity:    decimal.RequireFromString("0.75"),
		TradedAt:    time.Unix(2000, 0),
		BuyOrderID:  10,
		SellOrderID: 11,
	}

	response := tradeResponse(trade)

	assert.Equal(t, model.OrderSideSell, response.Side)
	assert.Equal(t, "2000.5", response.Price)
	assert.Equal(t, "0.75", response.Quantity)
}
