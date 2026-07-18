package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 크래시 시나리오 시뮬레이션: 체결이 outbox에 커밋된 직후 프로세스가 죽었다
// (정산 미실행, PENDING 잔존). 재부팅 리플레이가 실제 정산을 완결하고,
// 같은 이벤트를 다시 리플레이해도 멱등성 키가 이중 정산을 막아야 한다.
func TestIntegrationOutboxReplaySettlesPendingTradeExactlyOnce(t *testing.T) {
	db := openServiceIntegrationDB(t)
	buyerID := serviceTestUserID(95)
	sellerID := serviceTestUserID(96)
	defer cleanupServiceUsers(t, db, buyerID, sellerID)

	buyOrder, sellOrder := seedSettlementRows(t, db, buyerID, sellerID,
		decimal.NewFromInt(100_000), decimal.NewFromInt(5))

	engineEventID := fmt.Sprintf("outbox-replay-test-%d", time.Now().UnixNano())
	trade := &model.Trade{
		EngineSequence: 1,
		EngineEventID:  engineEventID,
		CoinSymbol:     "BTC",
		Price:          decimal.NewFromInt(100),
		Quantity:       decimal.NewFromInt(5),
		TradedAt:       time.Now().UTC(),
		BuyOrderID:     buyOrder.ID,
		SellOrderID:    sellOrder.ID,
	}
	row, err := NewTradeOutboxEvent(matching.ExecutionEvent{Trade: trade})
	require.NoError(t, err)
	require.NoError(t, db.Create(row).Error)
	defer cleanupOutboxRows(t, db, row.ID)

	settlementService := NewSettlementService(db, repository.NewOrderRepository(db), repository.NewWalletRepository(db))
	outboxRepo := repository.NewTradeOutboxRepository(db)
	replayer := &OutboxReplayer{
		Repo: outboxRepo,
		Process: func(event matching.ExecutionEvent) bool {
			_, err := settlementService.SettleTrade(event.Trade, 0)
			return err == nil
		},
		Logger: discardServiceLogger(),
	}

	result, err := replayer.Replay()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, result.Replayed, 1, "주입한 PENDING 이벤트가 리플레이돼야 한다")

	var replayedRow model.TradeOutboxEvent
	require.NoError(t, db.First(&replayedRow, row.ID).Error)
	assert.Equal(t, model.TradeOutboxStatusProcessed, replayedRow.Status)

	idempotencyKey := "engine:" + engineEventID
	var tradeCount int64
	require.NoError(t, db.Model(&model.Trade{}).Where("idempotency_key = ?", idempotencyKey).Count(&tradeCount).Error)
	assert.Equal(t, int64(1), tradeCount, "리플레이로 정산(체결 기록)이 완결돼야 한다")

	var buyerBTC model.Wallet
	require.NoError(t, db.Where("user_id = ? AND coin_symbol = ?", buyerID, "BTC").First(&buyerBTC).Error)
	assert.True(t, buyerBTC.AvailableBalance.Equal(decimal.NewFromInt(5)),
		"매수자가 코인을 받아야 한다 (got %s)", buyerBTC.AvailableBalance)

	// 같은 이벤트가 outbox에 한 번 더 남은 극단 케이스(마킹 실패 후 재부팅 등):
	// 리플레이는 멱등이어야 한다.
	duplicate, err := NewTradeOutboxEvent(matching.ExecutionEvent{Trade: trade})
	require.NoError(t, err)
	require.NoError(t, db.Create(duplicate).Error)
	defer cleanupOutboxRows(t, db, duplicate.ID)

	result, err = replayer.Replay()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, result.Replayed, 1)

	require.NoError(t, db.Model(&model.Trade{}).Where("idempotency_key = ?", idempotencyKey).Count(&tradeCount).Error)
	assert.Equal(t, int64(1), tradeCount, "중복 리플레이는 이중 정산을 만들면 안 된다")
	require.NoError(t, db.Where("user_id = ? AND coin_symbol = ?", buyerID, "BTC").First(&buyerBTC).Error)
	assert.True(t, buyerBTC.AvailableBalance.Equal(decimal.NewFromInt(5)), "잔고도 그대로여야 한다")
}

