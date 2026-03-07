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
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
)

type MatchingEngine struct {
	OrderBook *OrderBook // 오더북
	OrderCh chan *Order // 주문 받을 채널
	TradeCh chan *model.Trade // 체결 결과 내보낼 채널
}


func NewMatchingEngine() *MatchingEngine {
	return &MatchingEngine{
		OrderBook: NewOrderBook(),
		OrderCh: make(chan *Order, 1024),
	}
}