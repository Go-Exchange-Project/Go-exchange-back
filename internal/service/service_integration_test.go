package service

import (
	"context"
	"fmt"
	"sync/atomic"
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

// testIdemKeySeq는 테스트마다 고유한 멱등성 키를 만든다. 같은 키를 재사용하면
// 서로 다른 주문이 재시도로 오인된다.
var testIdemKeySeq atomic.Uint64

// createTestOrder는 멱등성 키 계약 도입 전 테스트들이 쓰던 (*model.Order, error) 모양을
// 유지한다. 이 테스트들은 키 계약이 아니라 주문·홀드 동작을 보므로 키는 자동으로 채운다.
func createTestOrder(svc *OrderService, input CreateOrderInput) (*model.Order, error) {
	if input.IdempotencyKey == "" {
		// 공유 테스트 DB에는 이전 실행의 키가 남는다. 순번만 쓰면 실행마다 1부터 다시
		// 시작해 같은 키가 쌓이고, (user_id, key) UNIQUE 밖의 검사가 그 중복에 걸린다.
		input.IdempotencyKey = fmt.Sprintf("test-key-%d-%d", time.Now().UnixNano(), testIdemKeySeq.Add(1))
	}
	result, err := svc.CreateOrder(input)
	if err != nil {
		return nil, err
	}
	return result.Order, nil
}

func serviceTestUserID(offset uint) uint {
	return uint(time.Now().UnixNano()%1_000_000_000) + 200_000 + offset
}

func cleanupServiceUsers(t *testing.T, db *gorm.DB, userIDs ...uint) {
	t.Helper()

	if len(userIDs) == 0 {
		return
	}

	// 주문 생성이 멱등성 키를 남기므로 함께 지운다. 남겨 두면 공유 DB가 계속 커지고,
	// 스키마를 검사하는 테스트가 이전 실행의 행에 걸린다.
	require.NoError(t, db.Where("user_id IN ?", userIDs).
		Delete(&model.OrderIdempotencyKey{}).Error)

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

	require.NoError(t, db.Where("user_id IN ?", userIDs).Delete(&model.Order{}).Error)
	require.NoError(t, db.Where("id IN ?", userIDs).Delete(&model.User{}).Error)
}

func newIntegrationOrderService(db *gorm.DB, me *matching.MatchingEngine) *OrderService {
	orderRepo := repository.NewOrderRepository(db)
	return NewOrderService(orderRepo, me)
}

func TestIntegrationCreateBuyOrderHoldsKRWAndSubmitsToEngine(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(1)
	defer cleanupServiceUsers(t, db, userID)

	seedLedgerFunds(t, db, userID, model.KRWAssetSymbol, decimal.NewFromInt(10000))

	me := matching.NewMatchingEngine()
	orderService := newIntegrationOrderService(db, me)

	order, err := createTestOrder(orderService, CreateOrderInput{
		UserID:     userID,
		CoinSymbol: "BTC",
		Side:       "BUY",
		Price:      "5000",
		Amount:     "1",
	})

	require.NoError(t, err)
	require.NotZero(t, order.ID)

	var orderCount int64
	require.NoError(t, db.Model(&model.Order{}).Where("id = ? AND user_id = ?", order.ID, userID).Count(&orderCount).Error)
	assert.Equal(t, int64(1), orderCount)

	assertLedgerBalances(t, db, userID, model.KRWAssetSymbol, decimal.RequireFromString("4997.5"), decimal.RequireFromString("5002.5"))
	requireJournalByKey(t, db, orderHoldKey(order.ID))

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

	seedLedgerFunds(t, db, userID, model.KRWAssetSymbol, decimal.NewFromInt(50))

	me := matching.NewMatchingEngine()
	orderService := newIntegrationOrderService(db, me)

	order, err := createTestOrder(orderService, CreateOrderInput{
		UserID:     userID,
		CoinSymbol: "BTC",
		Side:       "BUY",
		Price:      "5000",
		Amount:     "1",
	})

	require.Error(t, err)
	assert.Nil(t, order)

	var orderCount int64
	require.NoError(t, db.Model(&model.Order{}).Where("user_id = ?", userID).Count(&orderCount).Error)
	assert.Equal(t, int64(0), orderCount)

	assertLedgerBalances(t, db, userID, model.KRWAssetSymbol, decimal.NewFromInt(50), decimal.Zero)

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

	seedLedgerFunds(t, db, userID, "BTC", decimal.NewFromInt(5))

	me := matching.NewMatchingEngine()
	orderService := newIntegrationOrderService(db, me)

	order, err := createTestOrder(orderService, CreateOrderInput{
		UserID:     userID,
		CoinSymbol: "BTC",
		Side:       "SELL",
		Price:      "5000",
		Amount:     "2",
	})

	require.NoError(t, err)
	require.NotZero(t, order.ID)

	assertLedgerBalances(t, db, userID, "BTC", decimal.NewFromInt(3), decimal.NewFromInt(2))
	requireJournalByKey(t, db, orderHoldKey(order.ID))
}

func TestIntegrationCreateOrderAllowsOwnCrossingOrderAndSubmitsToEngine(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(4)
	defer cleanupServiceUsers(t, db, userID)

	seedLedgerFunds(t, db, userID, model.KRWAssetSymbol, decimal.NewFromInt(10000))
	require.NoError(t, db.Create(&model.Order{
		UserID:       userID,
		CoinSymbol:   "BTC",
		Side:         model.OrderSideSell,
		OrderType:    model.OrderTypeLimit,
		Status:       model.OrderStatusPending,
		Price:        decimal.NewFromInt(5000),
		Amount:       decimal.NewFromInt(2),
		FilledAmount: decimal.Zero,
	}).Error)

	me := matching.NewMatchingEngine()
	orderService := newIntegrationOrderService(db, me)

	order, err := createTestOrder(orderService, CreateOrderInput{
		UserID:     userID,
		CoinSymbol: "BTC",
		Side:       "BUY",
		Price:      "5000",
		Amount:     "1",
	})

	require.NoError(t, err)
	require.NotNil(t, order)

	assertLedgerBalances(t, db, userID, model.KRWAssetSymbol, decimal.RequireFromString("4997.5"), decimal.RequireFromString("5002.5"))

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
	// Stop이 투입된 두 주문을 모두 처리한 뒤 마지막 스냅샷을 flush하고 종료하므로,
	// 이후 오더북은 불변이고 엔진 goroutine과의 레이스가 없다. 스냅샷은 코얼레싱으로
	// 심볼당 1개 이상(주문 2개가 한 스냅샷으로 합쳐질 수 있음)만 확인한다.
	me.Stop()
	<-me.Done()
	requireCoinSnapshots(t, snapshots, coinSymbol, 1)

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

	buyOrder, sellOrder := seedSettlementRows(t, db, buyerID, sellerID, decimal.RequireFromString("500.25"), decimal.NewFromInt(5))
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db))

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

	result, err := settlementService.SettleTrade(trade, 0)
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
	assert.True(t, persistedTrade.FeeRate.Equal(decimal.RequireFromString("0.0005")))
	assert.True(t, persistedTrade.BuyerFee.Equal(decimal.RequireFromString("0.225")))
	assert.Equal(t, model.KRWAssetSymbol, persistedTrade.BuyerFeeAsset)
	assert.True(t, persistedTrade.SellerFee.Equal(decimal.RequireFromString("0.225")))
	assert.Equal(t, model.KRWAssetSymbol, persistedTrade.SellerFeeAsset)

	var persistedBuy model.Order
	var persistedSell model.Order
	require.NoError(t, db.First(&persistedBuy, buyOrder.ID).Error)
	require.NoError(t, db.First(&persistedSell, sellOrder.ID).Error)
	assert.Equal(t, model.OrderStatusFilled, persistedBuy.Status)
	assert.Equal(t, model.OrderStatusFilled, persistedSell.Status)
	assert.True(t, persistedBuy.FilledAmount.Equal(decimal.NewFromInt(5)))
	assert.True(t, persistedSell.FilledAmount.Equal(decimal.NewFromInt(5)))

	assertLedgerBalances(t, db, buyerID, model.KRWAssetSymbol, decimal.RequireFromString("50.025"), decimal.Zero)
	assertLedgerBalances(t, db, buyerID, "BTC", decimal.NewFromInt(5), decimal.Zero)
	assertLedgerBalances(t, db, sellerID, "BTC", decimal.Zero, decimal.Zero)
	assertLedgerBalances(t, db, sellerID, model.KRWAssetSymbol, decimal.RequireFromString("449.775"), decimal.Zero)

	buyerSnapshot := userAssetSnapshot(t, db, buyerID)
	assert.Contains(t, buyerSnapshot["BTC"], "avg=90.045")
	sellerSnapshot := userAssetSnapshot(t, db, sellerID)
	assert.Contains(t, sellerSnapshot["BTC"], "avg=0")

	requireJournalByKey(t, db, "trade:"+trade.IdempotencyKey)
}

