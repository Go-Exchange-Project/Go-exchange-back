// 매칭엔진 전용 주문 구조체
// 매칭엔진은 DB와 상관없이 메모리에서 빠르게 처리해야 함.
// 매칭엔진에서 주문을 처리하려면 어떤 정보가 필요할까
// 가격만 맞으면 되니까 시장가인지 지정가인지는 필요없지 않을까라 생각했지만
// '시장가'는 가격 상관없이 즉시 체결해야 되니까. 지정가와는 매칭 로직이 달라져서 필요
// '수량', '가격'은 당연히 필요할 거
// 맞는 조건 중에 오더북에 올라간 순서대로 처리해야 되니까 주문 요청한 '시간 정보'
// 그리고 일부 체결도 있으니까 체결 후 남은 수량도 계산 할 수 있게 '체결된 수량'

// 결론은 주문ID, 가격, 수량, 주문 시간, 체결된 수량, 매수/매도 구분, 지정가/시정가 구분, 주문 시간, 코인 종류

package matching

import (
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"time"
)

type Order struct {
	ID           uint
	UserID       uint
	Amount       decimal.Decimal // 주문 수량
	CoinSymbol   string
	Side         model.OrderSide
	FilledAmount decimal.Decimal // 체결된 수량, 체결됬는지 판단도 하기 때문에 Status 없앰.
	CreatedAt    time.Time
	OrderType    model.OrderType
	Price        decimal.Decimal
}
