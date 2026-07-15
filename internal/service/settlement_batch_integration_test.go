package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// batchScenarioFixture는 등가성 테스트의 세 시나리오(독립 trade, 대형 테이커의 주문이
// 여러 trade에 걸침, 같은 유저가 매수자이자 매도자)를 한 시퀀스에 담기 위한 픽스처다.
type batchScenarioFixture struct {
	userIDs                          []uint
	buyAID, sellBID                  uint
	buyCID, sellDID                  uint
	buyEID, sellFID, sellGID         uint
	buyHID, sellIID, buyJID, sellHID uint
}

func seedBatchScenarioFixture(t *testing.T, db *gorm.DB, offsetBase uint) batchScenarioFixture {
	t.Helper()

	userID := func(n uint) uint { return serviceTestUserID(offsetBase + n) }
	a, b := userID(0), userID(1)
	c, d := userID(2), userID(3)
	e, f, g := userID(4), userID(5), userID(6)
	h, i, j := userID(7), userID(8), userID(9)

	price := decimal.NewFromInt(100)
	lockedKRWFor := func(amount decimal.Decimal) decimal.Decimal {
		return quoteAmountWithTradingFee(price.Mul(amount))
	}

	wallets := []model.Wallet{
		// 시나리오 1: A(매수)/B(매도) — 독립
		{UserID: a, CoinSymbol: model.KRWAssetSymbol, KRW: lockedKRWFor(decimal.NewFromInt(2)), AvailableBalance: decimal.Zero, LockedBalance: lockedKRWFor(decimal.NewFromInt(2))},
		{UserID: a, CoinSymbol: "BTC", Quantity: decimal.Zero, AvailableBalance: decimal.Zero, LockedBalance: decimal.Zero},
		{UserID: b, CoinSymbol: "BTC", Quantity: decimal.NewFromInt(2), AvailableBalance: decimal.Zero, LockedBalance: decimal.NewFromInt(2)},
		{UserID: b, CoinSymbol: model.KRWAssetSymbol, KRW: decimal.Zero, AvailableBalance: decimal.Zero, LockedBalance: decimal.Zero},

		// 시나리오 1: C(매수)/D(매도) — 독립
		{UserID: c, CoinSymbol: model.KRWAssetSymbol, KRW: lockedKRWFor(decimal.NewFromInt(3)), AvailableBalance: decimal.Zero, LockedBalance: lockedKRWFor(decimal.NewFromInt(3))},
		{UserID: c, CoinSymbol: "BTC", Quantity: decimal.Zero, AvailableBalance: decimal.Zero, LockedBalance: decimal.Zero},
		{UserID: d, CoinSymbol: "BTC", Quantity: decimal.NewFromInt(3), AvailableBalance: decimal.Zero, LockedBalance: decimal.NewFromInt(3)},
		{UserID: d, CoinSymbol: model.KRWAssetSymbol, KRW: decimal.Zero, AvailableBalance: decimal.Zero, LockedBalance: decimal.Zero},

		// 시나리오 2: E는 대형 테이커 매수 주문(수량 10)으로 trade 2건에 걸친다. F, G가 매도자.
		{UserID: e, CoinSymbol: model.KRWAssetSymbol, KRW: lockedKRWFor(decimal.NewFromInt(10)), AvailableBalance: decimal.Zero, LockedBalance: lockedKRWFor(decimal.NewFromInt(10))},
		{UserID: e, CoinSymbol: "BTC", Quantity: decimal.Zero, AvailableBalance: decimal.Zero, LockedBalance: decimal.Zero},
		{UserID: f, CoinSymbol: "BTC", Quantity: decimal.NewFromInt(4), AvailableBalance: decimal.Zero, LockedBalance: decimal.NewFromInt(4)},
		{UserID: f, CoinSymbol: model.KRWAssetSymbol, KRW: decimal.Zero, AvailableBalance: decimal.Zero, LockedBalance: decimal.Zero},
		{UserID: g, CoinSymbol: "BTC", Quantity: decimal.NewFromInt(6), AvailableBalance: decimal.Zero, LockedBalance: decimal.NewFromInt(6)},
		{UserID: g, CoinSymbol: model.KRWAssetSymbol, KRW: decimal.Zero, AvailableBalance: decimal.Zero, LockedBalance: decimal.Zero},

		// 시나리오 3: H는 trade5의 매수자이자 trade6의 매도자 — 지갑이 배치 내에서 공유·진화한다.
		{UserID: h, CoinSymbol: model.KRWAssetSymbol, KRW: lockedKRWFor(decimal.NewFromInt(2)), AvailableBalance: decimal.Zero, LockedBalance: lockedKRWFor(decimal.NewFromInt(2))},
		{UserID: h, CoinSymbol: "BTC", Quantity: decimal.NewFromInt(1), AvailableBalance: decimal.Zero, LockedBalance: decimal.NewFromInt(1)},
		{UserID: i, CoinSymbol: "BTC", Quantity: decimal.NewFromInt(2), AvailableBalance: decimal.Zero, LockedBalance: decimal.NewFromInt(2)},
		{UserID: i, CoinSymbol: model.KRWAssetSymbol, KRW: decimal.Zero, AvailableBalance: decimal.Zero, LockedBalance: decimal.Zero},
		{UserID: j, CoinSymbol: model.KRWAssetSymbol, KRW: lockedKRWFor(decimal.NewFromInt(1)), AvailableBalance: decimal.Zero, LockedBalance: lockedKRWFor(decimal.NewFromInt(1))},
		{UserID: j, CoinSymbol: "BTC", Quantity: decimal.Zero, AvailableBalance: decimal.Zero, LockedBalance: decimal.Zero},
	}
	require.NoError(t, db.Create(&wallets).Error)

	mkOrder := func(userID uint, side model.OrderSide, amount decimal.Decimal) model.Order {
		order := model.Order{
			UserID:       userID,
			CoinSymbol:   "BTC",
			Side:         side,
			OrderType:    model.OrderTypeLimit,
			Price:        price,
			Amount:       amount,
			Status:       model.OrderStatusPending,
			FilledAmount: decimal.Zero,
		}
		require.NoError(t, db.Create(&order).Error)
		return order
	}

	buyA := mkOrder(a, model.OrderSideBuy, decimal.NewFromInt(2))
	sellB := mkOrder(b, model.OrderSideSell, decimal.NewFromInt(2))
	buyC := mkOrder(c, model.OrderSideBuy, decimal.NewFromInt(3))
	sellD := mkOrder(d, model.OrderSideSell, decimal.NewFromInt(3))
	buyE := mkOrder(e, model.OrderSideBuy, decimal.NewFromInt(10))
	sellF := mkOrder(f, model.OrderSideSell, decimal.NewFromInt(4))
	sellG := mkOrder(g, model.OrderSideSell, decimal.NewFromInt(6))
	buyH := mkOrder(h, model.OrderSideBuy, decimal.NewFromInt(2))
	sellI := mkOrder(i, model.OrderSideSell, decimal.NewFromInt(2))
	buyJ := mkOrder(j, model.OrderSideBuy, decimal.NewFromInt(1))
	sellH := mkOrder(h, model.OrderSideSell, decimal.NewFromInt(1))

	return batchScenarioFixture{
		userIDs: []uint{a, b, c, d, e, f, g, h, i, j},
		buyAID:  buyA.ID, sellBID: sellB.ID,
		buyCID: buyC.ID, sellDID: sellD.ID,
		buyEID: buyE.ID, sellFID: sellF.ID, sellGID: sellG.ID,
		buyHID: buyH.ID, sellIID: sellI.ID, buyJID: buyJ.ID, sellHID: sellH.ID,
	}
}