func TestIntegrationSettleTradeFailureRollsBackAllWrites(t *testing.T) {
	db := openServiceIntegrationDB(t)
	buyerID := serviceTestUserID(6)
	sellerID := serviceTestUserID(7)
	defer cleanupServiceUsers(t, db, buyerID, sellerID)

	buyOrder, sellOrder := seedSettlementRows(t, db, buyerID, sellerID, decimal.RequireFromString("500.25"), decimal.NewFromInt(1))
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db))

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

	_, err := settlementService.SettleTrade(trade, 0)
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

	assertLedgerBalances(t, db, buyerID, model.KRWAssetSymbol, decimal.Zero, decimal.RequireFromString("500.25"))
	assertLedgerBalances(t, db, sellerID, "BTC", decimal.Zero, decimal.NewFromInt(1))
}

func TestIntegrationSettleTradeDuplicateIsIdempotent(t *testing.T) {
	db := openServiceIntegrationDB(t)
	buyerID := serviceTestUserID(8)
	sellerID := serviceTestUserID(9)
	defer cleanupServiceUsers(t, db, buyerID, sellerID)

	buyOrder, sellOrder := seedSettlementRowsWithOrderAmount(t, db, buyerID, sellerID, decimal.NewFromInt(1000), decimal.NewFromInt(10), decimal.NewFromInt(10))
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db))

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

	firstResult, err := settlementService.SettleTrade(trade, 0)
	require.NoError(t, err)
	assert.True(t, firstResult.Applied)
	assert.False(t, firstResult.Duplicate)
	require.NotEmpty(t, trade.IdempotencyKey)
	assert.Equal(t, "engine:"+trade.EngineEventID, trade.IdempotencyKey)

	duplicate := *trade
	duplicate.ID = 0
	duplicate.IdempotencyKey = ""
	duplicate.TradedAt = trade.TradedAt.Add(time.Second)

	secondResult, err := settlementService.SettleTrade(&duplicate, 0)
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

	assertLedgerBalances(t, db, buyerID, model.KRWAssetSymbol, decimal.RequireFromString("50.025"), decimal.RequireFromString("499.75"))
	assertLedgerBalances(t, db, buyerID, "BTC", decimal.NewFromInt(5), decimal.Zero)
	assertLedgerBalances(t, db, sellerID, "BTC", decimal.Zero, decimal.NewFromInt(5))
	assertLedgerBalances(t, db, sellerID, model.KRWAssetSymbol, decimal.RequireFromString("449.775"), decimal.Zero)

	buyerSnapshot := userAssetSnapshot(t, db, buyerID)
	assert.Contains(t, buyerSnapshot["BTC"], "avg=90.045")
	requireJournalByKey(t, db, "trade:"+trade.IdempotencyKey)
}

