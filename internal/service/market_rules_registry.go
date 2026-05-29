package service

import (
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
)

type BaseQuantityPolicy struct {
	MinOrderQuantity decimal.Decimal
	BaseQuantityStep decimal.Decimal
}

type MarketRulesRegistry struct {
	minOrderNotional        decimal.Decimal
	feeRate                 decimal.Decimal
	defaultMarketStatus     MarketStatus
	marketStatuses          map[string]MarketStatus
	defaultBaseQuantityRule BaseQuantityPolicy
	baseQuantityRules       map[string]BaseQuantityPolicy
	tickRules               []krwTickRule
	maxTickSize             decimal.Decimal
}

var minKRWOrderNotional = decimal.NewFromInt(5000)
var defaultTradingFeeRate = decimal.RequireFromString("0.0005")
var defaultMarketStatus = MarketStatusActive
var defaultMinOrderQuantity = decimal.RequireFromString("0.00000001")
var defaultBaseQuantityStep = decimal.RequireFromString("0.00000001")

var krwMarketStatuses = map[string]MarketStatus{
	"HALT": MarketStatusHalted,
}

var defaultBaseQuantityPolicy = BaseQuantityPolicy{
	MinOrderQuantity: defaultMinOrderQuantity,
	BaseQuantityStep: defaultBaseQuantityStep,
}

var krwBaseQuantityPolicies = map[string]BaseQuantityPolicy{
	"BTC": defaultBaseQuantityPolicy,
	"ETH": {
		MinOrderQuantity: decimal.RequireFromString("0.0000001"),
		BaseQuantityStep: decimal.RequireFromString("0.0000001"),
	},
	"XRP": {
		MinOrderQuantity: decimal.NewFromInt(1),
		BaseQuantityStep: decimal.NewFromInt(1),
	},
}

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
var defaultMarketRulesRegistry = NewDefaultMarketRulesRegistry()

func NewDefaultMarketRulesRegistry() *MarketRulesRegistry {
	marketStatuses := make(map[string]MarketStatus, len(krwMarketStatuses))
	for coinSymbol, status := range krwMarketStatuses {
		marketStatuses[normalizeCoinSymbol(coinSymbol)] = status
	}

	baseQuantityRules := make(map[string]BaseQuantityPolicy, len(krwBaseQuantityPolicies))
	for coinSymbol, policy := range krwBaseQuantityPolicies {
		baseQuantityRules[normalizeCoinSymbol(coinSymbol)] = policy
	}

	tickRules := make([]krwTickRule, len(krwTickRules))
	copy(tickRules, krwTickRules)

	return &MarketRulesRegistry{
		minOrderNotional:        minKRWOrderNotional,
		feeRate:                 defaultTradingFeeRate,
		defaultMarketStatus:     defaultMarketStatus,
		marketStatuses:          marketStatuses,
		defaultBaseQuantityRule: defaultBaseQuantityPolicy,
		baseQuantityRules:       baseQuantityRules,
		tickRules:               tickRules,
		maxTickSize:             maxKRWTickSize,
	}
}

func (r *MarketRulesRegistry) ValidateLimitOrder(coinSymbol string, price decimal.Decimal, amount decimal.Decimal) error {
	if err := r.ValidateTradingEnabled(coinSymbol); err != nil {
		return err
	}
	if !r.IsValidKRWTickPrice(price) {
		return NewValidationErrorf("price must align with KRW tick size %s", r.KRWTickSize(price).String())
	}
	if err := r.ValidateBaseQuantity(coinSymbol, amount); err != nil {
		return err
	}

	notional := price.Mul(amount)
	if notional.LessThan(r.minOrderNotional) {
		return NewValidationErrorf("order notional must be at least %s KRW", r.minOrderNotional.String())
	}

	return nil
}

func (r *MarketRulesRegistry) ValidateMarketBuyOrder(coinSymbol string, quoteAmount decimal.Decimal) error {
	if err := r.ValidateTradingEnabled(coinSymbol); err != nil {
		return err
	}
	if quoteAmount.LessThan(r.minOrderNotional) {
		return NewValidationErrorf("market buy quote_amount must be at least %s KRW", r.minOrderNotional.String())
	}
	return nil
}

