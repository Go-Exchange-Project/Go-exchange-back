package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func statementTimeoutPgError() error {
	return &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}
}

func pendingOutboxEventFixture(t *testing.T, db *gorm.DB, coinSymbol string) model.TradeOutboxEvent {
	t.Helper()
	event := model.TradeOutboxEvent{
		EventType:  model.TradeOutboxEventTypeTrade,
		CoinSymbol: coinSymbol,
		Payload:    []byte(`{}`),
		Status:     model.TradeOutboxStatusPending,
	}
	require.NoError(t, db.Create(&event).Error)
	t.Cleanup(func() {
		db.Unscoped().Where("id = ?", event.ID).Delete(&model.TradeOutboxEvent{})
	})
	return event
}

// RecordBatchFailuresAndMarkOutboxProcessed는 (trade, outboxID) 쌍마다
// RecordFailure와 같은 방식으로 FailedSettlement를 만들어(SQLSTATE 태그 포함)
// repository의 원자적 배치 인계에 넘긴다.
func TestIntegrationRecordBatchFailuresAndMarkOutboxProcessedBuildsFailuresLikeRecordFailure(t *testing.T) {
	db := openServiceIntegrationDB(t)
	keyPrefix := fmt.Sprintf("svc-handoff-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		db.Unscoped().Where("trade_idempotency_key LIKE ?", keyPrefix+"%").Delete(&model.FailedSettlement{})
	})

	svc := NewFailedSettlementService(repository.NewFailedSettlementRepository(db))

	items := make([]TradeOutboxFailureItem, 3)
	trades := make([]*model.Trade, 3)
	for i := range items {
		event := pendingOutboxEventFixture(t, db, "BTC")
		trade := &model.Trade{
			EngineSequence: int64(i + 1),
			EngineEventID:  fmt.Sprintf("%s-engine-%d", keyPrefix, i),
			CoinSymbol:     "BTC",
			Price:          decimal.NewFromInt(90),
			Quantity:       decimal.NewFromInt(1),
			BuyOrderID:     uint(910000 + i),
			SellOrderID:    uint(920000 + i),
		}
		trades[i] = trade
		items[i] = TradeOutboxFailureItem{Trade: trade, OutboxID: event.ID}
	}

	settlementErr := statementTimeoutPgError()
	err := svc.RecordBatchFailuresAndMarkOutboxProcessed(items, settlementErr)
	require.NoError(t, err)

	for i, trade := range trades {
		require.NotEmpty(t, trade.IdempotencyKey, "trade %d의 IdempotencyKey가 채워져야 한다(RecordFailure와 동일)", i)

		var persisted model.FailedSettlement
		require.NoError(t, db.Where("trade_idempotency_key = ?", trade.IdempotencyKey).First(&persisted).Error)
		require.Equal(t, model.FailedSettlementStatusOpen, persisted.Status)
		require.Contains(t, persisted.ErrorMessage, "[SQLSTATE 57014]")

		category := ClassifyFailedSettlement(&persisted)
		require.Equal(t, FailedSettlementCategoryStatementTimeout, category)

		var event model.TradeOutboxEvent
		require.NoError(t, db.First(&event, items[i].OutboxID).Error)
		require.Equal(t, model.TradeOutboxStatusProcessed, event.Status)
	}
}

func TestIntegrationRecordBatchFailuresAndMarkOutboxProcessedRejectsEmptyItems(t *testing.T) {
	db := openServiceIntegrationDB(t)
	svc := NewFailedSettlementService(repository.NewFailedSettlementRepository(db))
	require.Error(t, svc.RecordBatchFailuresAndMarkOutboxProcessed(nil, statementTimeoutPgError()))
}