func batchScenarioOrderIDs(f batchScenarioFixture) []uint {
	return []uint{f.buyAID, f.sellBID, f.buyCID, f.sellDID, f.buyEID, f.sellFID, f.sellGID, f.buyHID, f.sellIID, f.buyJID, f.sellHID}
}

func batchScenarioTrades(f batchScenarioFixture, runTag string) []*model.Trade {
	price := decimal.NewFromInt(100)
	mk := func(buyOrderID uint, sellOrderID uint, quantity int64, tag string) *model.Trade {
		return &model.Trade{
			EngineSequence: 1,
			EngineEventID:  fmt.Sprintf("batch-equiv-%s-%s-%d", runTag, tag, time.Now().UnixNano()),
			CoinSymbol:     "BTC",
			Price:          price,
			Quantity:       decimal.NewFromInt(quantity),
			TradedAt:       time.Now().UTC(),
			BuyOrderID:     buyOrderID,
			SellOrderID:    sellOrderID,
		}
	}
	return []*model.Trade{
		mk(f.buyAID, f.sellBID, 2, "t1"),
		mk(f.buyCID, f.sellDID, 3, "t2"),
		mk(f.buyEID, f.sellFID, 4, "t3"),
		mk(f.buyEID, f.sellGID, 6, "t4"),
		mk(f.buyHID, f.sellIID, 2, "t5"),
		mk(f.buyJID, f.sellHID, 1, "t6"),
	}
}

