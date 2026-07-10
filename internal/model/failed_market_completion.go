package model

import (
	"time"

	"github.com/shopspring/decimal"
)

// FailedMarketCompletion은 시장가 주문 완료(CompleteMarketOrder)가 재시도 후에도
// 실패했을 때 남기는 내구 기록입니다. MarketOrderDone 이벤트는 엔진 메모리에만
// 존재하므로, 여기 저장된 필드만으로 완료를 재시도할 수 있어야 합니다.
type FailedMarketCompletion struct {
	ID                   uint                   `gorm:"primaryKey"`
	OrderID              uint                   `gorm:"not null;uniqueIndex:idx_failed_market_completions_order_id"`
	CoinSymbol           string                 `gorm:"not null"`
	FilledAmount         decimal.Decimal        `gorm:"type:numeric;not null;check:ck_failed_market_completions_filled_amount_non_negative,filled_amount >= 0"`
	FilledQuoteAmount    decimal.Decimal        `gorm:"type:numeric;not null;check:ck_failed_market_completions_filled_quote_non_negative,filled_quote_amount >= 0"`
	RemainingQuoteAmount decimal.Decimal        `gorm:"type:numeric;not null;check:ck_failed_market_completions_remaining_quote_non_negative,remaining_quote_amount >= 0"`
	ErrorMessage         string                 `gorm:"type:text;not null;check:ck_failed_market_completions_error_message_not_empty,length(btrim(error_message)) > 0"`
	Status               FailedSettlementStatus `gorm:"not null;default:OPEN;check:ck_failed_market_completions_status_valid,status IN ('OPEN', 'RESOLVED')"`
	RetryCount           uint                   `gorm:"not null;default:1;check:ck_failed_market_completions_retry_count_positive,retry_count > 0"`
	OccurredAt           time.Time              `gorm:"not null"`
	Resolution           string                 `gorm:"type:text"`
	ResolvedAt           *time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
}
