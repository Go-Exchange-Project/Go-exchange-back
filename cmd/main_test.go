package main

import (
	"bytes"
	"errors"
	"log"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeTradeSettler struct {
	result service.SettlementResult
	err    error
	// errs가 설정되면 호출마다 하나씩 소비하고, 소진되면 err/result로 응답한다.
	errs              []error
	calls             int
	lastOutboxEventID uint64
}

func (f *fakeTradeSettler) SettleTrade(_ *model.Trade, outboxEventID uint64) (service.SettlementResult, error) {
	f.calls++
	f.lastOutboxEventID = outboxEventID
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return service.SettlementResult{}, err
		}
		return f.result, nil
	}
	return f.result, f.err
}

func withFastTransientRetries(t *testing.T) {
	t.Helper()
	original := transientRetryDelays
	transientRetryDelays = []time.Duration{0, 0, 0}
	t.Cleanup(func() { transientRetryDelays = original })
}

type fakeFailureRecorder struct {
	calls int
	err   error
}

func (f *fakeFailureRecorder) RecordFailure(*model.Trade, error) (*model.FailedSettlement, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &model.FailedSettlement{ID: 1}, nil
}

func TestProcessTradeSettlementRecordsFailureWithoutBroadcast(t *testing.T) {
	settler := &fakeTradeSettler{err: errors.New("settlement failed")}
	recorder := &fakeFailureRecorder{}
	var broadcasts int

	processTradeSettlement(testTrade(), 0, settler, recorder, func([]byte) {
		broadcasts++
	}, discardLogger())

	assert.Equal(t, 1, recorder.calls)
	assert.Equal(t, 0, broadcasts)
}

func TestProcessTradeSettlementContinuesWhenFailureRecordFails(t *testing.T) {
	settler := &fakeTradeSettler{err: errors.New("settlement failed")}
	recorder := &fakeFailureRecorder{err: errors.New("db write failed")}
	var logBuffer bytes.Buffer
	logger := log.New(&logBuffer, "", 0)

	processTradeSettlement(testTrade(), 0, settler, recorder, func([]byte) {
		t.Fatal("unexpected broadcast")
	}, logger)

	assert.Equal(t, 1, recorder.calls)
	assert.Contains(t, logBuffer.String(), "record failed settlement failed")
	assert.Contains(t, logBuffer.String(), "settle trade failed")
}

func TestProcessTradeSettlementDoesNotBroadcastDuplicate(t *testing.T) {
	settler := &fakeTradeSettler{result: service.SettlementResult{Applied: false, Duplicate: true, TradeID: 1}}
	recorder := &fakeFailureRecorder{}

	processTradeSettlement(testTrade(), 0, settler, recorder, func([]byte) {
		t.Fatal("unexpected duplicate broadcast")
	}, discardLogger())

	assert.Equal(t, 0, recorder.calls)
}

func TestProcessTradeSettlementBroadcastsAppliedTrade(t *testing.T) {
	settler := &fakeTradeSettler{result: service.SettlementResult{Applied: true, TradeID: 1}}
	var broadcast []byte

	processTradeSettlement(testTrade(), 0, settler, nil, func(msg []byte) {
		broadcast = msg
	}, discardLogger())

	require.NotEmpty(t, broadcast)
	assert.Contains(t, string(broadcast), `"type":"trade"`)
	assert.Contains(t, string(broadcast), `"coin_symbol":"BTC"`)
	assert.Contains(t, string(broadcast), `"engine_sequence":3`)
	assert.Contains(t, string(broadcast), `"engine_event_id":"engine-test-3"`)
	assert.Contains(t, string(broadcast), `"price":"90"`)
}

func TestProcessTradeSettlementReportsMarkedInTxOnlyWhenOutboxIDGiven(t *testing.T) {
	// outboxEventID=0: SettleTrade가 마킹하지 않으므로 호출자가 fallback 마킹해야 한다.
	settler := &fakeTradeSettler{result: service.SettlementResult{Applied: true, TradeID: 1}}
	handled, markedInTx := processTradeSettlement(testTrade(), 0, settler, nil, func([]byte) {}, discardLogger())
	assert.True(t, handled)
	assert.False(t, markedInTx, "outboxEventID=0이면 트랜잭션 흡수 마킹이 없어야 한다")
	assert.Equal(t, uint64(0), settler.lastOutboxEventID)

	// outboxEventID>0: SettleTrade가 정산 트랜잭션에서 마킹까지 했으므로 markedInTx=true.
	settler = &fakeTradeSettler{result: service.SettlementResult{Applied: true, TradeID: 1}}
	handled, markedInTx = processTradeSettlement(testTrade(), 77, settler, nil, func([]byte) {}, discardLogger())
	assert.True(t, handled)
	assert.True(t, markedInTx, "정산 성공 + outboxEventID>0이면 트랜잭션이 이미 마킹했다")
	assert.Equal(t, uint64(77), settler.lastOutboxEventID, "outboxEventID가 SettleTrade로 전달돼야 한다")
}

