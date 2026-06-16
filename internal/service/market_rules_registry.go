package service

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

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

type MarketRulesConfig struct {
	MinOrderNotional        string                             `json:"min_order_notional"`
	FeeRate                 string                             `json:"fee_rate"`
	DefaultMarketStatus     string                             `json:"default_market_status"`
	DefaultMinOrderQuantity string                             `json:"default_min_order_quantity"`
	DefaultBaseQuantityStep string                             `json:"default_base_quantity_step"`
	Markets                 map[string]MarketRulesMarketConfig `json:"markets"`
	TickRules               []MarketRulesTickConfig            `json:"tick_rules"`
	MaxTickSize             string                             `json:"max_tick_size"`
}

type MarketRulesMarketConfig struct {
	TradingStatus    string `json:"trading_status"`
	MinOrderQuantity string `json:"min_order_quantity"`
	BaseQuantityStep string `json:"base_quantity_step"`
}

type MarketRulesTickConfig struct {
	UpperBound string `json:"upper_bound"`
	TickSize   string `json:"tick_size"`
}

const EnvMarketRulesPath = "GOEXCHANGE_MARKET_RULES_PATH"

var defaultMarketRulesConfigPath = "config/market_rules.json"
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
	{upperBound: decimal.RequireFromString("0.00001"), tickSize: decimal.RequireFromString("0.00000001")},
	{upperBound: decimal.RequireFromString("0.0001"), tickSize: decimal.RequireFromString("0.0000001")},
	{upperBound: decimal.RequireFromString("0.001"), tickSize: decimal.RequireFromString("0.000001")},
	{upperBound: decimal.RequireFromString("0.01"), tickSize: decimal.RequireFromString("0.00001")},
	{upperBound: decimal.RequireFromString("0.1"), tickSize: decimal.RequireFromString("0.0001")},
	{upperBound: decimal.NewFromInt(1), tickSize: decimal.RequireFromString("0.001")},
	{upperBound: decimal.NewFromInt(10), tickSize: decimal.RequireFromString("0.01")},
	{upperBound: decimal.NewFromInt(100), tickSize: decimal.RequireFromString("0.1")},
	{upperBound: decimal.NewFromInt(5000), tickSize: decimal.NewFromInt(1)},
	{upperBound: decimal.NewFromInt(10000), tickSize: decimal.NewFromInt(5)},
	{upperBound: decimal.NewFromInt(50000), tickSize: decimal.NewFromInt(10)},
	{upperBound: decimal.NewFromInt(100000), tickSize: decimal.NewFromInt(50)},
	{upperBound: decimal.NewFromInt(500000), tickSize: decimal.NewFromInt(100)},
	{upperBound: decimal.NewFromInt(1000000), tickSize: decimal.NewFromInt(500)},
	{upperBound: decimal.NewFromInt(2000000), tickSize: decimal.NewFromInt(1000)},
}

var maxKRWTickSize = decimal.NewFromInt(1000)
var defaultMarketRulesRegistry = NewDefaultMarketRulesRegistry()

func NewMarketRulesRegistryFromEnv() (*MarketRulesRegistry, error) {
	path := strings.TrimSpace(os.Getenv(EnvMarketRulesPath))
	if path == "" {
		path = defaultMarketRulesConfigPath
	}
	return NewMarketRulesRegistryFromFile(path)
}

func NewMarketRulesRegistryFromFile(path string) (*MarketRulesRegistry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var config MarketRulesConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse market rules config: %w", err)
	}
	return NewMarketRulesRegistryFromConfig(config)
}

