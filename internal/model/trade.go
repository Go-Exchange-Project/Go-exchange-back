package model

import (
	"time"

	"github.com/shopspring/decimal"
)

type Trade struct {
	ID		uint		`gorm:"primaryKey"`
	CoinSymbol	string	`gorm:"not null"`
	Price    	decimal.Decimal		`gorm:"type:numeric;not null"`
	Quantity	decimal.Decimal		`gorm:"type:numeric;not null"`
	TradedAt	time.Time		`gorm:"not null"`
	BuyOrderID		uint		`gorm:"not null"`
	SellOrderID		uint 		`gorm:"not null"`
}