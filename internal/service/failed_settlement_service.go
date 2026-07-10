package service

import (
	"fmt"
	"strings"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
)

const (
	maxFailedSettlementErrorLength      = 2048
	maxFailedSettlementResolutionLength = 512
	maxFailedSettlementResolvedByLength = 128
	maxFailedSettlementNotesLength      = 2048
)

type FailedSettlementCategory string

const (
	FailedSettlementCategoryCancelledOrder            FailedSettlementCategory = "CANCELLED_ORDER"
	FailedSettlementCategoryIdempotencyConflict       FailedSettlementCategory = "IDEMPOTENCY_CONFLICT"
	FailedSettlementCategoryInsufficientLockedBalance FailedSettlementCategory = "INSUFFICIENT_LOCKED_BALANCE"
	FailedSettlementCategoryDeadlock                  FailedSettlementCategory = "DEADLOCK"
	FailedSettlementCategorySerializationFailure      FailedSettlementCategory = "SERIALIZATION_FAILURE"
	FailedSettlementCategoryLockTimeout               FailedSettlementCategory = "LOCK_TIMEOUT"
	FailedSettlementCategoryUnknown                   FailedSettlementCategory = "UNKNOWN"
)

// IsTransientFailedSettlementCategory는 자동 재시도 대상 카테고리인지 판정합니다.
func IsTransientFailedSettlementCategory(category FailedSettlementCategory) bool {
	switch category {
	case FailedSettlementCategoryDeadlock,
		FailedSettlementCategorySerializationFailure,
		FailedSettlementCategoryLockTimeout:
		return true
	}
	return false
}

type ResolveFailureInput struct {
	ID         uint
	Resolution string
	ResolvedBy string
	Notes      string
}

type failedSettlementRepository interface {
	RecordFailure(failure *model.FailedSettlement) (*model.FailedSettlement, error)
	FindOpen(limit int) ([]model.FailedSettlement, error)
	FindByID(id uint) (*model.FailedSettlement, error)
	MarkResolved(id uint, resolution string, resolvedBy string, notes string) error
}

type FailedSettlementService struct {
	Repository failedSettlementRepository
}

func NewFailedSettlementService(repo *repository.FailedSettlementRepository) *FailedSettlementService {
	return &FailedSettlementService{Repository: repo}
}

