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

func seedPendingOutboxRow(t *testing.T, db *gorm.DB, engineEventID string) *model.TradeOutboxEvent {
	t.Helper()
	row := &model.TradeOutboxEvent{
		EventType:     model.TradeOutboxEventTypeTrade,
		CoinSymbol:    "BTC",
		EngineEventID: engineEventID,
		Payload:       []byte("{}"),
		Status:        model.TradeOutboxStatusPending,
	}
	require.NoError(t, db.Create(row).Error)
	return row
}

// 정산 성공 경로: outboxEventID를 주면 정산과 outbox PROCESSED 마킹이 한 트랜잭션에
// 함께 커밋돼야 한다(별도 MarkProcessed 왕복 제거의 핵심).
func TestIntegrationSettleTradeAbsorbsOutboxMarkInSameTransaction(t *testing.T) {
	db := openServiceIntegrationDB(t)
	buyerID := serviceTestUserID(70)
	sellerID := serviceTestUserID(71)
	defer cleanupServiceUsers(t, db, buyerID, sellerID)

	buyOrder, sellOrder := seedSettlementRows(t, db, buyerID, sellerID, decimal.NewFromInt(100_000), decimal.NewFromInt(5))
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db), repository.NewWalletRepository(db))

	trade := &model.Trade{
		EngineSequence: 1,
		EngineEventID:  fmt.Sprintf("outbox-absorb-%d", time.Now().UnixNano()),
		CoinSymbol:     "BTC",
		Price:          decimal.NewFromInt(100),
		Quantity:       decimal.NewFromInt(5),
		TradedAt:       time.Now().UTC(),
		BuyOrderID:     buyOrder.ID,
		SellOrderID:    sellOrder.ID,
	}
	outboxRow := seedPendingOutboxRow(t, db, trade.EngineEventID)
	defer cleanupOutboxRows(t, db, outboxRow.ID)

	result, err := settlementService.SettleTrade(trade, uint64(outboxRow.ID))
	require.NoError(t, err)
	require.True(t, result.Applied)

	var persisted model.TradeOutboxEvent
	require.NoError(t, db.First(&persisted, outboxRow.ID).Error)
	assert.Equal(t, model.TradeOutboxStatusProcessed, persisted.Status, "정산과 같은 트랜잭션에서 outbox가 PROCESSED로 마킹돼야 한다")
	require.NotNil(t, persisted.ProcessedAt)

	// 정산도 실제로 커밋됐는지 교차 확인
	var tradeCount int64
	require.NoError(t, db.Model(&model.Trade{}).Where("idempotency_key = ?", trade.IdempotencyKey).Count(&tradeCount).Error)
	assert.Equal(t, int64(1), tradeCount)
}

// 정산 실패 경로: 트랜잭션이 롤백되면 outbox 마킹도 함께 롤백돼 PENDING으로 남아야
// 한다. 흡수가 원자적임을 증명한다 — 정산이 안 됐는데 outbox만 PROCESSED가 되면
// 그 이벤트가 영영 재처리되지 않아 정합성이 깨진다.
func TestIntegrationSettleTradeFailureLeavesOutboxPending(t *testing.T) {
	db := openServiceIntegrationDB(t)
	buyerID := serviceTestUserID(72)
	sellerID := serviceTestUserID(73)
	defer cleanupServiceUsers(t, db, buyerID, sellerID)

	// 매수 주문을 취소 상태로 심어 정산이 validateOrderStatusForSettlement에서 실패하게 한다.
	buyOrder, sellOrder := seedSettlementRowsWithStatuses(t, db, buyerID, sellerID,
		decimal.NewFromInt(100_000), decimal.NewFromInt(5), decimal.NewFromInt(5),
		model.OrderStatusCancelled, model.OrderStatusPending)
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db), repository.NewWalletRepository(db))

	trade := &model.Trade{
		EngineSequence: 1,
		EngineEventID:  fmt.Sprintf("outbox-rollback-%d", time.Now().UnixNano()),
		CoinSymbol:     "BTC",
		Price:          decimal.NewFromInt(100),
		Quantity:       decimal.NewFromInt(5),
		TradedAt:       time.Now().UTC(),
		BuyOrderID:     buyOrder.ID,
		SellOrderID:    sellOrder.ID,
	}
	outboxRow := seedPendingOutboxRow(t, db, trade.EngineEventID)
	defer cleanupOutboxRows(t, db, outboxRow.ID)

	result, err := settlementService.SettleTrade(trade, uint64(outboxRow.ID))
	require.Error(t, err, "취소된 주문 정산은 실패해야 한다")
	assert.False(t, result.Applied)

	var persisted model.TradeOutboxEvent
	require.NoError(t, db.First(&persisted, outboxRow.ID).Error)
	assert.Equal(t, model.TradeOutboxStatusPending, persisted.Status, "정산 롤백 시 outbox 마킹도 롤백돼 PENDING으로 남아야 한다")
	assert.Nil(t, persisted.ProcessedAt)

	// 정산이 롤백됐으므로 trade도 없어야 한다
	var tradeCount int64
	require.NoError(t, db.Model(&model.Trade{}).Where("idempotency_key = ?", trade.IdempotencyKey).Count(&tradeCount).Error)
	assert.Equal(t, int64(0), tradeCount)
}
