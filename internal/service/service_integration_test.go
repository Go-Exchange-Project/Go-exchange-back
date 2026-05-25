package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/testdb"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func openServiceIntegrationDB(t *testing.T) *gorm.DB {
	t.Helper()

	return testdb.OpenIntegrationDB(t)
}

func serviceTestUserID(offset uint) uint {
	return uint(time.Now().UnixNano()%1_000_000_000) + 200_000 + offset
}

func cleanupServiceUsers(t *testing.T, db *gorm.DB, userIDs ...uint) {
	t.Helper()

	if len(userIDs) == 0 {
		return
	}

	var orders []model.Order
	require.NoError(t, db.Where("user_id IN ?", userIDs).Find(&orders).Error)

	orderIDs := make([]uint, 0, len(orders))
	for _, order := range orders {
		orderIDs = append(orderIDs, order.ID)
	}
	if len(orderIDs) > 0 {
		require.NoError(t, db.Where("buy_order_id IN ? OR sell_order_id IN ?", orderIDs, orderIDs).Delete(&model.FailedSettlement{}).Error)
		require.NoError(t, db.Where("buy_order_id IN ? OR sell_order_id IN ?", orderIDs, orderIDs).Delete(&model.Trade{}).Error)
	}

	require.NoError(t, db.Where("user_id IN ?", userIDs).Delete(&model.LedgerEntry{}).Error)
	require.NoError(t, db.Where("user_id IN ?", userIDs).Delete(&model.Order{}).Error)
	require.NoError(t, db.Where("user_id IN ?", userIDs).Delete(&model.Wallet{}).Error)
	require.NoError(t, db.Where("id IN ?", userIDs).Delete(&model.User{}).Error)
}

func newIntegrationOrderService(db *gorm.DB, me *matching.MatchingEngine) *OrderService {
	orderRepo := repository.NewOrderRepository(db)
	walletRepo := repository.NewWalletRepository(db)
	return NewOrderService(orderRepo, walletRepo, me)
}

func TestIntegrationCreateBuyOrderHoldsKRWAndSubmitsToEngine(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(1)
	defer cleanupServiceUsers(t, db, userID)

	require.NoError(t, db.Create(&model.Wallet{
		UserID:           userID,
		CoinSymbol:       model.KRWAssetSymbol,
		KRW:              decimal.NewFromInt(1000),
		AvailableBalance: decimal.NewFromInt(1000),
		LockedBalance:    decimal.Zero,
	}).Error)

	me := matching.NewMatchingEngine()
	orderService := newIntegrationOrderService(db, me)

	order, err := orderService.CreateOrder(CreateOrderInput{
		UserID:     userID,
		CoinSymbol: "BTC",
		Side:       "BUY",
		Price:      "100",
		Amount:     "2",
	})

	require.NoError(t, err)
	require.NotZero(t, order.ID)

	var orderCount int64
	require.NoError(t, db.Model(&model.Order{}).Where("id = ? AND user_id = ?", order.ID, userID).Count(&orderCount).Error)
	assert.Equal(t, int64(1), orderCount)

	walletRepo := repository.NewWalletRepository(db)
	krwWallet, err := walletRepo.FindKRWWalletByUserID(userID)
	require.NoError(t, err)
	assert.True(t, krwWallet.AvailableBalance.Equal(decimal.NewFromInt(800)))
	assert.True(t, krwWallet.LockedBalance.Equal(decimal.NewFromInt(200)))
	assert.True(t, krwWallet.KRW.Equal(decimal.NewFromInt(1000)))
	entries := requireLedgerEntries(t, db, userID, model.LedgerEntryTypeOrderHold, model.LedgerReferenceTypeOrder, order.ID)
	require.Len(t, entries, 1)
	assertLedgerDelta(t, entries[0], model.KRWAssetSymbol, "-200", "200", "800", "200")

	select {
	case engineOrder := <-me.OrderCh:
		assert.Equal(t, order.ID, engineOrder.ID)
		assert.Equal(t, model.OrderSideBuy, engineOrder.Side)
	case <-time.After(time.Second):
		t.Fatal("expected order to be submitted to matching engine")
	}
}

func TestIntegrationCreateBuyOrderHoldFailureRollsBackAndDoesNotSubmit(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(2)
	defer cleanupServiceUsers(t, db, userID)

	require.NoError(t, db.Create(&model.Wallet{
		UserID:           userID,
		CoinSymbol:       model.KRWAssetSymbol,
		KRW:              decimal.NewFromInt(50),
		AvailableBalance: decimal.NewFromInt(50),
		LockedBalance:    decimal.Zero,
	}).Error)

	me := matching.NewMatchingEngine()
	orderService := newIntegrationOrderService(db, me)

	order, err := orderService.CreateOrder(CreateOrderInput{
		UserID:     userID,
		CoinSymbol: "BTC",
		Side:       "BUY",
		Price:      "100",
		Amount:     "1",
	})

	require.Error(t, err)
	assert.Nil(t, order)

	var orderCount int64
	require.NoError(t, db.Model(&model.Order{}).Where("user_id = ?", userID).Count(&orderCount).Error)
	assert.Equal(t, int64(0), orderCount)

	walletRepo := repository.NewWalletRepository(db)
	krwWallet, err := walletRepo.FindKRWWalletByUserID(userID)
	require.NoError(t, err)
	assert.True(t, krwWallet.AvailableBalance.Equal(decimal.NewFromInt(50)))
	assert.True(t, krwWallet.LockedBalance.Equal(decimal.Zero))
	assert.True(t, krwWallet.KRW.Equal(decimal.NewFromInt(50)))
	assertLedgerCount(t, db, userID, 0)

	select {
	case engineOrder := <-me.OrderCh:
		t.Fatalf("unexpected matching engine order: %+v", engineOrder)
	default:
	}
}

func TestIntegrationCreateSellOrderHoldsCoin(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(3)
	defer cleanupServiceUsers(t, db, userID)

	require.NoError(t, db.Create(&model.Wallet{
		UserID:           userID,
		CoinSymbol:       "BTC",
		Quantity:         decimal.NewFromInt(5),
		AvailableBalance: decimal.NewFromInt(5),
		LockedBalance:    decimal.Zero,
	}).Error)

	me := matching.NewMatchingEngine()
	orderService := newIntegrationOrderService(db, me)

	order, err := orderService.CreateOrder(CreateOrderInput{
		UserID:     userID,
		CoinSymbol: "BTC",
		Side:       "SELL",
		Price:      "100",
		Amount:     "2",
	})

	require.NoError(t, err)
	require.NotZero(t, order.ID)

	walletRepo := repository.NewWalletRepository(db)
	btcWallet, err := walletRepo.FindByUserIDAndCoinSymbol(userID, "BTC")
	require.NoError(t, err)
	assert.True(t, btcWallet.AvailableBalance.Equal(decimal.NewFromInt(3)))
	assert.True(t, btcWallet.LockedBalance.Equal(decimal.NewFromInt(2)))
	assert.True(t, btcWallet.Quantity.Equal(decimal.NewFromInt(5)))
	entries := requireLedgerEntries(t, db, userID, model.LedgerEntryTypeOrderHold, model.LedgerReferenceTypeOrder, order.ID)
	require.Len(t, entries, 1)
	assertLedgerDelta(t, entries[0], "BTC", "-2", "2", "3", "2")
}

