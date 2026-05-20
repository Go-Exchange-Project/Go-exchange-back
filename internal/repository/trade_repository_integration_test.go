package repository

import (
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegrationListTradesByUserIDScopesAndComputesSide(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	buyerID := repositoryTestUserID(35)
	sellerID := repositoryTestUserID(36)
	otherUserID := repositoryTestUserID(37)
	defer cleanupRepositoryUsers(t, db, buyerID, sellerID, otherUserID)

	buyOrder := tradeRepositoryOrder(buyerID, "TRADE-LIST", model.OrderSideBuy)
	sellOrder := tradeRepositoryOrder(sellerID, "TRADE-LIST", model.OrderSideSell)
	otherBuy := tradeRepositoryOrder(otherUserID, "TRADE-LIST", model.OrderSideBuy)
	otherSell := tradeRepositoryOrder(otherUserID, "TRADE-LIST", model.OrderSideSell)
	for _, order := range []*model.Order{&buyOrder, &sellOrder, &otherBuy, &otherSell} {
		require.NoError(t, db.Create(order).Error)
	}

	userTrade := model.Trade{
		IdempotencyKey: "trade-repo-user",
		CoinSymbol:     "TRADE-LIST",
		Price:          decimal.NewFromInt(90),
		Quantity:       decimal.NewFromInt(5),
		TradedAt:       time.Now().UTC(),
		BuyOrderID:     buyOrder.ID,
		SellOrderID:    sellOrder.ID,
	}
	otherTrade := model.Trade{
		IdempotencyKey: "trade-repo-other",
		CoinSymbol:     "TRADE-LIST",
		Price:          decimal.NewFromInt(91),
		Quantity:       decimal.NewFromInt(1),
		TradedAt:       time.Now().UTC(),
		BuyOrderID:     otherBuy.ID,
		SellOrderID:    otherSell.ID,
	}
	require.NoError(t, db.Create(&userTrade).Error)
	require.NoError(t, db.Create(&otherTrade).Error)

	trades, err := NewTradeRepository(db).ListByUserID(buyerID, TradeListFilter{CoinSymbol: "TRADE-LIST", Limit: 10})
	require.NoError(t, err)

	require.Len(t, trades, 1)
	assert.Equal(t, userTrade.ID, trades[0].ID)
	assert.Equal(t, model.OrderSideBuy, trades[0].Side)
}

func tradeRepositoryOrder(userID uint, coinSymbol string, side model.OrderSide) model.Order {
	return model.Order{
		UserID:       userID,
		CoinSymbol:   coinSymbol,
		Side:         side,
		OrderType:    model.OrderTypeLimit,
		Price:        decimal.NewFromInt(100),
		Amount:       decimal.NewFromInt(5),
		Status:       model.OrderStatusFilled,
		FilledAmount: decimal.NewFromInt(5),
		CreatedAt:    time.Now().UTC(),
	}
}