func assertWalletsMatch(t *testing.T, walletRepo *repository.WalletRepository, leftUserID uint, rightUserID uint) {
	t.Helper()

	left, err := walletRepo.ListByUserID(leftUserID)
	require.NoError(t, err)
	right, err := walletRepo.ListByUserID(rightUserID)
	require.NoError(t, err)
	require.Equal(t, len(right), len(left), "wallet count mismatch user %d vs %d", leftUserID, rightUserID)
	for idx := range left {
		lw, rw := left[idx], right[idx]
		assert.Equal(t, rw.CoinSymbol, lw.CoinSymbol)
		assert.True(t, lw.AvailableBalance.Equal(rw.AvailableBalance), "AvailableBalance user %d/%d coin %s: %s vs %s", leftUserID, rightUserID, lw.CoinSymbol, lw.AvailableBalance, rw.AvailableBalance)
		assert.True(t, lw.LockedBalance.Equal(rw.LockedBalance), "LockedBalance user %d/%d coin %s: %s vs %s", leftUserID, rightUserID, lw.CoinSymbol, lw.LockedBalance, rw.LockedBalance)
		assert.True(t, lw.KRW.Equal(rw.KRW), "KRW user %d/%d coin %s: %s vs %s", leftUserID, rightUserID, lw.CoinSymbol, lw.KRW, rw.KRW)
		assert.True(t, lw.Quantity.Equal(rw.Quantity), "Quantity user %d/%d coin %s: %s vs %s", leftUserID, rightUserID, lw.CoinSymbol, lw.Quantity, rw.Quantity)
		assert.True(t, lw.AvgBuyPrice.Equal(rw.AvgBuyPrice), "AvgBuyPrice user %d/%d coin %s: %s vs %s", leftUserID, rightUserID, lw.CoinSymbol, lw.AvgBuyPrice, rw.AvgBuyPrice)
	}
}

func assertOrdersMatch(t *testing.T, db *gorm.DB, leftOrderID uint, rightOrderID uint) {
	t.Helper()

	var lo, ro model.Order
	require.NoError(t, db.First(&lo, leftOrderID).Error)
	require.NoError(t, db.First(&ro, rightOrderID).Error)
	assert.True(t, lo.FilledAmount.Equal(ro.FilledAmount), "FilledAmount order %d vs %d", leftOrderID, rightOrderID)
	assert.True(t, lo.FilledQuoteAmount.Equal(ro.FilledQuoteAmount), "FilledQuoteAmount order %d vs %d", leftOrderID, rightOrderID)
	assert.Equal(t, ro.Status, lo.Status, "Status order %d vs %d", leftOrderID, rightOrderID)
}

func assertLedgerSequencesMatch(t *testing.T, db *gorm.DB, leftUserID uint, rightUserID uint) {
	t.Helper()

	var left, right []model.LedgerEntry
	require.NoError(t, db.Where("user_id = ?", leftUserID).Order("coin_symbol ASC").Order("id ASC").Find(&left).Error)
	require.NoError(t, db.Where("user_id = ?", rightUserID).Order("coin_symbol ASC").Order("id ASC").Find(&right).Error)
	require.Equal(t, len(right), len(left), "ledger entry count user %d vs %d", leftUserID, rightUserID)
	for idx := range left {
		le, re := left[idx], right[idx]
		assert.Equal(t, re.CoinSymbol, le.CoinSymbol)
		assert.True(t, le.AvailableDelta.Equal(re.AvailableDelta), "AvailableDelta idx=%d user %d vs %d", idx, leftUserID, rightUserID)
		assert.True(t, le.LockedDelta.Equal(re.LockedDelta), "LockedDelta idx=%d user %d vs %d", idx, leftUserID, rightUserID)
		assert.True(t, le.AvailableBalanceAfter.Equal(re.AvailableBalanceAfter), "AvailableBalanceAfter idx=%d user %d vs %d", idx, leftUserID, rightUserID)
		assert.True(t, le.LockedBalanceAfter.Equal(re.LockedBalanceAfter), "LockedBalanceAfter idx=%d user %d vs %d", idx, leftUserID, rightUserID)
	}
}

