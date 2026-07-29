package main

import (
	"errors"
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
)

type stubBatchSettler struct{ err error }

func (s stubBatchSettler) SettleTradeBatch(items []service.TradeBatchItem) ([]service.SettlementResult, error) {
	if s.err != nil {
		return nil, s.err
	}
	results := make([]service.SettlementResult, len(items))
	for i := range results {
		results[i] = service.SettlementResult{Applied: true}
	}
	return results, nil
}

type stubSettler struct{ err error }

func (s stubSettler) SettleTrade(trade *model.Trade, outboxEventID uint64) (service.SettlementResult, error) {
	if s.err != nil {
		return service.SettlementResult{}, s.err
	}
	return service.SettlementResult{Applied: true}, nil
}

// recordErr가 nil이 아니면 실패 기록 자체가 실패한다 = undurable.
type stubFailureRecorder struct{ recordErr error }

func (s stubFailureRecorder) RecordFailure(trade *model.Trade, settlementErr error) (*model.FailedSettlement, error) {
	if s.recordErr != nil {
		return nil, s.recordErr
	}
	return &model.FailedSettlement{}, nil
}

type stubOutboxMarker struct{ markErr error }

func (s stubOutboxMarker) MarkProcessed(id uint64) error { return s.markErr }

// tradeOutboxEventForOrders는 기존 tradeOutboxEvent(outboxID, engineSequence)와
// 시그니처가 달라 별도 이름을 쓴다 — undurable 전파 테스트는 engineSequence가 아니라
// buyOrderID·sellOrderID로 quarantine 대상을 식별해야 한다.
func tradeOutboxEventForOrders(outboxID uint64, buyOrderID, sellOrderID uint) service.OutboxEvent {
	return service.OutboxEvent{
		OutboxID: outboxID,
		Event: matching.ExecutionEvent{Trade: &model.Trade{
			CoinSymbol:  "BTC",
			BuyOrderID:  buyOrderID,
			SellOrderID: sellOrderID,
			Price:       decimal.NewFromInt(100),
			Quantity:    decimal.NewFromInt(1),
		}},
	}
}

func TestSettleTradeBatchWithFallbackReportsUndurableOrders(t *testing.T) {
	batch := []service.OutboxEvent{
		tradeOutboxEventForOrders(1, 10, 20),
		tradeOutboxEventForOrders(2, 30, 40),
	}

	// 배치 실패 → 폴백 단건 → 단건도 실패 → 기록도 실패 = undurable
	undurable := settleTradeBatchWithFallback(
		batch,
		stubBatchSettler{err: errors.New("batch boom")},
		stubSettler{err: errors.New("single boom")},
		stubFailureRecorder{recordErr: errors.New("record boom")},
		nil, nil, nil,
		func(string, []byte) {},
		stubOutboxMarker{},
		discardLogger(),
	)

	assert.ElementsMatch(t, []uint{10, 20, 30, 40}, undurable)
}

func TestSettleTradeBatchWithFallbackNoUndurableWhenFailureRecorded(t *testing.T) {
	batch := []service.OutboxEvent{tradeOutboxEventForOrders(1, 10, 20)}

	undurable := settleTradeBatchWithFallback(
		batch,
		stubBatchSettler{err: errors.New("batch boom")},
		stubSettler{err: errors.New("single boom")},
		stubFailureRecorder{}, // 기록은 성공 → durable
		nil, nil, nil,
		func(string, []byte) {},
		stubOutboxMarker{},
		discardLogger(),
	)

	assert.Empty(t, undurable)
}

// 정산은 커밋됐고 outbox 마킹만 실패한 경우는 undurable이 아니다.
func TestSettleTradeBatchWithFallbackMarkProcessedFailureIsNotUndurable(t *testing.T) {
	batch := []service.OutboxEvent{tradeOutboxEventForOrders(1, 10, 20)}

	undurable := settleTradeBatchWithFallback(
		batch,
		stubBatchSettler{err: errors.New("batch boom")},
		stubSettler{}, // 정산 성공
		stubFailureRecorder{},
		nil, nil, nil,
		func(string, []byte) {},
		stubOutboxMarker{markErr: errors.New("mark boom")},
		discardLogger(),
	)

	assert.Empty(t, undurable)
}