func TestIntegrationSettleTradeSameIdempotencyKeyDifferentPayloadReturnsConflict(t *testing.T) {
	db := openServiceIntegrationDB(t)
	buyerID := serviceTestUserID(10)
	sellerID := serviceTestUserID(11)
	defer cleanupServiceUsers(t, db, buyerID, sellerID)

	buyOrder, sellOrder := seedSettlementRowsWithOrderAmount(t, db, buyerID, sellerID, decimal.NewFromInt(1000), decimal.NewFromInt(10), decimal.NewFromInt(10))
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db))

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

	firstResult, err := settlementService.SettleTrade(trade, 0)
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

	conflictResult, err := settlementService.SettleTrade(conflictingTrade, 0)
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
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db))

	trade := &model.Trade{
		CoinSymbol:  "BTC",
		Price:       decimal.NewFromInt(90),
		Quantity:    decimal.NewFromInt(5),
		TradedAt:    time.Now(),
		BuyOrderID:  buyOrder.ID,
		SellOrderID: sellOrder.ID,
	}

	result, err := settlementService.SettleTrade(trade, 0)

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
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db))
	failedSettlementService := NewFailedSettlementService(repository.NewFailedSettlementRepository(db))

	trade := &model.Trade{
		CoinSymbol:  "BTC",
		Price:       decimal.NewFromInt(90),
		Quantity:    decimal.NewFromInt(5),
		TradedAt:    time.Now(),
		BuyOrderID:  buyOrder.ID,
		SellOrderID: sellOrder.ID,
	}

	result, err := settlementService.SettleTrade(trade, 0)
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

