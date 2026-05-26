package matching

import (
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
)

type Order struct {
	ID                uint
	UserID            uint
	Amount            decimal.Decimal
	QuoteAmount       decimal.Decimal
	CoinSymbol        string
	Side              model.OrderSide
	FilledAmount      decimal.Decimal
	FilledQuoteAmount decimal.Decimal
	CreatedAt         time.Time
	OrderType         model.OrderType
	Price             decimal.Decimal
}
