package service

import "github.com/shopspring/decimal"

var minKRWOrderNotional = decimal.NewFromInt(5000)

type krwTickRule struct {
	upperBound decimal.Decimal
	tickSize   decimal.Decimal
}

var krwTickRules = []krwTickRule{
	{upperBound: decimal.NewFromInt(1), tickSize: decimal.RequireFromString("0.00001")},
	{upperBound: decimal.NewFromInt(10), tickSize: decimal.RequireFromString("0.01")},
	{upperBound: decimal.NewFromInt(100), tickSize: decimal.RequireFromString("0.1")},
	{upperBound: decimal.NewFromInt(1000), tickSize: decimal.NewFromInt(1)},
	{upperBound: decimal.NewFromInt(10000), tickSize: decimal.NewFromInt(5)},
	{upperBound: decimal.NewFromInt(100000), tickSize: decimal.NewFromInt(10)},
	{upperBound: decimal.NewFromInt(500000), tickSize: decimal.NewFromInt(50)},
	{upperBound: decimal.NewFromInt(1000000), tickSize: decimal.NewFromInt(100)},
	{upperBound: decimal.NewFromInt(2000000), tickSize: decimal.NewFromInt(500)},
}

var maxKRWTickSize = decimal.NewFromInt(1000)

func validateLimitOrderPolicy(price decimal.Decimal, amount decimal.Decimal) error {
	if !isValidKRWTickPrice(price) {
		return NewValidationErrorf("price must align with KRW tick size %s", krwTickSize(price).String())
	}

	notional := price.Mul(amount)
	if notional.LessThan(minKRWOrderNotional) {
		return NewValidationErrorf("order notional must be at least %s KRW", minKRWOrderNotional.String())
	}

	return nil
}

func isValidKRWTickPrice(price decimal.Decimal) bool {
	tick := krwTickSize(price)
	return price.Mod(tick).IsZero()
}

func krwTickSize(price decimal.Decimal) decimal.Decimal {
	for _, rule := range krwTickRules {
		if price.LessThan(rule.upperBound) {
			return rule.tickSize
		}
	}
	return maxKRWTickSize
}