func TestIntegrationCreateOrderAllowsOwnCrossingOrderAndSubmitsToEngine(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(4)
	defer cleanupServiceUsers(t, db, userID)

	require.NoError(t, db.Create(&model.Wallet{
		UserID:           userID,
		CoinSymbol:       model.KRWAssetSymbol,
		KRW:              decimal.NewFromInt(1000),
		AvailableBalance: decimal.NewFromInt(1000),
		LockedBalance:    decimal.Zero,
	}).Error)
	require.NoError(t, db.Create(&model.Order{
		UserID:       userID,
		CoinSymbol:   "BTC",
		Side:         model.OrderSideSell,
		OrderType:    model.OrderTypeLimit,
		Status:       model.OrderStatusPending,
		Price:        decimal.NewFromInt(100),
		Amount:       decimal.NewFromInt(2),
		FilledAmount: decimal.Zero,
	}).Error)

	me := matching.NewMatchingEngine()
	orderService := newIntegrationOrderService(db, me)

	order, err := orderService.CreateOrder(CreateOrderInput{
		UserID:     userID,
		CoinSymbol: "BTC",
		Side:       "BUY",
		Price:      "100",
		Amount:     "1",
	})

	require.NoError(t, err)
	require.NotNil(t, order)

	wallet, err := repository.NewWalletRepository(db).FindKRWWalletByUserID(userID)
	require.NoError(t, err)
	assert.True(t, wallet.AvailableBalance.Equal(decimal.NewFromInt(900)))
	assert.True(t, wallet.LockedBalance.Equal(decimal.NewFromInt(100)))

	var buyOrderCount int64
	require.NoError(t, db.Model(&model.Order{}).
		Where("user_id = ? AND side = ?", userID, model.OrderSideBuy).
		Count(&buyOrderCount).Error)
	assert.Equal(t, int64(1), buyOrderCount)

	select {
	case engineOrder := <-me.OrderCh:
		assert.Equal(t, order.ID, engineOrder.ID)
		assert.Equal(t, userID, engineOrder.UserID)
		assert.Equal(t, model.OrderSideBuy, engineOrder.Side)
	case <-time.After(time.Second):
		t.Fatal("expected order to be submitted to matching engine")
	}
}

func TestIntegrationBootstrapOpenOrdersRestoresOrderBook(t *testing.T) {
	db := openServiceIntegrationDB(t)
	buyerID := serviceTestUserID(28)
	sellerID := serviceTestUserID(29)
	defer cleanupServiceUsers(t, db, buyerID, sellerID)

	coinSymbol := fmt.Sprintf("BOOT-%d", time.Now().UnixNano())
	createdAt := time.Now().UTC().Add(-time.Hour)
	buyOrder := model.Order{
		UserID:       buyerID,
		CoinSymbol:   coinSymbol,
		Side:         model.OrderSideBuy,
		OrderType:    model.OrderTypeLimit,
		Price:        decimal.NewFromInt(90),
		Amount:       decimal.NewFromInt(10),
		Status:       model.OrderStatusPending,
		FilledAmount: decimal.Zero,
		CreatedAt:    createdAt,
	}
	sellOrder := model.Order{
		UserID:       sellerID,
		CoinSymbol:   coinSymbol,
		Side:         model.OrderSideSell,
		OrderType:    model.OrderTypeLimit,
		Price:        decimal.NewFromInt(120),
		Amount:       decimal.NewFromInt(10),
		Status:       model.OrderStatusPartial,
		FilledAmount: decimal.NewFromInt(4),
		CreatedAt:    createdAt.Add(time.Minute),
	}
	require.NoError(t, db.Create(&buyOrder).Error)
	require.NoError(t, db.Create(&sellOrder).Error)

	me := matching.NewMatchingEngine()
	me.Start()
	snapshots := drainIntegrationSnapshots(me)
	bootstrapService := NewMatchingBootstrapService(repository.NewOrderRepository(db), me)

	result, err := bootstrapService.BootstrapOpenOrders(context.Background())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, result.Submitted, 2)
	requireCoinSnapshots(t, snapshots, coinSymbol, 2)

	book := me.GetOrderBook(coinSymbol)
	buyLevel, ok := book.BuyOrders.Max()
	require.True(t, ok)
	assert.True(t, buyLevel.Price.Equal(decimal.NewFromInt(90)))
	require.Equal(t, 1, buyLevel.Orders.Len())
	assert.Equal(t, buyOrder.ID, buyLevel.Orders.Front().ID)
	assert.True(t, buyLevel.Orders.Front().Amount.Equal(decimal.NewFromInt(10)))

	sellLevel, ok := book.SellOrders.Min()
	require.True(t, ok)
	assert.True(t, sellLevel.Price.Equal(decimal.NewFromInt(120)))
	require.Equal(t, 1, sellLevel.Orders.Len())
	assert.Equal(t, sellOrder.ID, sellLevel.Orders.Front().ID)
	assert.True(t, sellLevel.Orders.Front().Amount.Equal(decimal.NewFromInt(6)))
}

