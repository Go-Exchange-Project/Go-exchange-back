package service

import (
	"fmt"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
)

type failedMarketCompletionRepository interface {
	RecordFailure(failure *model.FailedMarketCompletion) (*model.FailedMarketCompletion, error)
	FindOpen(limit int) ([]model.FailedMarketCompletion, error)
	MarkResolved(id uint, resolution string) error
}

type FailedMarketCompletionService struct {
	Repository failedMarketCompletionRepository
}

func NewFailedMarketCompletionService(repo *repository.FailedMarketCompletionRepository) *FailedMarketCompletionService {
	return &FailedMarketCompletionService{Repository: repo}
}

// RecordFailure는 시장가 완료 실패를 내구 기록으로 남깁니다.
// MarketOrderDone 이벤트는 엔진 메모리에만 존재하므로, 재시도에 필요한
// 입력(input) 전체와 coinSymbol을 그대로 보존합니다.
func (s *FailedMarketCompletionService) RecordFailure(input CompleteMarketOrderInput, coinSymbol string, completionErr error) (*model.FailedMarketCompletion, error) {
	if s == nil || s.Repository == nil {
		return nil, fmt.Errorf("failed market completion repository is required")
	}
	if input.OrderID == 0 {
		return nil, fmt.Errorf("order_id is required")
	}

	// RemainingQuoteAmount는 반올림 오차 등으로 아주 작은 음수가 될 수 있다(설계 문서
	// 참고). CHECK 제약(remaining_quote_amount >= 0) 위반으로 이 실패 기록 자체가
	// 실패하는 이중 실패를 막기 위해 저장 전 0으로 clamp한다.
	remainingQuoteAmount := decimal.Max(decimal.Zero, input.RemainingQuoteAmount)

	return s.Repository.RecordFailure(&model.FailedMarketCompletion{
		OrderID:              input.OrderID,
		CoinSymbol:           normalizeTradeCoinSymbol(coinSymbol),
		FilledAmount:         input.FilledAmount,
		FilledQuoteAmount:    input.FilledQuoteAmount,
		RemainingQuoteAmount: remainingQuoteAmount,
		ErrorMessage:         settlementErrorMessage(completionErr),
		Status:               model.FailedSettlementStatusOpen,
		RetryCount:           1,
		OccurredAt:           time.Now().UTC(),
	})
}

func (s *FailedMarketCompletionService) ListOpenFailures(limit int) ([]model.FailedMarketCompletion, error) {
	if s == nil || s.Repository == nil {
		return nil, fmt.Errorf("failed market completion repository is required")
	}
	return s.Repository.FindOpen(repository.NormalizeFailedSettlementListLimit(limit))
}

func (s *FailedMarketCompletionService) ResolveFailure(id uint, resolution string) error {
	if s == nil || s.Repository == nil {
		return fmt.Errorf("failed market completion repository is required")
	}
	return s.Repository.MarkResolved(id, resolution)
}
