package model

import "github.com/shopspring/decimal"

const KRWAssetSymbol = "KRW"

type Wallet struct {
	ID               uint            `gorm:"primaryKey"`
	KRW              decimal.Decimal `gorm:"type:numeric;not null;default:0;check:ck_wallets_krw_non_negative,krw >= 0"`
	CoinSymbol       string          `gorm:"not null;uniqueIndex:idx_wallets_user_id_coin_symbol"`
	Quantity         decimal.Decimal `gorm:"type:numeric;not null;default:0;check:ck_wallets_quantity_non_negative,quantity >= 0"`
	AvailableBalance decimal.Decimal `gorm:"type:numeric;not null;default:0;check:ck_wallets_available_balance_non_negative,available_balance >= 0"`
	LockedBalance    decimal.Decimal `gorm:"type:numeric;not null;default:0;check:ck_wallets_locked_balance_non_negative,locked_balance >= 0"`
	AvgBuyPrice      decimal.Decimal `gorm:"type:numeric;not null;default:0"`
	UserID           uint            `gorm:"not null;uniqueIndex:idx_wallets_user_id_coin_symbol"`
}
