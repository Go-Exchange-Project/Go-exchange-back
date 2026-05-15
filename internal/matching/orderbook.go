// 오더북 자료구조
// BTree + Deque

// 1. PriceLevel 구조체 정의 -> PriceLevel struct
// 2. OrderBook 구조체 정의 -> OrderBook struct
// 3. OrderBook 생성 함수 -> func NewOrderBook()
// 4. 주문 추가/제거 로직 -> AddOrder()

package matching

import (
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/gammazero/deque"
	"github.com/google/btree"
	"github.com/shopspring/decimal"
)

// 오더북에 들어갈 가격 정보와 같은 가격을 deque형태로 저장시키기 위한 구조체
type PriceLevel struct {
	Price  decimal.Decimal
	Orders *deque.Deque[*Order] // 같은 가격의 주문 목록
}

// 오더북에서 매도/매수 주문 구조체
type OrderBook struct {
	BuyOrders  *btree.BTreeG[*PriceLevel] // 매수 목록, Order 타입만 담을 수 있는 BTree
	SellOrders *btree.BTreeG[*PriceLevel] // 매도 목록
	// BTreeG에서 G는 Generic
	// BTreeG는 제네릭을 지원하는 BTree
}

// 오더북을 초기화해서 반환. 틀 만들기
func NewOrderBook() *OrderBook {
	return &OrderBook{
		BuyOrders: btree.NewG[*PriceLevel](32, func(a, b *PriceLevel) bool {
			return a.Price.LessThan(b.Price)
		}),
		SellOrders: btree.NewG[*PriceLevel](32, func(a, b *PriceLevel) bool {
			return a.Price.LessThan(b.Price)
		}),
	}
}

// 주문을 오더북에 넣는 함수(주문 전체 정보)
func (ob *OrderBook) AddOrder(order *Order) {
	if order.Side == model.OrderSideBuy {
		// 매수 처리
		// 1. BuyOrders BTree에서 이 가격의 PriceLevel이 있는지 확인
		existing, ok := ob.BuyOrders.Get(&PriceLevel{Price: order.Price})
		// 2. 있으면 -> 그 PriceLevel의 deque에 주문 추가
		if ok {
			existing.Orders.PushBack(order)
		} else { // 3. 없으면 -> 새 PriceLevel 만들어서 BTree에 추가
			newLevel := &PriceLevel{
				Price:  order.Price,
				Orders: &deque.Deque[*Order]{},
			}
			newLevel.Orders.PushBack(order)
			ob.BuyOrders.ReplaceOrInsert(newLevel)
		}
	} else {
		// 매도 처리
		existing, ok := ob.SellOrders.Get(&PriceLevel{Price: order.Price})
		if ok {
			existing.Orders.PushBack(order)
		} else {
			newLevel := &PriceLevel{
				Price:  order.Price,
				Orders: &deque.Deque[*Order]{},
			}
			newLevel.Orders.PushBack(order)
			ob.SellOrders.ReplaceOrInsert(newLevel)
		}

	}
}

// 주문 취소할 때 오더북에서 제거하는 함수
func (ob *OrderBook) RemoveOrder(order *Order) bool {
	if ob == nil || order == nil {
		return false
	}

	// 1. 매수인지 매도인지 확인
	// 2. 해당 가격의 PriceLevel 찾기
	// 3. PriceLevel의 deque에서 해당 주문 제거
	// 4. deque가 비었으면 BTree에서 PriceLevel도 제거
	if order.Side == model.OrderSideBuy {
		existing, ok := ob.BuyOrders.Get(&PriceLevel{Price: order.Price})
		if ok {
			for i := 0; i < existing.Orders.Len(); i++ {
				if existing.Orders.At(i).ID == order.ID {
					existing.Orders.Remove(i)
					if existing.Orders.Len() == 0 {
						ob.BuyOrders.Delete(existing)
					}
					return true
				}
			}
		}
	} else {
		existing, ok := ob.SellOrders.Get(&PriceLevel{Price: order.Price})
		if ok {
			for i := 0; i < existing.Orders.Len(); i++ {
				if existing.Orders.At(i).ID == order.ID {
					existing.Orders.Remove(i)
					if existing.Orders.Len() == 0 {
						ob.SellOrders.Delete(existing)
					}
					return true
				}
			}
		}
	}
	return false
}