func TestIntegrationFailedMarketCompletionRecordFailureClampsNegativeRemainingQuoteAmount(t *testing.T) {
	db := openServiceIntegrationDB(t)
	orderID := uint(time.Now().UnixNano() % 1_000_000_000)
	t.Cleanup(func() {
		require.NoError(t, db.Where("order_id = ?", orderID).Delete(&model.FailedMarketCompletion{}).Error)
	})

	failedMarketCompletionService := NewFailedMarketCompletionService(repository.NewFailedMarketCompletionRepository(db))

	input := CompleteMarketOrderInput{
		OrderID:              orderID,
		FilledAmount:         decimal.NewFromInt(1),
		FilledQuoteAmount:    decimal.NewFromInt(50000),
		RemainingQuoteAmount: decimal.RequireFromString("-0.0000000018775"),
	}

	failure, err := failedMarketCompletionService.RecordFailure(input, "BTC", fmt.Errorf("market buy order %d spent quote amount exceeds quote budget", orderID))
	require.NoError(t, err)
	assert.True(t, failure.RemainingQuoteAmount.Equal(decimal.Zero), "remaining_quote_amount=%s", failure.RemainingQuoteAmount.String())

	var count int64
	require.NoError(t, db.Model(&model.FailedMarketCompletion{}).Where("order_id = ?", orderID).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestIntegrationSettleTradeRejectsCancelledSellOrder(t *testing.T) {
	db := openServiceIntegrationDB(t)
	buyerID := serviceTestUserID(22)
	sellerID := serviceTestUserID(23)
	defer cleanupServiceUsers(t, db, buyerID, sellerID)

	buyOrder, sellOrder := seedSettlementRowsWithStatuses(t, db, buyerID, sellerID, decimal.NewFromInt(500), decimal.Zero, decimal.NewFromInt(5), model.OrderStatusPending, model.OrderStatusCancelled)
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db))

	trade := &model.Trade{
		CoinSymbol:  "BTC",
		Price:       decimal.NewFromInt(90),
		Quantity:    decimal.NewFromInt(5),
		TradedAt:    time.Now(),
		BuyOrderID:  buyOrder.ID,
		SellOrderID: sellOrder.ID,
	}

	result, err := settlementService.SettleTrade(trade, 0)

	require.Error(t, err)
	assert.False(t, result.Applied)
	assert.Contains(t, err.Error(), "sell order")
	assert.Contains(t, err.Error(), "CANCELLED")
	assertNoTradePersistedForOrders(t, db, buyOrder.ID, sellOrder.ID)
}