func countTradesForOrders(t *testing.T, db *gorm.DB, orderIDs []uint) int64 {
	t.Helper()

	var count int64
	require.NoError(t, db.Model(&model.Trade{}).Where("buy_order_id IN ? OR sell_order_id IN ?", orderIDs, orderIDs).Count(&count).Error)
	return count
}

// (a) 등가성 — 이 태스크의 핵심 테스트. 서로 다른 유저 집합으로 동일한 픽스처 2벌을
// 만들고, 같은 trade 시퀀스를 한쪽은 SettleTradeBatch 1회, 다른 쪽은 SettleTrade
// 루프로 정산한 뒤 최종 상태를 필드 단위로 비교한다.
func TestIntegrationSettleTradeBatchMatchesSequentialSingleSettlement(t *testing.T) {
	db := openServiceIntegrationDB(t)
	walletRepo := repository.NewWalletRepository(db)
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db), walletRepo)

	batchFixture := seedBatchScenarioFixture(t, db, 500)
	seqFixture := seedBatchScenarioFixture(t, db, 520)
	allUsers := append(append([]uint{}, batchFixture.userIDs...), seqFixture.userIDs...)
	defer cleanupServiceUsers(t, db, allUsers...)

	batchTrades := batchScenarioTrades(batchFixture, "batch")
	seqTrades := batchScenarioTrades(seqFixture, "seq")

	items := make([]TradeBatchItem, len(batchTrades))
	for i, trade := range batchTrades {
		items[i] = TradeBatchItem{Trade: trade}
	}
	results, err := settlementService.SettleTradeBatch(items)
	require.NoError(t, err)
	require.Len(t, results, len(batchTrades))
	for _, r := range results {
		assert.True(t, r.Applied)
		assert.False(t, r.Duplicate)
	}

	for _, trade := range seqTrades {
		_, err := settlementService.SettleTrade(trade, 0)
		require.NoError(t, err)
	}

	for idx := range batchFixture.userIDs {
		assertWalletsMatch(t, walletRepo, batchFixture.userIDs[idx], seqFixture.userIDs[idx])
		assertLedgerSequencesMatch(t, db, batchFixture.userIDs[idx], seqFixture.userIDs[idx])
	}

	batchOrderIDs := batchScenarioOrderIDs(batchFixture)
	seqOrderIDs := batchScenarioOrderIDs(seqFixture)
	for idx := range batchOrderIDs {
		assertOrdersMatch(t, db, batchOrderIDs[idx], seqOrderIDs[idx])
	}

	assert.Equal(t, int64(6), countTradesForOrders(t, db, batchOrderIDs))
	assert.Equal(t, countTradesForOrders(t, db, seqOrderIDs), countTradesForOrders(t, db, batchOrderIDs))
}

func captureWallets(t *testing.T, walletRepo *repository.WalletRepository, userIDs []uint) map[uint][]model.Wallet {
	t.Helper()

	result := make(map[uint][]model.Wallet, len(userIDs))
	for _, id := range userIDs {
		wallets, err := walletRepo.ListByUserID(id)
		require.NoError(t, err)
		result[id] = wallets
	}
	return result
}

func assertWalletSnapshotsEqual(t *testing.T, before map[uint][]model.Wallet, after map[uint][]model.Wallet) {
	t.Helper()

	for userID, beforeWallets := range before {
		afterWallets := after[userID]
		require.Equal(t, len(beforeWallets), len(afterWallets), "wallet count changed for user %d", userID)
		for idx := range beforeWallets {
			bw, aw := beforeWallets[idx], afterWallets[idx]
			assert.True(t, bw.AvailableBalance.Equal(aw.AvailableBalance), "AvailableBalance changed for user %d coin %s", userID, bw.CoinSymbol)
			assert.True(t, bw.LockedBalance.Equal(aw.LockedBalance), "LockedBalance changed for user %d coin %s", userID, bw.CoinSymbol)
			assert.True(t, bw.KRW.Equal(aw.KRW), "KRW changed for user %d coin %s", userID, bw.CoinSymbol)
			assert.True(t, bw.Quantity.Equal(aw.Quantity), "Quantity changed for user %d coin %s", userID, bw.CoinSymbol)
			assert.True(t, bw.AvgBuyPrice.Equal(aw.AvgBuyPrice), "AvgBuyPrice changed for user %d coin %s", userID, bw.CoinSymbol)
		}
	}
}

