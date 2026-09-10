package service

import (
	"fmt"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
)

// applyTradeFeePolicy는 수수료를 계산해 trade에 채운다. trade.FeeRate가 이미
// 양수면 계산하지 않고 그대로 둔다 — 정산 재시도(tradeFromFailedSettlement)가
// 최초 시도에서 저장된 수수료를 그대로 써야 하기 때문이다(설계 §13.2).
//
// 이것이 요율 변경 후의 재시도까지 보장하지는 않는다. 주문 잠금액(holdAmountFor),
// 해제액, 정산의 예약 차감액(reservedBuyDebitAmount)은 여전히
// defaultTradingFeeRate를 그때그때 읽는다. 그래서 저장된 수수료와 잠금 기준이
// 같은 요율을 가리키는 것은 요율이 0.0005로 고정돼 있다는 현재 전제 위에서만
// 성립한다. 요율을 바꿀 수 있게 만들려면 매수·매도 양측 요율, Trade 모델,
// 시장가 매수 executable quote, 부분 체결 후 해제 정책을 함께 설계해야 한다.
func applyTradeFeePolicy(trade *model.Trade) error {
	if trade == nil {
		return fmt.Errorf("trade is required")
	}
	if !trade.Quantity.GreaterThan(decimal.Zero) || !trade.Price.GreaterThan(decimal.Zero) {
		return fmt.Errorf("trade price and quantity must be greater than zero")
	}
	if trade.FeeRate.IsPositive() {
		return nil
	}
	if !defaultTradingFeeRate.GreaterThanOrEqual(decimal.Zero) || defaultTradingFeeRate.GreaterThanOrEqual(decimal.NewFromInt(1)) {
		return fmt.Errorf("invalid trading fee rate")
	}

	executionQuote := tradeQuoteAmount(trade)

	trade.FeeRate = defaultTradingFeeRate
	trade.BuyerFee = tradingFeeAmount(executionQuote)
	trade.BuyerFeeAsset = model.KRWAssetSymbol
	trade.SellerFee = tradingFeeAmount(executionQuote)
	trade.SellerFeeAsset = model.KRWAssetSymbol
	return nil
}

func tradingFeeAmount(amount decimal.Decimal) decimal.Decimal {
	return amount.Mul(defaultTradingFeeRate)
}

func quoteAmountWithTradingFee(amount decimal.Decimal) decimal.Decimal {
	return amount.Add(tradingFeeAmount(amount))
}

func marketBuyExecutableQuoteAmount(grossQuoteBudget decimal.Decimal) decimal.Decimal {
	if !grossQuoteBudget.GreaterThan(decimal.Zero) {
		return decimal.Zero
	}
	return grossQuoteBudget.Div(decimal.NewFromInt(1).Add(defaultTradingFeeRate))
}

func amountAfterFee(gross decimal.Decimal, fee decimal.Decimal, field string) (decimal.Decimal, error) {
	if !gross.GreaterThan(decimal.Zero) {
		return decimal.Zero, NewValidationErrorf("%s gross amount must be greater than zero", field)
	}
	if fee.IsNegative() {
		return decimal.Zero, NewValidationErrorf("%s fee must be greater than or equal to zero", field)
	}
	net := gross.Sub(fee)
	if !net.GreaterThan(decimal.Zero) {
		return decimal.Zero, NewValidationErrorf("%s fee must be less than gross amount", field)
	}
	return net, nil
}