// (A-4 수정으로 이 테스트가 검증하던 "DB 우선 CANCELLED 커밋 → 뒤늦은 체결
// 거부"는 더 이상 CancelOrder의 동작이 아니다 — CancelOrder는 DB를 건드리지
// 않으므로 이 테스트가 재현하던 시나리오 자체가 성립하지 않는다. 대체 증명은
// TestIntegrationCancelDuringInFlightPartialFillProducesNoFailedSettlements로
// 옮겨졌다.
//
// 엔진 미스는 이제 409가 아니다 — CancelOrder가 엔진을 호출하지 않으므로
// 오더북에 없는 주문도 접수된다(TestIntegrationCancelAcceptsOrderMissingFromEngineBook).

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
		LockedBalance: decimal.RequireFromString("500.25"),
	})
	me := matching.NewMatchingEngine()
	me.Start()
	submitIntegrationEngineOrder(t, me, order, decimal.NewFromInt(5))

	orderService := newIntegrationOrderService(db, me)
	result, err := orderService.CancelOrder(CancelOrderInput{UserID: userID, OrderID: order.ID})

	require.NoError(t, err)
	// 응답은 "취소 의도를 내구 기록했다"까지다. 오더북 제거는 worker가 별도로 한다.
	assert.Equal(t, CancelOrderAcceptedStatus, result.Status)
	assert.NotZero(t, result.CommandID)
	assert.Equal(t, order.ID, result.OrderID)
	defer cleanupServiceCancelCommands(t, db, result.CommandID)

	command := requireCancelCommand(t, db, result.CommandID)
	assert.Equal(t, model.CancelCommandStatusPending, command.Status)
	assert.Equal(t, order.CoinSymbol, command.CoinSymbol)
	assert.Equal(t, order.Side, command.Side)
	assert.True(t, command.Price.Equal(order.Price), "price=%s", command.Price)

	var afterAccept model.Order
	require.NoError(t, db.First(&afterAccept, order.ID).Error)
	assert.Equal(t, model.OrderStatusPending, afterAccept.Status, "CancelOrder만으로는 DB가 아직 CANCELLED가 되면 안 된다")

	// worker가 command를 엔진에 전달하는 것을 시뮬레이션한다.
	engineResult := me.CancelOrder(matching.CancelOrderCommand{
		CommandID:  result.CommandID,
		CoinSymbol: command.CoinSymbol,
		OrderID:    command.OrderID,
		Side:       command.Side,
		Price:      command.Price,
	})
	require.NoError(t, engineResult.Err)
	require.True(t, engineResult.Removed)
	requireIntegrationSnapshot(t, me)
	assert.Equal(t, 0, me.GetOrderBook("BTC").BuyOrders.Len())

	cancelled := requireIntegrationOrderCancelledEvent(t, me)
	assert.Equal(t, result.CommandID, cancelled.CommandID)
	assert.Equal(t, order.ID, cancelled.OrderID)
	require.NoError(t, orderService.ProcessOrderCancellation(cancelled))

	assertCancelledOrderAndBalance(t, db, order.ID, userID, model.KRWAssetSymbol, decimal.RequireFromString("500.25"), decimal.Zero)
	requireJournalByKey(t, db, orderReleaseKey(order.ID, releaseReasonCancel))
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
		LockedBalance: decimal.RequireFromString("600.3"),
	})
	me := matching.NewMatchingEngine()
	me.Start()
	submitIntegrationEngineOrder(t, me, order, decimal.NewFromInt(6))

	orderService := newIntegrationOrderService(db, me)
	result, err := orderService.CancelOrder(CancelOrderInput{UserID: userID, OrderID: order.ID})

	require.NoError(t, err)
	assert.Equal(t, CancelOrderAcceptedStatus, result.Status)
	require.NotZero(t, result.CommandID)
	defer cleanupServiceCancelCommands(t, db, result.CommandID)

	var afterAccept model.Order
	require.NoError(t, db.First(&afterAccept, order.ID).Error)
	assert.Equal(t, model.OrderStatusPartial, afterAccept.Status, "CancelOrder만으로는 DB가 아직 CANCELLED가 되면 안 된다")

	command := requireCancelCommand(t, db, result.CommandID)
	engineResult := me.CancelOrder(matching.CancelOrderCommand{
		CommandID:  result.CommandID,
		CoinSymbol: command.CoinSymbol,
		OrderID:    command.OrderID,
		Side:       command.Side,
		Price:      command.Price,
	})
	require.NoError(t, engineResult.Err)
	requireIntegrationSnapshot(t, me)

	cancelled := requireIntegrationOrderCancelledEvent(t, me)
	require.NoError(t, orderService.ProcessOrderCancellation(cancelled))

	assertCancelledOrderAndBalance(t, db, order.ID, userID, model.KRWAssetSymbol, decimal.RequireFromString("600.3"), decimal.Zero)
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
	assert.Equal(t, CancelOrderAcceptedStatus, result.Status)
	require.NotZero(t, result.CommandID)
	defer cleanupServiceCancelCommands(t, db, result.CommandID)

	var afterAccept model.Order
	require.NoError(t, db.First(&afterAccept, order.ID).Error)
	assert.Equal(t, model.OrderStatusPending, afterAccept.Status, "CancelOrder만으로는 DB가 아직 CANCELLED가 되면 안 된다")

	command := requireCancelCommand(t, db, result.CommandID)
	assert.Equal(t, model.OrderSideSell, command.Side)
	engineResult := me.CancelOrder(matching.CancelOrderCommand{
		CommandID:  result.CommandID,
		CoinSymbol: command.CoinSymbol,
		OrderID:    command.OrderID,
		Side:       command.Side,
		Price:      command.Price,
	})
	require.NoError(t, engineResult.Err)
	requireIntegrationSnapshot(t, me)
	assert.Equal(t, 0, me.GetOrderBook("BTC").SellOrders.Len())

	cancelled := requireIntegrationOrderCancelledEvent(t, me)
	require.NoError(t, orderService.ProcessOrderCancellation(cancelled))

	assertCancelledOrderAndBalance(t, db, order.ID, userID, "BTC", decimal.NewFromInt(5), decimal.Zero)
	requireJournalByKey(t, db, orderReleaseKey(order.ID, releaseReasonCancel))
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
		LockedBalance: decimal.RequireFromString("500.25"),
	})
	orderService := newIntegrationOrderService(db, nil)

	result, err := orderService.CancelOrder(CancelOrderInput{UserID: requestUserID, OrderID: order.ID})

	require.Error(t, err)
	assert.Nil(t, result)
	var persisted model.Order
	require.NoError(t, db.First(&persisted, order.ID).Error)
	assert.Equal(t, model.OrderStatusPending, persisted.Status)
	assertLedgerBalances(t, db, ownerID, model.KRWAssetSymbol, decimal.Zero, decimal.RequireFromString("500.25"))
}

