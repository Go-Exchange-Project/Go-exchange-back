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

	// 잠긴 잔액은 주문마다 잠금 분개 하나로 만든다. 계정에 직접 써넣으면 전기 합과
	// 잔액 캐시가 어긋난다.
	seedLockedBalance(t, db, a, model.KRWAssetSymbol, lockedKRWFor(decimal.NewFromInt(2)), buyA.ID)
	seedLockedBalance(t, db, b, "BTC", decimal.NewFromInt(2), sellB.ID)
	seedLockedBalance(t, db, c, model.KRWAssetSymbol, lockedKRWFor(decimal.NewFromInt(3)), buyC.ID)
	seedLockedBalance(t, db, d, "BTC", decimal.NewFromInt(3), sellD.ID)
	seedLockedBalance(t, db, e, model.KRWAssetSymbol, lockedKRWFor(decimal.NewFromInt(10)), buyE.ID)
	seedLockedBalance(t, db, f, "BTC", decimal.NewFromInt(4), sellF.ID)
	seedLockedBalance(t, db, g, "BTC", decimal.NewFromInt(6), sellG.ID)
	seedLockedBalance(t, db, h, model.KRWAssetSymbol, lockedKRWFor(decimal.NewFromInt(2)), buyH.ID)
	seedLockedBalance(t, db, i, "BTC", decimal.NewFromInt(2), sellI.ID)
	seedLockedBalance(t, db, j, model.KRWAssetSymbol, lockedKRWFor(decimal.NewFromInt(1)), buyJ.ID)
	seedLockedBalance(t, db, h, "BTC", decimal.NewFromInt(1), sellH.ID)

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

func assertWalletsMatch(t *testing.T, db *gorm.DB, leftUserID uint, rightUserID uint) {
	t.Helper()

	left := userAssetSnapshot(t, db, leftUserID)
	right := userAssetSnapshot(t, db, rightUserID)
	require.Equal(t, right, left, "자산 스냅샷이 user %d와 %d에서 다르다", leftUserID, rightUserID)
}

// userAssetSnapshot은 사용자의 자산별 (available, locked, 평단가)를 문자열로 굳힌다.
// 사용자 ID·계정 ID가 다른 두 세계를 비교하려면 그 값들을 빼야 한다.
func userAssetSnapshot(t *testing.T, db *gorm.DB, userID uint) map[string]string {
	t.Helper()

	var rows []struct {
		Asset       string
		Available   decimal.Decimal
		Locked      decimal.Decimal
		AvgBuyPrice decimal.Decimal
	}
	require.NoError(t, db.Raw(`
		SELECT
			a.asset AS asset,
			COALESCE(SUM(b.balance) FILTER (WHERE a.account_type = 'USER_AVAILABLE'), 0) AS available,
			COALESCE(SUM(b.balance) FILTER (WHERE a.account_type = 'USER_LOCKED'), 0)    AS locked,
			COALESCE(MAX(s.avg_buy_price), 0)                                            AS avg_buy_price
		FROM accounts a
		JOIN account_balances b ON b.account_id = a.id
		LEFT JOIN user_asset_stats s ON s.user_id = a.owner_user_id AND s.asset = a.asset
		WHERE a.owner_user_id = ?
		GROUP BY a.asset`, userID).Scan(&rows).Error)

	snapshot := make(map[string]string, len(rows))
	for _, row := range rows {
		snapshot[row.Asset] = fmt.Sprintf("available=%s locked=%s avg=%s",
			row.Available.String(), row.Locked.String(), row.AvgBuyPrice.String())
	}
	return snapshot
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

// assertLedgerSequencesMatch는 두 사용자의 전기 흐름이 같은지 본다. 계정 ID·분개
// ID는 세계마다 다르므로 (계정종류, 자산, 금액)만 남기고 분개 순서대로 비교한다.
func assertLedgerSequencesMatch(t *testing.T, db *gorm.DB, leftUserID uint, rightUserID uint) {
	t.Helper()

	require.Equal(t,
		userPostingSequence(t, db, rightUserID),
		userPostingSequence(t, db, leftUserID),
		"전기 흐름이 user %d와 %d에서 다르다", leftUserID, rightUserID)
}

func userPostingSequence(t *testing.T, db *gorm.DB, userID uint) []string {
	t.Helper()

	var rows []struct {
		JournalID   uint
		AccountType string
		Asset       string
		Amount      decimal.Decimal
	}
	require.NoError(t, db.Raw(`
		SELECT p.journal_id, a.account_type, p.asset, p.amount
		FROM postings p
		JOIN accounts a ON a.id = p.account_id
		WHERE a.owner_user_id = ?
		ORDER BY p.journal_id ASC, a.account_type ASC, p.asset ASC, p.amount ASC`, userID).Scan(&rows).Error)

	sequence := make([]string, 0, len(rows))
	for _, row := range rows {
		sequence = append(sequence, fmt.Sprintf("%s|%s|%s", row.AccountType, row.Asset, row.Amount.String()))
	}
	return sequence
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
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db))

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
		assertWalletsMatch(t, db, batchFixture.userIDs[idx], seqFixture.userIDs[idx])
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