func (s *FailedSettlementService) RecordFailure(trade *model.Trade, settlementErr error) (*model.FailedSettlement, error) {
	if s == nil || s.Repository == nil {
		return nil, fmt.Errorf("failed settlement repository is required")
	}

	failure, err := failedSettlementFromTrade(trade, settlementErr, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	return s.Repository.RecordFailure(failure)
}

func (s *FailedSettlementService) ListOpenFailures(limit int) ([]model.FailedSettlement, error) {
	if s == nil || s.Repository == nil {
		return nil, fmt.Errorf("failed settlement repository is required")
	}
	return s.Repository.FindOpen(repository.NormalizeFailedSettlementListLimit(limit))
}

func (s *FailedSettlementService) ResolveFailure(input ResolveFailureInput) (*model.FailedSettlement, error) {
	if s == nil || s.Repository == nil {
		return nil, fmt.Errorf("failed settlement repository is required")
	}
	if err := validateResolveFailureInput(input); err != nil {
		return nil, err
	}

	if err := s.Repository.MarkResolved(
		input.ID,
		strings.TrimSpace(input.Resolution),
		strings.TrimSpace(input.ResolvedBy),
		strings.TrimSpace(input.Notes),
	); err != nil {
		return nil, err
	}
	return s.Repository.FindByID(input.ID)
}

func ClassifyFailedSettlement(failure *model.FailedSettlement) FailedSettlementCategory {
	if failure == nil {
		return FailedSettlementCategoryUnknown
	}

	message := strings.ToUpper(failure.ErrorMessage)
	switch {
	// SQLSTATE 태그는 settlementErrorMessage가 기록 시점에 붙인다.
	// 메시지 본문은 lc_messages에 따라 번역될 수 있으므로 코드 태그를 우선 매칭한다.
	case strings.Contains(message, "[SQLSTATE "+pgCodeDeadlockDetected+"]"):
		return FailedSettlementCategoryDeadlock
	case strings.Contains(message, "[SQLSTATE "+pgCodeSerializationFailure+"]"):
		return FailedSettlementCategorySerializationFailure
	case strings.Contains(message, "[SQLSTATE "+pgCodeLockNotAvailable+"]"):
		return FailedSettlementCategoryLockTimeout
	case strings.Contains(message, "CANCELLED"):
		return FailedSettlementCategoryCancelledOrder
	case strings.Contains(message, "IDEMPOTENCY KEY CONFLICT"):
		return FailedSettlementCategoryIdempotencyConflict
	case strings.Contains(message, "INSUFFICIENT LOCKED"):
		return FailedSettlementCategoryInsufficientLockedBalance
	default:
		return FailedSettlementCategoryUnknown
	}
}

func failedSettlementFromTrade(trade *model.Trade, settlementErr error, occurredAt time.Time) (*model.FailedSettlement, error) {
	if trade == nil {
		return nil, fmt.Errorf("trade is required")
	}

	coinSymbol := normalizeTradeCoinSymbol(trade.CoinSymbol)
	if coinSymbol == "" {
		return nil, fmt.Errorf("trade coin symbol is required")
	}
	if trade.BuyOrderID == 0 || trade.SellOrderID == 0 {
		return nil, fmt.Errorf("trade buy_order_id and sell_order_id are required")
	}
	if !trade.Price.GreaterThan(decimal.Zero) || !trade.Quantity.GreaterThan(decimal.Zero) {
		return nil, fmt.Errorf("trade price and quantity must be greater than zero")
	}

	idempotencyKey := strings.TrimSpace(trade.IdempotencyKey)
	if idempotencyKey == "" {
		idempotencyKey = tradeIdempotencyKey(trade)
	}
	trade.IdempotencyKey = idempotencyKey

	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}

	var tradedAt *time.Time
	if !trade.TradedAt.IsZero() {
		tradedAtValue := trade.TradedAt
		tradedAt = &tradedAtValue
	}

	return &model.FailedSettlement{
		TradeIdempotencyKey: idempotencyKey,
		EngineSequence:      trade.EngineSequence,
		EngineEventID:       strings.TrimSpace(trade.EngineEventID),
		TradedAt:            tradedAt,
		CoinSymbol:          coinSymbol,
		BuyOrderID:          trade.BuyOrderID,
		SellOrderID:         trade.SellOrderID,
		Price:               trade.Price,
		Quantity:            trade.Quantity,
		ErrorMessage:        settlementErrorMessage(settlementErr),
		Status:              model.FailedSettlementStatusOpen,
		RetryCount:          1,
		OccurredAt:          occurredAt,
	}, nil
}

func settlementErrorMessage(err error) string {
	message := "unknown settlement failure"
	if err != nil {
		message = strings.TrimSpace(err.Error())
	}
	if message == "" {
		message = "unknown settlement failure"
	}
	// transient 오류는 저장된 메시지만으로 재분류할 수 있도록 SQLSTATE 태그를 붙인다.
	if code := settlementErrorSQLState(err); code != "" {
		message = "[SQLSTATE " + code + "] " + message
	}
	if len(message) > maxFailedSettlementErrorLength {
		return message[:maxFailedSettlementErrorLength]
	}
	return message
}

func validateResolveFailureInput(input ResolveFailureInput) error {
	if input.ID == 0 {
		return fmt.Errorf("failed settlement id is required")
	}
	resolution := strings.TrimSpace(input.Resolution)
	if resolution == "" {
		return fmt.Errorf("resolution is required")
	}
	if len(resolution) > maxFailedSettlementResolutionLength {
		return fmt.Errorf("resolution must be at most %d characters", maxFailedSettlementResolutionLength)
	}
	if len(strings.TrimSpace(input.ResolvedBy)) > maxFailedSettlementResolvedByLength {
		return fmt.Errorf("resolved_by must be at most %d characters", maxFailedSettlementResolvedByLength)
	}
	if len(strings.TrimSpace(input.Notes)) > maxFailedSettlementNotesLength {
		return fmt.Errorf("notes must be at most %d characters", maxFailedSettlementNotesLength)
	}
	return nil
}
