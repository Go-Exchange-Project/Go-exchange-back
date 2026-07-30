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

func corruptedOutboxRow(id uint64) model.TradeOutboxEvent {
	return model.TradeOutboxEvent{ID: id, EventType: "BOGUS", Payload: []byte("{}"), Status: model.TradeOutboxStatusPending}
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
		Process: func(sourceOutboxID uint64, event matching.ExecutionEvent) bool {
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

// 핵심 회귀: 내구 확정 실패(Process==false)는 replay를 즉시 중단해야 한다 —
// 뒤 이벤트를 계속 처리하면 미정산 trade 위에서 terminal이 실행될 수 있다.
func TestReplayStopsOnUndurableEvent(t *testing.T) {
	source := &fakeOutboxReplaySource{rows: []model.TradeOutboxEvent{
		pendingOutboxRow(t, 1, 10),
		pendingOutboxRow(t, 2, 11),
		pendingOutboxRow(t, 3, 12),
	}}
	var seen []uint64
	replayer := &OutboxReplayer{
		Repo: source,
		Process: func(sourceOutboxID uint64, event matching.ExecutionEvent) bool {
			seen = append(seen, sourceOutboxID)
			return sourceOutboxID != 2 // 2번에서 내구 확정 실패
		},
		Logger: discardServiceLogger(),
	}

	result, err := replayer.Replay()

	require.Error(t, err)
	assert.Equal(t, []uint64{1, 2}, seen, "3번은 처리되면 안 된다")
	assert.Equal(t, 1, result.Undurable)
	assert.NotContains(t, source.markedIDs, uint64(2), "undurable 행은 PENDING으로 남아야 한다")
}

// 핵심 회귀: corrupted 행을 PROCESSED로 마킹하면 처리되지 않은 금융 이벤트가
// 영구 소실된다 — 마킹하지 않고 replay를 중단해야 한다.
func TestReplayStopsOnCorruptedEventWithoutMarking(t *testing.T) {
	source := &fakeOutboxReplaySource{rows: []model.TradeOutboxEvent{
		pendingOutboxRow(t, 1, 10),
		corruptedOutboxRow(2),
		pendingOutboxRow(t, 3, 12),
	}}
	var seen []uint64
	replayer := &OutboxReplayer{
		Repo: source,
		Process: func(sourceOutboxID uint64, event matching.ExecutionEvent) bool {
			seen = append(seen, sourceOutboxID)
			return true
		},
		Logger: discardServiceLogger(),
	}

	result, err := replayer.Replay()

	require.Error(t, err)
	assert.Equal(t, []uint64{1}, seen)
	assert.Equal(t, 1, result.Corrupted)
	assert.NotContains(t, source.markedIDs, uint64(2),
		"corrupted 행을 PROCESSED로 마킹하면 처리되지 않은 금융 이벤트가 영구 소실된다")
}

func TestReplayContinuesWhenOnlyMarkProcessedFails(t *testing.T) {
	source := &fakeOutboxReplaySource{
		rows:          []model.TradeOutboxEvent{pendingOutboxRow(t, 1, 10), pendingOutboxRow(t, 2, 11)},
		markErrsForID: map[uint64]error{1: errors.New("db hiccup")},
	}
	var seen []uint64
	replayer := &OutboxReplayer{
		Repo: source,
		Process: func(sourceOutboxID uint64, event matching.ExecutionEvent) bool {
			seen = append(seen, sourceOutboxID)
			return true
		},
		Logger: discardServiceLogger(),
	}

	result, err := replayer.Replay()

	require.NoError(t, err)
	assert.Equal(t, []uint64{1, 2}, seen, "정산은 커밋됐으므로 계속 진행해야 한다")
	assert.Equal(t, 1, result.Deferred)
	assert.Equal(t, 1, result.Replayed)
}

func TestReplayPassesSourceOutboxIDToProcess(t *testing.T) {
	source := &fakeOutboxReplaySource{rows: []model.TradeOutboxEvent{pendingOutboxRow(t, 7, 1)}}
	var got uint64
	replayer := &OutboxReplayer{
		Repo: source,
		Process: func(sourceOutboxID uint64, event matching.ExecutionEvent) bool {
			got = sourceOutboxID
			return true
		},
		Logger: discardServiceLogger(),
	}

	_, err := replayer.Replay()

	require.NoError(t, err)
	assert.Equal(t, uint64(7), got)
}

func TestOutboxReplayerPaginatesWithKeyset(t *testing.T) {
	source := &fakeOutboxReplaySource{rows: []model.TradeOutboxEvent{
		pendingOutboxRow(t, 1, 10),
		pendingOutboxRow(t, 2, 11),
		pendingOutboxRow(t, 3, 12),
	}}
	replayer := &OutboxReplayer{
		Repo:     source,
		Process:  func(uint64, matching.ExecutionEvent) bool { return true },
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
		Process: func(uint64, matching.ExecutionEvent) bool { return true },
		Logger:  discardServiceLogger(),
	}

	_, err := replayer.Replay()
	require.Error(t, err, "리플레이 자체가 불가능하면 부팅을 막아야 한다(정합성 우선)")
}