func TestIntegrationSettleTradeUpdatesTradeOrdersAndWallets(t *testing.T) {
	db := openServiceIntegrationDB(t)
	buyerID := serviceTestUserID(4)
	sellerID := serviceTestUserID(5)
	defer cleanupServiceUsers(t, db, buyerID, sellerID)

	buyOrder, sellOrder := seedSettlementRows(t, db, buyerID, sellerID, decimal.NewFromInt(500), decimal.NewFromInt(5))
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db), repository.NewWalletRepository(db))

	trade := &model.Trade{
		EngineSequence: 12,
		EngineEventID:  fmt.Sprintf("integration-engine-event-%d", time.Now().UnixNano()),
		CoinSymbol:     "BTC",
		Price:          decimal.NewFromInt(90),
		Quantity:       decimal.NewFromInt(5),
		TradedAt:       time.Now(),
		BuyOrderID:     buyOrder.ID,
		SellOrderID:    sellOrder.ID,
	}

	result, err := settlementService.SettleTrade(trade)
	require.NoError(t, err)
	assert.True(t, result.Applied)
	assert.False(t, result.Duplicate)
	assert.NotEmpty(t, trade.IdempotencyKey)
	assert.Equal(t, "engine:"+trade.EngineEventID, trade.IdempotencyKey)

	var tradeCount int64
	require.NoError(t, db.Model(&model.Trade{}).Where("buy_order_id = ? AND sell_order_id = ?", buyOrder.ID, sellOrder.ID).Count(&tradeCount).Error)
	assert.Equal(t, int64(1), tradeCount)

	var persistedTrade model.Trade
	require.NoError(t, db.Where("idempotency_key = ?", trade.IdempotencyKey).First(&persistedTrade).Error)
	assert.Equal(t, trade.EngineSequence, persistedTrade.EngineSequence)
	assert.Equal(t, trade.EngineEventID, persistedTrade.EngineEventID)

	var persistedBuy model.Order
	var persistedSell model.Order
	require.NoError(t, db.First(&persistedBuy, buyOrder.ID).Error)
	require.NoError(t, db.First(&persistedSell, sellOrder.ID).Error)
	assert.Equal(t, model.OrderStatusFilled, persistedBuy.Status)
	assert.Equal(t, model.OrderStatusFilled, persistedSell.Status)
	assert.True(t, persistedBuy.FilledAmount.Equal(decimal.NewFromInt(5)))
	assert.True(t, persistedSell.FilledAmount.Equal(decimal.NewFromInt(5)))

	walletRepo := repository.NewWalletRepository(db)
	buyerKRW, err := walletRepo.FindKRWWalletByUserID(buyerID)
	require.NoError(t, err)
	buyerBTC, err := walletRepo.FindByUserIDAndCoinSymbol(buyerID, "BTC")
	require.NoError(t, err)
	sellerBTC, err := walletRepo.FindByUserIDAndCoinSymbol(sellerID, "BTC")
	require.NoError(t, err)
	sellerKRW, err := walletRepo.FindKRWWalletByUserID(sellerID)
	require.NoError(t, err)

	assert.True(t, buyerKRW.AvailableBalance.Equal(decimal.NewFromInt(50)))
	assert.True(t, buyerKRW.LockedBalance.Equal(decimal.Zero))
	assert.True(t, buyerBTC.AvailableBalance.Equal(decimal.NewFromInt(5)))
	assert.True(t, sellerBTC.LockedBalance.Equal(decimal.Zero))
	assert.True(t, sellerKRW.AvailableBalance.Equal(decimal.NewFromInt(450)))
	assertSettlementLedgerEntries(t, db, result.TradeID, trade.IdempotencyKey, buyerID, sellerID)
}

func TestIntegrationSettleTradeCreatesMissingDestinationWallets(t *testing.T) {
	db := openServiceIntegrationDB(t)
	buyerID := serviceTestUserID(61)
	sellerID := serviceTestUserID(62)
	defer cleanupServiceUsers(t, db, buyerID, sellerID)

	buyerKRW := model.Wallet{
		UserID:           buyerID,
		CoinSymbol:       model.KRWAssetSymbol,
		KRW:              decimal.NewFromInt(100),
		AvailableBalance: decimal.Zero,
		LockedBalance:    decimal.NewFromInt(100),
	}
	sellerBTC := model.Wallet{
		UserID:           sellerID,
		CoinSymbol:       "BTC",
		Quantity:         decimal.NewFromInt(1),
		AvailableBalance: decimal.Zero,
		LockedBalance:    decimal.NewFromInt(1),
	}
	require.NoError(t, db.Create(&[]model.Wallet{buyerKRW, sellerBTC}).Error)

	buyOrder := model.Order{
		UserID:       buyerID,
		CoinSymbol:   "BTC",
		Side:         model.OrderSideBuy,
		OrderType:    model.OrderTypeLimit,
		Price:        decimal.NewFromInt(100),
		Amount:       decimal.NewFromInt(1),
		Status:       model.OrderStatusPending,
		FilledAmount: decimal.Zero,
	}
	sellOrder := model.Order{
		UserID:       sellerID,
		CoinSymbol:   "BTC",
		Side:         model.OrderSideSell,
		OrderType:    model.OrderTypeLimit,
		Price:        decimal.NewFromInt(100),
		Amount:       decimal.NewFromInt(1),
		Status:       model.OrderStatusPending,
		FilledAmount: decimal.Zero,
	}
	require.NoError(t, db.Create(&buyOrder).Error)
	require.NoError(t, db.Create(&sellOrder).Error)

	settlementService := NewSettlementService(db, repository.NewOrderRepository(db), repository.NewWalletRepository(db))

	_, err := settlementService.SettleTrade(&model.Trade{
		CoinSymbol:  "BTC",
		Price:       decimal.NewFromInt(100),
		Quantity:    decimal.NewFromInt(1),
		TradedAt:    time.Now(),
		BuyOrderID:  buyOrder.ID,
		SellOrderID: sellOrder.ID,
	})

	require.NoError(t, err)

	walletRepo := repository.NewWalletRepository(db)
	persistedBuyerBTC, err := walletRepo.FindByUserIDAndCoinSymbol(buyerID, "BTC")
	require.NoError(t, err)
	persistedSellerKRW, err := walletRepo.FindKRWWalletByUserID(sellerID)
	require.NoError(t, err)
	assert.True(t, persistedBuyerBTC.AvailableBalance.Equal(decimal.NewFromInt(1)))
	assert.True(t, persistedBuyerBTC.LockedBalance.Equal(decimal.Zero))
	assert.True(t, persistedSellerKRW.AvailableBalance.Equal(decimal.NewFromInt(100)))
	assert.True(t, persistedSellerKRW.LockedBalance.Equal(decimal.Zero))
}

func TestIntegrationSettleTradeFailureRollsBackAllWrites(t *testing.T) {
	db := openServiceIntegrationDB(t)
	buyerID := serviceTestUserID(6)
	sellerID := serviceTestUserID(7)
	defer cleanupServiceUsers(t, db, buyerID, sellerID)

	buyOrder, sellOrder := seedSettlementRows(t, db, buyerID, sellerID, decimal.NewFromInt(500), decimal.NewFromInt(1))
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db), repository.NewWalletRepository(db))

	trade := &model.Trade{
		EngineSequence: 20,
		EngineEventID:  fmt.Sprintf("integration-failing-engine-event-%d", time.Now().UnixNano()),
		CoinSymbol:     "BTC",
		Price:          decimal.NewFromInt(90),
		Quantity:       decimal.NewFromInt(5),
		TradedAt:       time.Now(),
		BuyOrderID:     buyOrder.ID,
		SellOrderID:    sellOrder.ID,
	}

	_, err := settlementService.SettleTrade(trade)
	require.Error(t, err)

	var tradeCount int64
	require.NoError(t, db.Model(&model.Trade{}).Where("buy_order_id = ? AND sell_order_id = ?", buyOrder.ID, sellOrder.ID).Count(&tradeCount).Error)
	assert.Equal(t, int64(0), tradeCount)

	var persistedBuy model.Order
	var persistedSell model.Order
	require.NoError(t, db.First(&persistedBuy, buyOrder.ID).Error)
	require.NoError(t, db.First(&persistedSell, sellOrder.ID).Error)
	assert.Equal(t, model.OrderStatusPending, persistedBuy.Status)
	assert.Equal(t, model.OrderStatusPending, persistedSell.Status)
	assert.True(t, persistedBuy.FilledAmount.Equal(decimal.Zero))
	assert.True(t, persistedSell.FilledAmount.Equal(decimal.Zero))

	walletRepo := repository.NewWalletRepository(db)
	buyerKRW, err := walletRepo.FindKRWWalletByUserID(buyerID)
	require.NoError(t, err)
	sellerBTC, err := walletRepo.FindByUserIDAndCoinSymbol(sellerID, "BTC")
	require.NoError(t, err)
	assert.True(t, buyerKRW.LockedBalance.Equal(decimal.NewFromInt(500)))
	assert.True(t, sellerBTC.LockedBalance.Equal(decimal.NewFromInt(1)))
	assertLedgerCount(t, db, buyerID, 0)
	assertLedgerCount(t, db, sellerID, 0)
}