type tradePairFixture struct {
	buyerID     uint
	sellerID    uint
	buyOrderID  uint
	sellOrderID uint
}

func seedIndependentTradePairs(t *testing.T, db *gorm.DB, offsetBase uint, count int) []tradePairFixture {
	t.Helper()

	pairs := make([]tradePairFixture, count)
	for i := 0; i < count; i++ {
		buyerID := serviceTestUserID(offsetBase + uint(i*2))
		sellerID := serviceTestUserID(offsetBase + uint(i*2+1))
		buyOrder, sellOrder := seedSettlementRows(t, db, buyerID, sellerID, decimal.NewFromInt(100_000), decimal.NewFromInt(5))
		pairs[i] = tradePairFixture{buyerID: buyerID, sellerID: sellerID, buyOrderID: buyOrder.ID, sellOrderID: sellOrder.ID}
	}
	return pairs
}

func tradePairUserIDs(pairs []tradePairFixture) []uint {
	ids := make([]uint, 0, len(pairs)*2)
	for _, p := range pairs {
		ids = append(ids, p.buyerID, p.sellerID)
	}
	return ids
}

func tradeForPair(p tradePairFixture, sequence int64, tag string) *model.Trade {
	return &model.Trade{
		EngineSequence: sequence,
		EngineEventID:  fmt.Sprintf("batch-%s-%d-%d", tag, p.buyOrderID, time.Now().UnixNano()),
		CoinSymbol:     "BTC",
		Price:          decimal.NewFromInt(90),
		Quantity:       decimal.NewFromInt(5),
		TradedAt:       time.Now().UTC(),
		BuyOrderID:     p.buyOrderID,
		SellOrderID:    p.sellOrderID,
	}
}

// (b) 멱등성: 같은 배치를 2회 정산 → 2회차는 전부 Duplicate, 지갑·원장·주문 무변화,
// outbox는 마킹됨.
func TestIntegrationSettleTradeBatchIsIdempotent(t *testing.T) {
	db := openServiceIntegrationDB(t)
	walletRepo := repository.NewWalletRepository(db)
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db), walletRepo)

	pairs := seedIndependentTradePairs(t, db, 560, 3)
	userIDs := tradePairUserIDs(pairs)
	defer cleanupServiceUsers(t, db, userIDs...)

	trades := make([]*model.Trade, len(pairs))
	for i, p := range pairs {
		trades[i] = tradeForPair(p, int64(i+1), "idem")
	}

	firstOutboxIDs := make([]uint64, len(trades))
	firstItems := make([]TradeBatchItem, len(trades))
	for i, trade := range trades {
		row := seedPendingOutboxRow(t, db, trade.EngineEventID)
		firstOutboxIDs[i] = uint64(row.ID)
		firstItems[i] = TradeBatchItem{Trade: trade, OutboxEventID: uint64(row.ID)}
	}
	defer cleanupOutboxRows(t, db, firstOutboxIDs...)

	firstResults, err := settlementService.SettleTradeBatch(firstItems)
	require.NoError(t, err)
	for _, r := range firstResults {
		assert.True(t, r.Applied)
	}

	beforeWallets := captureWallets(t, walletRepo, userIDs)
	beforeLedgerCount := ledgerCountForUsers(t, db, userIDs)

	secondOutboxIDs := make([]uint64, len(trades))
	secondItems := make([]TradeBatchItem, len(trades))
	for i, trade := range trades {
		row := seedPendingOutboxRow(t, db, trade.EngineEventID)
		secondOutboxIDs[i] = uint64(row.ID)
		secondItems[i] = TradeBatchItem{Trade: trade, OutboxEventID: uint64(row.ID)}
	}
	defer cleanupOutboxRows(t, db, secondOutboxIDs...)

	secondResults, err := settlementService.SettleTradeBatch(secondItems)
	require.NoError(t, err)
	require.Len(t, secondResults, len(trades))
	for i, r := range secondResults {
		assert.False(t, r.Applied)
		assert.True(t, r.Duplicate)
		assert.Equal(t, firstResults[i].TradeID, r.TradeID)
	}

	afterWallets := captureWallets(t, walletRepo, userIDs)
	assertWalletSnapshotsEqual(t, beforeWallets, afterWallets)
	assert.Equal(t, beforeLedgerCount, ledgerCountForUsers(t, db, userIDs))

	for _, id := range secondOutboxIDs {
		var row model.TradeOutboxEvent
		require.NoError(t, db.First(&row, id).Error)
		assert.Equal(t, model.TradeOutboxStatusProcessed, row.Status)
		require.NotNil(t, row.ProcessedAt)
	}
}

