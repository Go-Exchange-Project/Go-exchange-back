package repository

import (
	"fmt"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func seedOutboxEvent(symbol string) *model.TradeOutboxEvent {
	return &model.TradeOutboxEvent{
		EventType:  model.TradeOutboxEventTypeTrade,
		CoinSymbol: symbol,
		Payload:    []byte(`{"CoinSymbol":"` + symbol + `"}`),
		Status:     model.TradeOutboxStatusPending,
	}
}

func cleanupOutboxEvents(t *testing.T, db *gorm.DB, ids []uint64) {
	t.Helper()
	if len(ids) == 0 {
		return
	}
	require.NoError(t, db.Where("id IN ?", ids).Delete(&model.TradeOutboxEvent{}).Error)
}

func TestIntegrationTradeOutboxInsertBatchAssignsAscendingIDs(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewTradeOutboxRepository(db)
	symbol := fmt.Sprintf("OBX%d", time.Now().UnixNano())

	events := []*model.TradeOutboxEvent{seedOutboxEvent(symbol), seedOutboxEvent(symbol), seedOutboxEvent(symbol)}
	require.NoError(t, repo.InsertBatch(events))
	defer cleanupOutboxEvents(t, db, []uint64{events[0].ID, events[1].ID, events[2].ID})

	require.NotZero(t, events[0].ID)
	assert.Less(t, events[0].ID, events[1].ID, "삽입 순서 = ID 순서여야 리플레이가 엔진 방출 순서를 재현한다")
	assert.Less(t, events[1].ID, events[2].ID)

	require.NoError(t, repo.InsertBatch(nil), "빈 배치는 no-op이어야 한다")
}

func TestIntegrationTradeOutboxFindPendingAfterAndMarkProcessed(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewTradeOutboxRepository(db)
	symbol := fmt.Sprintf("OBX%d", time.Now().UnixNano())

	events := []*model.TradeOutboxEvent{seedOutboxEvent(symbol), seedOutboxEvent(symbol), seedOutboxEvent(symbol)}
	require.NoError(t, repo.InsertBatch(events))
	ids := []uint64{events[0].ID, events[1].ID, events[2].ID}
	defer cleanupOutboxEvents(t, db, ids)

	// 공유 DB이므로 자기 심볼 행만 추려 검증한다.
	filterMine := func(rows []model.TradeOutboxEvent) []uint64 {
		var mine []uint64
		for _, row := range rows {
			if row.CoinSymbol == symbol {
				mine = append(mine, row.ID)
			}
		}
		return mine
	}

	pending, err := repo.FindPendingAfter(0, 10000)
	require.NoError(t, err)
	assert.Equal(t, ids, filterMine(pending), "PENDING 3건이 ID 순으로 조회돼야 한다")

	require.NoError(t, repo.MarkProcessed(events[1].ID))

	pending, err = repo.FindPendingAfter(0, 10000)
	require.NoError(t, err)
	assert.Equal(t, []uint64{ids[0], ids[2]}, filterMine(pending), "PROCESSED 행은 리플레이 대상에서 빠져야 한다")

	var marked model.TradeOutboxEvent
	require.NoError(t, db.First(&marked, events[1].ID).Error)
	assert.Equal(t, model.TradeOutboxStatusProcessed, marked.Status)
	require.NotNil(t, marked.ProcessedAt)

	// keyset 커서: 첫 ID 이후만 조회하면 첫 행은 빠진다.
	pending, err = repo.FindPendingAfter(ids[0], 10000)
	require.NoError(t, err)
	assert.Equal(t, []uint64{ids[2]}, filterMine(pending))
}

func TestIntegrationTradeOutboxMarkProcessedUnknownIDFails(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewTradeOutboxRepository(db)

	err := repo.MarkProcessed(0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "affected no rows")
}

// migrations/004_order_cancelled_event.sql이 ck_trade_outbox_event_type CHECK를
// 넓혀야 이 삽입이 성공한다 — AutoMigrate만으로는 기존 CHECK가 갱신되지 않는다.
func TestIntegrationTradeOutboxAllowsOrderCancelledEventType(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewTradeOutboxRepository(db)
	symbol := fmt.Sprintf("OBX%d", time.Now().UnixNano())

	event := &model.TradeOutboxEvent{
		EventType:  model.TradeOutboxEventTypeOrderCancelled,
		CoinSymbol: symbol,
		Payload:    []byte(`{"CoinSymbol":"` + symbol + `"}`),
		Status:     model.TradeOutboxStatusPending,
	}
	require.NoError(t, repo.InsertBatch([]*model.TradeOutboxEvent{event}))
	defer cleanupOutboxEvents(t, db, []uint64{event.ID})

	require.NotZero(t, event.ID)
}