func TestIntegrationSettleTradeDuplicateIsIdempotent(t *testing.T) {
	db := openServiceIntegrationDB(t)
	buyerID := serviceTestUserID(8)
	sellerID := serviceTestUserID(9)
	defer cleanupServiceUsers(t, db, buyerID, sellerID)

	buyOrder, sellOrder := seedSettlementRowsWithOrderAmount(t, db, buyerID, sellerID, decimal.NewFromInt(1000), decimal.NewFromInt(10), decimal.NewFromInt(10))
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db), repository.NewWalletRepository(db))

	trade := &model.Trade{
		EngineSequence: 30,
		EngineEventID:  fmt.Sprintf("integration-duplicate-engine-event-%d", time.Now().UnixNano()),
		CoinSymbol:     "BTC",
		Price:          decimal.NewFromInt(90),
		Quantity:       decimal.NewFromInt(5),
		TradedAt:       time.Now(),
		BuyOrderID:     buyOrder.ID,
		SellOrderID:    sellOrder.ID,
	}

	firstResult, err := settlementService.SettleTrade(trade)
	require.NoError(t, err)
	assert.True(t, firstResult.Applied)
	assert.False(t, firstResult.Duplicate)
	require.NotEmpty(t, trade.IdempotencyKey)
	assert.Equal(t, "engine:"+trade.EngineEventID, trade.IdempotencyKey)

	duplicate := *trade
	duplicate.ID = 0
	duplicate.IdempotencyKey = ""
	duplicate.TradedAt = trade.TradedAt.Add(time.Second)

	secondResult, err := settlementService.SettleTrade(&duplicate)
	require.NoError(t, err)
	assert.False(t, secondResult.Applied)
	assert.True(t, secondResult.Duplicate)
	assert.Equal(t, firstResult.TradeID, secondResult.TradeID)

	var tradeCount int64
	require.NoError(t, db.Model(&model.Trade{}).Where("idempotency_key = ?", trade.IdempotencyKey).Count(&tradeCount).Error)
	assert.Equal(t, int64(1), tradeCount)

	var persistedBuy model.Order
	var persistedSell model.Order
	require.NoError(t, db.First(&persistedBuy, buyOrder.ID).Error)
	require.NoError(t, db.First(&persistedSell, sellOrder.ID).Error)
	assert.Equal(t, model.OrderStatusPartial, persistedBuy.Status)
	assert.Equal(t, model.OrderStatusPartial, persistedSell.Status)
	assert.True(t, persistedBuy.FilledAmount.Equal(decimal.NewFromInt(5)))
	assert.True(t, persistedSell.FilledAmount.Equal(decimal.NewFromInt(5)))

	walletRepo := repository.NewWalletRepository(db)
	buyerKRW, err := walletRepo.FindKRWWalletByUserID(buyerID)
	require.NoError(t, err)
	buyerBTC, err := walletRepo.FindByUserIDAndCoinSymbol(buyerID, "BTC")
	require.NoError(t, err)
	sellerBTC, err := walletRepo.FindByUserIDAndCoinSymbol(sellerID, "BTC")
	require.NoError(t, err)
	sellerKRW, err := walletRepo.FindKRWWalletByUserID(sellerID)
	require.NoError(t, err)

	assert.True(t, buyerKRW.AvailableBalance.Equal(decimal.NewFromInt(50)))
	assert.True(t, buyerKRW.LockedBalance.Equal(decimal.NewFromInt(500)))
	assert.True(t, buyerBTC.AvailableBalance.Equal(decimal.NewFromInt(5)))
	assert.True(t, sellerBTC.LockedBalance.Equal(decimal.NewFromInt(5)))
	assert.True(t, sellerKRW.AvailableBalance.Equal(decimal.NewFromInt(450)))
	assertSettlementLedgerEntries(t, db, firstResult.TradeID, trade.IdempotencyKey, buyerID, sellerID)
}

func TestIntegrationSettleTradeSameIdempotencyKeyDifferentPayloadReturnsConflict(t *testing.T) {
	db := openServiceIntegrationDB(t)
	buyerID := serviceTestUserID(10)
	sellerID := serviceTestUserID(11)
	defer cleanupServiceUsers(t, db, buyerID, sellerID)

	buyOrder, sellOrder := seedSettlementRowsWithOrderAmount(t, db, buyerID, sellerID, decimal.NewFromInt(1000), decimal.NewFromInt(10), decimal.NewFromInt(10))
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db), repository.NewWalletRepository(db))

	idempotencyKey := fmt.Sprintf("service-conflict-key-%d", time.Now().UnixNano())
	trade := &model.Trade{
		IdempotencyKey: idempotencyKey,
		CoinSymbol:     "BTC",
		Price:          decimal.NewFromInt(90),
		Quantity:       decimal.NewFromInt(5),
		TradedAt:       time.Now(),
		BuyOrderID:     buyOrder.ID,
		SellOrderID:    sellOrder.ID,
	}

	firstResult, err := settlementService.SettleTrade(trade)
	require.NoError(t, err)
	assert.True(t, firstResult.Applied)

	conflictingTrade := &model.Trade{
		IdempotencyKey: idempotencyKey,
		CoinSymbol:     "BTC",
		Price:          decimal.NewFromInt(91),
		Quantity:       decimal.NewFromInt(5),
		TradedAt:       time.Now().Add(time.Second),
		BuyOrderID:     buyOrder.ID,
		SellOrderID:    sellOrder.ID,
	}

	conflictResult, err := settlementService.SettleTrade(conflictingTrade)
	require.Error(t, err)
	assert.False(t, conflictResult.Applied)
	assert.Contains(t, err.Error(), "idempotency key conflict")

	var tradeCount int64
	require.NoError(t, db.Model(&model.Trade{}).Where("idempotency_key = ?", idempotencyKey).Count(&tradeCount).Error)
	assert.Equal(t, int64(1), tradeCount)

	var persistedBuy model.Order
	require.NoError(t, db.First(&persistedBuy, buyOrder.ID).Error)
	assert.True(t, persistedBuy.FilledAmount.Equal(decimal.NewFromInt(5)))
}