func ledgerCountForUsers(t *testing.T, db *gorm.DB, userIDs []uint) int64 {
	t.Helper()

	var count int64
	require.NoError(t, db.Model(&model.LedgerEntry{}).Where("user_id IN ?", userIDs).Count(&count).Error)
	return count
}

// 배치에 기정산 1건 + 신규 2건 혼재 → 기정산은 Duplicate, 신규만 적용, outbox는
// 3건 모두 마킹된다.
func TestIntegrationSettleTradeBatchSkipsAlreadySettledTrades(t *testing.T) {
	db := openServiceIntegrationDB(t)
	walletRepo := repository.NewWalletRepository(db)
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db), walletRepo)

	pairs := seedIndependentTradePairs(t, db, 580, 3)
	userIDs := tradePairUserIDs(pairs)
	defer cleanupServiceUsers(t, db, userIDs...)

	trades := make([]*model.Trade, len(pairs))
	for i, p := range pairs {
		trades[i] = tradeForPair(p, int64(i+1), "skip")
	}

	preResult, err := settlementService.SettleTrade(trades[0], 0)
	require.NoError(t, err)
	require.True(t, preResult.Applied)

	beforeWallets := captureWallets(t, walletRepo, []uint{pairs[0].buyerID, pairs[0].sellerID})

	outboxIDs := make([]uint64, len(trades))
	items := make([]TradeBatchItem, len(trades))
	for i, trade := range trades {
		row := seedPendingOutboxRow(t, db, trade.EngineEventID)
		outboxIDs[i] = uint64(row.ID)
		items[i] = TradeBatchItem{Trade: trade, OutboxEventID: uint64(row.ID)}
	}
	defer cleanupOutboxRows(t, db, outboxIDs...)

	results, err := settlementService.SettleTradeBatch(items)
	require.NoError(t, err)
	require.Len(t, results, 3)
	assert.False(t, results[0].Applied)
	assert.True(t, results[0].Duplicate)
	assert.Equal(t, preResult.TradeID, results[0].TradeID)
	assert.True(t, results[1].Applied)
	assert.True(t, results[2].Applied)

	afterWallets := captureWallets(t, walletRepo, []uint{pairs[0].buyerID, pairs[0].sellerID})
	assertWalletSnapshotsEqual(t, beforeWallets, afterWallets)

	var order1, order2 model.Order
	require.NoError(t, db.First(&order1, pairs[1].buyOrderID).Error)
	require.NoError(t, db.First(&order2, pairs[2].buyOrderID).Error)
	assert.Equal(t, model.OrderStatusFilled, order1.Status)
	assert.Equal(t, model.OrderStatusFilled, order2.Status)

	for _, id := range outboxIDs {
		var row model.TradeOutboxEvent
		require.NoError(t, db.First(&row, id).Error)
		assert.Equal(t, model.TradeOutboxStatusProcessed, row.Status)
		require.NotNil(t, row.ProcessedAt)
	}
}

