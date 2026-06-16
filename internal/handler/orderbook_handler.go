package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/httpapi"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/gin-gonic/gin"
)

type OrderBookSnapshotProvider interface {
	RequestOrderBookSnapshot(coinSymbol string, depth int) (matching.OrderBookSnapshot, error)
}

type OrderBookHandler struct {
	SnapshotProvider OrderBookSnapshotProvider
}

func NewOrderBookHandler(provider OrderBookSnapshotProvider) *OrderBookHandler {
	return &OrderBookHandler{SnapshotProvider: provider}
}

func (h *OrderBookHandler) GetSnapshot(c *gin.Context) {
	coinSymbol := strings.ToUpper(strings.TrimSpace(c.Query("coin_symbol")))
	if coinSymbol == "" {
		httpapi.WriteError(c, http.StatusUnprocessableEntity, httpapi.CodeValidation, "coin_symbol is required")
		return
	}
	if h == nil || h.SnapshotProvider == nil {
		httpapi.WriteError(c, http.StatusInternalServerError, httpapi.CodeInternal, "orderbook snapshot provider is required")
		return
	}

	depth, err := parseDepthQuery(c.Query("depth"))
	if err != nil {
		httpapi.WriteError(c, http.StatusUnprocessableEntity, httpapi.CodeValidation, err.Error())
		return
	}

	snapshot, err := h.SnapshotProvider.RequestOrderBookSnapshot(coinSymbol, depth)
	if err != nil {
		httpapi.WriteError(c, http.StatusInternalServerError, httpapi.CodeInternal, err.Error())
		return
	}

	httpapi.WriteData(c, http.StatusOK, snapshot)
}

func parseDepthQuery(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return matching.DefaultSnapshotDepth, nil
	}
	depth, err := strconv.Atoi(value)
	if err != nil || depth <= 0 {
		return 0, errors.New("depth must be a positive integer")
	}
	return depth, nil
}
