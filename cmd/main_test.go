package main

import (
	"bytes"
	"errors"
	"log"
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeTradeSettler struct {
	result service.SettlementResult
	err    error
}

func (f *fakeTradeSettler) SettleTrade(*model.Trade) (service.SettlementResult, error) {
	return f.result, f.err
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

	processTradeSettlement(testTrade(), settler, recorder, func([]byte) {
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

	processTradeSettlement(testTrade(), settler, recorder, func([]byte) {
		t.Fatal("unexpected broadcast")
	}, logger)

	assert.Equal(t, 1, recorder.calls)
	assert.Contains(t, logBuffer.String(), "record failed settlement failed")
	assert.Contains(t, logBuffer.String(), "settle trade failed")
}

func TestProcessTradeSettlementDoesNotBroadcastDuplicate(t *testing.T) {
	settler := &fakeTradeSettler{result: service.SettlementResult{Applied: false, Duplicate: true, TradeID: 1}}
	recorder := &fakeFailureRecorder{}

	processTradeSettlement(testTrade(), settler, recorder, func([]byte) {
		t.Fatal("unexpected duplicate broadcast")
	}, discardLogger())

	assert.Equal(t, 0, recorder.calls)
}

func TestProcessTradeSettlementBroadcastsAppliedTrade(t *testing.T) {
	settler := &fakeTradeSettler{result: service.SettlementResult{Applied: true, TradeID: 1}}
	var broadcast []byte

	processTradeSettlement(testTrade(), settler, nil, func(msg []byte) {
		broadcast = msg
	}, discardLogger())

	require.NotEmpty(t, broadcast)
	assert.Contains(t, string(broadcast), `"type":"trade"`)
	assert.Contains(t, string(broadcast), `"coin_symbol":"BTC"`)
	assert.Contains(t, string(broadcast), `"engine_sequence":3`)
	assert.Contains(t, string(broadcast), `"engine_event_id":"engine-test-3"`)
	assert.Contains(t, string(broadcast), `"price":"90"`)
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
