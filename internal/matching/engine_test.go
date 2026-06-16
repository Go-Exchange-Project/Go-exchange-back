package matching

import (
	"errors"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testOrder(id uint, symbol string, side model.OrderSide, price int64, amount int64) *Order {
	return &Order{
		ID:         id,
		CoinSymbol: symbol,
		Side:       side,
		Price:      decimal.NewFromInt(price),
		Amount:     decimal.NewFromInt(amount),
		CreatedAt:  time.Now(),
	}
}

func testUserOrder(id uint, userID uint, symbol string, side model.OrderSide, price int64, amount int64) *Order {
	order := testOrder(id, symbol, side, price, amount)
	order.UserID = userID
	return order
}

func submitAndWaitSnapshot(t *testing.T, me *MatchingEngine, order *Order) OrderBookSnapshot {
	t.Helper()

	me.OrderCh <- order
	select {
	case snapshot := <-me.SnapshotCh:
		return snapshot
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for orderbook snapshot")
		return OrderBookSnapshot{}
	}
}

func requireNextTrade(t *testing.T, me *MatchingEngine) *model.Trade {
	t.Helper()

	select {
	case trade := <-me.TradeCh:
		return trade
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for trade")
		return nil
	}
}

func requireNextExecutionEvent(t *testing.T, me *MatchingEngine) ExecutionEvent {
	t.Helper()

	select {
	case event := <-me.ExecutionCh:
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for execution event")
		return ExecutionEvent{}
	}
}

func assertNoTrade(t *testing.T, me *MatchingEngine) {
	t.Helper()

	select {
	case trade := <-me.TradeCh:
		t.Fatalf("unexpected trade: %+v", trade)
	default:
	}
}

func requireCancelSnapshot(t *testing.T, me *MatchingEngine) OrderBookSnapshot {
	t.Helper()

	select {
	case snapshot := <-me.SnapshotCh:
		return snapshot
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for cancel snapshot")
		return OrderBookSnapshot{}
	}
}

func TestMatch_BuyPriceCrossesAsk(t *testing.T) {
	me := NewMatchingEngine()
	me.Start()

	submitAndWaitSnapshot(t, me, testOrder(1, "BTC", model.OrderSideSell, 50000, 1))
	submitAndWaitSnapshot(t, me, testOrder(2, "BTC", model.OrderSideBuy, 55000, 1))

	trade := requireNextTrade(t, me)

	assert.Equal(t, "BTC", trade.CoinSymbol)
	assert.Equal(t, decimal.NewFromInt(50000), trade.Price)
	assert.Equal(t, decimal.NewFromInt(1), trade.Quantity)
	assert.Equal(t, int64(1), trade.EngineSequence)
	assert.NotEmpty(t, trade.EngineEventID)
}

func TestMatch_BuyPriceBelowAskDoesNotCross(t *testing.T) {
	me := NewMatchingEngine()
	me.Start()

	submitAndWaitSnapshot(t, me, testOrder(1, "BTC", model.OrderSideSell, 55000, 1))
	submitAndWaitSnapshot(t, me, testOrder(2, "BTC", model.OrderSideBuy, 50000, 1))

	assertNoTrade(t, me)
}

func TestMatch_EmptyOppositeBookDoesNotCross(t *testing.T) {
	me := NewMatchingEngine()
	me.Start()

	submitAndWaitSnapshot(t, me, testOrder(2, "BTC", model.OrderSideBuy, 55000, 10))

	assertNoTrade(t, me)
}

func TestMatch_DifferentSymbolsDoNotCross(t *testing.T) {
	me := NewMatchingEngine()
	me.Start()

	submitAndWaitSnapshot(t, me, testOrder(1, "BTC", model.OrderSideSell, 50000, 1))
	submitAndWaitSnapshot(t, me, testOrder(2, "ETH", model.OrderSideBuy, 60000, 1))

	assertNoTrade(t, me)
	assert.Equal(t, 1, me.GetOrderBook("BTC").SellOrders.Len())
	assert.Equal(t, 1, me.GetOrderBook("ETH").BuyOrders.Len())
}

func TestMatch_BuyPriorityUsesHighestPrice(t *testing.T) {
	me := NewMatchingEngine()
	me.Start()

	submitAndWaitSnapshot(t, me, testOrder(1, "BTC", model.OrderSideBuy, 40000, 1))
	submitAndWaitSnapshot(t, me, testOrder(2, "BTC", model.OrderSideBuy, 60000, 1))
	submitAndWaitSnapshot(t, me, testOrder(3, "BTC", model.OrderSideSell, 50000, 1))

	trade := requireNextTrade(t, me)

	assert.Equal(t, uint(2), trade.BuyOrderID)
	assert.Equal(t, decimal.NewFromInt(60000), trade.Price)
	assertNoTrade(t, me)
}

func TestMatch_SellPriorityUsesLowestPrice(t *testing.T) {
	me := NewMatchingEngine()
	me.Start()

	submitAndWaitSnapshot(t, me, testOrder(1, "BTC", model.OrderSideSell, 60000, 1))
	submitAndWaitSnapshot(t, me, testOrder(2, "BTC", model.OrderSideSell, 40000, 1))
	submitAndWaitSnapshot(t, me, testOrder(3, "BTC", model.OrderSideBuy, 50000, 1))

	trade := requireNextTrade(t, me)

	assert.Equal(t, uint(2), trade.SellOrderID)
	assert.Equal(t, decimal.NewFromInt(40000), trade.Price)
	assertNoTrade(t, me)
}

func TestMatch_FIFOWithinSamePriceLevel(t *testing.T) {
	me := NewMatchingEngine()
	me.Start()

	submitAndWaitSnapshot(t, me, testOrder(1, "BTC", model.OrderSideBuy, 50000, 1))
	submitAndWaitSnapshot(t, me, testOrder(2, "BTC", model.OrderSideBuy, 50000, 1))
	submitAndWaitSnapshot(t, me, testOrder(3, "BTC", model.OrderSideSell, 50000, 1))

	trade := requireNextTrade(t, me)

	assert.Equal(t, uint(1), trade.BuyOrderID)
	buyLevel, ok := me.GetOrderBook("BTC").BuyOrders.Max()
	require.True(t, ok)
	assert.Equal(t, uint(2), buyLevel.Orders.Front().ID)
}

func TestMatch_BuySkipsOwnSellOrderAndMatchesOtherUser(t *testing.T) {
	me := NewMatchingEngine()
	me.Start()

	ownSell := testUserOrder(1, 10, "BTC", model.OrderSideSell, 50000, 1)
	otherSell := testUserOrder(2, 20, "BTC", model.OrderSideSell, 50000, 1)
	incomingBuy := testUserOrder(3, 10, "BTC", model.OrderSideBuy, 50000, 1)
	submitAndWaitSnapshot(t, me, ownSell)
	submitAndWaitSnapshot(t, me, otherSell)
	submitAndWaitSnapshot(t, me, incomingBuy)

	trade := requireNextTrade(t, me)

	assert.Equal(t, incomingBuy.ID, trade.BuyOrderID)
	assert.Equal(t, otherSell.ID, trade.SellOrderID)
	assertNoTrade(t, me)
	sellLevel, ok := me.GetOrderBook("BTC").SellOrders.Min()
	require.True(t, ok)
	require.Equal(t, 1, sellLevel.Orders.Len())
	assert.Equal(t, ownSell.ID, sellLevel.Orders.Front().ID)
}

func TestMatch_SellSkipsOwnBuyOrderAndMatchesOtherUser(t *testing.T) {
	me := NewMatchingEngine()
	me.Start()

	ownBuy := testUserOrder(1, 10, "BTC", model.OrderSideBuy, 50000, 1)
	otherBuy := testUserOrder(2, 20, "BTC", model.OrderSideBuy, 50000, 1)
	incomingSell := testUserOrder(3, 10, "BTC", model.OrderSideSell, 50000, 1)
	submitAndWaitSnapshot(t, me, ownBuy)
	submitAndWaitSnapshot(t, me, otherBuy)
	submitAndWaitSnapshot(t, me, incomingSell)

	trade := requireNextTrade(t, me)

	assert.Equal(t, otherBuy.ID, trade.BuyOrderID)
	assert.Equal(t, incomingSell.ID, trade.SellOrderID)
	assertNoTrade(t, me)
	buyLevel, ok := me.GetOrderBook("BTC").BuyOrders.Max()
	require.True(t, ok)
	require.Equal(t, 1, buyLevel.Orders.Len())
	assert.Equal(t, ownBuy.ID, buyLevel.Orders.Front().ID)
}

func TestMatch_SelfTradeOnlyDoesNotEmitTrade(t *testing.T) {
	me := NewMatchingEngine()
	me.Start()

	submitAndWaitSnapshot(t, me, testUserOrder(1, 10, "BTC", model.OrderSideSell, 50000, 1))
	submitAndWaitSnapshot(t, me, testUserOrder(2, 10, "BTC", model.OrderSideBuy, 60000, 1))

	assertNoTrade(t, me)
	assert.Equal(t, 1, me.GetOrderBook("BTC").SellOrders.Len())
	assert.Equal(t, 1, me.GetOrderBook("BTC").BuyOrders.Len())
}

func TestMatch_LargeBuyMatchesMultipleSellOrders(t *testing.T) {
	me := NewMatchingEngine()
	me.Start()

	submitAndWaitSnapshot(t, me, testOrder(1, "BTC", model.OrderSideSell, 50000, 2))
	submitAndWaitSnapshot(t, me, testOrder(2, "BTC", model.OrderSideSell, 51000, 3))
	submitAndWaitSnapshot(t, me, testOrder(3, "BTC", model.OrderSideBuy, 60000, 5))

	firstTrade := requireNextTrade(t, me)
	secondTrade := requireNextTrade(t, me)

	assert.Equal(t, uint(1), firstTrade.SellOrderID)
	assert.Equal(t, decimal.NewFromInt(2), firstTrade.Quantity)
	assert.Equal(t, uint(2), secondTrade.SellOrderID)
	assert.Equal(t, decimal.NewFromInt(3), secondTrade.Quantity)
	assert.Equal(t, int64(1), firstTrade.EngineSequence)
	assert.Equal(t, int64(2), secondTrade.EngineSequence)
	assert.NotEqual(t, firstTrade.EngineEventID, secondTrade.EngineEventID)
	assertNoTrade(t, me)
	assert.Equal(t, 0, me.GetOrderBook("BTC").BuyOrders.Len())
	assert.Equal(t, 0, me.GetOrderBook("BTC").SellOrders.Len())
}

func TestMatch_LargeSellMatchesMultipleBuyOrders(t *testing.T) {
	me := NewMatchingEngine()
	me.Start()

	submitAndWaitSnapshot(t, me, testOrder(1, "BTC", model.OrderSideBuy, 60000, 2))
	submitAndWaitSnapshot(t, me, testOrder(2, "BTC", model.OrderSideBuy, 55000, 3))
	submitAndWaitSnapshot(t, me, testOrder(3, "BTC", model.OrderSideSell, 50000, 5))

	firstTrade := requireNextTrade(t, me)
	secondTrade := requireNextTrade(t, me)

	assert.Equal(t, uint(1), firstTrade.BuyOrderID)
	assert.Equal(t, decimal.NewFromInt(2), firstTrade.Quantity)
	assert.Equal(t, uint(2), secondTrade.BuyOrderID)
	assert.Equal(t, decimal.NewFromInt(3), secondTrade.Quantity)
	assert.Equal(t, int64(1), firstTrade.EngineSequence)
	assert.Equal(t, int64(2), secondTrade.EngineSequence)
	assert.NotEqual(t, firstTrade.EngineEventID, secondTrade.EngineEventID)
	assertNoTrade(t, me)
	assert.Equal(t, 0, me.GetOrderBook("BTC").BuyOrders.Len())
	assert.Equal(t, 0, me.GetOrderBook("BTC").SellOrders.Len())
}

func TestMatch_PartialFillLeavesRemainingQuantityOnBook(t *testing.T) {
	me := NewMatchingEngine()
	me.Start()

	buyOrder := testOrder(2, "BTC", model.OrderSideBuy, 55000, 10)
	submitAndWaitSnapshot(t, me, testOrder(1, "BTC", model.OrderSideSell, 50000, 7))
	submitAndWaitSnapshot(t, me, buyOrder)

	trade := requireNextTrade(t, me)

	assert.Equal(t, decimal.NewFromInt(7), trade.Quantity)
	buyLevel, ok := me.GetOrderBook("BTC").BuyOrders.Max()
	require.True(t, ok)
	assert.Equal(t, decimal.NewFromInt(3), buyLevel.Orders.Front().Amount)
	assert.Equal(t, decimal.NewFromInt(7), buyOrder.FilledAmount)
}

func TestMatch_FilledOrdersAndEmptyPriceLevelsAreRemoved(t *testing.T) {
	me := NewMatchingEngine()
	me.Start()

	submitAndWaitSnapshot(t, me, testOrder(1, "BTC", model.OrderSideSell, 50000, 1))
	submitAndWaitSnapshot(t, me, testOrder(2, "BTC", model.OrderSideBuy, 50000, 1))

	trade := requireNextTrade(t, me)

	assert.Equal(t, decimal.NewFromInt(1), trade.Quantity)
	assert.Equal(t, 0, me.GetOrderBook("BTC").BuyOrders.Len())
	assert.Equal(t, 0, me.GetOrderBook("BTC").SellOrders.Len())
}

func TestMarketBuyConsumesAsksByQuoteBudgetAndDoesNotRest(t *testing.T) {
	me := NewMatchingEngine()

	me.Match(testUserOrder(1, 20, "BTC", model.OrderSideSell, 5000, 1))
	me.Match(testUserOrder(2, 30, "BTC", model.OrderSideSell, 7000, 2))
	me.Match(&Order{
		ID:          3,
		UserID:      10,
		CoinSymbol:  "BTC",
		Side:        model.OrderSideBuy,
		OrderType:   model.OrderTypeMarket,
		QuoteAmount: decimal.NewFromInt(12000),
	})

	firstTrade := requireNextTrade(t, me)
	secondTrade := requireNextTrade(t, me)
	firstEvent := requireNextExecutionEvent(t, me)
	secondEvent := requireNextExecutionEvent(t, me)
	doneEvent := requireNextExecutionEvent(t, me)

	assert.Equal(t, uint(1), firstTrade.SellOrderID)
	assert.Equal(t, decimal.NewFromInt(5000), firstTrade.Price)
	assert.Equal(t, decimal.NewFromInt(1), firstTrade.Quantity)
	assert.Equal(t, uint(2), secondTrade.SellOrderID)
	assert.Equal(t, decimal.NewFromInt(7000), secondTrade.Price)
	assert.True(t, secondTrade.Quantity.Equal(decimal.NewFromInt(1)))
	require.NotNil(t, firstEvent.Trade)
	require.NotNil(t, secondEvent.Trade)
	require.NotNil(t, doneEvent.MarketOrderDone)
	assert.True(t, doneEvent.MarketOrderDone.RemainingQuoteAmount.IsZero())
	assert.Equal(t, 0, me.GetOrderBook("BTC").BuyOrders.Len())
	sellLevel, ok := me.GetOrderBook("BTC").SellOrders.Min()
	require.True(t, ok)
	assert.True(t, sellLevel.Orders.Front().Amount.Equal(decimal.NewFromInt(1)))
}

func TestMarketBuyWithNoLiquidityDoesNotRest(t *testing.T) {
	me := NewMatchingEngine()

	me.Match(&Order{
		ID:          3,
		UserID:      10,
		CoinSymbol:  "BTC",
		Side:        model.OrderSideBuy,
		OrderType:   model.OrderTypeMarket,
		QuoteAmount: decimal.NewFromInt(12000),
	})

	doneEvent := requireNextExecutionEvent(t, me)

	require.NotNil(t, doneEvent.MarketOrderDone)
	assert.Equal(t, decimal.NewFromInt(12000), doneEvent.MarketOrderDone.RemainingQuoteAmount)
	assert.Equal(t, 0, me.GetOrderBook("BTC").BuyOrders.Len())
}

func TestMarketBuySkipsOwnAskOnlyAndDoesNotRest(t *testing.T) {
	me := NewMatchingEngine()

	me.Match(testUserOrder(1, 10, "BTC", model.OrderSideSell, 5000, 1))
	me.Match(&Order{
		ID:          2,
		UserID:      10,
		CoinSymbol:  "BTC",
		Side:        model.OrderSideBuy,
		OrderType:   model.OrderTypeMarket,
		QuoteAmount: decimal.NewFromInt(5000),
	})

	doneEvent := requireNextExecutionEvent(t, me)

	assertNoTrade(t, me)
	require.NotNil(t, doneEvent.MarketOrderDone)
	assert.Equal(t, decimal.NewFromInt(5000), doneEvent.MarketOrderDone.RemainingQuoteAmount)
	assert.Equal(t, 0, me.GetOrderBook("BTC").BuyOrders.Len())
	sellLevel, ok := me.GetOrderBook("BTC").SellOrders.Min()
	require.True(t, ok)
	assert.Equal(t, uint(1), sellLevel.Orders.Front().ID)
	assert.Equal(t, decimal.NewFromInt(1), sellLevel.Orders.Front().Amount)
}

func TestMarketSellConsumesHighestBidsAndDoesNotRest(t *testing.T) {
	me := NewMatchingEngine()

	me.Match(testUserOrder(1, 20, "BTC", model.OrderSideBuy, 7000, 1))
	me.Match(testUserOrder(2, 30, "BTC", model.OrderSideBuy, 5000, 2))
	me.Match(&Order{
		ID:         3,
		UserID:     10,
		CoinSymbol: "BTC",
		Side:       model.OrderSideSell,
		OrderType:  model.OrderTypeMarket,
		Amount:     decimal.NewFromInt(2),
	})

	firstTrade := requireNextTrade(t, me)
	secondTrade := requireNextTrade(t, me)
	requireNextExecutionEvent(t, me)
	requireNextExecutionEvent(t, me)
	doneEvent := requireNextExecutionEvent(t, me)

	assert.Equal(t, uint(1), firstTrade.BuyOrderID)
	assert.Equal(t, decimal.NewFromInt(7000), firstTrade.Price)
	assert.Equal(t, decimal.NewFromInt(1), firstTrade.Quantity)
	assert.Equal(t, uint(2), secondTrade.BuyOrderID)
	assert.Equal(t, decimal.NewFromInt(5000), secondTrade.Price)
	assert.Equal(t, decimal.NewFromInt(1), secondTrade.Quantity)
	require.NotNil(t, doneEvent.MarketOrderDone)
	assert.True(t, doneEvent.MarketOrderDone.RemainingAmount.IsZero())
	assert.Equal(t, 0, me.GetOrderBook("BTC").SellOrders.Len())
	buyLevel, ok := me.GetOrderBook("BTC").BuyOrders.Max()
	require.True(t, ok)
	assert.Equal(t, decimal.NewFromInt(1), buyLevel.Orders.Front().Amount)
}

func TestMarketSellSkipsOwnBidOnlyAndDoesNotRest(t *testing.T) {
	me := NewMatchingEngine()

	me.Match(testUserOrder(1, 10, "BTC", model.OrderSideBuy, 5000, 1))
	me.Match(&Order{
		ID:         2,
		UserID:     10,
		CoinSymbol: "BTC",
		Side:       model.OrderSideSell,
		OrderType:  model.OrderTypeMarket,
		Amount:     decimal.NewFromInt(1),
	})

	doneEvent := requireNextExecutionEvent(t, me)

	assertNoTrade(t, me)
	require.NotNil(t, doneEvent.MarketOrderDone)
	assert.Equal(t, decimal.NewFromInt(1), doneEvent.MarketOrderDone.RemainingAmount)
	assert.Equal(t, 0, me.GetOrderBook("BTC").SellOrders.Len())
	buyLevel, ok := me.GetOrderBook("BTC").BuyOrders.Max()
	require.True(t, ok)
	assert.Equal(t, uint(1), buyLevel.Orders.Front().ID)
	assert.Equal(t, decimal.NewFromInt(1), buyLevel.Orders.Front().Amount)
}

func TestGetOrderBookSnapshot_AsksAscendingBidsDescending(t *testing.T) {
	me := NewMatchingEngine()
	me.Start()

	submitAndWaitSnapshot(t, me, testOrder(1, "BTC", model.OrderSideSell, 70000, 1))
	submitAndWaitSnapshot(t, me, testOrder(2, "BTC", model.OrderSideSell, 60000, 1))
	submitAndWaitSnapshot(t, me, testOrder(3, "BTC", model.OrderSideBuy, 50000, 1))
	submitAndWaitSnapshot(t, me, testOrder(4, "BTC", model.OrderSideBuy, 40000, 1))

	snapshot := me.GetOrderBookSnapshot("BTC")

	require.Len(t, snapshot.Asks, 2)
	require.Len(t, snapshot.Bids, 2)
	assert.Equal(t, decimal.NewFromInt(60000), snapshot.Asks[0].Price)
	assert.Equal(t, decimal.NewFromInt(70000), snapshot.Asks[1].Price)
	assert.Equal(t, decimal.NewFromInt(50000), snapshot.Bids[0].Price)
	assert.Equal(t, decimal.NewFromInt(40000), snapshot.Bids[1].Price)
}

func TestRequestOrderBookSnapshot_ReturnsCurrentSymbolBook(t *testing.T) {
	me := NewMatchingEngine()
	me.Start()

	submitAndWaitSnapshot(t, me, testOrder(1, "AVAX", model.OrderSideSell, 10300, 1))
	submitAndWaitSnapshot(t, me, testOrder(2, "AVAX", model.OrderSideSell, 10200, 1))

	snapshot, err := me.RequestOrderBookSnapshot("AVAX", DefaultSnapshotDepth)

	require.NoError(t, err)
	assert.Equal(t, "AVAX", snapshot.CoinSymbol)
	require.Len(t, snapshot.Asks, 2)
	assert.Equal(t, decimal.NewFromInt(10200), snapshot.Asks[0].Price)
	assert.Equal(t, decimal.NewFromInt(10300), snapshot.Asks[1].Price)
	assert.Empty(t, snapshot.Bids)
}

func TestGetOrderBookSnapshot_LimitsDepth(t *testing.T) {
	me := NewMatchingEngine()

	for i := int64(0); i < int64(DefaultSnapshotDepth+5); i++ {
		me.Match(testOrder(uint(i+1), "BTC", model.OrderSideSell, 1000+i, 1))
		me.Match(testOrder(uint(i+100), "BTC", model.OrderSideBuy, 900-i, 1))
	}

	snapshot := me.GetOrderBookSnapshot("BTC")

	require.Len(t, snapshot.Asks, DefaultSnapshotDepth)
	require.Len(t, snapshot.Bids, DefaultSnapshotDepth)
	assert.Equal(t, decimal.NewFromInt(1000), snapshot.Asks[0].Price)
	assert.Equal(t, decimal.NewFromInt(int64(1000+DefaultSnapshotDepth-1)), snapshot.Asks[DefaultSnapshotDepth-1].Price)
	assert.Equal(t, decimal.NewFromInt(900), snapshot.Bids[0].Price)
	assert.Equal(t, decimal.NewFromInt(int64(900-DefaultSnapshotDepth+1)), snapshot.Bids[DefaultSnapshotDepth-1].Price)
}

func TestGetOrderBookSnapshotWithDepth_UsesCustomDepth(t *testing.T) {
	me := NewMatchingEngine()

	for i := int64(0); i < 5; i++ {
		me.Match(testOrder(uint(i+1), "BTC", model.OrderSideSell, 1000+i, 1))
		me.Match(testOrder(uint(i+100), "BTC", model.OrderSideBuy, 900-i, 1))
	}

	snapshot := me.GetOrderBookSnapshotWithDepth("BTC", 2)

	require.Len(t, snapshot.Asks, 2)
	require.Len(t, snapshot.Bids, 2)
	assert.Equal(t, decimal.NewFromInt(1000), snapshot.Asks[0].Price)
	assert.Equal(t, decimal.NewFromInt(1001), snapshot.Asks[1].Price)
	assert.Equal(t, decimal.NewFromInt(900), snapshot.Bids[0].Price)
	assert.Equal(t, decimal.NewFromInt(899), snapshot.Bids[1].Price)
}

func TestCancelOrder_RemovesOrderFromOrderBook(t *testing.T) {
	me := NewMatchingEngine()
	me.Start()

	order := testOrder(10, "BTC", model.OrderSideBuy, 50000, 2)
	submitAndWaitSnapshot(t, me, order)

	result := me.CancelOrder(CancelOrderCommand{
		CoinSymbol: "BTC",
		OrderID:    order.ID,
		Side:       order.Side,
		Price:      order.Price,
	})
	require.NoError(t, result.Err)
	assert.True(t, result.Removed)
	requireCancelSnapshot(t, me)

	assert.Equal(t, 0, me.GetOrderBook("BTC").BuyOrders.Len())
}

func TestCancelOrder_PublishesUpdatedSnapshot(t *testing.T) {
	me := NewMatchingEngine()
	me.Start()

	first := testOrder(11, "BTC", model.OrderSideBuy, 50000, 2)
	second := testOrder(12, "BTC", model.OrderSideBuy, 49000, 1)
	submitAndWaitSnapshot(t, me, first)
	submitAndWaitSnapshot(t, me, second)

	result := me.CancelOrder(CancelOrderCommand{
		CoinSymbol: "BTC",
		OrderID:    first.ID,
		Side:       first.Side,
		Price:      first.Price,
	})
	require.NoError(t, result.Err)

	snapshot := requireCancelSnapshot(t, me)
	require.Len(t, snapshot.Bids, 1)
	assert.Equal(t, decimal.NewFromInt(49000), snapshot.Bids[0].Price)
	assert.Equal(t, decimal.NewFromInt(1), snapshot.Bids[0].Quantity)
}

func TestCancelOrder_ReturnsNotFoundForMissingOrder(t *testing.T) {
	me := NewMatchingEngine()
	me.Start()

	result := me.CancelOrder(CancelOrderCommand{
		CoinSymbol: "BTC",
		OrderID:    999,
		Side:       model.OrderSideBuy,
		Price:      decimal.NewFromInt(50000),
	})

	require.Error(t, result.Err)
	assert.True(t, errors.Is(result.Err, ErrCancelOrderNotFound))
	assert.False(t, result.Removed)
}

func TestCancelOrder_DoesNotRemoveDifferentSymbolOrder(t *testing.T) {
	me := NewMatchingEngine()
	me.Start()

	order := testOrder(13, "BTC", model.OrderSideSell, 50000, 2)
	submitAndWaitSnapshot(t, me, order)

	result := me.CancelOrder(CancelOrderCommand{
		CoinSymbol: "ETH",
		OrderID:    order.ID,
		Side:       order.Side,
		Price:      order.Price,
	})

	require.Error(t, result.Err)
	assert.True(t, errors.Is(result.Err, ErrCancelOrderNotFound))
	assert.Equal(t, 1, me.GetOrderBook("BTC").SellOrders.Len())
}