func (r *MarketRulesRegistry) ValidateMarketSellOrder(coinSymbol string, amount decimal.Decimal) error {
	if err := r.ValidateTradingEnabled(coinSymbol); err != nil {
		return err
	}
	return r.ValidateBaseQuantity(coinSymbol, amount)
}

func (r *MarketRulesRegistry) ValidateTradingEnabled(coinSymbol string) error {
	normalizedSymbol := normalizeCoinSymbol(coinSymbol)
	if !r.TradingEnabled(normalizedSymbol) {
		return NewConflictErrorf("%s market is not accepting orders", normalizedSymbol)
	}
	return nil
}

func (r *MarketRulesRegistry) TradingEnabled(coinSymbol string) bool {
	return r.TradingStatus(coinSymbol) == MarketStatusActive
}

func (r *MarketRulesRegistry) TradingStatus(coinSymbol string) MarketStatus {
	normalizedSymbol := normalizeCoinSymbol(coinSymbol)
	if status, ok := r.marketStatuses[normalizedSymbol]; ok {
		return status
	}
	return r.defaultMarketStatus
}

func (r *MarketRulesRegistry) ValidateBaseQuantity(coinSymbol string, amount decimal.Decimal) error {
	normalizedSymbol := normalizeCoinSymbol(coinSymbol)
	policy := r.BaseQuantityPolicy(normalizedSymbol)
	if amount.LessThan(policy.MinOrderQuantity) {
		return NewValidationErrorf("%s order amount must be at least %s", normalizedSymbol, policy.MinOrderQuantity.String())
	}
	if !amount.Mod(policy.BaseQuantityStep).IsZero() {
		return NewValidationErrorf("%s order amount must align with quantity step %s", normalizedSymbol, policy.BaseQuantityStep.String())
	}
	return nil
}

func (r *MarketRulesRegistry) BaseQuantityPolicy(coinSymbol string) BaseQuantityPolicy {
	normalizedSymbol := normalizeCoinSymbol(coinSymbol)
	if policy, ok := r.baseQuantityRules[normalizedSymbol]; ok {
		return policy
	}
	return r.defaultBaseQuantityRule
}

func (r *MarketRulesRegistry) IsValidKRWTickPrice(price decimal.Decimal) bool {
	tick := r.KRWTickSize(price)
	return price.Mod(tick).IsZero()
}

func (r *MarketRulesRegistry) KRWTickSize(price decimal.Decimal) decimal.Decimal {
	for _, rule := range r.tickRules {
		if price.LessThan(rule.upperBound) {
			return rule.tickSize
		}
	}
	return r.maxTickSize
}

func (r *MarketRulesRegistry) KRWMarketRules(coinSymbol string) (MarketRules, error) {
	normalizedSymbol := normalizeCoinSymbol(coinSymbol)
	if normalizedSymbol == "" {
		return MarketRules{}, NewValidationErrorf("coin_symbol is required")
	}

	tickRules := make([]MarketTickRule, 0, len(r.tickRules)+1)
	for _, rule := range r.tickRules {
		upperBound := rule.upperBound
		tickRules = append(tickRules, MarketTickRule{
			UpperBound: &upperBound,
			TickSize:   rule.tickSize,
		})
	}
	tickRules = append(tickRules, MarketTickRule{
		UpperBound: nil,
		TickSize:   r.maxTickSize,
	})

	baseQuantityPolicy := r.BaseQuantityPolicy(normalizedSymbol)
	tradingStatus := r.TradingStatus(normalizedSymbol)

	return MarketRules{
		CoinSymbol:       normalizedSymbol,
		QuoteSymbol:      model.KRWAssetSymbol,
		TradingEnabled:   tradingStatus == MarketStatusActive,
		TradingStatus:    tradingStatus,
		MinOrderNotional: r.minOrderNotional,
		MinOrderQuantity: baseQuantityPolicy.MinOrderQuantity,
		BaseQuantityStep: baseQuantityPolicy.BaseQuantityStep,
		FeeRate:          r.feeRate,
		TickRules:        tickRules,
	}, nil
}
