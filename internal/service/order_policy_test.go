package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKRWTickSizeUsesPriceBands(t *testing.T) {
	tests := []struct {
		price string
		want  string
	}{
		{price: "0.000009", want: "0.00000001"},
		{price: "0.00009", want: "0.0000001"},
		{price: "0.0009", want: "0.000001"},
		{price: "0.00922", want: "0.00001"},
		{price: "0.09", want: "0.0001"},
		{price: "0.9", want: "0.001"},
		{price: "9.99", want: "0.01"},
		{price: "10", want: "0.1"},
		{price: "100", want: "1"},
		{price: "1000", want: "1"},
		{price: "4999", want: "1"},
		{price: "5000", want: "5"},
		{price: "10000", want: "10"},
		{price: "50000", want: "50"},
		{price: "100000", want: "100"},
		{price: "500000", want: "500"},
		{price: "1000000", want: "1000"},
		{price: "2000000", want: "1000"},
	}

	for _, tt := range tests {
		t.Run(tt.price, func(t *testing.T) {
			got := krwTickSize(decimal.RequireFromString(tt.price))
			assert.True(t, got.Equal(decimal.RequireFromString(tt.want)))
		})
	}
}

func TestKRWMarketRulesReturnsSerializablePolicy(t *testing.T) {
	rules, err := KRWMarketRules(" btc ")

	require.NoError(t, err)
	assert.Equal(t, "BTC", rules.CoinSymbol)
	assert.Equal(t, "KRW", rules.QuoteSymbol)
	assert.True(t, rules.TradingEnabled)
	assert.Equal(t, MarketStatusActive, rules.TradingStatus)
	assert.True(t, rules.MinOrderNotional.Equal(decimal.Zero))
	assert.True(t, rules.MinOrderQuantity.Equal(decimal.RequireFromString("0.00000001")))
	assert.True(t, rules.BaseQuantityStep.Equal(decimal.RequireFromString("0.00000001")))
	assert.True(t, rules.FeeRate.Equal(decimal.RequireFromString("0.0005")))
	require.Len(t, rules.TickRules, len(krwTickRules)+1)
	require.NotNil(t, rules.TickRules[0].UpperBound)
	assert.True(t, rules.TickRules[0].UpperBound.Equal(decimal.RequireFromString("0.00001")))
	assert.True(t, rules.TickRules[0].TickSize.Equal(decimal.RequireFromString("0.00000001")))
	assert.Nil(t, rules.TickRules[len(rules.TickRules)-1].UpperBound)
	assert.True(t, rules.TickRules[len(rules.TickRules)-1].TickSize.Equal(decimal.NewFromInt(1000)))
}

func TestKRWMarketRulesUsesCoinSpecificTradingStatus(t *testing.T) {
	tests := []struct {
		coinSymbol     string
		tradingEnabled bool
		tradingStatus  MarketStatus
	}{
		{coinSymbol: "BTC", tradingEnabled: true, tradingStatus: MarketStatusActive},
		{coinSymbol: "HALT", tradingEnabled: false, tradingStatus: MarketStatusHalted},
		{coinSymbol: "E2EFOO", tradingEnabled: true, tradingStatus: MarketStatusActive},
	}

	for _, tt := range tests {
		t.Run(tt.coinSymbol, func(t *testing.T) {
			rules, err := KRWMarketRules(tt.coinSymbol)

			require.NoError(t, err)
			assert.Equal(t, tt.tradingEnabled, rules.TradingEnabled)
			assert.Equal(t, tt.tradingStatus, rules.TradingStatus)
		})
	}
}

func TestKRWMarketRulesUsesCoinSpecificBaseQuantityPolicy(t *testing.T) {
	tests := []struct {
		coinSymbol       string
		minOrderQuantity string
		baseQuantityStep string
	}{
		{coinSymbol: "BTC", minOrderQuantity: "0.00000001", baseQuantityStep: "0.00000001"},
		{coinSymbol: "ETH", minOrderQuantity: "0.0000001", baseQuantityStep: "0.0000001"},
		{coinSymbol: "XRP", minOrderQuantity: "1", baseQuantityStep: "1"},
		{coinSymbol: "E2EFOO", minOrderQuantity: "0.00000001", baseQuantityStep: "0.00000001"},
	}

	for _, tt := range tests {
		t.Run(tt.coinSymbol, func(t *testing.T) {
			rules, err := KRWMarketRules(tt.coinSymbol)

			require.NoError(t, err)
			assert.True(t, rules.MinOrderQuantity.Equal(decimal.RequireFromString(tt.minOrderQuantity)))
			assert.True(t, rules.BaseQuantityStep.Equal(decimal.RequireFromString(tt.baseQuantityStep)))
		})
	}
}

