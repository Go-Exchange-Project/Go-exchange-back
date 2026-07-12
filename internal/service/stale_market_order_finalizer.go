package service

import (
	"fmt"
	"log"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
)

type staleMarketOrderSource interface {
	FindOpenMarketOrders() ([]model.Order, error)
}

type staleMarketOrderCompleter interface {
	CompleteMarketOrder(input CompleteMarketOrderInput) error
}

type staleMarketCompletionFailureRecorder interface {
	RecordFailure(input CompleteMarketOrderInput, coinSymbol string, completionErr error) (*model.FailedMarketCompletion, error)
}

// StaleMarketOrderFinalizer는 부팅 리플레이 직후, DB에 PENDING/PARTIAL로 남은
// 시장가 주문의 잔여 hold를 해제합니다. 시장가는 오더북에 rest하지 않으므로
// 이 시점(엔진 메모리 소멸, 라이브 파이프라인 개시 전)에 남아 있다는 것은
// 더 이상 체결될 수 없다는 뜻입니다 — MarketOrderDone이 outbox에 남지 못하고
// 크래시한 케이스의 자동 복구입니다. 반드시 리플레이 뒤에 실행해야 합니다:
// 리플레이 중인 trade 정산이 끝나기 전에 완료를 시도하면 filled 검증에 걸립니다.
type StaleMarketOrderFinalizer struct {
	Orders          staleMarketOrderSource
	Completer       staleMarketOrderCompleter
	FailureRecorder staleMarketCompletionFailureRecorder
	Logger          *log.Logger
}

type StaleMarketOrderFinalizeResult struct {
	Finalized int
	Failed    int // CompleteMarketOrder 실패 — 내구 기록으로 재시도 워커에 위임
}

func (f *StaleMarketOrderFinalizer) FinalizeAll() (StaleMarketOrderFinalizeResult, error) {
	var result StaleMarketOrderFinalizeResult
	if f.Orders == nil || f.Completer == nil {
		return result, fmt.Errorf("stale market order finalizer requires orders source and completer")
	}

	orders, err := f.Orders.FindOpenMarketOrders()
	if err != nil {
		return result, fmt.Errorf("load open market orders: %w", err)
	}

	for _, order := range orders {
		input := CompleteMarketOrderInput{
			OrderID:              order.ID,
			FilledAmount:         order.FilledAmount,
			FilledQuoteAmount:    order.FilledQuoteAmount,
			RemainingQuoteAmount: staleMarketOrderRemainingQuote(order),
		}
		if err := f.Completer.CompleteMarketOrder(input); err != nil {
			result.Failed++
			f.logf("stale market order %d finalize failed: %v", order.ID, err)
			if f.FailureRecorder != nil {
				if _, recordErr := f.FailureRecorder.RecordFailure(input, order.CoinSymbol, err); recordErr != nil {
					f.logf("record stale market order %d finalize failure failed: %v", order.ID, recordErr)
				}
			}
			continue
		}
		result.Finalized++
	}
	return result, nil
}

// staleMarketOrderRemainingQuote는 상태 결정(FILLED/CANCELLED)용 잔여 예산입니다.
// 실제 hold 해제량은 CompleteMarketOrder가 DB 값으로 자체 계산하므로 안전합니다.
func staleMarketOrderRemainingQuote(order model.Order) decimal.Decimal {
	if order.Side != model.OrderSideBuy {
		return decimal.Zero
	}
	remaining := order.QuoteAmount.Sub(order.FilledQuoteAmount)
	if remaining.IsNegative() {
		return decimal.Zero
	}
	return remaining
}

func (f *StaleMarketOrderFinalizer) logf(format string, args ...interface{}) {
	logger := f.Logger
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf(format, args...)
}
