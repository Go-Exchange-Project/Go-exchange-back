package model

import "github.com/shopspring/decimal"


type Wallet struct {
	ID    uint    `gorm:"primaryKey"`
	KRW   decimal.Decimal `gorm:"type:numeric;default:0"`
	CoinSymbol    string    `gorm:"not null"`
	Quantity    decimal.Decimal    `gorm:"type:numeric;default:0"`
	AvgBuyPrice    decimal.Decimal    `gorm:"type:numeric;default:0"`
	UserID    uint    `gorm:"not null"`
}