func TestProcessTradeSettlementFailureNeverReportsMarkedInTx(t *testing.T) {
	// 정산 실패는 트랜잭션이 롤백되므로, outboxEventID를 줬어도 마킹되지 않았다 —
	// 호출자가 내구기록 후 fallback 마킹을 해야 한다.
	settler := &fakeTradeSettler{err: errors.New("settlement failed")}
	recorder := &fakeFailureRecorder{}
	handled, markedInTx := processTradeSettlement(testTrade(), 77, settler, recorder, func([]byte) {}, discardLogger())
	assert.True(t, handled, "내구기록 성공이므로 handled=true")
	assert.False(t, markedInTx, "정산 롤백 경로는 트랜잭션 마킹이 없어야 한다")
	assert.Equal(t, 1, recorder.calls)
}

func testTrade() *model.Trade {
	return &model.Trade{
		EngineSequence: 3,
		EngineEventID:  "engine-test-3",
		CoinSymbol:     "BTC",
		Price:          decimal.NewFromInt(90),
		Quantity:       decimal.NewFromInt(5),
		BuyOrderID:     1,
		SellOrderID:    2,
	}
}

func discardLogger() *log.Logger {
	return log.New(&bytes.Buffer{}, "", 0)
}

func histogramSampleCount(t *testing.T, h prometheus.Histogram) uint64 {
	t.Helper()
	m := &dto.Metric{}
	require.NoError(t, h.Write(m))
	return m.GetHistogram().GetSampleCount()
}

func TestProcessTradeSettlementRecordsSettlementDuration(t *testing.T) {
	before := histogramSampleCount(t, metrics.OrderSettlementDuration)

	settler := &fakeTradeSettler{result: service.SettlementResult{Applied: true, TradeID: 1}}
	processTradeSettlement(testTrade(), 0, settler, nil, func([]byte) {}, discardLogger())

	after := histogramSampleCount(t, metrics.OrderSettlementDuration)
	assert.Equal(t, before+1, after)
}

func deadlockError() error {
	return &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}
}

func TestProcessTradeSettlementRetriesTransientErrorInPlace(t *testing.T) {
	withFastTransientRetries(t)

	settler := &fakeTradeSettler{
		errs:   []error{deadlockError(), deadlockError(), nil},
		result: service.SettlementResult{Applied: true, TradeID: 1},
	}
	recorder := &fakeFailureRecorder{}
	var broadcasts int

	processTradeSettlement(testTrade(), 0, settler, recorder, func([]byte) {
		broadcasts++
	}, discardLogger())

	assert.Equal(t, 3, settler.calls)
	assert.Equal(t, 0, recorder.calls, "성공했으므로 실패 기록이 없어야 한다")
	assert.Equal(t, 1, broadcasts)
}

func TestProcessTradeSettlementRecordsFailureAfterTransientRetriesExhausted(t *testing.T) {
	withFastTransientRetries(t)

	settler := &fakeTradeSettler{err: deadlockError()}
	recorder := &fakeFailureRecorder{}

	processTradeSettlement(testTrade(), 0, settler, recorder, func([]byte) {
		t.Fatal("unexpected broadcast")
	}, discardLogger())

	assert.Equal(t, 1+len(transientRetryDelays), settler.calls)
	assert.Equal(t, 1, recorder.calls)
}

func TestProcessTradeSettlementDoesNotRetryPermanentError(t *testing.T) {
	withFastTransientRetries(t)

	settler := &fakeTradeSettler{err: errors.New("cancelled order cannot be settled")}
	recorder := &fakeFailureRecorder{}

	processTradeSettlement(testTrade(), 0, settler, recorder, func([]byte) {}, discardLogger())

	assert.Equal(t, 1, settler.calls)
	assert.Equal(t, 1, recorder.calls)
}

type fakeMarketCompleter struct {
	errs  []error
	err   error
	calls int
}

func (f *fakeMarketCompleter) CompleteMarketOrder(service.CompleteMarketOrderInput) error {
	f.calls++
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return err
	}
	return f.err
}

type fakeCompletionFailureRecorder struct {
	calls  int
	inputs []service.CompleteMarketOrderInput
}

func (f *fakeCompletionFailureRecorder) RecordFailure(input service.CompleteMarketOrderInput, _ string, _ error) (*model.FailedMarketCompletion, error) {
	f.calls++
	f.inputs = append(f.inputs, input)
	return &model.FailedMarketCompletion{ID: 1}, nil
}

func testMarketOrderDone() *matching.MarketOrderDone {
	return &matching.MarketOrderDone{
		OrderID:              42,
		CoinSymbol:           "BTC",
		Side:                 model.OrderSideBuy,
		FilledAmount:         decimal.NewFromInt(1),
		FilledQuoteAmount:    decimal.NewFromInt(100),
		RemainingQuoteAmount: decimal.Zero,
	}
}