func TestMarketRulesRegistrySharesQuantityPolicyAcrossRulesAndValidation(t *testing.T) {
	registry := NewDefaultMarketRulesRegistry()
	rules, err := registry.KRWMarketRules("xrp")
	require.NoError(t, err)

	err = registry.ValidateMarketSellOrder("xrp", decimal.RequireFromString("1.5"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "XRP order amount must align with quantity step "+rules.BaseQuantityStep.String())
}

func TestMarketRulesRegistryRejectsDisabledTradingAcrossOrderTypes(t *testing.T) {
	registry := NewDefaultMarketRulesRegistry()

	err := registry.ValidateLimitOrder("HALT", decimal.NewFromInt(5000), decimal.NewFromInt(1))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HALT market is not accepting orders")

	err = registry.ValidateMarketBuyOrder("HALT", decimal.NewFromInt(5000))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HALT market is not accepting orders")

	err = registry.ValidateMarketSellOrder("HALT", decimal.NewFromInt(1))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HALT market is not accepting orders")
}

func TestMarketRulesRegistryCopiesDefaultPolicyMap(t *testing.T) {
	registry := NewDefaultMarketRulesRegistry()
	krwBaseQuantityPolicies["ZZZ"] = BaseQuantityPolicy{
		MinOrderQuantity: decimal.NewFromInt(100),
		BaseQuantityStep: decimal.NewFromInt(100),
	}
	defer delete(krwBaseQuantityPolicies, "ZZZ")

	policy := registry.BaseQuantityPolicy("ZZZ")

	assert.True(t, policy.MinOrderQuantity.Equal(defaultMinOrderQuantity))
	assert.True(t, policy.BaseQuantityStep.Equal(defaultBaseQuantityStep))
}

func TestMarketRulesRegistryCopiesDefaultStatusMap(t *testing.T) {
	registry := NewDefaultMarketRulesRegistry()
	krwMarketStatuses["ZZZ"] = MarketStatusHalted
	defer delete(krwMarketStatuses, "ZZZ")

	assert.Equal(t, MarketStatusActive, registry.TradingStatus("ZZZ"))
}

func TestMarketRulesRegistryReturnsDefensiveTickRules(t *testing.T) {
	registry := NewDefaultMarketRulesRegistry()
	rules, err := registry.KRWMarketRules("BTC")
	require.NoError(t, err)
	require.NotNil(t, rules.TickRules[0].UpperBound)

	*rules.TickRules[0].UpperBound = decimal.NewFromInt(999)

	assert.True(t, registry.KRWTickSize(decimal.RequireFromString("0.5")).Equal(decimal.RequireFromString("0.001")))
}

func TestNewMarketRulesRegistryFromConfig(t *testing.T) {
	registry, err := NewMarketRulesRegistryFromConfig(MarketRulesConfig{
		MinOrderNotional:        "10000",
		FeeRate:                 "0.001",
		DefaultMarketStatus:     "ACTIVE",
		DefaultMinOrderQuantity: "0.01",
		DefaultBaseQuantityStep: "0.01",
		Markets: map[string]MarketRulesMarketConfig{
			"abc": {
				TradingStatus:    "HALTED",
				MinOrderQuantity: "5",
				BaseQuantityStep: "5",
			},
			"def": {},
		},
		TickRules: []MarketRulesTickConfig{
			{UpperBound: "1000", TickSize: "1"},
		},
		MaxTickSize: "10",
	})

	require.NoError(t, err)
	abcRules, err := registry.KRWMarketRules("abc")
	require.NoError(t, err)
	assert.Equal(t, "ABC", abcRules.CoinSymbol)
	assert.Equal(t, MarketStatusHalted, abcRules.TradingStatus)
	assert.False(t, abcRules.TradingEnabled)
	assert.True(t, abcRules.MinOrderNotional.Equal(decimal.NewFromInt(10000)))
	assert.True(t, abcRules.FeeRate.Equal(decimal.RequireFromString("0.001")))
	assert.True(t, abcRules.MinOrderQuantity.Equal(decimal.NewFromInt(5)))
	assert.True(t, abcRules.BaseQuantityStep.Equal(decimal.NewFromInt(5)))
	assert.True(t, registry.KRWTickSize(decimal.NewFromInt(1000)).Equal(decimal.NewFromInt(10)))

	defRules, err := registry.KRWMarketRules("def")
	require.NoError(t, err)
	assert.Equal(t, MarketStatusActive, defRules.TradingStatus)
	assert.True(t, defRules.MinOrderQuantity.Equal(decimal.RequireFromString("0.01")))
	assert.True(t, defRules.BaseQuantityStep.Equal(decimal.RequireFromString("0.01")))
}

func TestNewMarketRulesRegistryFromConfigRejectsInvalidConfig(t *testing.T) {
	_, err := NewMarketRulesRegistryFromConfig(MarketRulesConfig{
		MinOrderNotional:        "5000",
		FeeRate:                 "0.0005",
		DefaultMarketStatus:     "BROKEN",
		DefaultMinOrderQuantity: "0.00000001",
		DefaultBaseQuantityStep: "0.00000001",
		TickRules:               []MarketRulesTickConfig{{UpperBound: "1", TickSize: "0.00001"}},
		MaxTickSize:             "1000",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid default_market_status")
}

func TestNewMarketRulesRegistryFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "market_rules.json")
	require.NoError(t, os.WriteFile(path, []byte(`{
		"min_order_notional": "7000",
		"fee_rate": "0.0007",
		"default_market_status": "ACTIVE",
		"default_min_order_quantity": "0.001",
		"default_base_quantity_step": "0.001",
		"markets": {
			"abc": {
				"trading_status": "HALTED",
				"min_order_quantity": "2",
				"base_quantity_step": "2"
			}
		},
		"tick_rules": [{ "upper_bound": "1000", "tick_size": "1" }],
		"max_tick_size": "10"
	}`), 0o600))

	registry, err := NewMarketRulesRegistryFromFile(path)

	require.NoError(t, err)
	rules, err := registry.KRWMarketRules("abc")
	require.NoError(t, err)
	assert.Equal(t, MarketStatusHalted, rules.TradingStatus)
	assert.True(t, rules.MinOrderNotional.Equal(decimal.NewFromInt(7000)))
	assert.True(t, rules.MinOrderQuantity.Equal(decimal.NewFromInt(2)))
}

func TestNewMarketRulesRegistryFromEnv(t *testing.T) {
	path := writeMarketRulesConfigFile(t, `{
		"min_order_notional": "8000",
		"fee_rate": "0.0008",
		"default_market_status": "ACTIVE",
		"default_min_order_quantity": "0.01",
		"default_base_quantity_step": "0.01",
		"markets": {
			"abc": { "trading_status": "HALTED" }
		},
		"tick_rules": [{ "upper_bound": "1000", "tick_size": "1" }],
		"max_tick_size": "10"
	}`)
	t.Setenv(EnvMarketRulesPath, path)

	registry, err := NewMarketRulesRegistryFromEnv()

	require.NoError(t, err)
	rules, err := registry.KRWMarketRules("abc")
	require.NoError(t, err)
	assert.Equal(t, MarketStatusHalted, rules.TradingStatus)
	assert.True(t, rules.MinOrderNotional.Equal(decimal.NewFromInt(8000)))
	assert.True(t, rules.MinOrderQuantity.Equal(decimal.RequireFromString("0.01")))
}

func TestCommittedMarketRulesConfigFileLoads(t *testing.T) {
	registry, err := NewMarketRulesRegistryFromFile(filepath.Join("..", "..", "config", "market_rules.json"))

	require.NoError(t, err)
	xrpRules, err := registry.KRWMarketRules("xrp")
	require.NoError(t, err)
	assert.Equal(t, MarketStatusActive, xrpRules.TradingStatus)
	assert.True(t, xrpRules.MinOrderQuantity.Equal(decimal.NewFromInt(1)))
	haltRules, err := registry.KRWMarketRules("halt")
	require.NoError(t, err)
	assert.Equal(t, MarketStatusHalted, haltRules.TradingStatus)
}

func TestBuildOrderWithRegistryUsesInjectedMarketRules(t *testing.T) {
	registry, err := NewMarketRulesRegistryFromConfig(MarketRulesConfig{
		MinOrderNotional:        "5000",
		FeeRate:                 "0.0005",
		DefaultMarketStatus:     "ACTIVE",
		DefaultMinOrderQuantity: "0.00000001",
		DefaultBaseQuantityStep: "0.00000001",
		Markets: map[string]MarketRulesMarketConfig{
			"abc": {TradingStatus: "HALTED"},
		},
		TickRules:   []MarketRulesTickConfig{{UpperBound: "1000", TickSize: "1"}},
		MaxTickSize: "10",
	})
	require.NoError(t, err)

	order, err := BuildOrderWithRegistry(CreateOrderInput{
		UserID:     1,
		CoinSymbol: "ABC",
		Side:       "BUY",
		OrderType:  "LIMIT",
		Price:      "5000",
		Amount:     "1",
	}, registry)

	require.Error(t, err)
	assert.Nil(t, order)
	assert.Contains(t, err.Error(), "ABC market is not accepting orders")
}

func writeMarketRulesConfigFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "market_rules.json")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestKRWMarketRulesRejectsMissingCoinSymbol(t *testing.T) {
	rules, err := KRWMarketRules(" ")

	require.Error(t, err)
	assert.Empty(t, rules.CoinSymbol)
	assert.Contains(t, err.Error(), "coin_symbol is required")
}