// (c) 실패 원자성: 배치 중간에 불량 trade(취소된 주문) → 에러 반환, trade 행 0,
// 지갑 무변화, outbox 전부 PENDING. settlement_outbox_integration_test.go의
// TestIntegrationSettleTradeFailureLeavesOutboxPending과 동형.
func TestIntegrationSettleTradeBatchFailureRollsBackEverything(t *testing.T) {
	db := openServiceIntegrationDB(t)
	walletRepo := repository.NewWalletRepository(db)
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db), walletRepo)

	goodPairs := seedIndependentTradePairs(t, db, 600, 1)
	badBuyerID := serviceTestUserID(610)
	badSellerID := serviceTestUserID(611)
	defer cleanupServiceUsers(t, db, append(tradePairUserIDs(goodPairs), badBuyerID, badSellerID)...)

	badBuyOrder, badSellOrder := seedSettlementRowsWithStatuses(t, db, badBuyerID, badSellerID,
		decimal.NewFromInt(100_000), decimal.NewFromInt(5), decimal.NewFromInt(5),
		model.OrderStatusCancelled, model.OrderStatusPending)

	goodTrade := tradeForPair(goodPairs[0], 1, "fail-good")
	badTrade := &model.Trade{
		EngineSequence: 2,
		EngineEventID:  fmt.Sprintf("batch-fail-bad-%d", time.Now().UnixNano()),
		CoinSymbol:     "BTC",
		Price:          decimal.NewFromInt(90),
		Quantity:       decimal.NewFromInt(5),
		TradedAt:       time.Now().UTC(),
		BuyOrderID:     badBuyOrder.ID,
		SellOrderID:    badSellOrder.ID,
	}

	goodOutbox := seedPendingOutboxRow(t, db, goodTrade.EngineEventID)
	badOutbox := seedPendingOutboxRow(t, db, badTrade.EngineEventID)
	defer cleanupOutboxRows(t, db, goodOutbox.ID, badOutbox.ID)

	items := []TradeBatchItem{
		{Trade: goodTrade, OutboxEventID: uint64(goodOutbox.ID)},
		{Trade: badTrade, OutboxEventID: uint64(badOutbox.ID)},
	}

	results, err := settlementService.SettleTradeBatch(items)
	require.Error(t, err, "취소된 주문이 섞인 배치는 실패해야 한다")
	assert.Contains(t, err.Error(), "CANCELLED")
	assert.Nil(t, results, "실패한 배치는 결과를 반환하지 않아야 한다 — 호출자는 단건 폴백으로 넘어간다")

	var tradeCount int64
	require.NoError(t, db.Model(&model.Trade{}).
		Where("buy_order_id IN ?", []uint{goodTrade.BuyOrderID, badTrade.BuyOrderID}).
		Count(&tradeCount).Error)
	assert.Equal(t, int64(0), tradeCount)

	var persistedGoodBuy model.Order
	require.NoError(t, db.First(&persistedGoodBuy, goodTrade.BuyOrderID).Error)
	assert.Equal(t, model.OrderStatusPending, persistedGoodBuy.Status)
	assert.True(t, persistedGoodBuy.FilledAmount.IsZero())

	buyerKRW, err := walletRepo.FindKRWWalletByUserID(goodPairs[0].buyerID)
	require.NoError(t, err)
	assert.True(t, buyerKRW.LockedBalance.Equal(decimal.NewFromInt(100_000)))

	for _, id := range []uint64{goodOutbox.ID, badOutbox.ID} {
		var row model.TradeOutboxEvent
		require.NoError(t, db.First(&row, id).Error)
		assert.Equal(t, model.TradeOutboxStatusPending, row.Status, "정산 롤백 시 outbox 마킹도 롤백돼 PENDING으로 남아야 한다")
		assert.Nil(t, row.ProcessedAt)
	}
}

// (d) outbox 흡수: 성공 배치 → 모든 outbox 행이 같은 트랜잭션에서 PROCESSED된다.
func TestIntegrationSettleTradeBatchMarksAllOutboxRowsProcessed(t *testing.T) {
	db := openServiceIntegrationDB(t)
	walletRepo := repository.NewWalletRepository(db)
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db), walletRepo)

	pairs := seedIndependentTradePairs(t, db, 620, 3)
	userIDs := tradePairUserIDs(pairs)
	defer cleanupServiceUsers(t, db, userIDs...)

	trades := make([]*model.Trade, len(pairs))
	for i, p := range pairs {
		trades[i] = tradeForPair(p, int64(i+1), "outbox")
	}

	outboxIDs := make([]uint64, len(trades))
	items := make([]TradeBatchItem, len(trades))
	for i, trade := range trades {
		row := seedPendingOutboxRow(t, db, trade.EngineEventID)
		outboxIDs[i] = uint64(row.ID)
		items[i] = TradeBatchItem{Trade: trade, OutboxEventID: uint64(row.ID)}
	}
	defer cleanupOutboxRows(t, db, outboxIDs...)

	results, err := settlementService.SettleTradeBatch(items)
	require.NoError(t, err)
	for _, r := range results {
		assert.True(t, r.Applied)
	}

	for _, id := range outboxIDs {
		var row model.TradeOutboxEvent
		require.NoError(t, db.First(&row, id).Error)
		assert.Equal(t, model.TradeOutboxStatusProcessed, row.Status)
		require.NotNil(t, row.ProcessedAt)
	}
}