func TestIntegrationSettleTradeRejectsCancelledBuyOrder(t *testing.T) {
	db := openServiceIntegrationDB(t)
	buyerID := serviceTestUserID(20)
	sellerID := serviceTestUserID(21)
	defer cleanupServiceUsers(t, db, buyerID, sellerID)

	buyOrder, sellOrder := seedSettlementRowsWithStatuses(t, db, buyerID, sellerID, decimal.Zero, decimal.NewFromInt(5), decimal.NewFromInt(5), model.OrderStatusCancelled, model.OrderStatusPending)
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db), repository.NewWalletRepository(db))

	trade := &model.Trade{
		CoinSymbol:  "BTC",
		Price:       decimal.NewFromInt(90),
		Quantity:    decimal.NewFromInt(5),
		TradedAt:    time.Now(),
		BuyOrderID:  buyOrder.ID,
		SellOrderID: sellOrder.ID,
	}

	result, err := settlementService.SettleTrade(trade)

	require.Error(t, err)
	assert.False(t, result.Applied)
	assert.Contains(t, err.Error(), "buy order")
	assert.Contains(t, err.Error(), "CANCELLED")
	assertNoTradePersistedForOrders(t, db, buyOrder.ID, sellOrder.ID)
}

func TestIntegrationFailedSettlementRecordedForCancelledOrderTrade(t *testing.T) {
	db := openServiceIntegrationDB(t)
	buyerID := serviceTestUserID(26)
	sellerID := serviceTestUserID(27)
	defer cleanupServiceUsers(t, db, buyerID, sellerID)

	buyOrder, sellOrder := seedSettlementRowsWithStatuses(t, db, buyerID, sellerID, decimal.Zero, decimal.NewFromInt(5), decimal.NewFromInt(5), model.OrderStatusCancelled, model.OrderStatusPending)
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db), repository.NewWalletRepository(db))
	failedSettlementService := NewFailedSettlementService(repository.NewFailedSettlementRepository(db))

	trade := &model.Trade{
		CoinSymbol:  "BTC",
		Price:       decimal.NewFromInt(90),
		Quantity:    decimal.NewFromInt(5),
		TradedAt:    time.Now(),
		BuyOrderID:  buyOrder.ID,
		SellOrderID: sellOrder.ID,
	}

	result, err := settlementService.SettleTrade(trade)
	require.Error(t, err)
	assert.False(t, result.Applied)

	failure, recordErr := failedSettlementService.RecordFailure(trade, err)
	require.NoError(t, recordErr)
	assert.NotZero(t, failure.ID)
	assert.Equal(t, trade.IdempotencyKey, failure.TradeIdempotencyKey)
	assert.Equal(t, model.FailedSettlementStatusOpen, failure.Status)
	assert.Equal(t, uint(1), failure.RetryCount)
	assert.Contains(t, failure.ErrorMessage, "CANCELLED")
	assertNoTradePersistedForOrders(t, db, buyOrder.ID, sellOrder.ID)

	var count int64
	require.NoError(t, db.Model(&model.FailedSettlement{}).Where("trade_idempotency_key = ?", trade.IdempotencyKey).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestIntegrationSettleTradeRejectsCancelledSellOrder(t *testing.T) {
	db := openServiceIntegrationDB(t)
	buyerID := serviceTestUserID(22)
	sellerID := serviceTestUserID(23)
	defer cleanupServiceUsers(t, db, buyerID, sellerID)

	buyOrder, sellOrder := seedSettlementRowsWithStatuses(t, db, buyerID, sellerID, decimal.NewFromInt(500), decimal.Zero, decimal.NewFromInt(5), model.OrderStatusPending, model.OrderStatusCancelled)
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db), repository.NewWalletRepository(db))

	trade := &model.Trade{
		CoinSymbol:  "BTC",
		Price:       decimal.NewFromInt(90),
		Quantity:    decimal.NewFromInt(5),
		TradedAt:    time.Now(),
		BuyOrderID:  buyOrder.ID,
		SellOrderID: sellOrder.ID,
	}

	result, err := settlementService.SettleTrade(trade)

	require.Error(t, err)
	assert.False(t, result.Applied)
	assert.Contains(t, err.Error(), "sell order")
	assert.Contains(t, err.Error(), "CANCELLED")
	assertNoTradePersistedForOrders(t, db, buyOrder.ID, sellOrder.ID)
}

func TestIntegrationCancelEngineMissThenLateTradeIsRejected(t *testing.T) {
	db := openServiceIntegrationDB(t)
	buyerID := serviceTestUserID(24)
	sellerID := serviceTestUserID(25)
	defer cleanupServiceUsers(t, db, buyerID, sellerID)

	buyOrder := seedCancelOrderRows(t, db, cancelOrderSeed{
		UserID:        buyerID,
		CoinSymbol:    "BTC",
		Side:          model.OrderSideBuy,
		Status:        model.OrderStatusPending,
		Price:         decimal.NewFromInt(100),
		Amount:        decimal.NewFromInt(5),
		FilledAmount:  decimal.Zero,
		LockedBalance: decimal.NewFromInt(500),
	})
	sellOrder := seedCancelOrderRows(t, db, cancelOrderSeed{
		UserID:        sellerID,
		CoinSymbol:    "BTC",
		Side:          model.OrderSideSell,
		Status:        model.OrderStatusPending,
		Price:         decimal.NewFromInt(90),
		Amount:        decimal.NewFromInt(5),
		FilledAmount:  decimal.Zero,
		LockedBalance: decimal.NewFromInt(5),
	})
	me := matching.NewMatchingEngine()
	me.Start()
	orderService := newIntegrationOrderService(db, me)

	cancelResult, err := orderService.CancelOrder(CancelOrderInput{UserID: buyerID, OrderID: buyOrder.ID})
	require.Error(t, err)
	require.NotNil(t, cancelResult)
	assert.Equal(t, model.OrderStatusCancelled, cancelResult.Status)
	assert.False(t, cancelResult.EngineRemoved)

	settlementService := NewSettlementService(db, repository.NewOrderRepository(db), repository.NewWalletRepository(db))
	lateTrade := &model.Trade{
		CoinSymbol:  "BTC",
		Price:       decimal.NewFromInt(90),
		Quantity:    decimal.NewFromInt(5),
		TradedAt:    time.Now(),
		BuyOrderID:  buyOrder.ID,
		SellOrderID: sellOrder.ID,
	}

	settleResult, err := settlementService.SettleTrade(lateTrade)

	require.Error(t, err)
	assert.False(t, settleResult.Applied)
	assert.Contains(t, err.Error(), "CANCELLED")
	assertNoTradePersistedForOrders(t, db, buyOrder.ID, sellOrder.ID)
	assertCancelledOrderAndWallet(t, db, buyOrder.ID, buyerID, model.KRWAssetSymbol, decimal.NewFromInt(500), decimal.Zero)

	var persistedSell model.Order
	require.NoError(t, db.First(&persistedSell, sellOrder.ID).Error)
	assert.Equal(t, model.OrderStatusPending, persistedSell.Status)
	sellerWallet, err := repository.NewWalletRepository(db).FindByUserIDAndCoinSymbol(sellerID, "BTC")
	require.NoError(t, err)
	assert.True(t, sellerWallet.AvailableBalance.Equal(decimal.Zero))
	assert.True(t, sellerWallet.LockedBalance.Equal(decimal.NewFromInt(5)))
}

