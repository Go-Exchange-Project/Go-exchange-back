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

// InsertBatchAndMarkCancelCommands는 execution outbox INSERT와 취소 command의
// PROCESSED 전환을 한 커밋에 묶는다. 두 상태가 갈라지면 크래시 복구 표에 어느
// 쪽으로도 덮이지 않는 구간이 생긴다.
func TestIntegrationTradeOutboxAndCancelCommandCommitTogether(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	outboxRepo := NewTradeOutboxRepository(db)
	commandRepo := NewCancelCommandRepository(db)
	symbol := fmt.Sprintf("OBX%d", time.Now().UnixNano())

	command, _, err := commandRepo.CreateOrGet(seedCancelCommand(uniqueCancelOrderID()))
	require.NoError(t, err)
	defer cleanupCancelCommands(t, db, []uint64{command.ID})

	trade := seedOutboxEvent(symbol)
	cancelled := &model.TradeOutboxEvent{
		EventType:  model.TradeOutboxEventTypeOrderCancelled,
		CoinSymbol: symbol,
		Payload:    []byte(`{"CoinSymbol":"` + symbol + `"}`),
		Status:     model.TradeOutboxStatusPending,
	}

	require.NoError(t, outboxRepo.InsertBatchAndMarkCancelCommands(
		[]*model.TradeOutboxEvent{trade, cancelled}, []uint64{command.ID}))
	defer cleanupOutboxEvents(t, db, []uint64{trade.ID, cancelled.ID})

	assert.NotZero(t, trade.ID)
	assert.NotZero(t, cancelled.ID)

	var got model.CancelCommand
	require.NoError(t, db.Where("id = ?", command.ID).First(&got).Error)
	assert.Equal(t, model.CancelCommandStatusProcessed, got.Status)
}

// 한 배치에 같은 command의 이벤트가 두 번 들어올 수 있다. dedup하지 않으면
// RowsAffected(1)와 len(ids)(2)가 어긋나 정상 배치가 rollback된다.
func TestIntegrationTradeOutboxDedupesCancelCommandIDs(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	outboxRepo := NewTradeOutboxRepository(db)
	commandRepo := NewCancelCommandRepository(db)
	symbol := fmt.Sprintf("OBX%d", time.Now().UnixNano())

	command, _, err := commandRepo.CreateOrGet(seedCancelCommand(uniqueCancelOrderID()))
	require.NoError(t, err)
	defer cleanupCancelCommands(t, db, []uint64{command.ID})

	event := seedOutboxEvent(symbol)
	require.NoError(t, outboxRepo.InsertBatchAndMarkCancelCommands(
		[]*model.TradeOutboxEvent{event}, []uint64{command.ID, command.ID, 0}))
	defer cleanupOutboxEvents(t, db, []uint64{event.ID})

	var got model.CancelCommand
	require.NoError(t, db.Where("id = ?", command.ID).First(&got).Error)
	assert.Equal(t, model.CancelCommandStatusProcessed, got.Status)
}

// 행 수가 어긋난다는 것은 "이미 처리됐다" 또는 "command가 없다"이다. 둘 다 조용히
// 넘기면 안 되는 상태이므로 outbox INSERT까지 함께 rollback한다.
func TestIntegrationTradeOutboxRollsBackOnCancelCommandMismatch(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	outboxRepo := NewTradeOutboxRepository(db)
	commandRepo := NewCancelCommandRepository(db)
	symbol := fmt.Sprintf("OBX%d", time.Now().UnixNano())

	command, _, err := commandRepo.CreateOrGet(seedCancelCommand(uniqueCancelOrderID()))
	require.NoError(t, err)
	defer cleanupCancelCommands(t, db, []uint64{command.ID})

	missingID := command.ID + 90_000_000
	event := seedOutboxEvent(symbol)

	err = outboxRepo.InsertBatchAndMarkCancelCommands(
		[]*model.TradeOutboxEvent{event}, []uint64{command.ID, missingID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected 2")

	var outboxCount int64
	require.NoError(t, db.Model(&model.TradeOutboxEvent{}).
		Where("coin_symbol = ?", symbol).Count(&outboxCount).Error)
	assert.Zero(t, outboxCount, "command UPDATE가 실패했는데 outbox 행이 남았다")

	var got model.CancelCommand
	require.NoError(t, db.Where("id = ?", command.ID).First(&got).Error)
	assert.Equal(t, model.CancelCommandStatusPending, got.Status,
		"rollback됐는데 command만 PROCESSED로 남았다")
}

// 이미 PROCESSED인 command를 다시 마킹하려 하면 WHERE status='PENDING'에 걸려
// 0행이 되고 배치 전체가 rollback된다 — worker가 outbox commit 전에 같은 command를
// 재투입하면 실제로 이 경로가 열린다.
func TestIntegrationTradeOutboxRejectsAlreadyProcessedCommand(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	outboxRepo := NewTradeOutboxRepository(db)
	commandRepo := NewCancelCommandRepository(db)
	symbol := fmt.Sprintf("OBX%d", time.Now().UnixNano())

	command, _, err := commandRepo.CreateOrGet(seedCancelCommand(uniqueCancelOrderID()))
	require.NoError(t, err)
	defer cleanupCancelCommands(t, db, []uint64{command.ID})

	require.NoError(t, db.Model(&model.CancelCommand{}).Where("id = ?", command.ID).
		Update("status", model.CancelCommandStatusProcessed).Error)

	event := seedOutboxEvent(symbol)
	err = outboxRepo.InsertBatchAndMarkCancelCommands(
		[]*model.TradeOutboxEvent{event}, []uint64{command.ID})
	require.Error(t, err)

	var outboxCount int64
	require.NoError(t, db.Model(&model.TradeOutboxEvent{}).
		Where("coin_symbol = ?", symbol).Count(&outboxCount).Error)
	assert.Zero(t, outboxCount)
}

// command ID가 없는 배치는 기존 InsertBatch와 완전히 같아야 한다. UPDATE를 조건
// 없이 실행하면 취소가 없는 대다수 배치에 불필요한 문장이 붙는다.
func TestIntegrationTradeOutboxWithoutCancelCommandsBehavesLikeInsertBatch(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	outboxRepo := NewTradeOutboxRepository(db)
	symbol := fmt.Sprintf("OBX%d", time.Now().UnixNano())

	events := []*model.TradeOutboxEvent{seedOutboxEvent(symbol), seedOutboxEvent(symbol)}
	require.NoError(t, outboxRepo.InsertBatchAndMarkCancelCommands(events, nil))
	defer cleanupOutboxEvents(t, db, []uint64{events[0].ID, events[1].ID})

	assert.NotZero(t, events[0].ID)
	assert.Greater(t, events[1].ID, events[0].ID)
}
