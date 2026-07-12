package service

import (
	"errors"
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeOutboxReplaySource struct {
	rows          []model.TradeOutboxEvent
	findCalls     []uint64
	findErr       error
	markedIDs     []uint64
	markErrsForID map[uint64]error
}

func (f *fakeOutboxReplaySource) FindPendingAfter(afterID uint64, limit int) ([]model.TradeOutboxEvent, error) {
	f.findCalls = append(f.findCalls, afterID)
	if f.findErr != nil {
		return nil, f.findErr
	}
	var page []model.TradeOutboxEvent
	for _, row := range f.rows {
		if row.ID > afterID && len(page) < limit {
			page = append(page, row)
		}
	}
	return page, nil
}

func (f *fakeOutboxReplaySource) MarkProcessed(id uint64) error {
	if err, ok := f.markErrsForID[id]; ok {
		return err
	}
	f.markedIDs = append(f.markedIDs, id)
	return nil
}

func pendingOutboxRow(t *testing.T, id uint64, sequence int64) model.TradeOutboxEvent {
	t.Helper()
	row, err := NewTradeOutboxEvent(outboxTestTrade("BTC", sequence))
	require.NoError(t, err)
	row.ID = id
	return *row
}

func TestOutboxReplayerProcessesInOrderAndMarks(t *testing.T) {
	source := &fakeOutboxReplaySource{rows: []model.TradeOutboxEvent{
		pendingOutboxRow(t, 1, 10),
		pendingOutboxRow(t, 2, 11),
		pendingOutboxRow(t, 3, 12),
	}}
	var processedSequences []int64
	replayer := &OutboxReplayer{
		Repo: source,
		Process: func(event matching.ExecutionEvent) bool {
			processedSequences = append(processedSequences, event.Trade.EngineSequence)
			return true
		},
		Logger: discardServiceLogger(),
	}

	result, err := replayer.Replay()
	require.NoError(t, err)
	assert.Equal(t, OutboxReplayResult{Replayed: 3}, result)
	assert.Equal(t, []int64{10, 11, 12}, processedSequences, "ID 순서 = 엔진 방출 순서로 처리돼야 한다")
	assert.Equal(t, []uint64{1, 2, 3}, source.markedIDs)
}

func TestOutboxReplayerLeavesNonDurableEventsPending(t *testing.T) {
	source := &fakeOutboxReplaySource{rows: []model.TradeOutboxEvent{
		pendingOutboxRow(t, 1, 10),
		pendingOutboxRow(t, 2, 11),
	}}
	replayer := &OutboxReplayer{
		Repo: source,
		Process: func(event matching.ExecutionEvent) bool {
			return event.Trade.EngineSequence != 10 // 첫 이벤트만 내구 확정 실패
		},
		Logger: discardServiceLogger(),
	}

	result, err := replayer.Replay()
	require.NoError(t, err)
	assert.Equal(t, 1, result.Replayed)
	assert.Equal(t, 1, result.Deferred)
	assert.Equal(t, []uint64{2}, source.markedIDs, "내구 확정 실패 이벤트는 PENDING으로 남아야 한다")
}

func TestOutboxReplayerIsolatesCorruptedRows(t *testing.T) {
	corrupted := model.TradeOutboxEvent{ID: 1, EventType: "BOGUS", Payload: []byte("{}"), Status: model.TradeOutboxStatusPending}
	source := &fakeOutboxReplaySource{rows: []model.TradeOutboxEvent{
		corrupted,
		pendingOutboxRow(t, 2, 11),
	}}
	processed := 0
	replayer := &OutboxReplayer{
		Repo:    source,
		Process: func(matching.ExecutionEvent) bool { processed++; return true },
		Logger:  discardServiceLogger(),
	}

	result, err := replayer.Replay()
	require.NoError(t, err)
	assert.Equal(t, 1, result.Corrupted)
	assert.Equal(t, 1, result.Replayed)
	assert.Equal(t, 1, processed, "손상 행은 Process에 전달되지 않아야 한다")
	assert.Equal(t, []uint64{1, 2}, source.markedIDs, "손상 행은 마킹으로 격리돼야 매 부팅 재시도를 막는다")
}

func TestOutboxReplayerCountsMarkFailuresAsDeferred(t *testing.T) {
	source := &fakeOutboxReplaySource{
		rows:          []model.TradeOutboxEvent{pendingOutboxRow(t, 1, 10)},
		markErrsForID: map[uint64]error{1: errors.New("db hiccup")},
	}
	replayer := &OutboxReplayer{
		Repo:    source,
		Process: func(matching.ExecutionEvent) bool { return true },
		Logger:  discardServiceLogger(),
	}

	result, err := replayer.Replay()
	require.NoError(t, err)
	assert.Equal(t, OutboxReplayResult{Deferred: 1}, result)
}

func TestOutboxReplayerPaginatesWithKeyset(t *testing.T) {
	source := &fakeOutboxReplaySource{rows: []model.TradeOutboxEvent{
		pendingOutboxRow(t, 1, 10),
		pendingOutboxRow(t, 2, 11),
		pendingOutboxRow(t, 3, 12),
	}}
	replayer := &OutboxReplayer{
		Repo:     source,
		Process:  func(matching.ExecutionEvent) bool { return true },
		PageSize: 2,
		Logger:   discardServiceLogger(),
	}

	result, err := replayer.Replay()
	require.NoError(t, err)
	assert.Equal(t, 3, result.Replayed)
	assert.Equal(t, []uint64{0, 2}, source.findCalls, "풀 페이지 후 마지막 ID 커서로 다음 페이지를 요청해야 한다")
}

func TestOutboxReplayerPropagatesQueryError(t *testing.T) {
	source := &fakeOutboxReplaySource{findErr: errors.New("db unavailable")}
	replayer := &OutboxReplayer{
		Repo:    source,
		Process: func(matching.ExecutionEvent) bool { return true },
		Logger:  discardServiceLogger(),
	}

	_, err := replayer.Replay()
	require.Error(t, err, "리플레이 자체가 불가능하면 부팅을 막아야 한다(정합성 우선)")
}