func TestIntegrationCancelPendingBuyOrderReleasesKRWAndRemovesFromEngine(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(12)
	defer cleanupServiceUsers(t, db, userID)

	order := seedCancelOrderRows(t, db, cancelOrderSeed{
		UserID:        userID,
		CoinSymbol:    "BTC",
		Side:          model.OrderSideBuy,
		Status:        model.OrderStatusPending,
		Price:         decimal.NewFromInt(100),
		Amount:        decimal.NewFromInt(5),
		FilledAmount:  decimal.Zero,
		LockedBalance: decimal.NewFromInt(500),
	})
	me := matching.NewMatchingEngine()
	me.Start()
	submitIntegrationEngineOrder(t, me, order, decimal.NewFromInt(5))

	orderService := newIntegrationOrderService(db, me)
	result, err := orderService.CancelOrder(CancelOrderInput{UserID: userID, OrderID: order.ID})

	require.NoError(t, err)
	assert.Equal(t, model.OrderStatusCancelled, result.Status)
	assert.Equal(t, model.KRWAssetSymbol, result.ReleasedAsset)
	assert.True(t, result.ReleasedAmount.Equal(decimal.NewFromInt(500)))
	assert.True(t, result.EngineRemoved)
	requireIntegrationSnapshot(t, me)
	assert.Equal(t, 0, me.GetOrderBook("BTC").BuyOrders.Len())

	assertCancelledOrderAndWallet(t, db, order.ID, userID, model.KRWAssetSymbol, decimal.NewFromInt(500), decimal.Zero)
	entries := requireLedgerEntries(t, db, userID, model.LedgerEntryTypeOrderRelease, model.LedgerReferenceTypeOrder, order.ID)
	require.Len(t, entries, 1)
	assertLedgerDelta(t, entries[0], model.KRWAssetSymbol, "500", "-500", "500", "0")
}

func TestIntegrationCancelPartialBuyOrderReleasesRemainingKRW(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(13)
	defer cleanupServiceUsers(t, db, userID)

	order := seedCancelOrderRows(t, db, cancelOrderSeed{
		UserID:        userID,
		CoinSymbol:    "BTC",
		Side:          model.OrderSideBuy,
		Status:        model.OrderStatusPartial,
		Price:         decimal.NewFromInt(100),
		Amount:        decimal.NewFromInt(10),
		FilledAmount:  decimal.NewFromInt(4),
		LockedBalance: decimal.NewFromInt(600),
	})
	me := matching.NewMatchingEngine()
	me.Start()
	submitIntegrationEngineOrder(t, me, order, decimal.NewFromInt(6))

	orderService := newIntegrationOrderService(db, me)
	result, err := orderService.CancelOrder(CancelOrderInput{UserID: userID, OrderID: order.ID})

	require.NoError(t, err)
	assert.True(t, result.ReleasedAmount.Equal(decimal.NewFromInt(600)))
	assert.True(t, result.EngineRemoved)
	requireIntegrationSnapshot(t, me)
	assertCancelledOrderAndWallet(t, db, order.ID, userID, model.KRWAssetSymbol, decimal.NewFromInt(600), decimal.Zero)
}

func TestIntegrationCancelPendingSellOrderReleasesCoin(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(14)
	defer cleanupServiceUsers(t, db, userID)

	order := seedCancelOrderRows(t, db, cancelOrderSeed{
		UserID:        userID,
		CoinSymbol:    "BTC",
		Side:          model.OrderSideSell,
		Status:        model.OrderStatusPending,
		Price:         decimal.NewFromInt(100),
		Amount:        decimal.NewFromInt(5),
		FilledAmount:  decimal.Zero,
		LockedBalance: decimal.NewFromInt(5),
	})
	me := matching.NewMatchingEngine()
	me.Start()
	submitIntegrationEngineOrder(t, me, order, decimal.NewFromInt(5))

	orderService := newIntegrationOrderService(db, me)
	result, err := orderService.CancelOrder(CancelOrderInput{UserID: userID, OrderID: order.ID})

	require.NoError(t, err)
	assert.Equal(t, "BTC", result.ReleasedAsset)
	assert.True(t, result.ReleasedAmount.Equal(decimal.NewFromInt(5)))
	assert.True(t, result.EngineRemoved)
	requireIntegrationSnapshot(t, me)
	assert.Equal(t, 0, me.GetOrderBook("BTC").SellOrders.Len())
	assertCancelledOrderAndWallet(t, db, order.ID, userID, "BTC", decimal.NewFromInt(5), decimal.Zero)
	entries := requireLedgerEntries(t, db, userID, model.LedgerEntryTypeOrderRelease, model.LedgerReferenceTypeOrder, order.ID)
	require.Len(t, entries, 1)
	assertLedgerDelta(t, entries[0], "BTC", "5", "-5", "5", "0")
}

func TestIntegrationCancelFilledOrderIsRejected(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(15)
	defer cleanupServiceUsers(t, db, userID)

	order := seedCancelOrderRows(t, db, cancelOrderSeed{
		UserID:        userID,
		CoinSymbol:    "BTC",
		Side:          model.OrderSideBuy,
		Status:        model.OrderStatusFilled,
		Price:         decimal.NewFromInt(100),
		Amount:        decimal.NewFromInt(5),
		FilledAmount:  decimal.NewFromInt(5),
		LockedBalance: decimal.Zero,
	})
	orderService := newIntegrationOrderService(db, nil)

	result, err := orderService.CancelOrder(CancelOrderInput{UserID: userID, OrderID: order.ID})

	require.Error(t, err)
	assert.Nil(t, result)
	var persisted model.Order
	require.NoError(t, db.First(&persisted, order.ID).Error)
	assert.Equal(t, model.OrderStatusFilled, persisted.Status)
}

func TestIntegrationCancelOtherUserOrderIsRejected(t *testing.T) {
	db := openServiceIntegrationDB(t)
	ownerID := serviceTestUserID(16)
	requestUserID := serviceTestUserID(17)
	defer cleanupServiceUsers(t, db, ownerID, requestUserID)

	order := seedCancelOrderRows(t, db, cancelOrderSeed{
		UserID:        ownerID,
		CoinSymbol:    "BTC",
		Side:          model.OrderSideBuy,
		Status:        model.OrderStatusPending,
		Price:         decimal.NewFromInt(100),
		Amount:        decimal.NewFromInt(5),
		FilledAmount:  decimal.Zero,
		LockedBalance: decimal.NewFromInt(500),
	})
	orderService := newIntegrationOrderService(db, nil)

	result, err := orderService.CancelOrder(CancelOrderInput{UserID: requestUserID, OrderID: order.ID})

	require.Error(t, err)
	assert.Nil(t, result)
	var persisted model.Order
	require.NoError(t, db.First(&persisted, order.ID).Error)
	assert.Equal(t, model.OrderStatusPending, persisted.Status)
	walletRepo := repository.NewWalletRepository(db)
	wallet, err := walletRepo.FindKRWWalletByUserID(ownerID)
	require.NoError(t, err)
	assert.True(t, wallet.LockedBalance.Equal(decimal.NewFromInt(500)))
}