// hold 해제(releaseOrderHold)의 소유권이 CancelOrder에서 ProcessOrderCancellation으로
// 옮겨갔으므로(A-4), "지갑 잔고 부족 시 롤백" 시나리오도 이제
// ProcessOrderCancellation에서 검증한다 — CancelOrder는 더 이상 지갑을 건드리지
// 않아 이 실패 모드 자체가 발생할 수 없다.
func TestIntegrationProcessOrderCancellationRollsBackWhenWalletLockedBalanceIsInsufficient(t *testing.T) {
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
		LockedBalance: decimal.NewFromInt(100), // 해제에 필요한 500.25보다 부족
	})
	orderService := newIntegrationOrderService(db, nil)

	err := orderService.ProcessOrderCancellation(matching.OrderCancelled{
		OrderID:    order.ID,
		CoinSymbol: order.CoinSymbol,
		Side:       order.Side,
	})

	require.Error(t, err)
	var persisted model.Order
	require.NoError(t, db.First(&persisted, order.ID).Error)
	assert.Equal(t, model.OrderStatusPending, persisted.Status, "실패 시 상태 커밋이 롤백돼야 한다")
	assertLedgerBalances(t, db, userID, model.KRWAssetSymbol, decimal.Zero, decimal.NewFromInt(100))
}

