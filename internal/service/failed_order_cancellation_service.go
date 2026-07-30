package service

import (
	"fmt"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
)

type failedOrderCancellationRepository interface {
	RecordFailure(failure *model.FailedOrderCancellation) (*model.FailedOrderCancellation, error)
	EnsureDeferred(failure *model.FailedOrderCancellation) (*model.FailedOrderCancellation, error)
	FindOpen(limit int) ([]model.FailedOrderCancellation, error)
	MarkResolved(id uint, resolution string) error
}

type FailedOrderCancellationService struct {
	Repository failedOrderCancellationRepository
}

func NewFailedOrderCancellationService(repo *repository.FailedOrderCancellationRepository) *FailedOrderCancellationService {
	return &FailedOrderCancellationService{Repository: repo}
}

// RecordFailure는 취소 실행 실패를 내구 기록으로 남깁니다.
// OrderCancelled 이벤트는 엔진 메모리에만 존재하므로, 재시도에 필요한 필드
// 전부와 원본 outbox 행 ID(provenance)를 그대로 보존합니다.
func (s *FailedOrderCancellationService) RecordFailure(cancelled matching.OrderCancelled, sourceOutboxID uint64, executionErr error) (*model.FailedOrderCancellation, error) {
	if s == nil || s.Repository == nil {
		return nil, fmt.Errorf("failed order cancellation repository is required")
	}
	failure, err := failedOrderCancellationFrom(cancelled, sourceOutboxID, executionErr, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	failure.RetryCount = 1
	return s.Repository.RecordFailure(failure)
}

// EnsureDeferred는 dependency 차단으로 취소 terminal을 실행하지 않았을 때 씁니다.
// 실행 시도가 없었으므로 retry count를 소비하지 않습니다.
func (s *FailedOrderCancellationService) EnsureDeferred(cancelled matching.OrderCancelled, sourceOutboxID uint64, reason error) (*model.FailedOrderCancellation, error) {
	if s == nil || s.Repository == nil {
		return nil, fmt.Errorf("failed order cancellation repository is required")
	}
	failure, err := failedOrderCancellationFrom(cancelled, sourceOutboxID, reason, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	failure.RetryCount = 0
	return s.Repository.EnsureDeferred(failure)
}

func (s *FailedOrderCancellationService) ListOpenFailures(limit int) ([]model.FailedOrderCancellation, error) {
	if s == nil || s.Repository == nil {
		return nil, fmt.Errorf("failed order cancellation repository is required")
	}
	return s.Repository.FindOpen(repository.NormalizeFailedSettlementListLimit(limit))
}

func (s *FailedOrderCancellationService) ResolveFailure(id uint, resolution string) error {
	if s == nil || s.Repository == nil {
		return fmt.Errorf("failed order cancellation repository is required")
	}
	return s.Repository.MarkResolved(id, resolution)
}

// failedOrderCancellationFrom은 RecordFailure/EnsureDeferred가 공유하는 모델 조립
// 로직이다. RetryCount는 호출자가 의미에 맞게 설정한다(실행 실패=1, 차단=0).
// 오류 메시지는 settlementErrorMessage와 동일하게 길이를 자르고 빈 문자열을
// 대체해 CHECK 제약(error_message not empty) 위반을 막는다.
func failedOrderCancellationFrom(cancelled matching.OrderCancelled, sourceOutboxID uint64, reason error, occurredAt time.Time) (*model.FailedOrderCancellation, error) {
	if cancelled.OrderID == 0 {
		return nil, fmt.Errorf("order_id is required")
	}

	return &model.FailedOrderCancellation{
		OrderID:       cancelled.OrderID,
		OutboxEventID: sourceOutboxID,
		CoinSymbol:    normalizeTradeCoinSymbol(cancelled.CoinSymbol),
		Side:          cancelled.Side,
		EngineEventID: cancelled.EngineEventID,
		ErrorMessage:  settlementErrorMessage(reason),
		Status:        model.FailedSettlementStatusOpen,
		OccurredAt:    occurredAt,
	}, nil
}