// 크래시 시나리오 시뮬레이션(A-4 대칭): 부분 체결 하나(4주)와 그 잔여분에 대한
// OrderCancelled가 둘 다 outbox에 커밋된 직후 프로세스가 죽었다(정산·취소 확정
// 미실행, 둘 다 PENDING 잔존). 재부팅 리플레이가 outbox ID 순서대로(trade 먼저,
// cancel 나중) 처리해 Task 1E의 라이브 경로 순서 보장을 그대로 재현하고,
// 최종적으로 주문을 CANCELLED로 완결해야 한다. 같은 취소 이벤트가 중복으로
// 남아 있어도(마킹 실패 후 재부팅 등) 리플레이는 hold를 이중 해제하면 안 된다.
func TestIntegrationOutboxReplayFinishesPendingCancelExactlyOnce(t *testing.T) {
	db := openServiceIntegrationDB(t)
	buyerID := serviceTestUserID(100)
	sellerID := serviceTestUserID(101)
	defer cleanupServiceUsers(t, db, buyerID, sellerID)

	// 매수 10주(가격 100, 수수료 포함 hold 1000.5) 대기 중, 매도자는 10주 보유.
	buyOrder, sellOrder := seedSettlementRowsWithOrderAmount(t, db, buyerID, sellerID,
		decimal.RequireFromString("1000.5"), decimal.NewFromInt(10), decimal.NewFromInt(10))

	engineEventID := fmt.Sprintf("outbox-replay-cancel-test-trade-%d", time.Now().UnixNano())
	trade := &model.Trade{
		EngineSequence: 1,
		EngineEventID:  engineEventID,
		CoinSymbol:     "BTC",
		Price:          decimal.NewFromInt(100),
		Quantity:       decimal.NewFromInt(4),
		TradedAt:       time.Now().UTC(),
		BuyOrderID:     buyOrder.ID,
		SellOrderID:    sellOrder.ID,
	}
	tradeRow, err := NewTradeOutboxEvent(matching.ExecutionEvent{Trade: trade})
	require.NoError(t, err)
	require.NoError(t, db.Create(tradeRow).Error)
	defer cleanupOutboxRows(t, db, tradeRow.ID)

	cancelEngineEventID := fmt.Sprintf("outbox-replay-cancel-test-cancel-%d", time.Now().UnixNano())
	cancelEvent := matching.OrderCancelled{
		OrderID:       buyOrder.ID,
		CoinSymbol:    buyOrder.CoinSymbol,
		Side:          buyOrder.Side,
		EngineEventID: cancelEngineEventID,
	}
	cancelRow, err := NewTradeOutboxEvent(matching.ExecutionEvent{OrderCancelled: &cancelEvent})
	require.NoError(t, err)
	require.NoError(t, db.Create(cancelRow).Error)
	defer cleanupOutboxRows(t, db, cancelRow.ID)
	require.Less(t, tradeRow.ID, cancelRow.ID, "trade가 cancel보다 먼저 outbox에 커밋된 순서를 전제로 한 테스트")

	settlementService := NewSettlementService(db, repository.NewOrderRepository(db), repository.NewWalletRepository(db))
	orderService := newIntegrationOrderService(db, nil)
	outboxRepo := repository.NewTradeOutboxRepository(db)
	replayer := &OutboxReplayer{
		Repo: outboxRepo,
		Process: func(event matching.ExecutionEvent) bool {
			switch {
			case event.Trade != nil:
				_, err := settlementService.SettleTrade(event.Trade, 0)
				return err == nil
			case event.OrderCancelled != nil:
				return orderService.ProcessOrderCancellation(*event.OrderCancelled) == nil
			default:
				return false
			}
		},
		Logger: discardServiceLogger(),
	}

	result, err := replayer.Replay()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, result.Replayed, 2, "trade와 cancel 두 PENDING 이벤트가 모두 리플레이돼야 한다")

	var persisted model.Order
	require.NoError(t, db.First(&persisted, buyOrder.ID).Error)
	assert.Equal(t, model.OrderStatusCancelled, persisted.Status, "리플레이가 취소를 CANCELLED까지 완결해야 한다")
	assert.True(t, persisted.FilledAmount.Equal(decimal.NewFromInt(4)),
		"선행 체결(4주)이 outbox ID 순서대로 먼저 정산된 뒤 취소가 처리돼야 한다 (filled=%s)", persisted.FilledAmount.String())

	var rows []model.TradeOutboxEvent
	require.NoError(t, db.Where("id IN ?", []uint64{tradeRow.ID, cancelRow.ID}).Find(&rows).Error)
	require.Len(t, rows, 2)
	for _, row := range rows {
		assert.Equal(t, model.TradeOutboxStatusProcessed, row.Status)
	}

	entries := requireLedgerEntries(t, db, buyerID, model.LedgerEntryTypeOrderRelease, model.LedgerReferenceTypeOrder, buyOrder.ID)
	require.Len(t, entries, 1, "잔여 hold 해제가 원장에 정확히 한 번 남아야 한다")

	walletRepo := repository.NewWalletRepository(db)
	afterFirstReplay, err := walletRepo.FindKRWWalletByUserID(buyerID)
	require.NoError(t, err)
	// hold(10주, 1000.5) 중 체결된 4주 몫(400.2, 정산이 소진)과 잔여 6주 몫(600.3,
	// 취소가 해제)이 정확히 전액을 커버해 locked가 0으로 떨어져야 한다.
	assert.True(t, afterFirstReplay.LockedBalance.IsZero(), "locked=%s", afterFirstReplay.LockedBalance.String())

	// 같은 취소 이벤트가 outbox에 한 번 더 남은 극단 케이스(마킹 실패 후 재부팅 등):
	// 리플레이는 멱등이어야 한다 — hold 이중 해제 없음.
	duplicateCancelRow, err := NewTradeOutboxEvent(matching.ExecutionEvent{OrderCancelled: &cancelEvent})
	require.NoError(t, err)
	require.NoError(t, db.Create(duplicateCancelRow).Error)
	defer cleanupOutboxRows(t, db, duplicateCancelRow.ID)

	result, err = replayer.Replay()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, result.Replayed, 1)

	require.NoError(t, db.First(&persisted, buyOrder.ID).Error)
	assert.Equal(t, model.OrderStatusCancelled, persisted.Status)

	entries = requireLedgerEntries(t, db, buyerID, model.LedgerEntryTypeOrderRelease, model.LedgerReferenceTypeOrder, buyOrder.ID)
	require.Len(t, entries, 1, "중복 리플레이는 hold를 이중 해제하면 안 된다")

	afterDuplicateReplay, err := walletRepo.FindKRWWalletByUserID(buyerID)
	require.NoError(t, err)
	assert.True(t, afterDuplicateReplay.AvailableBalance.Equal(afterFirstReplay.AvailableBalance), "중복 리플레이 후 잔고가 그대로여야 한다")
	assert.True(t, afterDuplicateReplay.LockedBalance.Equal(afterFirstReplay.LockedBalance), "중복 리플레이 후 hold가 그대로여야 한다")
}

