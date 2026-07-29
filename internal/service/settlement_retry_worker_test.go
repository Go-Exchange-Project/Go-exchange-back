package service

import (
	"errors"
	"io"
	"log"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/prometheus/client_golang/prometheus/testutil"
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

// HasOpenFailureForOrder는 기본적으로 dependency가 없다고 답한다 — 이 fake를 쓰는
// 기존 정산 재시도 테스트들은 completion guard와 무관하므로 항상 통과시킨다.
func (s *fakeFailedSettlementStore) HasOpenFailureForOrder(uint) (bool, error) {
	return false, nil
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
	worker := &SettlementRetryWorker{MarketCompleter: completer, FailedCompletions: store,
		FailedSettlements: &fakeFailedSettlementStore{}, Logger: discardServiceLogger()}

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
	worker := &SettlementRetryWorker{MarketCompleter: completer, FailedCompletions: store,
		FailedSettlements: &fakeFailedSettlementStore{}, Logger: discardServiceLogger()}

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
	worker := &SettlementRetryWorker{MarketCompleter: completer, FailedCompletions: store,
		FailedSettlements: &fakeFailedSettlementStore{}, Logger: discardServiceLogger()}

	worker.RunOnce()

	assert.Equal(t, 0, completer.calls)
	assert.Empty(t, store.resolved)
}

type fakeMarketCompleter struct {
	called bool
}

func (f *fakeMarketCompleter) CompleteMarketOrder(CompleteMarketOrderInput) error {
	f.called = true
	return nil
}

type fakeCompletionStore struct {
	failures        []model.FailedMarketCompletion
	resolved        bool
	recordedFailure bool
}

func (s *fakeCompletionStore) ListOpenFailures(int) ([]model.FailedMarketCompletion, error) {
	return s.failures, nil
}

func (s *fakeCompletionStore) ResolveFailure(uint, string) error {
	s.resolved = true
	return nil
}

func (s *fakeCompletionStore) RecordFailure(CompleteMarketOrderInput, string, error) (*model.FailedMarketCompletion, error) {
	s.recordedFailure = true
	return &model.FailedMarketCompletion{}, nil
}

type fakeSettlementStore struct {
	hasOpen      bool
	hasOpenErr   error
	hasOpenCalls int
}

func (s *fakeSettlementStore) ListOpenFailures(int) ([]model.FailedSettlement, error) {
	return nil, nil
}

func (s *fakeSettlementStore) ResolveFailure(ResolveFailureInput) (*model.FailedSettlement, error) {
	return nil, nil
}

func (s *fakeSettlementStore) RecordFailure(*model.Trade, error) (*model.FailedSettlement, error) {
	return nil, nil
}

func (s *fakeSettlementStore) HasOpenFailureForOrder(uint) (bool, error) {
	s.hasOpenCalls++
	return s.hasOpen, s.hasOpenErr
}

func TestRetryFailedCompletionsRespectsDependencyGuard(t *testing.T) {
	cases := []struct {
		name           string
		hasOpen        bool
		hasOpenErr     error
		nilStore       bool
		wantCompleted  bool // CompleteMarketOrder 호출됐나
		wantBlockedInc bool // blocked counter 증가했나
	}{
		{name: "OPEN dependency면 차단", hasOpen: true, wantCompleted: false, wantBlockedInc: true},
		{name: "dependency 없으면 실행", hasOpen: false, wantCompleted: true, wantBlockedInc: false},
		{name: "조회 오류면 fail-closed", hasOpenErr: errors.New("db down"), wantCompleted: false, wantBlockedInc: false},
		{name: "store가 nil이면 phase 중단", nilStore: true, wantCompleted: false, wantBlockedInc: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			completer := &fakeMarketCompleter{}
			completions := &fakeCompletionStore{failures: []model.FailedMarketCompletion{
				{ID: 1, OrderID: 1001, RetryCount: 1, Status: model.FailedSettlementStatusOpen},
			}}
			w := &SettlementRetryWorker{MarketCompleter: completer, FailedCompletions: completions, Logger: discardServiceLogger()}
			if !tc.nilStore {
				w.FailedSettlements = &fakeSettlementStore{hasOpen: tc.hasOpen, hasOpenErr: tc.hasOpenErr}
			}
			before := testutil.ToFloat64(metrics.SettlementCompletionBlockedTotal)

			w.RunOnce()

			assert.Equal(t, tc.wantCompleted, completer.called)
			assert.Equal(t, uint(1), completions.failures[0].RetryCount, "차단 시 retry count 미소비")
			// dependency가 없어 정상 실행된 케이스는 completer가 성공을 반환하므로 resolve된다 —
			// 나머지(차단·fail-closed) 케이스는 completion 상태를 건드리면 안 된다.
			assert.Equal(t, tc.wantCompleted, completions.resolved)
			assert.False(t, completions.recordedFailure, "차단 시 실패 처리 금지")
			after := testutil.ToFloat64(metrics.SettlementCompletionBlockedTotal)
			if tc.wantBlockedInc {
				assert.Equal(t, before+1, after)
			} else {
				assert.Equal(t, before, after)
			}
		})
	}
}

// phase 중단: 첫 조회가 실패하면 뒤 completion도 이번 사이클엔 실행되지 않는다.
func TestRetryFailedCompletionsAbortsPhaseOnQueryError(t *testing.T) {
	completer := &fakeMarketCompleter{}
	completions := &fakeCompletionStore{failures: []model.FailedMarketCompletion{
		{ID: 1, OrderID: 1001, RetryCount: 1, Status: model.FailedSettlementStatusOpen},
		{ID: 2, OrderID: 1002, RetryCount: 1, Status: model.FailedSettlementStatusOpen},
	}}
	settlements := &fakeSettlementStore{hasOpenErr: errors.New("db down")}
	w := &SettlementRetryWorker{
		MarketCompleter:   completer,
		FailedCompletions: completions,
		FailedSettlements: settlements,
		Logger:            discardServiceLogger(),
	}

	w.RunOnce()

	assert.False(t, completer.called, "첫 조회 실패 시 뒤 completion도 미실행")
	assert.Equal(t, 1, settlements.hasOpenCalls, "phase 중단이므로 조회도 1회만")
}

// 해결 후 다음 RunOnce에서 실행된다(핵심 흐름).
func TestRetryFailedCompletionsRunsAfterDependencyResolved(t *testing.T) {
	completer := &fakeMarketCompleter{}
	settlements := &fakeSettlementStore{hasOpen: true}
	completions := &fakeCompletionStore{failures: []model.FailedMarketCompletion{
		{ID: 1, OrderID: 1001, RetryCount: 1, Status: model.FailedSettlementStatusOpen},
	}}
	w := &SettlementRetryWorker{MarketCompleter: completer, FailedCompletions: completions,
		FailedSettlements: settlements, Logger: discardServiceLogger()}

	w.RunOnce()
	require.False(t, completer.called, "dependency OPEN이면 미실행")

	settlements.hasOpen = false // 복구됨
	w.RunOnce()
	assert.True(t, completer.called, "해결 후 다음 RunOnce에서 실행")
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
