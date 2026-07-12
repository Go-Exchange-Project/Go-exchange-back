package service

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeOutboxInserter struct {
	mu      sync.Mutex
	batches [][]*model.TradeOutboxEvent
	errs    []error // 호출마다 하나씩 소비, 소진되면 성공
	nextID  uint64
}

func (f *fakeOutboxInserter) InsertBatch(events []*model.TradeOutboxEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return err
		}
	}
	for _, event := range events {
		f.nextID++
		event.ID = f.nextID
	}
	batch := make([]*model.TradeOutboxEvent, len(events))
	copy(batch, events)
	f.batches = append(f.batches, batch)
	return nil
}

func (f *fakeOutboxInserter) batchSizes() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	sizes := make([]int, len(f.batches))
	for i, batch := range f.batches {
		sizes[i] = len(batch)
	}
	return sizes
}

func outboxTestTrade(symbol string, sequence int64) matching.ExecutionEvent {
	return matching.ExecutionEvent{Trade: &model.Trade{
		EngineSequence: sequence,
		EngineEventID:  "engine-test-outbox",
		CoinSymbol:     symbol,
		Price:          decimal.NewFromInt(100),
		Quantity:       decimal.NewFromInt(2),
		TradedAt:       time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC),
		BuyOrderID:     1,
		SellOrderID:    2,
	}}
}

func outboxTestDone() matching.ExecutionEvent {
	return matching.ExecutionEvent{MarketOrderDone: &matching.MarketOrderDone{
		OrderID:              42,
		CoinSymbol:           "BTC",
		Side:                 model.OrderSideBuy,
		FilledAmount:         decimal.NewFromInt(1),
		FilledQuoteAmount:    decimal.NewFromInt(100),
		RemainingQuoteAmount: decimal.NewFromInt(3),
	}}
}

// 직렬화 왕복

func TestTradeOutboxEventRoundTripTrade(t *testing.T) {
	original := outboxTestTrade("BTC", 7)
	row, err := NewTradeOutboxEvent(original)
	require.NoError(t, err)
	assert.Equal(t, model.TradeOutboxEventTypeTrade, row.EventType)
	assert.Equal(t, "BTC", row.CoinSymbol)
	assert.Equal(t, "engine-test-outbox", row.EngineEventID)

	restored, err := ExecutionEventFromOutbox(*row)
	require.NoError(t, err)
	require.NotNil(t, restored.Trade)
	assert.Equal(t, int64(7), restored.Trade.EngineSequence)
	assert.True(t, restored.Trade.Price.Equal(decimal.NewFromInt(100)))
	assert.True(t, restored.Trade.Quantity.Equal(decimal.NewFromInt(2)))
	assert.Equal(t, uint(1), restored.Trade.BuyOrderID)
	assert.True(t, restored.Trade.TradedAt.Equal(original.Trade.TradedAt))
}

func TestTradeOutboxEventRoundTripMarketOrderDone(t *testing.T) {
	row, err := NewTradeOutboxEvent(outboxTestDone())
	require.NoError(t, err)
	assert.Equal(t, model.TradeOutboxEventTypeMarketOrderDone, row.EventType)

	restored, err := ExecutionEventFromOutbox(*row)
	require.NoError(t, err)
	require.NotNil(t, restored.MarketOrderDone)
	assert.Equal(t, uint(42), restored.MarketOrderDone.OrderID)
	assert.True(t, restored.MarketOrderDone.RemainingQuoteAmount.Equal(decimal.NewFromInt(3)))
}

func TestNewTradeOutboxEventRejectsEmptyEvent(t *testing.T) {
	_, err := NewTradeOutboxEvent(matching.ExecutionEvent{})
	require.Error(t, err)
}

func TestExecutionEventFromOutboxRejectsUnknownType(t *testing.T) {
	_, err := ExecutionEventFromOutbox(model.TradeOutboxEvent{ID: 1, EventType: "BOGUS", Payload: []byte("{}")})
	require.Error(t, err)
}

// 배치/전달 동작

type forwardCollector struct {
	mu     sync.Mutex
	events []OutboxEvent
}

func (c *forwardCollector) forward(event OutboxEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
}

func (c *forwardCollector) snapshot() []OutboxEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]OutboxEvent, len(c.events))
	copy(out, c.events)
	return out
}

func runOutboxWriter(t *testing.T, writer *OutboxWriter) chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		writer.Run()
		close(done)
	}()
	return done
}

func waitOutboxWriterDone(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("outbox writer did not finish in time")
	}
}