// captureWallets는 사용자별 자산 스냅샷을 찍는다. 롤백 검증에서 "아무것도 변하지
// 않았다"를 보려면 전후를 같은 방식으로 굳혀 비교해야 한다.
func captureWallets(t *testing.T, db *gorm.DB, userIDs []uint) map[uint]map[string]string {
	t.Helper()

	result := make(map[uint]map[string]string, len(userIDs))
	for _, id := range userIDs {
		result[id] = userAssetSnapshot(t, db, id)
	}
	return result
}

func assertWalletSnapshotsEqual(t *testing.T, before map[uint]map[string]string, after map[uint]map[string]string) {
	t.Helper()

	require.Equal(t, before, after, "자산 스냅샷이 변했다")
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
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db))

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

	beforeWallets := captureWallets(t, db, userIDs)
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

	afterWallets := captureWallets(t, db, userIDs)
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
	require.NoError(t, db.Raw(`
		SELECT COUNT(*) FROM postings p
		JOIN accounts a ON a.id = p.account_id
		WHERE a.owner_user_id IN ?`, userIDs).Scan(&count).Error)
	return count
}

// 배치에 기정산 1건 + 신규 2건 혼재 → 기정산은 Duplicate, 신규만 적용, outbox는
// 3건 모두 마킹된다.
func TestIntegrationSettleTradeBatchSkipsAlreadySettledTrades(t *testing.T) {
	db := openServiceIntegrationDB(t)
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db))

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

	beforeWallets := captureWallets(t, db, []uint{pairs[0].buyerID, pairs[0].sellerID})

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

	afterWallets := captureWallets(t, db, []uint{pairs[0].buyerID, pairs[0].sellerID})
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
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db))

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

	_, buyerLocked := ledgerBalances(t, db, goodPairs[0].buyerID, model.KRWAssetSymbol)
	assert.True(t, buyerLocked.Equal(decimal.NewFromInt(100_000)))

	for _, id := range []uint64{goodOutbox.ID, badOutbox.ID} {
		var row model.TradeOutboxEvent
		require.NoError(t, db.First(&row, id).Error)
		assert.Equal(t, model.TradeOutboxStatusPending, row.Status, "정산 롤백 시 outbox 마킹도 롤백돼 PENDING으로 남아야 한다")
		assert.Nil(t, row.ProcessedAt)
	}

	// goodTrade는 2단계 배치 INSERT에서 RETURNING id로 ID를 부여받지만, 그 INSERT는
	// badTrade 때문에 이후 롤백된다. 호출자가 들고 있는 포인터(goodTrade)에 phantom
	// ID가 남아있으면 안 된다 — cmd/main.go의 폴백 경로가 이 포인터를 그대로 재사용해
	// SettleTrade를 호출하기 때문이다.
	assert.Equal(t, uint(0), goodTrade.ID, "롤백된 배치의 trade.ID는 0으로 리셋되어야 한다")
	assert.Equal(t, uint(0), badTrade.ID, "롤백된 배치의 trade.ID는 0으로 리셋되어야 한다")

	// 실제 폴백 경로 재현: 같은 포인터로 SettleTrade를 호출해도 정상적으로 신규
	// insert·정산되어야 한다. phantom ID가 남아있으면 GORM이 명시적 id로 INSERT를
	// 시도하게 되어, 이 계약이 문서화도 테스트도 되지 않은 채 방치된다.
	fallbackResult, err := settlementService.SettleTrade(goodTrade, uint64(goodOutbox.ID))
	require.NoError(t, err, "롤백 후 단건 폴백은 성공해야 한다")
	assert.True(t, fallbackResult.Applied)
	assert.NotZero(t, goodTrade.ID, "폴백 정산 후에는 실제 auto-generated ID가 채워져야 한다")

	var settledGoodBuy model.Order
	require.NoError(t, db.First(&settledGoodBuy, goodTrade.BuyOrderID).Error)
	assert.Equal(t, model.OrderStatusFilled, settledGoodBuy.Status)
	assert.True(t, settledGoodBuy.FilledAmount.Equal(decimal.NewFromInt(5)))
}

// (d) outbox 흡수: 성공 배치 → 모든 outbox 행이 같은 트랜잭션에서 PROCESSED된다.
func TestIntegrationSettleTradeBatchMarksAllOutboxRowsProcessed(t *testing.T) {
	db := openServiceIntegrationDB(t)
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db))

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
