package handler

import (
	"net/http"
	"strconv"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/gin-gonic/gin"
)

const temporaryUserID uint = 1

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
	var req CreateOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	order, err := h.OrderService.CreateOrder(service.CreateOrderInput{
		UserID:     temporaryUserID, // TODO: Replace with authenticated user ID.
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

func (h *OrderHandler) CancelOrder(c *gin.Context) {
	orderID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || orderID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid order id"})
		return
	}

	result, err := h.OrderService.CancelOrder(service.CancelOrderInput{
		UserID:  temporaryUserID, // TODO: Replace with authenticated user ID.
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