func TestValidateLimitOrderPolicyAcceptsValidTickAndNotional(t *testing.T) {
	err := validateLimitOrderPolicy(
		"BTC",
		decimal.RequireFromString("50000"),
		decimal.RequireFromString("0.1"),
	)

	require.NoError(t, err)
}

func TestValidateLimitOrderPolicyRejectsPriceOutsideTick(t *testing.T) {
	err := validateLimitOrderPolicy(
		"BTC",
		decimal.RequireFromString("50001"),
		decimal.RequireFromString("1"),
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "tick size")
}

func TestValidateLimitOrderPolicyAllowsSmallNotionalByDefault(t *testing.T) {
	err := validateLimitOrderPolicy(
		"XRP",
		decimal.RequireFromString("1848"),
		decimal.NewFromInt(1),
	)

	require.NoError(t, err)
}

func TestMarketRulesRegistryRejectsSmallNotionalWhenConfigured(t *testing.T) {
	registry, err := NewMarketRulesRegistryFromConfig(MarketRulesConfig{
		MinOrderNotional:        "5000",
		FeeRate:                 "0.0005",
		DefaultMarketStatus:     "ACTIVE",
		DefaultMinOrderQuantity: "0.00000001",
		DefaultBaseQuantityStep: "0.00000001",
		Markets:                 map[string]MarketRulesMarketConfig{"btc": {}},
		TickRules:               []MarketRulesTickConfig{{UpperBound: "10000", TickSize: "1"}},
		MaxTickSize:             "1",
	})
	require.NoError(t, err)

	err = registry.ValidateLimitOrder("BTC", decimal.NewFromInt(1000), decimal.NewFromInt(1))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least 5000 KRW")

	err = registry.ValidateMarketBuyOrder("BTC", decimal.NewFromInt(1000))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least 5000 KRW")
}

