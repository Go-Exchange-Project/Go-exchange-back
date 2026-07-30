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
	EnsureDeferred(failure *model.FailedMarketCompletion) (*model.FailedMarketCompletion, error)
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
	failure, err := failedMarketCompletionFrom(input, coinSymbol, completionErr, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	failure.RetryCount = 1
	return s.Repository.RecordFailure(failure)
}

// EnsureDeferred는 dependency 차단으로 terminal을 실행하지 않았을 때 쓴다.
// 실행 시도가 없었으므로 retry count를 소비하지 않는다.
func (s *FailedMarketCompletionService) EnsureDeferred(input CompleteMarketOrderInput, coinSymbol string, reason error) (*model.FailedMarketCompletion, error) {
	if s == nil || s.Repository == nil {
		return nil, fmt.Errorf("failed market completion repository is required")
	}
	failure, err := failedMarketCompletionFrom(input, coinSymbol, reason, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	failure.RetryCount = 0
	return s.Repository.EnsureDeferred(failure)
}

// failedMarketCompletionFrom은 RecordFailure/EnsureDeferred가 공유하는 모델 조립
// 로직이다. MarketOrderDone 이벤트는 엔진 메모리에만 존재하므로, 재시도에 필요한
// 입력(input) 전체와 coinSymbol을 그대로 보존한다. RetryCount는 호출자가 의미에
// 맞게 설정한다(실행 실패=1, 차단=0).
func failedMarketCompletionFrom(input CompleteMarketOrderInput, coinSymbol string, reason error, occurredAt time.Time) (*model.FailedMarketCompletion, error) {
	if input.OrderID == 0 {
		return nil, fmt.Errorf("order_id is required")
	}

	// RemainingQuoteAmount는 반올림 오차 등으로 아주 작은 음수가 될 수 있다(설계 문서
	// 참고). CHECK 제약(remaining_quote_amount >= 0) 위반으로 이 실패 기록 자체가
	// 실패하는 이중 실패를 막기 위해 저장 전 0으로 clamp한다.
	remainingQuoteAmount := decimal.Max(decimal.Zero, input.RemainingQuoteAmount)

	return &model.FailedMarketCompletion{
		OrderID:              input.OrderID,
		CoinSymbol:           normalizeTradeCoinSymbol(coinSymbol),
		FilledAmount:         input.FilledAmount,
		FilledQuoteAmount:    input.FilledQuoteAmount,
		RemainingQuoteAmount: remainingQuoteAmount,
		ErrorMessage:         settlementErrorMessage(reason),
		Status:               model.FailedSettlementStatusOpen,
		OccurredAt:           occurredAt,
	}, nil
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
