// Handler가 받아야 할 것
// HTTP 요청에서 주문 정보 꺼내기
// Service로 넘기기

package handler

import (
	"github.com/gin-gonic/gin"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
)

type OrderHandler struct {
	OrderService *service.OrderService
}

// 사용자가 거래소에 매수/매도 버튼 눌렀을 때 받아야 할 정보
type CreateOrderRequest struct {
	CoinSymbol string `json:"coin_symbol"`
	Side	string	`json:"side"`
	Price	int64	`json:"price"`
	Amount	int64	`json:"amount"`
}

func NewOrderHandler(service *service.OrderService) *OrderHandler {
	return &OrderHandler{OrderService: service}
}

// HTTP 요청 받기
func (h *OrderHandler) CreateOrder(c *gin.Context){
	var req CreateOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil{
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	err := h.OrderService.CreateOrder(req.CoinSymbol, req.Side, req.Price, req.Amount)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	c.JSON(200, gin.H{"message":"주문 접수 완료"})
}

