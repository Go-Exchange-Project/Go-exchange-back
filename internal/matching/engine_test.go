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

	// 매도 구조체
	sellOrder := &Order{
			ID: 1,
			CoinSymbol: "BTC",
			Side: model.OrderSideSell,
			Price: decimal.NewFromInt(50000),
			Amount: decimal.NewFromInt(1),
			CreatedAt: time.Now(),
		}
	me.OrderCh <- sellOrder

	// 매수 구조체
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
func TestMatch_매수가가_매도가보다_낮으면_체결안된다(t *testing.T){
	me := NewMatchingEngine()
	me.Start()

	// 매도 구조체
	sellOrder := &Order{
			ID: 1,
			CoinSymbol: "BTC",
			Side: model.OrderSideSell,
			Price: decimal.NewFromInt(55000),
			Amount: decimal.NewFromInt(1),
			CreatedAt: time.Now(),
		}
	me.OrderCh <- sellOrder

	// 매수 구조체
	buyOrder := &Order{
			ID: 2,
			CoinSymbol: "BTC",
			Side: model.OrderSideBuy,
			Price: decimal.NewFromInt(50000),
			Amount: decimal.NewFromInt(1),
			CreatedAt: time.Now(),
		}
	me.OrderCh <- buyOrder

	time.Sleep(100 * time.Millisecond) // 채널로 보낸 주문을 고루틴이 처리하는 데 약간의 시간이 필요. 안 기다리면 처리전에 체크 할 수 도 있음.
	assert.Equal(t, 0, len(me.TradeCh))
}

// 테스트 3
func TestMatch_일부체결_매수_10개_매도_7개(t *testing.T){
	me := NewMatchingEngine()
	me.Start()

	// 매도 구조체
	sellOrder := &Order{
			ID: 1,
			CoinSymbol: "BTC",
			Side: model.OrderSideSell,
			Price: decimal.NewFromInt(50000),
			Amount: decimal.NewFromInt(7),
			CreatedAt: time.Now(),
		}
	me.OrderCh <- sellOrder

	// 매수 구조체
	buyOrder := &Order{
			ID: 2,
			CoinSymbol: "BTC",
			Side: model.OrderSideBuy,
			Price: decimal.NewFromInt(55000),
			Amount: decimal.NewFromInt(10),
			CreatedAt: time.Now(),
		}
	me.OrderCh <- buyOrder

	trade := <-me.TradeCh
	assert.Equal(t, decimal.NewFromInt(7), trade.Quantity) // 체결 수량 확인

	time.Sleep(100 * time.Millisecond)

	buyLevel, ok := me.OrderBook.BuyOrders.Max()
	assert.True(t, ok)
	assert.Equal(t, decimal.NewFromInt(3), buyLevel.Orders.Front().Amount) // 오더북에 남아 있는지
}

// 테스트 4
func TestMatch_오더북이_비어있으면_체결_안_된다(t *testing.T){
	me := NewMatchingEngine()
	me.Start()

	// 매수 구조체
	buyOrder := &Order{
			ID: 2,
			CoinSymbol: "BTC",
			Side: model.OrderSideBuy,
			Price: decimal.NewFromInt(55000),
			Amount: decimal.NewFromInt(10),
			CreatedAt: time.Now(),
		}
	me.OrderCh <- buyOrder

	time.Sleep(100 * time.Millisecond)

	assert.Equal(t, 0, len(me.TradeCh))
}