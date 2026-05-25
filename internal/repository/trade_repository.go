package repository

import (
	"strings"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

type TradeRepository struct {
	DB *gorm.DB
}

type TradeListFilter struct {
	CoinSymbol string
	Limit      int
}

type UserTrade struct {
	ID             uint
	IdempotencyKey string
	EngineSequence int64
	EngineEventID  string
	CoinSymbol     string
	Price          decimal.Decimal
	Quantity       decimal.Decimal
	FeeRate        decimal.Decimal
	BuyerFee       decimal.Decimal
	BuyerFeeAsset  string
	SellerFee      decimal.Decimal
	SellerFeeAsset string
	TradedAt       time.Time
	BuyOrderID     uint
	SellOrderID    uint
	Side           model.OrderSide
}

func NewTradeRepository(db *gorm.DB) *TradeRepository {
	return &TradeRepository{DB: db}
}

func (r *TradeRepository) ListByUserID(userID uint, filter TradeListFilter) ([]UserTrade, error) {
	var trades []UserTrade
	query := r.DB.Table("trades").
		Select(
			"trades.id, trades.idempotency_key, trades.engine_sequence, trades.engine_event_id, trades.coin_symbol, trades.price, trades.quantity, trades.fee_rate, trades.buyer_fee, trades.buyer_fee_asset, trades.seller_fee, trades.seller_fee_asset, trades.traded_at, trades.buy_order_id, trades.sell_order_id, CASE WHEN buy_orders.user_id = ? THEN ? ELSE ? END AS side",
			userID,
			model.OrderSideBuy,
			model.OrderSideSell,
		).
		Joins("JOIN orders AS buy_orders ON buy_orders.id = trades.buy_order_id").
		Joins("JOIN orders AS sell_orders ON sell_orders.id = trades.sell_order_id").
		Where("buy_orders.user_id = ? OR sell_orders.user_id = ?", userID, userID)

	if strings.TrimSpace(filter.CoinSymbol) != "" {
		query = query.Where("trades.coin_symbol = ?", filter.CoinSymbol)
	}

	err := query.
		Order("trades.traded_at DESC").
		Order("trades.id DESC").
		Limit(filter.Limit).
		Scan(&trades).Error
	return trades, err
}