func TestValidateLimitOrderPolicyRejectsAmountOutsideQuantityStep(t *testing.T) {
	err := validateLimitOrderPolicy(
		"BTC",
		decimal.RequireFromString("50000"),
		decimal.RequireFromString("0.000000015"),
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "BTC order amount must align with quantity step 0.00000001")
}

func TestValidateLimitOrderPolicyRejectsSmallQuantity(t *testing.T) {
	err := validateLimitOrderPolicy(
		"BTC",
		decimal.RequireFromString("1000000000000"),
		decimal.RequireFromString("0.000000001"),
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "BTC order amount must be at least 0.00000001")
}

func TestValidateMarketBuyOrderPolicyAllowsSmallQuoteAmountByDefault(t *testing.T) {
	err := validateMarketBuyOrderPolicy("BTC", decimal.NewFromInt(1))

	require.NoError(t, err)
}

func TestValidateMarketSellOrderPolicyRejectsAmountOutsideQuantityStep(t *testing.T) {
	err := validateMarketSellOrderPolicy("BTC", decimal.RequireFromString("0.123456789"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "quantity step 0.00000001")
}

func TestValidateMarketSellOrderPolicyUsesCoinSpecificQuantityStep(t *testing.T) {
	err := validateMarketSellOrderPolicy("XRP", decimal.RequireFromString("0.5"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "XRP order amount must be at least 1")

	err = validateMarketSellOrderPolicy("XRP", decimal.RequireFromString("1.5"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "XRP order amount must align with quantity step 1")
}

func TestBuildOrderRejectsInvalidLimitOrderPolicy(t *testing.T) {
	tests := []struct {
		name   string
		price  string
		amount string
		want   string
	}{
		{name: "invalid tick", price: "10001", amount: "1", want: "tick size"},
		{name: "invalid quantity step", price: "1000000000000", amount: "0.000000015", want: "quantity step"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			order, err := BuildOrder(CreateOrderInput{
				UserID:     1,
				CoinSymbol: "BTC",
				Side:       "BUY",
				OrderType:  "LIMIT",
				Price:      tt.price,
				Amount:     tt.amount,
			})

			require.Error(t, err)
			assert.Nil(t, order)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestBuildOrderRejectsDisabledMarket(t *testing.T) {
	order, err := BuildOrder(CreateOrderInput{
		UserID:     1,
		CoinSymbol: "HALT",
		Side:       "BUY",
		OrderType:  "LIMIT",
		Price:      "5000",
		Amount:     "1",
	})

	require.Error(t, err)
	assert.Nil(t, order)
	assert.Contains(t, err.Error(), "HALT market is not accepting orders")
}
