package service

import (
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
		{price: "0.00922", want: "0.00001"},
		{price: "9.99", want: "0.01"},
		{price: "10", want: "0.1"},
		{price: "100", want: "1"},
		{price: "1000", want: "5"},
		{price: "10000", want: "10"},
		{price: "100000", want: "50"},
		{price: "500000", want: "100"},
		{price: "1000000", want: "500"},
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
	assert.True(t, rules.MinOrderNotional.Equal(decimal.NewFromInt(5000)))
	assert.True(t, rules.MinOrderQuantity.Equal(decimal.RequireFromString("0.00000001")))
	assert.True(t, rules.BaseQuantityStep.Equal(decimal.RequireFromString("0.00000001")))
	assert.True(t, rules.FeeRate.Equal(decimal.RequireFromString("0.0005")))
	require.Len(t, rules.TickRules, len(krwTickRules)+1)
	require.NotNil(t, rules.TickRules[0].UpperBound)
	assert.True(t, rules.TickRules[0].UpperBound.Equal(decimal.NewFromInt(1)))
	assert.True(t, rules.TickRules[0].TickSize.Equal(decimal.RequireFromString("0.00001")))
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

	assert.True(t, registry.KRWTickSize(decimal.RequireFromString("0.5")).Equal(decimal.RequireFromString("0.00001")))
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

func TestValidateLimitOrderPolicyRejectsSmallNotional(t *testing.T) {
	err := validateLimitOrderPolicy(
		"BTC",
		decimal.RequireFromString("50000"),
		decimal.RequireFromString("0.099"),
	)

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

func TestValidateMarketBuyOrderPolicyRejectsSmallQuoteAmount(t *testing.T) {
	err := validateMarketBuyOrderPolicy("BTC", decimal.RequireFromString("4999.999"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least 5000 KRW")
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
		{name: "below minimum notional", price: "10000", amount: "0.499", want: "at least 5000 KRW"},
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
