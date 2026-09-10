package model

import (
	"time"

	"github.com/shopspring/decimal"
)

type FailedSettlementStatus string

const (
	FailedSettlementStatusOpen     FailedSettlementStatus = "OPEN"
	FailedSettlementStatusResolved FailedSettlementStatus = "RESOLVED"
)

type FailedSettlement struct {
	ID                  uint   `gorm:"primaryKey"`
	TradeIdempotencyKey string `gorm:"size:128;not null;uniqueIndex:idx_failed_settlements_trade_idempotency_key;check:ck_failed_settlements_trade_idempotency_key_not_empty,length(btrim(trade_idempotency_key)) > 0"`
	EngineSequence      int64  `gorm:"not null;default:0"`
	EngineEventID       string `gorm:"size:128;not null;default:''"`
	TradedAt            *time.Time
	CoinSymbol          string          `gorm:"not null"`
	BuyOrderID          uint            `gorm:"not null"`
	SellOrderID         uint            `gorm:"not null"`
	Price               decimal.Decimal `gorm:"type:numeric;not null;check:ck_failed_settlements_price_positive,price > 0"`
	Quantity            decimal.Decimal `gorm:"type:numeric;not null;check:ck_failed_settlements_quantity_positive,quantity > 0"`
	// BuyerFee·SellerFee·FeeRate는 최초 시도에서 계산된 값을 그대로 보존한다.
	// 재시도가 다시 계산하면 같은 체결이 두 시점의 산술을 섞어 쓰게 된다 — 설계 §13.2.
	//
	// 잠금·해제·예약 차감액은 여전히 현재 요율을 읽으므로, 이 보존만으로 요율 변경
	// 후의 재시도가 성립하지는 않는다. 요율 0.0005 고정이 현재 전제다.
	BuyerFee     decimal.Decimal        `gorm:"type:numeric;not null;default:0;check:ck_failed_settlements_buyer_fee_non_negative,buyer_fee >= 0"`
	SellerFee    decimal.Decimal        `gorm:"type:numeric;not null;default:0;check:ck_failed_settlements_seller_fee_non_negative,seller_fee >= 0"`
	FeeRate      decimal.Decimal        `gorm:"type:numeric;not null;default:0;check:ck_failed_settlements_fee_rate_non_negative,fee_rate >= 0"`
	ErrorMessage string                 `gorm:"type:text;not null;check:ck_failed_settlements_error_message_not_empty,length(btrim(error_message)) > 0"`
	Status       FailedSettlementStatus `gorm:"not null;default:OPEN;check:ck_failed_settlements_status_not_empty,length(btrim(status)) > 0"`
	RetryCount   uint                   `gorm:"not null;default:1;check:ck_failed_settlements_retry_count_positive,retry_count > 0"`
	OccurredAt   time.Time              `gorm:"not null"`
	Resolution   string                 `gorm:"type:text"`
	ResolvedBy   string                 `gorm:"size:128"`
	Notes        string                 `gorm:"type:text"`
	ResolvedAt   *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}
