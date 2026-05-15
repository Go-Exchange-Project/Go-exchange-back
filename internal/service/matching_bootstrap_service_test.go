package service

import (
	"context"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeBootstrapOrderRepository struct {
	orders []model.Order
	err    error
}

func (r fakeBootstrapOrderRepository) FindOpenOrdersForBootstrap() ([]model.Order, error) {
	return r.orders, r.err
}

func TestMatchingOrderFromModelOrderUsesRemainingAmount(t *testing.T) {
	dbOrder := bootstrapOrderFixture(10, model.OrderStatusPartial)
	dbOrder.Amount = decimal.NewFromInt(10)
	dbOrder.FilledAmount = decimal.NewFromInt(3)

	order, err := matchingOrderFromModelOrder(dbOrder)

	require.NoError(t, err)
	require.NotNil(t, order)
	assert.Equal(t, dbOrder.ID, order.ID)
	assert.Equal(t, dbOrder.UserID, order.UserID)
	assert.Equal(t, "BTC", order.CoinSymbol)
	assert.True(t, order.Amount.Equal(decimal.NewFromInt(7)))
	assert.True(t, order.FilledAmount.Equal(decimal.NewFromInt(3)))
}

func TestBootstrapOpenOrdersSubmitsPendingAndPartialOrders(t *testing.T) {
	pending := bootstrapOrderFixture(1, model.OrderStatusPending)
	partial := bootstrapOrderFixture(2, model.OrderStatusPartial)
	partial.Amount = decimal.NewFromInt(8)
	partial.FilledAmount = decimal.NewFromInt(5)
	me := matching.NewMatchingEngine()
	service := NewMatchingBootstrapService(fakeBootstrapOrderRepository{orders: []model.Order{pending, partial}}, me)

	result, err := service.BootstrapOpenOrders(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 2, result.Loaded)
	assert.Equal(t, 2, result.Submitted)
	assert.Equal(t, 0, result.Skipped)
	assert.Equal(t, 1, result.StatusCounts[model.OrderStatusPending])
	assert.Equal(t, 1, result.StatusCounts[model.OrderStatusPartial])

	first := <-me.OrderCh
	second := <-me.OrderCh
	assert.Equal(t, pending.ID, first.ID)
	assert.True(t, first.Amount.Equal(pending.Amount))
	assert.Equal(t, partial.ID, second.ID)
	assert.True(t, second.Amount.Equal(decimal.NewFromInt(3)))
}

func TestBootstrapOpenOrdersSkipsNoRemainingOrders(t *testing.T) {
	empty := bootstrapOrderFixture(1, model.OrderStatusPartial)
	empty.Amount = decimal.NewFromInt(5)
	empty.FilledAmount = decimal.NewFromInt(5)
	me := matching.NewMatchingEngine()
	service := NewMatchingBootstrapService(fakeBootstrapOrderRepository{orders: []model.Order{empty}}, me)

	result, err := service.BootstrapOpenOrders(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 1, result.Loaded)
	assert.Equal(t, 0, result.Submitted)
	assert.Equal(t, 1, result.Skipped)
	select {
	case order := <-me.OrderCh:
		t.Fatalf("unexpected submitted order: %+v", order)
	default:
	}
}

func TestBootstrapOpenOrdersRejectsInvalidSide(t *testing.T) {
	invalid := bootstrapOrderFixture(1, model.OrderStatusPending)
	invalid.Side = model.OrderSide("BROKEN")
	service := NewMatchingBootstrapService(fakeBootstrapOrderRepository{orders: []model.Order{invalid}}, matching.NewMatchingEngine())

	result, err := service.BootstrapOpenOrders(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid order side")
	assert.Equal(t, 1, result.Loaded)
	assert.Equal(t, 0, result.Submitted)
}

func TestBootstrapOpenOrdersCanGenerateTrade(t *testing.T) {
	buy := bootstrapOrderFixture(1, model.OrderStatusPending)
	buy.Side = model.OrderSideBuy
	buy.Price = decimal.NewFromInt(100)
	sell := bootstrapOrderFixture(2, model.OrderStatusPending)
	sell.Side = model.OrderSideSell
	sell.Price = decimal.NewFromInt(90)

	me := matching.NewMatchingEngine()
	me.Start()
	service := NewMatchingBootstrapService(fakeBootstrapOrderRepository{orders: []model.Order{buy, sell}}, me)

	result, err := service.BootstrapOpenOrders(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 2, result.Submitted)

	select {
	case trade := <-me.TradeCh:
		assert.Equal(t, buy.ID, trade.BuyOrderID)
		assert.Equal(t, sell.ID, trade.SellOrderID)
		assert.True(t, trade.Quantity.Equal(decimal.NewFromInt(5)))
	case <-time.After(time.Second):
		t.Fatal("expected bootstrap orders to generate a trade")
	}
}

func bootstrapOrderFixture(id uint, status model.OrderStatus) model.Order {
	return model.Order{
		ID:           id,
		UserID:       100 + id,
		CoinSymbol:   "btc",
		Side:         model.OrderSideBuy,
		OrderType:    model.OrderTypeLimit,
		Price:        decimal.NewFromInt(100),
		Amount:       decimal.NewFromInt(5),
		Status:       status,
		FilledAmount: decimal.Zero,
		CreatedAt:    time.Now(),
	}
}
