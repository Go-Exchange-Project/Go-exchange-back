package service

import (
	"context"
	"log"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
)

const (
	defaultSettlementRetryInterval = 10 * time.Second
	defaultSettlementRetryMax      = 5
	settlementRetryBatchLimit      = 50
	settlementRetryResolvedBy      = "settlement-retry-worker"
)

type retryTradeSettler interface {
	SettleTrade(trade *model.Trade) (SettlementResult, error)
}

type retryMarketOrderCompleter interface {
	CompleteMarketOrder(input CompleteMarketOrderInput) error
}

type retryFailedSettlementStore interface {
	ListOpenFailures(limit int) ([]model.FailedSettlement, error)
	ResolveFailure(input ResolveFailureInput) (*model.FailedSettlement, error)
	RecordFailure(trade *model.Trade, settlementErr error) (*model.FailedSettlement, error)
}

type retryFailedCompletionStore interface {
	ListOpenFailures(limit int) ([]model.FailedMarketCompletion, error)
	ResolveFailure(id uint, resolution string) error
	RecordFailure(input CompleteMarketOrderInput, coinSymbol string, completionErr error) (*model.FailedMarketCompletion, error)
}

// SettlementRetryWorker는 transient 정산 실패와 시장가 완료 실패를 주기적으로
// 재시도합니다. 멱등성 키가 이중 정산을 막으므로 at-least-once 재시도가 안전합니다.
// 루프 본문이 동기라 사이클이 겹치지 않습니다.
type SettlementRetryWorker struct {
	Settler           retryTradeSettler
	MarketCompleter   retryMarketOrderCompleter
	FailedSettlements retryFailedSettlementStore
	FailedCompletions retryFailedCompletionStore
	Interval          time.Duration
	MaxRetryCount     uint
	Logger            *log.Logger
}

func (w *SettlementRetryWorker) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = defaultSettlementRetryInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.RunOnce()
		}
	}
}

func (w *SettlementRetryWorker) RunOnce() {
	w.retryFailedSettlements()
	w.retryFailedCompletions()
}

func (w *SettlementRetryWorker) retryFailedSettlements() {
	if w.Settler == nil || w.FailedSettlements == nil {
		return
	}

	failures, err := w.FailedSettlements.ListOpenFailures(settlementRetryBatchLimit)
	if err != nil {
		w.logf("retry worker: list open failed settlements failed: %v", err)
		return
	}

	for i := range failures {
		failure := &failures[i]
		if !IsTransientFailedSettlementCategory(ClassifyFailedSettlement(failure)) {
			continue
		}
		if failure.RetryCount >= w.maxRetryCount() {
			continue
		}

		trade := tradeFromFailedSettlement(failure)
		if _, err := w.Settler.SettleTrade(trade); err != nil {
			if _, recordErr := w.FailedSettlements.RecordFailure(trade, err); recordErr != nil {
				w.logf("retry worker: record failed settlement failed: %v", recordErr)
			}
			w.logf("retry worker: settle trade %s failed: %v", failure.TradeIdempotencyKey, err)
			continue
		}

		if _, err := w.FailedSettlements.ResolveFailure(ResolveFailureInput{
			ID:         failure.ID,
			Resolution: "auto-retry: transient settlement error resolved",
			ResolvedBy: settlementRetryResolvedBy,
		}); err != nil {
			w.logf("retry worker: resolve failed settlement %d failed: %v", failure.ID, err)
		}
	}
}

func (w *SettlementRetryWorker) retryFailedCompletions() {
	if w.MarketCompleter == nil || w.FailedCompletions == nil {
		return
	}

	failures, err := w.FailedCompletions.ListOpenFailures(settlementRetryBatchLimit)
	if err != nil {
		w.logf("retry worker: list open failed market completions failed: %v", err)
		return
	}

	for i := range failures {
		failure := &failures[i]
		if failure.RetryCount >= w.maxRetryCount() {
			continue
		}

		input := CompleteMarketOrderInput{
			OrderID:              failure.OrderID,
			FilledAmount:         failure.FilledAmount,
			FilledQuoteAmount:    failure.FilledQuoteAmount,
			RemainingQuoteAmount: failure.RemainingQuoteAmount,
		}
		if err := w.MarketCompleter.CompleteMarketOrder(input); err != nil {
			if _, recordErr := w.FailedCompletions.RecordFailure(input, failure.CoinSymbol, err); recordErr != nil {
				w.logf("retry worker: record failed market completion failed: %v", recordErr)
			}
			w.logf("retry worker: complete market order %d failed: %v", failure.OrderID, err)
			continue
		}

		if err := w.FailedCompletions.ResolveFailure(failure.ID, "auto-retry: market order completion succeeded"); err != nil {
			w.logf("retry worker: resolve failed market completion %d failed: %v", failure.ID, err)
		}
	}
}

// tradeFromFailedSettlement는 저장된 실패 기록에서 정산 재시도용 trade를 복원합니다.
// 멱등성 키를 원본 그대로 보존하므로 이중 정산이 발생하지 않습니다.
// 수수료는 SettleTrade의 applyTradeFeePolicy가 price×quantity에서 결정적으로
// 재계산합니다 — 수수료율이 사용자별로 달라지면 실패 기록에 수수료도 저장해야 합니다.
func tradeFromFailedSettlement(failure *model.FailedSettlement) *model.Trade {
	tradedAt := failure.OccurredAt
	if failure.TradedAt != nil && !failure.TradedAt.IsZero() {
		tradedAt = *failure.TradedAt
	}
	return &model.Trade{
		IdempotencyKey: failure.TradeIdempotencyKey,
		EngineSequence: failure.EngineSequence,
		EngineEventID:  failure.EngineEventID,
		CoinSymbol:     failure.CoinSymbol,
		Price:          failure.Price,
		Quantity:       failure.Quantity,
		TradedAt:       tradedAt,
		BuyOrderID:     failure.BuyOrderID,
		SellOrderID:    failure.SellOrderID,
	}
}

func (w *SettlementRetryWorker) maxRetryCount() uint {
	if w.MaxRetryCount > 0 {
		return w.MaxRetryCount
	}
	return defaultSettlementRetryMax
}

func (w *SettlementRetryWorker) logf(format string, args ...interface{}) {
	logger := w.Logger
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf(format, args...)
}
