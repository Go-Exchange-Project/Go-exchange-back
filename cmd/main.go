package main

import (
	"github.com/Go-Exchange-Project/Go-exchange-back/config"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/handler"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/gin-gonic/gin"
)

func main() {
	config.ConnectDB()

	config.DB.AutoMigrate(
		&model.User{},
		&model.Order{},
		&model.Wallet{},
		&model.Trade{},
	)

	me := matching.NewMatchingEngine()
	me.Start()
	
	// 의존성 주입
	orderRepo := repository.NewOrderRepository(config.DB)
	orderService := service.NewOrderService(orderRepo, me)
	orderHandler := handler.NewOrderHandler(orderService)

	// TradeCh 결과를 DB에 저장하는 고루틴
	go func() {
    for trade := range me.TradeCh {
        config.DB.Create(trade)

        // 매수 주문 상태 업데이트
        buyOrder, _ := orderRepo.FindByID(trade.BuyOrderID)
        buyStatus := model.OrderStatusFilled
        if trade.Quantity.LessThan(buyOrder.Amount) {
            buyStatus = model.OrderStatusPartial
        }
        orderRepo.UpdateOrderStatus(trade.BuyOrderID, buyStatus, trade.Quantity)

        // 매도 주문 상태 업데이트
        sellOrder, _ := orderRepo.FindByID(trade.SellOrderID)
        sellStatus := model.OrderStatusFilled
        if trade.Quantity.LessThan(sellOrder.Amount) {
            sellStatus = model.OrderStatusPartial
        }
        orderRepo.UpdateOrderStatus(trade.SellOrderID, sellStatus, trade.Quantity)
    }
}()


	r := gin.Default()
	r.GET("/ping", func(c *gin.Context){
		c.JSON(200, gin.H{
			"message": "pong",
		})
	})

	r.POST("/orders", orderHandler.CreateOrder)

	r.Run(":8080")
}