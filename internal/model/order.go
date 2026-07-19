package model

import (
	"time"

	"github.com/shopspring/decimal"
)

// Go에는 enum 타입이 없어서 따로 만듦
type OrderType string
type OrderSide string
type OrderStatus string

// OrderType에 들어갈 상수 정하기 - 지정가, 시장가
const (
	OrderTypeLimit  OrderType = "LIMIT"  // 지정가
	OrderTypeMarket OrderType = "MARKET" // 시장가
)

// OrderSide에 들어갈 상수 - 매수, 매도
const (
	OrderSideBuy  OrderSide = "BUY"
	OrderSideSell OrderSide = "SELL"
)

// OrderStatus에 들어갈 상수 - 대기, 일부체결, 전체체결, 취소
const (
	OrderStatusPending   OrderStatus = "PENDING"
	OrderStatusPartial   OrderStatus = "PARTIAL"
	OrderStatusFilled    OrderStatus = "FILLED"
	OrderStatusCancelled OrderStatus = "CANCELLED"
	OrderStatusRejected  OrderStatus = "REJECTED" // 시스템 부하로 접수 거절(유저 취소와 구분). 터미널.
)

type Order struct {
	ID                uint            `gorm:"primaryKey"`
	UserID            uint            `gorm:"not null"`
	Amount            decimal.Decimal `gorm:"type:numeric;not null;default:0;check:ck_orders_amount_non_negative,amount >= 0"`
	QuoteAmount       decimal.Decimal `gorm:"type:numeric;not null;default:0;check:ck_orders_quote_amount_non_negative,quote_amount >= 0"`
	CoinSymbol        string          `gorm:"not null"`
	Side              OrderSide       `gorm:"not null"`
	Status            OrderStatus     `gorm:"not null;default:PENDING"`
	FilledAmount      decimal.Decimal `gorm:"type:numeric;not null;default:0;check:ck_orders_filled_amount_non_negative,filled_amount >= 0"`
	FilledQuoteAmount decimal.Decimal `gorm:"type:numeric;not null;default:0;check:ck_orders_filled_quote_amount_non_negative,filled_quote_amount >= 0"`
	CreatedAt         time.Time
	OrderType         OrderType       `gorm:"not null"`
	Price             decimal.Decimal `gorm:"type:numeric;not null;check:ck_orders_price_non_negative,price >= 0"`
}