// 엔진 상태는 더 이상 취소 접수의 판정 근거가 아니다. CancelOrder는 엔진을
// 호출하지 않으므로 오더북에 없는 주문도 202로 접수된다 — "엔진에 없음"의 해석은
// worker가 DB 주문 상태를 보고 결정한다(NOOP 또는 재시도).
//
// 이 계약이 바뀐 이유: 예전에는 엔진 미스를 409로 돌려줬는데, 그러려면 응답 전에
// 엔진을 동기 호출해야 하고 그건 내구 기록보다 앞선다.
func TestIntegrationCancelAcceptsOrderMissingFromEngineBook(t *testing.T) {
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
		LockedBalance: decimal.RequireFromString("500.25"),
	})
	me := matching.NewMatchingEngine()
	me.Start()
	orderService := newIntegrationOrderService(db, me)

	// 주문을 엔진에 제출하지 않았다 — 엔진 오더북 관점에서는 "없음"(이미
	// 체결/소진된 것과 동일한 신호).
	result, err := orderService.CancelOrder(CancelOrderInput{UserID: userID, OrderID: order.ID})

	require.NoError(t, err, "엔진 상태는 접수 판정에 쓰이지 않는다")
	require.NotZero(t, result.CommandID)
	defer cleanupServiceCancelCommands(t, db, result.CommandID)
	assert.Equal(t, CancelOrderAcceptedStatus, result.Status)

	var persisted model.Order
	require.NoError(t, db.First(&persisted, order.ID).Error)
	assert.Equal(t, model.OrderStatusPending, persisted.Status, "CancelOrder는 주문을 건드리지 않는다")
	assertLedgerBalances(t, db, userID, model.KRWAssetSymbol, decimal.Zero, decimal.RequireFromString("500.25"))
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

	// 잠긴 잔액은 원장에 지급한 뒤 잠금 분개로 옮겨 만든다. 계정에 직접 값을
	// 써넣지 않는다 — 그러면 전기 합과 잔액 캐시가 어긋나 검산 2가 걸린다.
	seedLockedBalance(t, db, buyerID, model.KRWAssetSymbol, buyerLockedKRW, buyOrder.ID)
	seedLockedBalance(t, db, sellerID, "BTC", sellerLockedBTC, sellOrder.ID)
	return buyOrder, sellOrder
}

// seedLedgerFunds는 개발용 지급으로 사용 가능 잔액을 만든다.
func seedLedgerFunds(t *testing.T, db *gorm.DB, userID uint, asset string, amount decimal.Decimal) {
	t.Helper()

	if !amount.IsPositive() {
		return
	}
	_, err := NewDevWalletService(db).FundWallet(FundWalletInput{
		UserID: userID, CoinSymbol: asset, Amount: amount.String(),
		RequestKey: fmt.Sprintf("seed-fund-%d-%s-%d-%d", userID, asset, time.Now().UnixNano(), testIdemKeySeq.Add(1)),
	})
	require.NoError(t, err)
}

// seedLockedBalance는 지급 후 amount만큼을 잠근 상태로 만든다. 실제 주문 잠금과
// 같은 모양의 분개(available → locked)를 쓰므로 검산 4종이 통과한다.
func seedLockedBalance(t *testing.T, db *gorm.DB, userID uint, asset string, amount decimal.Decimal, orderID uint) {
	t.Helper()

	if !amount.IsPositive() {
		return
	}
	seedLedgerFunds(t, db, userID, asset, amount)

	owner := userID
	ledger := NewLedgerService(db)
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		_, _, err := ledger.Record(tx, JournalInput{
			EventType:      model.JournalEventOrderHold,
			IdempotencyKey: orderHoldKey(orderID),
			ReferenceType:  model.JournalReferenceOrder,
			ReferenceID:    orderID,
			Postings: []PostingInput{
				{AccountType: model.AccountUserAvailable, OwnerUserID: &owner, Asset: asset, Amount: amount.Neg()},
				{AccountType: model.AccountUserLocked, OwnerUserID: &owner, Asset: asset, Amount: amount},
			},
		})
		return err
	}))
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

	asset := model.KRWAssetSymbol
	if seed.Side == model.OrderSideSell {
		asset = seed.CoinSymbol
	}
	seedLockedBalance(t, db, seed.UserID, asset, seed.LockedBalance, order.ID)
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