func TestProcessMarketOrderDoneRetriesConflictThenSucceeds(t *testing.T) {
	withFastTransientRetries(t)

	conflict := service.NewConflictErrorf("market order 42 settlement is not complete")
	completer := &fakeMarketCompleter{errs: []error{conflict, conflict, nil}}
	recorder := &fakeCompletionFailureRecorder{}

	processMarketOrderDone(testMarketOrderDone(), completer, recorder, discardLogger())

	assert.Equal(t, 3, completer.calls)
	assert.Equal(t, 0, recorder.calls, "성공했으므로 실패 기록이 없어야 한다")
}

func TestProcessMarketOrderDoneRecordsFailureAfterRetriesExhausted(t *testing.T) {
	withFastTransientRetries(t)

	completer := &fakeMarketCompleter{err: service.NewConflictErrorf("market order 42 settlement is not complete")}
	recorder := &fakeCompletionFailureRecorder{}

	processMarketOrderDone(testMarketOrderDone(), completer, recorder, discardLogger())

	assert.Equal(t, 1+len(transientRetryDelays), completer.calls)
	require.Equal(t, 1, recorder.calls, "재시도 소진 후 내구 기록이 남아야 한다")
	assert.Equal(t, uint(42), recorder.inputs[0].OrderID)
	assert.True(t, recorder.inputs[0].FilledQuoteAmount.Equal(decimal.NewFromInt(100)))
}

func TestProcessMarketOrderDoneDoesNotRetryValidationError(t *testing.T) {
	withFastTransientRetries(t)

	completer := &fakeMarketCompleter{err: service.NewValidationErrorf("order 42 is not a market order")}
	recorder := &fakeCompletionFailureRecorder{}

	processMarketOrderDone(testMarketOrderDone(), completer, recorder, discardLogger())

	assert.Equal(t, 1, completer.calls)
	assert.Equal(t, 1, recorder.calls)
}

func TestSettlementWorkerIndexIsStableAndBounded(t *testing.T) {
	for _, workerCount := range []int{1, 2, 4, 10} {
		first := settlementWorkerIndex("BTC", workerCount)
		assert.Equal(t, first, settlementWorkerIndex("BTC", workerCount), "같은 심볼은 항상 같은 워커")
		assert.GreaterOrEqual(t, first, 0)
		assert.Less(t, first, workerCount)
	}
}

func TestForwardToSettlementQueueRoutesSameSymbolInOrder(t *testing.T) {
	const workerCount = 4
	queues := make([]chan service.OutboxEvent, workerCount)
	for i := range queues {
		queues[i] = make(chan service.OutboxEvent, 8)
	}

	// BTC와 다른 워커로 해시되는 심볼을 런타임에 고른다(해시 충돌에 안전하게).
	otherSymbol := ""
	for _, candidate := range []string{"ETH", "XRP", "SOL", "DOGE", "ADA"} {
		if settlementWorkerIndex(candidate, workerCount) != settlementWorkerIndex("BTC", workerCount) {
			otherSymbol = candidate
			break
		}
	}
	require.NotEmpty(t, otherSymbol, "BTC와 다른 워커로 가는 심볼이 있어야 한다")

	forwardToSettlementQueue(queues, service.OutboxEvent{
		OutboxID: 1,
		Event:    matching.ExecutionEvent{Trade: &model.Trade{CoinSymbol: "BTC", EngineSequence: 1}},
	})
	forwardToSettlementQueue(queues, service.OutboxEvent{
		OutboxID: 2,
		Event:    matching.ExecutionEvent{Trade: &model.Trade{CoinSymbol: otherSymbol, EngineSequence: 2}},
	})
	forwardToSettlementQueue(queues, service.OutboxEvent{
		OutboxID: 3,
		Event:    matching.ExecutionEvent{MarketOrderDone: testMarketOrderDone()},
	})

	btcQueue := queues[settlementWorkerIndex("BTC", workerCount)]
	require.Equal(t, 2, len(btcQueue), "BTC trade와 Done은 같은 워커 큐로 가야 한다")
	first := <-btcQueue
	second := <-btcQueue
	require.NotNil(t, first.Event.Trade, "trade가 Done보다 먼저 나와야 한다")
	assert.Equal(t, int64(1), first.Event.Trade.EngineSequence)
	assert.Equal(t, uint64(1), first.OutboxID, "outbox ID가 이벤트와 함께 전달돼야 한다")
	require.NotNil(t, second.Event.MarketOrderDone)
	assert.Equal(t, uint(42), second.Event.MarketOrderDone.OrderID)

	otherQueue := queues[settlementWorkerIndex(otherSymbol, workerCount)]
	assert.Equal(t, 1, len(otherQueue))
}
