package main

import (
	"github.com/Go-Exchange-Project/Go-exchange-back/config"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/handler"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
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

			orderRepo.UpdateOrderStatus(trade.BuyOrderID, model.OrderStatusFilled, trade.Quantity)
        	orderRepo.UpdateOrderStatus(trade.SellOrderID, model.OrderStatusFilled, trade.Quantity)
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