func TestOutboxWriterFlushesByBatchSizeAndForwardsInOrder(t *testing.T) {
	repo := &fakeOutboxInserter{}
	collector := &forwardCollector{}
	source := make(chan matching.ExecutionEvent, 8)
	writer := &OutboxWriter{
		Repo:          repo,
		Source:        source,
		Forward:       collector.forward,
		BatchSize:     2,
		FlushInterval: time.Hour, // 배치 크기만으로 flush되게
		Logger:        discardServiceLogger(),
	}

	for seq := int64(1); seq <= 4; seq++ {
		source <- outboxTestTrade("BTC", seq)
	}
	close(source)
	done := runOutboxWriter(t, writer)
	waitOutboxWriterDone(t, done)

	assert.Equal(t, []int{2, 2}, repo.batchSizes(), "2건씩 두 배치로 커밋돼야 한다")
	forwarded := collector.snapshot()
	require.Len(t, forwarded, 4)
	for i, event := range forwarded {
		assert.Equal(t, uint64(i+1), event.OutboxID, "outbox ID가 순서대로 전달돼야 한다")
		assert.Equal(t, int64(i+1), event.Event.Trade.EngineSequence)
	}
}

func TestOutboxWriterFlushesOnIntervalWithoutFullBatch(t *testing.T) {
	repo := &fakeOutboxInserter{}
	collector := &forwardCollector{}
	source := make(chan matching.ExecutionEvent, 8)
	writer := &OutboxWriter{
		Repo:          repo,
		Source:        source,
		Forward:       collector.forward,
		BatchSize:     100,
		FlushInterval: 5 * time.Millisecond,
		Logger:        discardServiceLogger(),
	}
	done := runOutboxWriter(t, writer)

	source <- outboxTestTrade("BTC", 1)
	require.Eventually(t, func() bool {
		return len(collector.snapshot()) == 1
	}, 3*time.Second, 5*time.Millisecond, "배치가 안 찼어도 flush 간격에 커밋·전달돼야 한다")

	close(source)
	waitOutboxWriterDone(t, done)
}

func TestOutboxWriterFlushesRemainderWhenSourceCloses(t *testing.T) {
	repo := &fakeOutboxInserter{}
	collector := &forwardCollector{}
	source := make(chan matching.ExecutionEvent, 8)
	writer := &OutboxWriter{
		Repo:          repo,
		Source:        source,
		Forward:       collector.forward,
		BatchSize:     100,
		FlushInterval: time.Hour,
		Logger:        discardServiceLogger(),
	}

	source <- outboxTestTrade("BTC", 1)
	source <- outboxTestTrade("BTC", 2)
	source <- outboxTestDone()
	close(source)
	done := runOutboxWriter(t, writer)
	waitOutboxWriterDone(t, done)

	assert.Equal(t, []int{3}, repo.batchSizes(), "graceful shutdown 시 잔여 이벤트가 한 배치로 flush돼야 한다")
	require.Len(t, collector.snapshot(), 3)
}

func TestOutboxWriterRetriesInsertUntilSuccessAndForwardsOnce(t *testing.T) {
	repo := &fakeOutboxInserter{errs: []error{errors.New("db down"), errors.New("still down")}}
	collector := &forwardCollector{}
	source := make(chan matching.ExecutionEvent, 8)
	writer := &OutboxWriter{
		Repo:           repo,
		Source:         source,
		Forward:        collector.forward,
		BatchSize:      1,
		FlushInterval:  time.Millisecond,
		RetryBaseDelay: time.Millisecond,
		Logger:         discardServiceLogger(),
	}

	source <- outboxTestTrade("BTC", 1)
	close(source)
	done := runOutboxWriter(t, writer)
	waitOutboxWriterDone(t, done)

	assert.Equal(t, []int{1}, repo.batchSizes(), "실패한 INSERT는 성공할 때까지 재시도돼야 한다")
	forwarded := collector.snapshot()
	require.Len(t, forwarded, 1, "커밋 성공 후 정확히 한 번만 전달돼야 한다")
	assert.Equal(t, uint64(1), forwarded[0].OutboxID)
}

func TestOutboxWriterSkipsUnencodableEvent(t *testing.T) {
	repo := &fakeOutboxInserter{}
	collector := &forwardCollector{}
	source := make(chan matching.ExecutionEvent, 8)
	writer := &OutboxWriter{
		Repo:          repo,
		Source:        source,
		Forward:       collector.forward,
		BatchSize:     10,
		FlushInterval: time.Hour,
		Logger:        discardServiceLogger(),
	}

	source <- matching.ExecutionEvent{} // trade도 done도 없는 빈 이벤트
	source <- outboxTestTrade("BTC", 1)
	close(source)
	done := runOutboxWriter(t, writer)
	waitOutboxWriterDone(t, done)

	assert.Equal(t, []int{1}, repo.batchSizes())
	require.Len(t, collector.snapshot(), 1)
}