// 크래시로 MarketOrderDone이 outbox에 남지 못한 시장가: 리플레이 후 파이널라이저가
// 잔여 hold를 해제하고 상태를 확정해야 한다 (영구 동결 방지).
func TestIntegrationStaleMarketOrderFinalizerReleasesHold(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(97)
	defer cleanupServiceUsers(t, db, userID)

	hold := decimal.NewFromInt(100_000)
	wallet := model.Wallet{
		UserID:           userID,
		CoinSymbol:       model.KRWAssetSymbol,
		KRW:              hold,
		AvailableBalance: decimal.Zero,
		LockedBalance:    hold,
	}
	require.NoError(t, db.Create(&wallet).Error)

	staleOrder := model.Order{
		UserID:       userID,
		CoinSymbol:   "BTC",
		Side:         model.OrderSideBuy,
		OrderType:    model.OrderTypeMarket,
		Status:       model.OrderStatusPending,
		Price:        decimal.Zero,
		Amount:       decimal.Zero,
		QuoteAmount:  hold,
		FilledAmount: decimal.Zero,
	}
	require.NoError(t, db.Create(&staleOrder).Error)

	orderRepo := repository.NewOrderRepository(db)
	finalizer := &StaleMarketOrderFinalizer{
		Orders:    &singleUserMarketOrderSource{repo: orderRepo, userID: userID},
		Completer: NewOrderService(orderRepo, repository.NewWalletRepository(db), nil),
		Logger:    discardServiceLogger(),
	}

	result, err := finalizer.FinalizeAll()
	require.NoError(t, err)
	assert.Equal(t, StaleMarketOrderFinalizeResult{Finalized: 1}, result)

	var finalized model.Order
	require.NoError(t, db.First(&finalized, staleOrder.ID).Error)
	assert.Equal(t, model.OrderStatusCancelled, finalized.Status, "체결 없이 죽은 시장가는 취소로 확정돼야 한다")

	var releasedWallet model.Wallet
	require.NoError(t, db.First(&releasedWallet, wallet.ID).Error)
	assert.True(t, releasedWallet.LockedBalance.IsZero(), "잔여 hold가 해제돼야 한다 (locked=%s)", releasedWallet.LockedBalance)
	assert.True(t, releasedWallet.AvailableBalance.Equal(hold))

	var releaseEntries int64
	require.NoError(t, db.Model(&model.LedgerEntry{}).
		Where("user_id = ? AND reference_id = ? AND entry_type = ?", userID, staleOrder.ID, model.LedgerEntryTypeOrderRelease).
		Count(&releaseEntries).Error)
	assert.Equal(t, int64(1), releaseEntries, "hold 해제가 원장에 남아야 한다")
}

// 공유 테스트 DB에서 다른 테스트의 시장가 주문을 건드리지 않도록
// 자기 유저의 주문만 반환하는 소스 래퍼.
type singleUserMarketOrderSource struct {
	repo   *repository.OrderRepository
	userID uint
}

func (s *singleUserMarketOrderSource) FindOpenMarketOrders() ([]model.Order, error) {
	orders, err := s.repo.FindOpenMarketOrders()
	if err != nil {
		return nil, err
	}
	var mine []model.Order
	for _, order := range orders {
		if order.UserID == s.userID {
			mine = append(mine, order)
		}
	}
	return mine, nil
}

func cleanupOutboxRows(t *testing.T, db *gorm.DB, ids ...uint64) {
	t.Helper()
	require.NoError(t, db.Where("id IN ?", ids).Delete(&model.TradeOutboxEvent{}).Error)
}
