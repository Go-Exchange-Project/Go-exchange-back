/* 매칭엔진
1. 채널로 주문을 받음
2. 채널에서 주문을 꺼내서 오더북에 추가
3. 매칭 시도(매수/매도 가격 비교)
4. 조건 맞으면 체결 -> Trade 생성
5. 조건 안 맞으면 오더북에 대기
*/

// 매칭엔진이 가져야 할 것들
// 1. 오더북
// 2. 주문을 받을 채널 (링버퍼 역할) -> 수신채널, 외부에서 매칭엔진으로 주문이 들어오는 통로(수신)
// 3. 체결 결과를 내보낼 채널 -> 송신 채널, 매칭엔진에서 외부로 체결 결과가 나가는 통로(송신)
package matching

import (
	"time"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
)

type MatchingEngine struct {
	OrderBook *OrderBook // 오더북
	OrderCh chan *Order // 주문 받을 채널
	TradeCh chan *model.Trade // 체결 결과 내보낼 채널
}

// 초기화 함수
// 오더북과 주문이 들어갈 오더 채널, 체결된 정보가 들어갈 트레이드 채널
func NewMatchingEngine() *MatchingEngine {
	return &MatchingEngine{
		OrderBook: NewOrderBook(),
		OrderCh: make(chan *Order, 1024),
		TradeCh: make(chan *model.Trade, 1024),
	}
}

/*
1. 고루틴 실행
2. 채널에서 주문을 계속 기다리고 -> order가 들어올 때마다 실행
3. 주문 들어오면 오더북에 추가하고
4. 매칭 시도 하는 함수
main.go에서 me.Start()가 호출되는 순간 고루틴이 백그라운데서 분리되어서 계속 돌아감.(Go런타임이 관리하는 아주 가벼운 것)
*/
func (me *MatchingEngine) Start() {
	// 1
	go func() {
		for order := range me.OrderCh {
			// 2
			me.OrderBook.AddOrder(order)
			me.Match(order)
		}
	}()
}

/*
1. 매수 주문이면 -> 매도 오더북에서 가장 낮은 가격 찾기
2. 매도 주문이면 -> 매수 오더북에서 가장 높은 가격 찾기
3. 가격 조건 맞으면 -> 체결 (Trade 생성)
4. 조건 안 맞으면 -> 오더북에서 대기(이미 AddOrder로 추가됨)
*/

func (me *MatchingEngine) Match(order *Order) {
	if order.Side == model.OrderSideBuy {
		sellLevel, ok := me.OrderBook.SellOrders.Min()
		if ok {
			if order.Price.GreaterThanOrEqual(sellLevel.Price) {
				// 1. 매도 주문 꺼내기
				sellOrder := sellLevel.Orders.PopFront()
				// 2. 체결 수량 계산(매수/매도 중 더 작은 쪽)
				tradeQty := decimal.Min(order.Amount, sellOrder.Amount)
				// 3. 트레이드 생성
				trade := &model.Trade{
					CoinSymbol:  order.CoinSymbol,
    				Price:       sellLevel.Price,
    				Quantity:    tradeQty,
    				TradedAt:    time.Now(),
    				BuyOrderID:  order.ID,
    				SellOrderID: sellOrder.ID,
				}
				// 4.TradeCh 채널로 체결 결과 내보내기
				me.TradeCh <- trade

				// 5. 남은 수량 처리
				order.Amount = order.Amount.Sub(tradeQty)
				order.FilledAmount = order.FilledAmount.Add(tradeQty)
				sellOrder.Amount = sellOrder.Amount.Sub(tradeQty)

				if sellOrder.Amount.IsZero() {
    				if sellLevel.Orders.Len() == 0 {
        				me.OrderBook.SellOrders.Delete(sellLevel)
    				}
				} else {
    				sellLevel.Orders.PushFront(sellOrder)
				}

				if order.Amount.IsZero() {
    				me.OrderBook.RemoveOrder(order)
				}
			}
		}
	} else if order.Side == model.OrderSideSell{
		buyLevel, ok := me.OrderBook.BuyOrders.Max()
		if ok {
        	if buyLevel.Price.GreaterThanOrEqual(order.Price) {
            	buyOrder := buyLevel.Orders.PopFront()
            	tradeQty := decimal.Min(order.Amount, buyOrder.Amount)
            	trade := &model.Trade{
                	CoinSymbol:  order.CoinSymbol,
                	Price:       buyLevel.Price,
                	Quantity:    tradeQty,
                	TradedAt:    time.Now(),
                	BuyOrderID:  buyOrder.ID,
                	SellOrderID: order.ID,
            	}
            	me.TradeCh <- trade

				order.Amount = order.Amount.Sub(tradeQty)
				order.FilledAmount = order.FilledAmount.Add(tradeQty)
				buyOrder.Amount = buyOrder.Amount.Sub(tradeQty)

				if buyOrder.Amount.IsZero() {
    				if buyLevel.Orders.Len() == 0 {
        				me.OrderBook.BuyOrders.Delete(buyLevel)
    				}
				} else {
    				buyLevel.Orders.PushFront(buyOrder)
				}
				
				if order.Amount.IsZero() {
    				me.OrderBook.RemoveOrder(order)
				}
        	}
    	}
	}
}