func TestIntegrationCancelRollbackWhenWalletLockedBalanceIsInsufficient(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(18)
	defer cleanupServiceUsers(t, db, userID)

	order := seedCancelOrderRows(t, db, cancelOrderSeed{
		UserID:        userID,
		CoinSymbol:    "BTC",
		Side:          model.OrderSideBuy,
		Status:        model.OrderStatusPending,
		Price:         decimal.NewFromInt(100),
		Amount:        decimal.NewFromInt(5),
		FilledAmount:  decimal.Zero,
		LockedBalance: decimal.NewFromInt(100),
	})
	orderService := newIntegrationOrderService(db, nil)

	result, err := orderService.CancelOrder(CancelOrderInput{UserID: userID, OrderID: order.ID})

	require.Error(t, err)
	assert.Nil(t, result)
	var persisted model.Order
	require.NoError(t, db.First(&persisted, order.ID).Error)
	assert.Equal(t, model.OrderStatusPending, persisted.Status)
	walletRepo := repository.NewWalletRepository(db)
	wallet, err := walletRepo.FindKRWWalletByUserID(userID)
	require.NoError(t, err)
	assert.True(t, wallet.AvailableBalance.Equal(decimal.Zero))
	assert.True(t, wallet.LockedBalance.Equal(decimal.NewFromInt(100)))
}

func TestIntegrationCancelReturnsEngineErrorAfterDBCommitWhenOrderMissingFromBook(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(19)
	defer cleanupServiceUsers(t, db, userID)

	order := seedCancelOrderRows(t, db, cancelOrderSeed{
		UserID:        userID,
		CoinSymbol:    "BTC",
		Side:          model.OrderSideBuy,
		Status:        model.OrderStatusPending,
		Price:         decimal.NewFromInt(100),
		Amount:        decimal.NewFromInt(5),
		FilledAmount:  decimal.Zero,
		LockedBalance: decimal.NewFromInt(500),
	})
	me := matching.NewMatchingEngine()
	me.Start()
	orderService := newIntegrationOrderService(db, me)

	result, err := orderService.CancelOrder(CancelOrderInput{UserID: userID, OrderID: order.ID})

	require.Error(t, err)
	require.NotNil(t, result)
	assert.Contains(t, err.Error(), "matching engine cancel failed")
	assert.Equal(t, model.OrderStatusCancelled, result.Status)
	assert.False(t, result.EngineRemoved)
	assertCancelledOrderAndWallet(t, db, order.ID, userID, model.KRWAssetSymbol, decimal.NewFromInt(500), decimal.Zero)
}

func seedSettlementRows(t *testing.T, db *gorm.DB, buyerID uint, sellerID uint, buyerLockedKRW decimal.Decimal, sellerLockedBTC decimal.Decimal) (model.Order, model.Order) {
	t.Helper()

	return seedSettlementRowsWithOrderAmount(t, db, buyerID, sellerID, buyerLockedKRW, sellerLockedBTC, decimal.NewFromInt(5))
}

func seedSettlementRowsWithOrderAmount(t *testing.T, db *gorm.DB, buyerID uint, sellerID uint, buyerLockedKRW decimal.Decimal, sellerLockedBTC decimal.Decimal, orderAmount decimal.Decimal) (model.Order, model.Order) {
	t.Helper()

	return seedSettlementRowsWithStatuses(t, db, buyerID, sellerID, buyerLockedKRW, sellerLockedBTC, orderAmount, model.OrderStatusPending, model.OrderStatusPending)
}

func seedSettlementRowsWithStatuses(t *testing.T, db *gorm.DB, buyerID uint, sellerID uint, buyerLockedKRW decimal.Decimal, sellerLockedBTC decimal.Decimal, orderAmount decimal.Decimal, buyStatus model.OrderStatus, sellStatus model.OrderStatus) (model.Order, model.Order) {
	t.Helper()

	wallets := []model.Wallet{
		{UserID: buyerID, CoinSymbol: model.KRWAssetSymbol, KRW: buyerLockedKRW, AvailableBalance: decimal.Zero, LockedBalance: buyerLockedKRW},
		{UserID: buyerID, CoinSymbol: "BTC", Quantity: decimal.Zero, AvailableBalance: decimal.Zero, LockedBalance: decimal.Zero},
		{UserID: sellerID, CoinSymbol: "BTC", Quantity: sellerLockedBTC, AvailableBalance: decimal.Zero, LockedBalance: sellerLockedBTC},
		{UserID: sellerID, CoinSymbol: model.KRWAssetSymbol, KRW: decimal.Zero, AvailableBalance: decimal.Zero, LockedBalance: decimal.Zero},
	}
	require.NoError(t, db.Create(&wallets).Error)

	buyOrder := model.Order{
		UserID:       buyerID,
		CoinSymbol:   "BTC",
		Side:         model.OrderSideBuy,
		OrderType:    model.OrderTypeLimit,
		Price:        decimal.NewFromInt(100),
		Amount:       orderAmount,
		Status:       buyStatus,
		FilledAmount: decimal.Zero,
	}
	sellOrder := model.Order{
		UserID:       sellerID,
		CoinSymbol:   "BTC",
		Side:         model.OrderSideSell,
		OrderType:    model.OrderTypeLimit,
		Price:        decimal.NewFromInt(90),
		Amount:       orderAmount,
		Status:       sellStatus,
		FilledAmount: decimal.Zero,
	}
	require.NoError(t, db.Create(&buyOrder).Error)
	require.NoError(t, db.Create(&sellOrder).Error)
	return buyOrder, sellOrder
}

func assertNoTradePersistedForOrders(t *testing.T, db *gorm.DB, buyOrderID uint, sellOrderID uint) {
	t.Helper()

	var tradeCount int64
	require.NoError(t, db.Model(&model.Trade{}).Where("buy_order_id = ? AND sell_order_id = ?", buyOrderID, sellOrderID).Count(&tradeCount).Error)
	assert.Equal(t, int64(0), tradeCount)
}

type cancelOrderSeed struct {
	UserID        uint
	CoinSymbol    string
	Side          model.OrderSide
	Status        model.OrderStatus
	Price         decimal.Decimal
	Amount        decimal.Decimal
	FilledAmount  decimal.Decimal
	LockedBalance decimal.Decimal
}