func NewMarketRulesRegistryFromConfig(config MarketRulesConfig) (*MarketRulesRegistry, error) {
	minOrderNotional, err := parseConfigDecimal(config.MinOrderNotional, "min_order_notional", true)
	if err != nil {
		return nil, err
	}
	feeRate, err := parseConfigDecimal(config.FeeRate, "fee_rate", false)
	if err != nil {
		return nil, err
	}
	defaultMinOrderQuantity, err := parseConfigDecimal(config.DefaultMinOrderQuantity, "default_min_order_quantity", true)
	if err != nil {
		return nil, err
	}
	defaultBaseQuantityStep, err := parseConfigDecimal(config.DefaultBaseQuantityStep, "default_base_quantity_step", true)
	if err != nil {
		return nil, err
	}
	defaultMarketStatus, err := parseMarketStatusConfig(config.DefaultMarketStatus, "default_market_status")
	if err != nil {
		return nil, err
	}
	maxTickSize, err := parseConfigDecimal(config.MaxTickSize, "max_tick_size", true)
	if err != nil {
		return nil, err
	}

	tickRules := make([]krwTickRule, 0, len(config.TickRules))
	for index, tickRule := range config.TickRules {
		upperBound, err := parseConfigDecimal(tickRule.UpperBound, fmt.Sprintf("tick_rules[%d].upper_bound", index), true)
		if err != nil {
			return nil, err
		}
		tickSize, err := parseConfigDecimal(tickRule.TickSize, fmt.Sprintf("tick_rules[%d].tick_size", index), true)
		if err != nil {
			return nil, err
		}
		tickRules = append(tickRules, krwTickRule{
			upperBound: upperBound,
			tickSize:   tickSize,
		})
	}

	defaultBaseQuantityRule := BaseQuantityPolicy{
		MinOrderQuantity: defaultMinOrderQuantity,
		BaseQuantityStep: defaultBaseQuantityStep,
	}
	marketStatuses := make(map[string]MarketStatus, len(config.Markets))
	baseQuantityRules := make(map[string]BaseQuantityPolicy, len(config.Markets))
	for coinSymbol, marketConfig := range config.Markets {
		normalizedSymbol := normalizeCoinSymbol(coinSymbol)
		if normalizedSymbol == "" {
			return nil, NewValidationErrorf("markets coin symbol is required")
		}

		status := defaultMarketStatus
		if strings.TrimSpace(marketConfig.TradingStatus) != "" {
			status, err = parseMarketStatusConfig(marketConfig.TradingStatus, fmt.Sprintf("markets.%s.trading_status", normalizedSymbol))
			if err != nil {
				return nil, err
			}
		}
		marketStatuses[normalizedSymbol] = status

		minOrderQuantity := defaultMinOrderQuantity
		if strings.TrimSpace(marketConfig.MinOrderQuantity) != "" {
			minOrderQuantity, err = parseConfigDecimal(marketConfig.MinOrderQuantity, fmt.Sprintf("markets.%s.min_order_quantity", normalizedSymbol), true)
			if err != nil {
				return nil, err
			}
		}
		baseQuantityStep := defaultBaseQuantityStep
		if strings.TrimSpace(marketConfig.BaseQuantityStep) != "" {
			baseQuantityStep, err = parseConfigDecimal(marketConfig.BaseQuantityStep, fmt.Sprintf("markets.%s.base_quantity_step", normalizedSymbol), true)
			if err != nil {
				return nil, err
			}
		}
		baseQuantityRules[normalizedSymbol] = BaseQuantityPolicy{
			MinOrderQuantity: minOrderQuantity,
			BaseQuantityStep: baseQuantityStep,
		}
	}

	return &MarketRulesRegistry{
		minOrderNotional:        minOrderNotional,
		feeRate:                 feeRate,
		defaultMarketStatus:     defaultMarketStatus,
		marketStatuses:          marketStatuses,
		defaultBaseQuantityRule: defaultBaseQuantityRule,
		baseQuantityRules:       baseQuantityRules,
		tickRules:               tickRules,
		maxTickSize:             maxTickSize,
	}, nil
}

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

func parseConfigDecimal(value string, field string, mustBePositive bool) (decimal.Decimal, error) {
	parsed, err := decimal.NewFromString(strings.TrimSpace(value))
	if err != nil {
		return decimal.Zero, NewValidationErrorf("invalid %s", field)
	}
	if mustBePositive && !parsed.GreaterThan(decimal.Zero) {
		return decimal.Zero, NewValidationErrorf("%s must be greater than zero", field)
	}
	if !mustBePositive && parsed.IsNegative() {
		return decimal.Zero, NewValidationErrorf("%s must be zero or greater", field)
	}
	return parsed, nil
}

func parseMarketStatusConfig(value string, field string) (MarketStatus, error) {
	status := MarketStatus(strings.ToUpper(strings.TrimSpace(value)))
	switch status {
	case MarketStatusActive, MarketStatusHalted:
		return status, nil
	default:
		return "", NewValidationErrorf("invalid %s", field)
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