// requireIntegrationOrderCancelledEvent는 CancelOrder 접수 후 엔진이
// ExecutionCh에 방출한 OrderCancelled 이벤트를 기다린다. 정산 파이프라인이
// ProcessOrderCancellation으로 이 이벤트를 소비하는 것을 테스트에서 흉내내기
// 위한 헬퍼다(A-4: CancelOrder 자신은 더 이상 DB를 확정하지 않는다).
func requireIntegrationOrderCancelledEvent(t *testing.T, me *matching.MatchingEngine) matching.OrderCancelled {
	t.Helper()

	select {
	case event := <-me.ExecutionCh:
		require.NotNil(t, event.OrderCancelled, "OrderCancelled 이벤트가 방출돼야 한다")
		return *event.OrderCancelled
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for matching engine OrderCancelled event")
		return matching.OrderCancelled{}
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


// ledgerBalances는 사용자의 (available, locked) 잔액을 원장 캐시에서 읽는다.
// 계정이 아직 없으면 0이다 — 그 자산을 만진 적이 없다는 뜻이다.
func ledgerBalances(t *testing.T, db *gorm.DB, userID uint, asset string) (decimal.Decimal, decimal.Decimal) {
	t.Helper()

	var row struct {
		Available decimal.Decimal
		Locked    decimal.Decimal
	}
	require.NoError(t, db.Raw(`
		SELECT
			COALESCE(SUM(b.balance) FILTER (WHERE a.account_type = 'USER_AVAILABLE'), 0) AS available,
			COALESCE(SUM(b.balance) FILTER (WHERE a.account_type = 'USER_LOCKED'), 0)    AS locked
		FROM accounts a
		JOIN account_balances b ON b.account_id = a.id
		WHERE a.owner_user_id = ? AND a.asset = ?`, userID, asset).Scan(&row).Error)
	return row.Available, row.Locked
}

// assertLedgerBalances는 원장 잔액이 기대와 같은지 본다. 지갑 시절의
// assertWalletBalances를 대신한다.
func assertLedgerBalances(t *testing.T, db *gorm.DB, userID uint, asset string, available decimal.Decimal, locked decimal.Decimal) {
	t.Helper()

	gotAvailable, gotLocked := ledgerBalances(t, db, userID, asset)
	assert.True(t, gotAvailable.Equal(available),
		"%s available=%s, 기대 %s", asset, gotAvailable.String(), available.String())
	assert.True(t, gotLocked.Equal(locked),
		"%s locked=%s, 기대 %s", asset, gotLocked.String(), locked.String())
}

// assertCancelledOrderAndBalance는 주문이 CANCELLED로 커밋됐고 해제된 자산의
// 원장 잔액이 기대와 같은지 함께 본다 — 단식 원장 시절의
// assertCancelledOrderAndWallet을 대신한다.
func assertCancelledOrderAndBalance(t *testing.T, db *gorm.DB, orderID uint, userID uint, asset string, available decimal.Decimal, locked decimal.Decimal) {
	t.Helper()

	var order model.Order
	require.NoError(t, db.First(&order, orderID).Error)
	assert.Equal(t, model.OrderStatusCancelled, order.Status)
	assertLedgerBalances(t, db, userID, asset, available, locked)
}

// requireJournalByKey는 멱등성 키로 분개를 찾는다. 사건이 실제로 기록됐는지
// 보는 단언이다 — 단식 원장 시절의 requireLedgerEntries를 대신한다.
func requireJournalByKey(t *testing.T, db *gorm.DB, key string) model.JournalEntry {
	t.Helper()

	var journal model.JournalEntry
	require.NoError(t, db.Where("idempotency_key = ?", key).First(&journal).Error,
		"분개 %s가 없다", key)
	return journal
}

// requireNoJournalByKey는 그 사건이 기록되지 않았음을 본다.
func requireNoJournalByKey(t *testing.T, db *gorm.DB, key string) {
	t.Helper()

	var count int64
	require.NoError(t, db.Model(&model.JournalEntry{}).Where("idempotency_key = ?", key).Count(&count).Error)
	assert.Equal(t, int64(0), count, "분개 %s가 있으면 안 된다", key)
}

// userPostingCount는 사용자의 계정에 달린 전기 수를 센다. assertLedgerCount의
// 대체다 — "이 사용자에 대해 아무것도 기록되지 않았다"를 보는 데 쓴다.
func userPostingCount(t *testing.T, db *gorm.DB, userID uint) int64 {
	t.Helper()

	var count int64
	require.NoError(t, db.Raw(`
		SELECT COUNT(*)
		FROM postings p
		JOIN accounts a ON a.id = p.account_id
		WHERE a.owner_user_id = ?`, userID).Scan(&count).Error)
	return count
}

// orderHoldKey·orderReleaseKey는 order_ledger.go가 만드는 멱등성 키와 같은
// 문자열을 만든다. 테스트가 키 문자열을 직접 적으면 구현이 바뀔 때 조용히 어긋난다.
func orderHoldKey(orderID uint) string {
	return fmt.Sprintf("order-hold:%d", orderID)
}

func orderReleaseKey(orderID uint, reason string) string {
	return fmt.Sprintf("order-release:%d:%s", orderID, reason)
}
