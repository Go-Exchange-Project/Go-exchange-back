package service

import (
	"fmt"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
)

func applyTradeFeePolicy(trade *model.Trade) error {
	if trade == nil {
		return fmt.Errorf("trade is required")
	}
	if !trade.Quantity.GreaterThan(decimal.Zero) || !trade.Price.GreaterThan(decimal.Zero) {
		return fmt.Errorf("trade price and quantity must be greater than zero")
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
