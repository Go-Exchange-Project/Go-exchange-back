package service

import (
	"errors"
	"io"
	"log"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func discardServiceLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

type fakeRetrySettler struct {
	err               error
	calls             int
	trades            []*model.Trade
	lastOutboxEventID uint64
}

func (f *fakeRetrySettler) SettleTrade(trade *model.Trade, outboxEventID uint64) (SettlementResult, error) {
	f.calls++
	f.trades = append(f.trades, trade)
	f.lastOutboxEventID = outboxEventID
	if f.err != nil {
		return SettlementResult{}, f.err
	}
	return SettlementResult{Applied: true, TradeID: 1}, nil
}

type fakeRetryCompleter struct {
	err    error
	calls  int
	inputs []CompleteMarketOrderInput
}

func (f *fakeRetryCompleter) CompleteMarketOrder(input CompleteMarketOrderInput) error {
	f.calls++
	f.inputs = append(f.inputs, input)
	return f.err
}

type fakeFailedSettlementStore struct {
	open     []model.FailedSettlement
	resolved []uint
	recorded int
}

func (s *fakeFailedSettlementStore) ListOpenFailures(int) ([]model.FailedSettlement, error) {
	return s.open, nil
}

func (s *fakeFailedSettlementStore) ResolveFailure(input ResolveFailureInput) (*model.FailedSettlement, error) {
	s.resolved = append(s.resolved, input.ID)
	return &model.FailedSettlement{ID: input.ID}, nil
}

func (s *fakeFailedSettlementStore) RecordFailure(*model.Trade, error) (*model.FailedSettlement, error) {
	s.recorded++
	return &model.FailedSettlement{}, nil
}

type fakeFailedCompletionStore struct {
	open     []model.FailedMarketCompletion
	resolved []uint
	recorded int
}

func (s *fakeFailedCompletionStore) ListOpenFailures(int) ([]model.FailedMarketCompletion, error) {
	return s.open, nil
}

func (s *fakeFailedCompletionStore) ResolveFailure(id uint, _ string) error {
	s.resolved = append(s.resolved, id)
	return nil
}

func (s *fakeFailedCompletionStore) RecordFailure(CompleteMarketOrderInput, string, error) (*model.FailedMarketCompletion, error) {
	s.recorded++
	return &model.FailedMarketCompletion{}, nil
}

func transientOpenFailure(id uint, retryCount uint) model.FailedSettlement {
	return model.FailedSettlement{
		ID:                  id,
		TradeIdempotencyKey: "engine:engine-test-1",
		EngineSequence:      1,
		EngineEventID:       "engine-test-1",
		CoinSymbol:          "BTC",
		BuyOrderID:          10,
		SellOrderID:         11,
		Price:               decimal.NewFromInt(100),
		Quantity:            decimal.NewFromInt(1),
		ErrorMessage:        "[SQLSTATE 40P01] settle: deadlock detected",
		Status:              model.FailedSettlementStatusOpen,
		RetryCount:          retryCount,
		OccurredAt:          time.Now().UTC(),
	}
}

func TestRetryWorkerRetriesTransientSettlementAndResolves(t *testing.T) {
	settler := &fakeRetrySettler{}
	store := &fakeFailedSettlementStore{open: []model.FailedSettlement{transientOpenFailure(3, 1)}}
	worker := &SettlementRetryWorker{Settler: settler, FailedSettlements: store, Logger: discardServiceLogger()}

	worker.RunOnce()

	require.Equal(t, 1, settler.calls)
	assert.Equal(t, []uint{3}, store.resolved)
	assert.Equal(t, 0, store.recorded)

	trade := settler.trades[0]
	assert.Equal(t, "engine:engine-test-1", trade.IdempotencyKey)
	assert.Equal(t, int64(1), trade.EngineSequence)
	assert.Equal(t, "engine-test-1", trade.EngineEventID)
	assert.Equal(t, uint(10), trade.BuyOrderID)
	assert.Equal(t, uint(11), trade.SellOrderID)
}

func TestRetryWorkerSkipsPermanentFailure(t *testing.T) {
	failure := transientOpenFailure(3, 1)
	failure.ErrorMessage = "buy order 10 status CANCELLED cannot be settled"
	settler := &fakeRetrySettler{}
	store := &fakeFailedSettlementStore{open: []model.FailedSettlement{failure}}
	worker := &SettlementRetryWorker{Settler: settler, FailedSettlements: store, Logger: discardServiceLogger()}

	worker.RunOnce()

	assert.Equal(t, 0, settler.calls)
	assert.Empty(t, store.resolved)
}

func TestRetryWorkerSkipsExhaustedRetryCount(t *testing.T) {
	settler := &fakeRetrySettler{}
	store := &fakeFailedSettlementStore{open: []model.FailedSettlement{transientOpenFailure(3, 5)}}
	worker := &SettlementRetryWorker{Settler: settler, FailedSettlements: store, Logger: discardServiceLogger()}

	worker.RunOnce()

	assert.Equal(t, 0, settler.calls)
	assert.Empty(t, store.resolved)
}

func TestRetryWorkerRecordsFailureWhenRetryFails(t *testing.T) {
	settler := &fakeRetrySettler{err: errors.New("still failing")}
	store := &fakeFailedSettlementStore{open: []model.FailedSettlement{transientOpenFailure(3, 1)}}
	worker := &SettlementRetryWorker{Settler: settler, FailedSettlements: store, Logger: discardServiceLogger()}

	worker.RunOnce()

	assert.Equal(t, 1, settler.calls)
	assert.Empty(t, store.resolved)
	assert.Equal(t, 1, store.recorded)
}

func TestRetryWorkerRetriesCompletionAndResolves(t *testing.T) {
	completer := &fakeRetryCompleter{}
	store := &fakeFailedCompletionStore{open: []model.FailedMarketCompletion{{
		ID:                   7,
		OrderID:              42,
		CoinSymbol:           "BTC",
		FilledAmount:         decimal.NewFromInt(1),
		FilledQuoteAmount:    decimal.NewFromInt(100),
		RemainingQuoteAmount: decimal.Zero,
		Status:               model.FailedSettlementStatusOpen,
		RetryCount:           1,
	}}}
	worker := &SettlementRetryWorker{MarketCompleter: completer, FailedCompletions: store, Logger: discardServiceLogger()}

	worker.RunOnce()

	require.Equal(t, 1, completer.calls)
	assert.Equal(t, uint(42), completer.inputs[0].OrderID)
	assert.True(t, completer.inputs[0].FilledQuoteAmount.Equal(decimal.NewFromInt(100)))
	assert.Equal(t, []uint{7}, store.resolved)
	assert.Equal(t, 0, store.recorded)
}

func TestRetryWorkerRecordsCompletionFailure(t *testing.T) {
	completer := &fakeRetryCompleter{err: errors.New("still not complete")}
	store := &fakeFailedCompletionStore{open: []model.FailedMarketCompletion{{
		ID:         7,
		OrderID:    42,
		CoinSymbol: "BTC",
		Status:     model.FailedSettlementStatusOpen,
		RetryCount: 1,
	}}}
	worker := &SettlementRetryWorker{MarketCompleter: completer, FailedCompletions: store, Logger: discardServiceLogger()}

	worker.RunOnce()

	assert.Equal(t, 1, completer.calls)
	assert.Empty(t, store.resolved)
	assert.Equal(t, 1, store.recorded)
}

func TestRetryWorkerSkipsExhaustedCompletionRetryCount(t *testing.T) {
	completer := &fakeRetryCompleter{}
	store := &fakeFailedCompletionStore{open: []model.FailedMarketCompletion{{
		ID:         7,
		OrderID:    42,
		Status:     model.FailedSettlementStatusOpen,
		RetryCount: 5,
	}}}
	worker := &SettlementRetryWorker{MarketCompleter: completer, FailedCompletions: store, Logger: discardServiceLogger()}

	worker.RunOnce()

	assert.Equal(t, 0, completer.calls)
	assert.Empty(t, store.resolved)
}

func TestTradeFromFailedSettlementFallsBackToOccurredAt(t *testing.T) {
	occurredAt := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	failure := transientOpenFailure(1, 1)
	failure.TradedAt = nil
	failure.OccurredAt = occurredAt

	trade := tradeFromFailedSettlement(&failure)
	assert.True(t, trade.TradedAt.Equal(occurredAt))

	tradedAt := occurredAt.Add(-time.Minute)
	failure.TradedAt = &tradedAt
	trade = tradeFromFailedSettlement(&failure)
	assert.True(t, trade.TradedAt.Equal(tradedAt))
}
