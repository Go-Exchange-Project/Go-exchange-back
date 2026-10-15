package repository

import (
	"fmt"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// pendingOutboxFixture는 배치 인계 대상 outbox 행 1건을 PENDING으로 만든다.
func pendingOutboxFixture(t *testing.T, db *gorm.DB, coinSymbol string) model.TradeOutboxEvent {
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

func cleanupHandoffFailedSettlements(t *testing.T, db *gorm.DB, keyPrefix string) {
	t.Helper()
	t.Cleanup(func() {
		db.Unscoped().Where("trade_idempotency_key LIKE ?", keyPrefix+"%").Delete(&model.FailedSettlement{})
	})
}

func TestIntegrationRecordFailuresAndMarkOutboxProcessedCommitsBothAtomically(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	keyPrefix := fmt.Sprintf("repo-handoff-commit-%d", time.Now().UnixNano())
	cleanupHandoffFailedSettlements(t, db, keyPrefix)

	items := make([]SettlementFailureHandoff, 3)
	outboxIDs := make([]uint64, 3)
	for i := range items {
		event := pendingOutboxFixture(t, db, "BTC")
		outboxIDs[i] = event.ID
		f := failedSettlementFixture(fmt.Sprintf("%s-%d", keyPrefix, i), fmt.Sprintf("[SQLSTATE 57014] handoff %d", i))
		items[i] = SettlementFailureHandoff{Failure: &f, OutboxID: event.ID}
	}

	repo := NewFailedSettlementRepository(db)
	err := repo.RecordFailuresAndMarkOutboxProcessed(items)
	require.NoError(t, err)

	var failureCount int64
	require.NoError(t, db.Model(&model.FailedSettlement{}).Where("trade_idempotency_key LIKE ?", keyPrefix+"%").Count(&failureCount).Error)
	assert.EqualValues(t, 3, failureCount)

	for i, id := range outboxIDs {
		var event model.TradeOutboxEvent
		require.NoError(t, db.First(&event, id).Error)
		assert.Equal(t, model.TradeOutboxStatusProcessed, event.Status, "outbox %d는 PROCESSED여야 한다", i)
		assert.NotNil(t, event.ProcessedAt)
	}

	var failures []model.FailedSettlement
	require.NoError(t, db.Where("trade_idempotency_key LIKE ?", keyPrefix+"%").Order("trade_idempotency_key").Find(&failures).Error)
	for i, f := range failures {
		assert.Equal(t, model.FailedSettlementStatusOpen, f.Status)
		assert.Equal(t, uint(1), f.RetryCount)
		assert.Contains(t, f.ErrorMessage, "[SQLSTATE 57014]")
		_ = i
	}
}

// conflict 시 갱신 범위는 기존 RecordFailure와 같다 — error_message·status=OPEN·
// retry_count+1·resolution/resolved_by/notes/resolved_at 초기화.
func TestIntegrationRecordFailuresAndMarkOutboxProcessedUpsertMatchesRecordFailure(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	keyPrefix := fmt.Sprintf("repo-handoff-upsert-%d", time.Now().UnixNano())
	cleanupHandoffFailedSettlements(t, db, keyPrefix)

	key := keyPrefix + "-1"
	existing := model.FailedSettlement{
		TradeIdempotencyKey: key,
		CoinSymbol:          "BTC",
		BuyOrderID:          10,
		SellOrderID:         20,
		Price:               decimal.NewFromInt(90),
		Quantity:            decimal.NewFromInt(5),
		ErrorMessage:        "previous error",
		Status:              model.FailedSettlementStatusResolved,
		Resolution:          "resolved once",
		ResolvedBy:          "ops",
		Notes:               "handled",
		ResolvedAt:          ptrTime(time.Now().UTC()),
		RetryCount:          1,
		OccurredAt:          time.Now().UTC(),
	}
	require.NoError(t, db.Create(&existing).Error)

	event := pendingOutboxFixture(t, db, "BTC")
	f := failedSettlementFixture(key, "[SQLSTATE 57014] handoff retry")
	repo := NewFailedSettlementRepository(db)
	require.NoError(t, repo.RecordFailuresAndMarkOutboxProcessed([]SettlementFailureHandoff{{Failure: &f, OutboxID: event.ID}}))

	var persisted model.FailedSettlement
	require.NoError(t, db.Where("trade_idempotency_key = ?", key).First(&persisted).Error)
	assert.Equal(t, model.FailedSettlementStatusOpen, persisted.Status)
	assert.Equal(t, uint(2), persisted.RetryCount, "retry_count가 +1 돼야 한다")
	assert.Equal(t, "[SQLSTATE 57014] handoff retry", persisted.ErrorMessage)
	assert.Equal(t, "", persisted.Resolution)
	assert.Equal(t, "", persisted.ResolvedBy)
	assert.Equal(t, "", persisted.Notes)
	assert.Nil(t, persisted.ResolvedAt)
}

func TestIntegrationRecordFailuresAndMarkOutboxProcessedRejectsEmptyInput(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	err := NewFailedSettlementRepository(db).RecordFailuresAndMarkOutboxProcessed(nil)
	require.Error(t, err)
}

func TestIntegrationRecordFailuresAndMarkOutboxProcessedRejectsNilFailure(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	event := pendingOutboxFixture(t, db, "BTC")

	err := NewFailedSettlementRepository(db).RecordFailuresAndMarkOutboxProcessed(
		[]SettlementFailureHandoff{{Failure: nil, OutboxID: event.ID}},
	)
	require.Error(t, err)
}

func TestIntegrationRecordFailuresAndMarkOutboxProcessedRejectsZeroOutboxID(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	keyPrefix := fmt.Sprintf("repo-handoff-zero-outbox-%d", time.Now().UnixNano())
	cleanupHandoffFailedSettlements(t, db, keyPrefix)

	f := failedSettlementFixture(keyPrefix, "zero outbox id")
	err := NewFailedSettlementRepository(db).RecordFailuresAndMarkOutboxProcessed(
		[]SettlementFailureHandoff{{Failure: &f, OutboxID: 0}},
	)
	require.Error(t, err)
}

func TestIntegrationRecordFailuresAndMarkOutboxProcessedRejectsDuplicateOutboxID(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	keyPrefix := fmt.Sprintf("repo-handoff-dup-outbox-%d", time.Now().UnixNano())
	cleanupHandoffFailedSettlements(t, db, keyPrefix)

	event := pendingOutboxFixture(t, db, "BTC")
	f1 := failedSettlementFixture(keyPrefix+"-1", "dup outbox 1")
	f2 := failedSettlementFixture(keyPrefix+"-2", "dup outbox 2")

	err := NewFailedSettlementRepository(db).RecordFailuresAndMarkOutboxProcessed([]SettlementFailureHandoff{
		{Failure: &f1, OutboxID: event.ID},
		{Failure: &f2, OutboxID: event.ID},
	})
	require.Error(t, err)

	var count int64
	require.NoError(t, db.Model(&model.FailedSettlement{}).Where("trade_idempotency_key LIKE ?", keyPrefix+"%").Count(&count).Error)
	assert.Zero(t, count, "거부되면 아무것도 남지 않아야 한다")
}

func TestIntegrationRecordFailuresAndMarkOutboxProcessedRejectsDuplicateIdempotencyKey(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	keyPrefix := fmt.Sprintf("repo-handoff-dup-key-%d", time.Now().UnixNano())
	cleanupHandoffFailedSettlements(t, db, keyPrefix)

	event1 := pendingOutboxFixture(t, db, "BTC")
	event2 := pendingOutboxFixture(t, db, "BTC")
	f1 := failedSettlementFixture(keyPrefix, "dup key 1")
	f2 := failedSettlementFixture(keyPrefix, "dup key 2")

	err := NewFailedSettlementRepository(db).RecordFailuresAndMarkOutboxProcessed([]SettlementFailureHandoff{
		{Failure: &f1, OutboxID: event1.ID},
		{Failure: &f2, OutboxID: event2.ID},
	})
	require.Error(t, err)

	var event model.TradeOutboxEvent
	require.NoError(t, db.First(&event, event1.ID).Error)
	assert.Equal(t, model.TradeOutboxStatusPending, event.Status, "거부되면 outbox도 PENDING 그대로여야 한다")
}

// 행 수 불일치(outbox 중 하나가 이미 PROCESSED)면 트랜잭션 전체가 rollback된다 —
// 이 트랜잭션의 생성·갱신이 하나도 반영되지 않는다.
func TestIntegrationRecordFailuresAndMarkOutboxProcessedRollsBackOnRowCountMismatch(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	keyPrefix := fmt.Sprintf("repo-handoff-mismatch-%d", time.Now().UnixNano())
	cleanupHandoffFailedSettlements(t, db, keyPrefix)

	okEvent := pendingOutboxFixture(t, db, "BTC")
	alreadyProcessedEvent := pendingOutboxFixture(t, db, "BTC")
	require.NoError(t, db.Model(&model.TradeOutboxEvent{}).Where("id = ?", alreadyProcessedEvent.ID).
		Updates(map[string]any{"status": model.TradeOutboxStatusProcessed, "processed_at": time.Now().UTC()}).Error)

	f1 := failedSettlementFixture(keyPrefix+"-1", "mismatch 1")
	f2 := failedSettlementFixture(keyPrefix+"-2", "mismatch 2")

	err := NewFailedSettlementRepository(db).RecordFailuresAndMarkOutboxProcessed([]SettlementFailureHandoff{
		{Failure: &f1, OutboxID: okEvent.ID},
		{Failure: &f2, OutboxID: alreadyProcessedEvent.ID},
	})
	require.Error(t, err)

	var count int64
	require.NoError(t, db.Model(&model.FailedSettlement{}).Where("trade_idempotency_key LIKE ?", keyPrefix+"%").Count(&count).Error)
	assert.Zero(t, count, "rollback되면 failure가 하나도 남지 않아야 한다")

	var okEventAfter model.TradeOutboxEvent
	require.NoError(t, db.First(&okEventAfter, okEvent.ID).Error)
	assert.Equal(t, model.TradeOutboxStatusPending, okEventAfter.Status, "rollback되면 다른 outbox도 PENDING 그대로여야 한다")
}
