package handler

import (
	"net/http"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/gin-gonic/gin"
)

type MarketHandler struct{}

type MarketRulesResponse struct {
	CoinSymbol       string                   `json:"coin_symbol"`
	QuoteSymbol      string                   `json:"quote_symbol"`
	TradingEnabled   bool                     `json:"trading_enabled"`
	TradingStatus    string                   `json:"trading_status"`
	MinOrderNotional string                   `json:"min_order_notional"`
	MinOrderQuantity string                   `json:"min_order_quantity"`
	BaseQuantityStep string                   `json:"base_quantity_step"`
	FeeRate          string                   `json:"fee_rate"`
	TickRules        []MarketTickRuleResponse `json:"tick_rules"`
}

type MarketTickRuleResponse struct {
	UpperBound *string `json:"upper_bound"`
	TickSize   string  `json:"tick_size"`
}

func NewMarketHandler() *MarketHandler {
	return &MarketHandler{}
}

func (h *MarketHandler) GetRules(c *gin.Context) {
	rules, err := service.KRWMarketRules(c.Query("coin_symbol"))
	if err != nil {
		writeServiceError(c, err)
		return
	}

	c.JSON(http.StatusOK, marketRulesResponse(rules))
}

func marketRulesResponse(rules service.MarketRules) MarketRulesResponse {
	tickRules := make([]MarketTickRuleResponse, 0, len(rules.TickRules))
	for _, rule := range rules.TickRules {
		var upperBound *string
		if rule.UpperBound != nil {
			value := rule.UpperBound.String()
			upperBound = &value
		}
		tickRules = append(tickRules, MarketTickRuleResponse{
			UpperBound: upperBound,
			TickSize:   rule.TickSize.String(),
		})
	}

	return MarketRulesResponse{
		CoinSymbol:       rules.CoinSymbol,
		QuoteSymbol:      rules.QuoteSymbol,
		TradingEnabled:   rules.TradingEnabled,
		TradingStatus:    string(rules.TradingStatus),
		MinOrderNotional: rules.MinOrderNotional.String(),
		MinOrderQuantity: rules.MinOrderQuantity.String(),
		BaseQuantityStep: rules.BaseQuantityStep.String(),
		FeeRate:          rules.FeeRate.String(),
		TickRules:        tickRules,
	}
}
