package matching

import (
	"testing"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"time"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
)

// 테스트 1
func TestMatch_매수가가_매도가보다_높으면_체결된다(t *testing.T) {
	me := NewMatchingEngine()
	me.Start()

	sellOrder := &Order{
			ID: 1,
			CoinSymbol: "BTC",
			Side: model.OrderSideSell,
			Price: decimal.NewFromInt(50000),
			Amount: decimal.NewFromInt(1),
			CreatedAt: time.Now(),
		}
	me.OrderCh <- sellOrder

	buyOrder := &Order{
			ID: 2,
			CoinSymbol: "BTC",
			Side: model.OrderSideBuy,
			Price: decimal.NewFromInt(55000),
			Amount: decimal.NewFromInt(1),
			CreatedAt: time.Now(),
		}
	me.OrderCh <- buyOrder

	trade := <-me.TradeCh

	assert.Equal(t, "BTC", trade.CoinSymbol)
	assert.Equal(t, decimal.NewFromInt(50000), trade.Price)
	assert.Equal(t, decimal.NewFromInt(1), trade.Quantity)
}

// 테스트 2