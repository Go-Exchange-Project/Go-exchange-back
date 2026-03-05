// 오더북 자료구조
// BTree + Deque

// 1. PriceLevel 구조체 정의
// 2. OrderBook 구조체 정의
// 3. OrderBook 생성 함수
// 4. 주문 추가/제거 로직

package matching

import (
	"github.com/google/btree"
	"github.com/shopspring/decimal"
	"github.com/gammazero/deque"
)

// 오더북에 들어갈 가격 정보와 같은 가격을 deque형태로 저장시키기 위한 구조체
type PriceLevel struct {
	Price decimal.Decimal
	Orders *deque.Deque[*Order] // 같은 가격의 주문 목록 
}

// 오더북에서 매도/매수 주문 구조체
type OrderBook struct {
	BuyOrders *btree.BTreeG[*PriceLevel] // 매수 목록, Order 타입만 담을 수 있는 BTree
	SellOrders *btree.BTreeG[*PriceLevel] // 매도 목록
	// BTreeG에서 G는 Generic
	// BTreeG는 제네릭을 지원하는 BTree
}

// 오더북을 초기화해서 반환. 틀 만들기
func NewOrderBook() *OrderBook {
	return &OrderBook{
		BuyOrders: btree.NewG[*PriceLevel](32, func(a, b *PriceLevel) bool {
			return a.Price.GreaterThan(b.Price) // 높은 가격 우선
		}),
		SellOrders: btree.NewG[*PriceLevel](32, func(a, b *PriceLevel) bool {
			return a.Price.LessThan(b.Price) // 낮은 가격 우선
		}),
	}
}