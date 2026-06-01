package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarketRulesResponseUsesDecimalStringsAndOpenEndedFinalTick(t *testing.T) {
	rules, err := service.KRWMarketRules("btc")
	require.NoError(t, err)

	response := marketRulesResponse(rules)

	assert.Equal(t, "BTC", response.CoinSymbol)
	assert.Equal(t, "KRW", response.QuoteSymbol)
	assert.True(t, response.TradingEnabled)
	assert.Equal(t, "ACTIVE", response.TradingStatus)
	assert.Equal(t, "5000", response.MinOrderNotional)
	assert.Equal(t, "0.00000001", response.MinOrderQuantity)
	assert.Equal(t, "0.00000001", response.BaseQuantityStep)
	assert.Equal(t, "0.0005", response.FeeRate)
	require.NotEmpty(t, response.TickRules)
	assert.Equal(t, "1", *response.TickRules[0].UpperBound)
	assert.Equal(t, "0.00001", response.TickRules[0].TickSize)
	assert.Nil(t, response.TickRules[len(response.TickRules)-1].UpperBound)
	assert.Equal(t, "1000", response.TickRules[len(response.TickRules)-1].TickSize)
}

func TestMarketHandlerGetRules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/markets/rules", NewMarketHandler().GetRules)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/markets/rules?coin_symbol=btc", nil)
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	body := decodeDataResponse[MarketRulesResponse](t, w.Body.Bytes())
	assert.Equal(t, "BTC", body.CoinSymbol)
	assert.Equal(t, "KRW", body.QuoteSymbol)
	assert.True(t, body.TradingEnabled)
	assert.Equal(t, "ACTIVE", body.TradingStatus)
	assert.Equal(t, "5000", body.MinOrderNotional)
	assert.Equal(t, "0.00000001", body.MinOrderQuantity)
	assert.Equal(t, "0.00000001", body.BaseQuantityStep)
	assert.Equal(t, "0.0005", body.FeeRate)
	require.Len(t, body.TickRules, 10)
}

func TestMarketHandlerGetRulesUsesCoinSpecificQuantityPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/markets/rules", NewMarketHandler().GetRules)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/markets/rules?coin_symbol=xrp", nil)
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	body := decodeDataResponse[MarketRulesResponse](t, w.Body.Bytes())
	assert.Equal(t, "XRP", body.CoinSymbol)
	assert.Equal(t, "1", body.MinOrderQuantity)
	assert.Equal(t, "1", body.BaseQuantityStep)
}

func TestMarketHandlerGetRulesUsesCoinSpecificTradingStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/markets/rules", NewMarketHandler().GetRules)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/markets/rules?coin_symbol=halt", nil)
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	body := decodeDataResponse[MarketRulesResponse](t, w.Body.Bytes())
	assert.Equal(t, "HALT", body.CoinSymbol)
	assert.False(t, body.TradingEnabled)
	assert.Equal(t, "HALTED", body.TradingStatus)
}

func TestMarketHandlerGetRulesUsesInjectedRegistry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry, err := service.NewMarketRulesRegistryFromConfig(service.MarketRulesConfig{
		MinOrderNotional:        "7000",
		FeeRate:                 "0.0007",
		DefaultMarketStatus:     "ACTIVE",
		DefaultMinOrderQuantity: "0.001",
		DefaultBaseQuantityStep: "0.001",
		Markets: map[string]service.MarketRulesMarketConfig{
			"abc": {
				TradingStatus:    "HALTED",
				MinOrderQuantity: "2",
				BaseQuantityStep: "2",
			},
		},
		TickRules:   []service.MarketRulesTickConfig{{UpperBound: "1000", TickSize: "1"}},
		MaxTickSize: "10",
	})
	require.NoError(t, err)
	router := gin.New()
	router.GET("/markets/rules", NewMarketHandler(registry).GetRules)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/markets/rules?coin_symbol=abc", nil)
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	body := decodeDataResponse[MarketRulesResponse](t, w.Body.Bytes())
	assert.Equal(t, "ABC", body.CoinSymbol)
	assert.False(t, body.TradingEnabled)
	assert.Equal(t, "HALTED", body.TradingStatus)
	assert.Equal(t, decimal.NewFromInt(7000).String(), body.MinOrderNotional)
	assert.Equal(t, "2", body.MinOrderQuantity)
	assert.Equal(t, "2", body.BaseQuantityStep)
}

func TestMarketHandlerRejectsMissingCoinSymbol(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/markets/rules", NewMarketHandler().GetRules)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/markets/rules", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
}

func decodeDataResponse[T any](t *testing.T, body []byte) T {
	t.Helper()

	var envelope struct {
		Data T `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &envelope))
	return envelope.Data
}
