package model

import (
	"time"

	"github.com/shopspring/decimal"
)

type Trade struct {
	ID             uint            `gorm:"primaryKey"`
	IdempotencyKey string          `gorm:"size:128;not null;uniqueIndex:idx_trades_idempotency_key;check:ck_trades_idempotency_key_not_empty,length(btrim(idempotency_key)) > 0"`
	EngineSequence int64           `gorm:"not null;default:0;index:idx_trades_engine_sequence;check:ck_trades_engine_sequence_non_negative,engine_sequence >= 0"`
	EngineEventID  string          `gorm:"size:128;not null;default:''"`
	CoinSymbol     string          `gorm:"not null"`
	Price          decimal.Decimal `gorm:"type:numeric;not null;check:ck_trades_price_positive,price > 0"`
	Quantity       decimal.Decimal `gorm:"type:numeric;not null;check:ck_trades_quantity_positive,quantity > 0"`
	FeeRate        decimal.Decimal `gorm:"type:numeric;not null;default:0;check:ck_trades_fee_rate_non_negative,fee_rate >= 0"`
	BuyerFee       decimal.Decimal `gorm:"type:numeric;not null;default:0;check:ck_trades_buyer_fee_non_negative,buyer_fee >= 0"`
	BuyerFeeAsset  string          `gorm:"size:32;not null;default:''"`
	SellerFee      decimal.Decimal `gorm:"type:numeric;not null;default:0;check:ck_trades_seller_fee_non_negative,seller_fee >= 0"`
	SellerFeeAsset string          `gorm:"size:32;not null;default:''"`
	TradedAt       time.Time       `gorm:"not null"`
	BuyOrderID     uint            `gorm:"not null"`
	SellOrderID    uint            `gorm:"not null"`
}
