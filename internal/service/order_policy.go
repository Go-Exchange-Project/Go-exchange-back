package service

import "github.com/shopspring/decimal"

type MarketRules struct {
	CoinSymbol       string
	QuoteSymbol      string
	MinOrderNotional decimal.Decimal
	MinOrderQuantity decimal.Decimal
	BaseQuantityStep decimal.Decimal
	FeeRate          decimal.Decimal
	TickRules        []MarketTickRule
}

type MarketTickRule struct {
	UpperBound *decimal.Decimal
	TickSize   decimal.Decimal
}

func validateLimitOrderPolicy(coinSymbol string, price decimal.Decimal, amount decimal.Decimal) error {
	return defaultMarketRulesRegistry.ValidateLimitOrder(coinSymbol, price, amount)
}

func validateMarketBuyOrderPolicy(quoteAmount decimal.Decimal) error {
	return defaultMarketRulesRegistry.ValidateMarketBuyOrder(quoteAmount)
}

func validateMarketSellOrderPolicy(coinSymbol string, amount decimal.Decimal) error {
	return defaultMarketRulesRegistry.ValidateMarketSellOrder(coinSymbol, amount)
}

func validateBaseQuantityPolicy(coinSymbol string, amount decimal.Decimal) error {
	return defaultMarketRulesRegistry.ValidateBaseQuantity(coinSymbol, amount)
}

func baseQuantityPolicyForCoinSymbol(coinSymbol string) BaseQuantityPolicy {
	return defaultMarketRulesRegistry.BaseQuantityPolicy(coinSymbol)
}

func isValidKRWTickPrice(price decimal.Decimal) bool {
	return defaultMarketRulesRegistry.IsValidKRWTickPrice(price)
}

func krwTickSize(price decimal.Decimal) decimal.Decimal {
	return defaultMarketRulesRegistry.KRWTickSize(price)
}

func KRWMarketRules(coinSymbol string) (MarketRules, error) {
	return defaultMarketRulesRegistry.KRWMarketRules(coinSymbol)
}
