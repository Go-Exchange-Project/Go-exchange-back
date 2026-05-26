package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/auth"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/httpapi"
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
	Price      string `json:"price"`
	Amount     string `json:"amount"`
	QuoteAmount string `json:"quote_amount"`
}

func NewOrderHandler(service *service.OrderService) *OrderHandler {
	return &OrderHandler{OrderService: service}
}

func (h *OrderHandler) CreateOrder(c *gin.Context) {
	userID, ok := authenticatedUserID(c)
	if !ok {
		httpapi.WriteError(c, http.StatusUnauthorized, httpapi.CodeAuthRequired, "authenticated user is required")
		return
	}

	var req CreateOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeBindingError(c, err)
		return
	}

	order, err := h.OrderService.CreateOrder(service.CreateOrderInput{
		UserID:     userID,
		CoinSymbol: req.CoinSymbol,
		Side:       req.Side,
		OrderType:  req.OrderType,
		Price:      req.Price,
		Amount:     req.Amount,
		QuoteAmount: req.QuoteAmount,
	})
	if err != nil {
		writeServiceError(c, err)
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
		httpapi.WriteError(c, http.StatusUnauthorized, httpapi.CodeAuthRequired, "authenticated user is required")
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
		httpapi.WriteError(c, http.StatusUnauthorized, httpapi.CodeAuthRequired, "authenticated user is required")
		return
	}

	orderID, err := parseIDParam(c.Param("id"))
	if err != nil {
		httpapi.WriteError(c, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid order id")
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
		httpapi.WriteError(c, http.StatusUnauthorized, httpapi.CodeAuthRequired, "authenticated user is required")
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
		httpapi.WriteError(c, http.StatusUnauthorized, httpapi.CodeAuthRequired, "authenticated user is required")
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
		httpapi.WriteError(c, http.StatusUnauthorized, httpapi.CodeAuthRequired, "authenticated user is required")
		return
	}

	orderID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || orderID == 0 {
		httpapi.WriteError(c, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid order id")
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
		} else {
			status = serviceErrorStatus(err)
		}
		httpapi.WriteError(c, status, errorCodeForStatus(status), err.Error())
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
	QuoteAmount  string            `json:"quote_amount"`
	FilledAmount string            `json:"filled_amount"`
	FilledQuoteAmount string       `json:"filled_quote_amount"`
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
	ID             uint            `json:"id"`
	IdempotencyKey string          `json:"idempotency_key"`
	EngineSequence int64           `json:"engine_sequence"`
	EngineEventID  string          `json:"engine_event_id"`
	CoinSymbol     string          `json:"coin_symbol"`
	Side           model.OrderSide `json:"side"`
	Price          string          `json:"price"`
	Quantity       string          `json:"quantity"`
	FeeRate        string          `json:"fee_rate"`
	BuyerFee       string          `json:"buyer_fee"`
	BuyerFeeAsset  string          `json:"buyer_fee_asset"`
	SellerFee      string          `json:"seller_fee"`
	SellerFeeAsset string          `json:"seller_fee_asset"`
	TradedAt       time.Time       `json:"traded_at"`
	BuyOrderID     uint            `json:"buy_order_id"`
	SellOrderID    uint            `json:"sell_order_id"`
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
		QuoteAmount:  order.QuoteAmount.String(),
		FilledAmount: order.FilledAmount.String(),
		FilledQuoteAmount: order.FilledQuoteAmount.String(),
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
		ID:             trade.ID,
		IdempotencyKey: trade.IdempotencyKey,
		EngineSequence: trade.EngineSequence,
		EngineEventID:  trade.EngineEventID,
		CoinSymbol:     trade.CoinSymbol,
		Side:           trade.Side,
		Price:          trade.Price.String(),
		Quantity:       trade.Quantity.String(),
		FeeRate:        trade.FeeRate.String(),
		BuyerFee:       trade.BuyerFee.String(),
		BuyerFeeAsset:  trade.BuyerFeeAsset,
		SellerFee:      trade.SellerFee.String(),
		SellerFeeAsset: trade.SellerFeeAsset,
		TradedAt:       trade.TradedAt,
		BuyOrderID:     trade.BuyOrderID,
		SellOrderID:    trade.SellOrderID,
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
	status := serviceErrorStatus(err)
	message := err.Error()
	if errors.Is(err, gorm.ErrRecordNotFound) {
		message = "not found"
	}
	httpapi.WriteError(c, status, errorCodeForStatus(status), message)
}

func writeBindingError(c *gin.Context, err error) {
	httpapi.WriteError(c, http.StatusUnprocessableEntity, httpapi.CodeValidation, err.Error())
}

func serviceErrorStatus(err error) int {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return http.StatusNotFound
	}

	if kind, ok := service.DomainErrorKind(err); ok {
		switch kind {
		case service.ErrorKindValidation:
			return http.StatusUnprocessableEntity
		case service.ErrorKindConflict:
			return http.StatusConflict
		case service.ErrorKindForbidden:
			return http.StatusForbidden
		}
	}

	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "does not belong"):
		return http.StatusForbidden
	case strings.Contains(message, "insufficient"),
		strings.Contains(message, "cannot be cancelled"),
		strings.Contains(message, "no remaining quantity"),
		strings.Contains(message, "already registered"):
		return http.StatusConflict
	case strings.Contains(message, "invalid"),
		strings.Contains(message, "required"),
		strings.Contains(message, "must be"),
		strings.Contains(message, "not supported"):
		return http.StatusUnprocessableEntity
	default:
		return http.StatusBadRequest
	}
}

func errorCodeForStatus(status int) string {
	return httpapi.CodeForStatus(status)
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