func seedCancelOrderRows(t *testing.T, db *gorm.DB, seed cancelOrderSeed) model.Order {
	t.Helper()

	wallet := model.Wallet{
		UserID:           seed.UserID,
		CoinSymbol:       model.KRWAssetSymbol,
		KRW:              seed.LockedBalance,
		AvailableBalance: decimal.Zero,
		LockedBalance:    seed.LockedBalance,
	}
	if seed.Side == model.OrderSideSell {
		wallet.CoinSymbol = seed.CoinSymbol
		wallet.KRW = decimal.Zero
		wallet.Quantity = seed.LockedBalance
	}
	require.NoError(t, db.Create(&wallet).Error)

	order := model.Order{
		UserID:       seed.UserID,
		CoinSymbol:   seed.CoinSymbol,
		Side:         seed.Side,
		OrderType:    model.OrderTypeLimit,
		Price:        seed.Price,
		Amount:       seed.Amount,
		Status:       seed.Status,
		FilledAmount: seed.FilledAmount,
	}
	require.NoError(t, db.Create(&order).Error)
	return order
}

func submitIntegrationEngineOrder(t *testing.T, me *matching.MatchingEngine, order model.Order, remaining decimal.Decimal) {
	t.Helper()

	me.OrderCh <- &matching.Order{
		ID:           order.ID,
		UserID:       order.UserID,
		CoinSymbol:   order.CoinSymbol,
		Side:         order.Side,
		Price:        order.Price,
		Amount:       remaining,
		CreatedAt:    order.CreatedAt,
		OrderType:    order.OrderType,
		FilledAmount: order.FilledAmount,
	}
	requireIntegrationSnapshot(t, me)
}

func requireIntegrationSnapshot(t *testing.T, me *matching.MatchingEngine) matching.OrderBookSnapshot {
	t.Helper()

	select {
	case snapshot := <-me.SnapshotCh:
		return snapshot
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for matching engine snapshot")
		return matching.OrderBookSnapshot{}
	}
}

func drainIntegrationSnapshots(me *matching.MatchingEngine) <-chan matching.OrderBookSnapshot {
	snapshots := make(chan matching.OrderBookSnapshot, 512)
	go func() {
		for snapshot := range me.SnapshotCh {
			snapshots <- snapshot
		}
	}()
	return snapshots
}

func requireCoinSnapshots(t *testing.T, snapshots <-chan matching.OrderBookSnapshot, coinSymbol string, count int) {
	t.Helper()

	deadline := time.After(time.Second)
	seen := 0
	for seen < count {
		select {
		case snapshot := <-snapshots:
			if snapshot.CoinSymbol == coinSymbol {
				seen++
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %d bootstrap snapshots for %s; saw %d", count, coinSymbol, seen)
		}
	}
}

func assertCancelledOrderAndWallet(t *testing.T, db *gorm.DB, orderID uint, userID uint, asset string, available decimal.Decimal, locked decimal.Decimal) {
	t.Helper()

	var persisted model.Order
	require.NoError(t, db.First(&persisted, orderID).Error)
	assert.Equal(t, model.OrderStatusCancelled, persisted.Status)

	walletRepo := repository.NewWalletRepository(db)
	var wallet *model.Wallet
	var err error
	if asset == model.KRWAssetSymbol {
		wallet, err = walletRepo.FindKRWWalletByUserID(userID)
	} else {
		wallet, err = walletRepo.FindByUserIDAndCoinSymbol(userID, asset)
	}
	require.NoError(t, err)
	assert.True(t, wallet.AvailableBalance.Equal(available))
	assert.True(t, wallet.LockedBalance.Equal(locked))
}

func requireLedgerEntries(t *testing.T, db *gorm.DB, userID uint, entryType model.LedgerEntryType, referenceType model.LedgerReferenceType, referenceID uint) []model.LedgerEntry {
	t.Helper()

	var entries []model.LedgerEntry
	require.NoError(t, db.
		Where("user_id = ? AND entry_type = ? AND reference_type = ? AND reference_id = ?", userID, entryType, referenceType, referenceID).
		Order("created_at DESC").
		Order("id DESC").
		Find(&entries).Error)
	return entries
}

func assertLedgerCount(t *testing.T, db *gorm.DB, userID uint, expected int64) {
	t.Helper()

	var count int64
	require.NoError(t, db.Model(&model.LedgerEntry{}).Where("user_id = ?", userID).Count(&count).Error)
	assert.Equal(t, expected, count)
}

func assertLedgerDelta(t *testing.T, entry model.LedgerEntry, asset string, availableDelta string, lockedDelta string, availableAfter string, lockedAfter string) {
	t.Helper()

	assert.Equal(t, asset, entry.CoinSymbol)
	assert.True(t, entry.AvailableDelta.Equal(decimal.RequireFromString(availableDelta)), "available_delta=%s", entry.AvailableDelta.String())
	assert.True(t, entry.LockedDelta.Equal(decimal.RequireFromString(lockedDelta)), "locked_delta=%s", entry.LockedDelta.String())
	assert.True(t, entry.AvailableBalanceAfter.Equal(decimal.RequireFromString(availableAfter)), "available_after=%s", entry.AvailableBalanceAfter.String())
	assert.True(t, entry.LockedBalanceAfter.Equal(decimal.RequireFromString(lockedAfter)), "locked_after=%s", entry.LockedBalanceAfter.String())
}

func assertSettlementLedgerEntries(t *testing.T, db *gorm.DB, tradeID uint, idempotencyKey string, buyerID uint, sellerID uint) {
	t.Helper()

	var entries []model.LedgerEntry
	require.NoError(t, db.
		Where("entry_type = ? AND reference_type = ? AND reference_id = ?", model.LedgerEntryTypeTradeSettlement, model.LedgerReferenceTypeTrade, tradeID).
		Order("user_id ASC").
		Order("coin_symbol ASC").
		Find(&entries).Error)
	require.Len(t, entries, 4)
	for _, entry := range entries {
		assert.Equal(t, idempotencyKey, entry.ReferenceKey)
	}

	byUserAsset := make(map[string]model.LedgerEntry, len(entries))
	for _, entry := range entries {
		byUserAsset[fmt.Sprintf("%d/%s", entry.UserID, entry.CoinSymbol)] = entry
	}
	assertLedgerDelta(t, byUserAsset[fmt.Sprintf("%d/%s", buyerID, model.KRWAssetSymbol)], model.KRWAssetSymbol, "50", "-500", "50", "0")
	assertLedgerDelta(t, byUserAsset[fmt.Sprintf("%d/%s", buyerID, "BTC")], "BTC", "5", "0", "5", "0")
	assertLedgerDelta(t, byUserAsset[fmt.Sprintf("%d/%s", sellerID, "BTC")], "BTC", "0", "-5", "0", "0")
	assertLedgerDelta(t, byUserAsset[fmt.Sprintf("%d/%s", sellerID, model.KRWAssetSymbol)], model.KRWAssetSymbol, "450", "0", "450", "0")
}
