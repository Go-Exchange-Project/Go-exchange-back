package handler

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/auth"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

type OrderHandler struct {
	OrderService *service.OrderService
}

type CreateOrderRequest struct {
	CoinSymbol string `json:"coin_symbol" binding:"required"`
	Side       string `json:"side" binding:"required"`
	OrderType  string `json:"order_type"`
	Price      string `json:"price" binding:"required"`
	Amount     string `json:"amount" binding:"required"`
}

func NewOrderHandler(service *service.OrderService) *OrderHandler {
	return &OrderHandler{OrderService: service}
}

func (h *OrderHandler) CreateOrder(c *gin.Context) {
	userID, ok := authenticatedUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authenticated user is required"})
		return
	}

	var req CreateOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	order, err := h.OrderService.CreateOrder(service.CreateOrderInput{
		UserID:     userID,
		CoinSymbol: req.CoinSymbol,
		Side:       req.Side,
		OrderType:  req.OrderType,
		Price:      req.Price,
		Amount:     req.Amount,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":  "order accepted",
		"order_id": order.ID,
	})
}

func (h *OrderHandler) ListOrders(c *gin.Context) {
	userID, ok := authenticatedUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authenticated user is required"})
		return
	}

	orders, err := h.OrderService.ListOrders(service.ListOrdersInput{
		UserID:     userID,
		Status:     c.Query("status"),
		CoinSymbol: c.Query("coin_symbol"),
		Limit:      parseLimitQuery(c),
	})
	if err != nil {
		writeServiceError(c, err)
		return
	}

	response := make([]OrderResponse, 0, len(orders))
	for _, order := range orders {
		response = append(response, orderResponse(order))
	}
	c.JSON(http.StatusOK, gin.H{"orders": response})
}

func (h *OrderHandler) GetOrder(c *gin.Context) {
	userID, ok := authenticatedUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authenticated user is required"})
		return
	}

	orderID, err := parseIDParam(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid order id"})
		return
	}

	order, err := h.OrderService.GetOrder(userID, orderID)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"order": orderResponse(*order)})
}

func (h *OrderHandler) ListWallets(c *gin.Context) {
	userID, ok := authenticatedUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authenticated user is required"})
		return
	}

	wallets, err := h.OrderService.ListWallets(userID)
	if err != nil {
		writeServiceError(c, err)
		return
	}

	response := make([]WalletResponse, 0, len(wallets))
	for _, wallet := range wallets {
		response = append(response, walletResponse(wallet))
	}
	c.JSON(http.StatusOK, gin.H{"wallets": response})
}

func (h *OrderHandler) ListTrades(c *gin.Context) {
	userID, ok := authenticatedUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authenticated user is required"})
		return
	}

	trades, err := h.OrderService.ListTrades(service.ListTradesInput{
		UserID:     userID,
		CoinSymbol: c.Query("coin_symbol"),
		Limit:      parseLimitQuery(c),
	})
	if err != nil {
		writeServiceError(c, err)
		return
	}

	response := make([]TradeResponse, 0, len(trades))
	for _, trade := range trades {
		response = append(response, tradeResponse(trade))
	}
	c.JSON(http.StatusOK, gin.H{"trades": response})
}

func (h *OrderHandler) CancelOrder(c *gin.Context) {
	userID, ok := authenticatedUserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authenticated user is required"})
		return
	}

	orderID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || orderID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid order id"})
		return
	}

	result, err := h.OrderService.CancelOrder(service.CancelOrderInput{
		UserID:  userID,
		OrderID: uint(orderID),
	})
	if err != nil {
		status := http.StatusBadRequest
		if result != nil && result.Status == "CANCELLED" {
			status = http.StatusInternalServerError
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":         "order cancelled",
		"order_id":        result.OrderID,
		"status":          result.Status,
		"released_asset":  result.ReleasedAsset,
		"released_amount": result.ReleasedAmount.String(),
		"engine_removed":  result.EngineRemoved,
	})
}

type OrderResponse struct {
	ID           uint              `json:"id"`
	CoinSymbol   string            `json:"coin_symbol"`
	Side         model.OrderSide   `json:"side"`
	OrderType    model.OrderType   `json:"order_type"`
	Status       model.OrderStatus `json:"status"`
	Price        string            `json:"price"`
	Amount       string            `json:"amount"`
	FilledAmount string            `json:"filled_amount"`
	Remaining    string            `json:"remaining"`
	CreatedAt    time.Time         `json:"created_at"`
}

type WalletResponse struct {
	ID               uint   `json:"id"`
	CoinSymbol       string `json:"coin_symbol"`
	AvailableBalance string `json:"available_balance"`
	LockedBalance    string `json:"locked_balance"`
	TotalBalance     string `json:"total_balance"`
	AvgBuyPrice      string `json:"avg_buy_price"`
}

type TradeResponse struct {
	ID          uint            `json:"id"`
	CoinSymbol  string          `json:"coin_symbol"`
	Side        model.OrderSide `json:"side"`
	Price       string          `json:"price"`
	Quantity    string          `json:"quantity"`
	TradedAt    time.Time       `json:"traded_at"`
	BuyOrderID  uint            `json:"buy_order_id"`
	SellOrderID uint            `json:"sell_order_id"`
}

func orderResponse(order model.Order) OrderResponse {
	remaining := order.Amount.Sub(order.FilledAmount)
	if remaining.IsNegative() {
		remaining = decimal.Zero
	}
	return OrderResponse{
		ID:           order.ID,
		CoinSymbol:   order.CoinSymbol,
		Side:         order.Side,
		OrderType:    order.OrderType,
		Status:       order.Status,
		Price:        order.Price.String(),
		Amount:       order.Amount.String(),
		FilledAmount: order.FilledAmount.String(),
		Remaining:    remaining.String(),
		CreatedAt:    order.CreatedAt,
	}
}

func walletResponse(wallet model.Wallet) WalletResponse {
	total := wallet.AvailableBalance.Add(wallet.LockedBalance)
	return WalletResponse{
		ID:               wallet.ID,
		CoinSymbol:       wallet.CoinSymbol,
		AvailableBalance: wallet.AvailableBalance.String(),
		LockedBalance:    wallet.LockedBalance.String(),
		TotalBalance:     total.String(),
		AvgBuyPrice:      wallet.AvgBuyPrice.String(),
	}
}

func tradeResponse(trade repository.UserTrade) TradeResponse {
	return TradeResponse{
		ID:          trade.ID,
		CoinSymbol:  trade.CoinSymbol,
		Side:        trade.Side,
		Price:       trade.Price.String(),
		Quantity:    trade.Quantity.String(),
		TradedAt:    trade.TradedAt,
		BuyOrderID:  trade.BuyOrderID,
		SellOrderID: trade.SellOrderID,
	}
}

func parseIDParam(value string) (uint, error) {
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || parsed == 0 {
		return 0, errors.New("invalid id")
	}
	return uint(parsed), nil
}

func parseLimitQuery(c *gin.Context) int {
	value := c.Query("limit")
	if value == "" {
		return 0
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return parsed
}

func writeServiceError(c *gin.Context, err error) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
}

func authenticatedUserID(c *gin.Context) (uint, bool) {
	value, exists := c.Get(auth.UserIDContextKey)
	if !exists {
		return 0, false
	}

	switch userID := value.(type) {
	case uint:
		return userID, userID != 0
	case uint64:
		return uint(userID), userID != 0
	case int:
		if userID <= 0 {
			return 0, false
		}
		return uint(userID), true
	default:
		return 0, false
	}
}
