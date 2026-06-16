package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeOrderBookSnapshotProvider struct {
	coinSymbol string
	depth      int
	snapshot   matching.OrderBookSnapshot
	err        error
}

func (f *fakeOrderBookSnapshotProvider) RequestOrderBookSnapshot(coinSymbol string, depth int) (matching.OrderBookSnapshot, error) {
	f.coinSymbol = coinSymbol
	f.depth = depth
	if f.err != nil {
		return matching.OrderBookSnapshot{}, f.err
	}
	return f.snapshot, nil
}

func TestOrderBookHandlerGetSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	provider := &fakeOrderBookSnapshotProvider{
		snapshot: matching.OrderBookSnapshot{
			CoinSymbol: "AVAX",
			Asks: []matching.PriceLevelData{
				{Price: decimal.NewFromInt(10200), Quantity: decimal.NewFromInt(1)},
				{Price: decimal.NewFromInt(10300), Quantity: decimal.NewFromInt(1)},
			},
		},
	}
	router := gin.New()
	router.GET("/orderbook", NewOrderBookHandler(provider).GetSnapshot)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/orderbook?coin_symbol=avax&depth=2", nil)
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "AVAX", provider.coinSymbol)
	assert.Equal(t, 2, provider.depth)
	body := decodeDataResponse[matching.OrderBookSnapshot](t, w.Body.Bytes())
	assert.Equal(t, "AVAX", body.CoinSymbol)
	require.Len(t, body.Asks, 2)
	assert.True(t, body.Asks[0].Price.Equal(decimal.NewFromInt(10200)))
	assert.True(t, body.Asks[1].Price.Equal(decimal.NewFromInt(10300)))
}

func TestOrderBookHandlerRejectsMissingCoinSymbol(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/orderbook", NewOrderBookHandler(&fakeOrderBookSnapshotProvider{}).GetSnapshot)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/orderbook", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
}

func TestOrderBookHandlerReturnsProviderError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/orderbook", NewOrderBookHandler(&fakeOrderBookSnapshotProvider{err: errors.New("engine failed")}).GetSnapshot)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/orderbook?coin_symbol=AVAX", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}
