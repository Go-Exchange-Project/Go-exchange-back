package main

import (
	"encoding/json"
	"fmt"

	"github.com/Go-Exchange-Project/Go-exchange-back/config"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/handler"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/upbit"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/ws"
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

	// Hub 생성 및 실행
	hub := ws.NewHub()
	go hub.Run()

	// 업비트 WebSocket 연결
	upbitClient, err := upbit.NewUpbitClient()
	if err != nil {
		panic(err)
	}
	upbitClient.Subscribe([]string{"KRW-BTC"})

	// 업비트 시세를 Hub로 브로드캐스트
	go upbitClient.Listen(func(price float64) {
		msg := fmt.Sprintf(`{"type":"ticker","price":%f}`, price)
		hub.Broadcast <- []byte(msg)
	})

	// 의존성 주입
	orderRepo := repository.NewOrderRepository(config.DB)
	walletRepo := repository.NewWalletRepository(config.DB)
	orderService := service.NewOrderService(orderRepo, walletRepo, me)
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

			// 매수자
			buyWallet, _ := walletRepo.FindByUserID(1)
			newKRW := buyWallet.KRW.Sub(trade.Price.Mul(trade.Quantity))
			walletRepo.UpdateKRW(1, newKRW)
			orderRepo.UpdateOrderStatus(trade.BuyOrderID, buyStatus, trade.Quantity)

			// 매도 주문 상태 업데이트
			sellOrder, _ := orderRepo.FindByID(trade.SellOrderID)
			sellStatus := model.OrderStatusFilled
			if trade.Quantity.LessThan(sellOrder.Amount) {
				sellStatus = model.OrderStatusPartial
			}

			// 매도자
			sellWallet, _ := walletRepo.FindByUserID(2)
			newQuantity := sellWallet.Quantity.Sub(trade.Quantity)
			walletRepo.UpdateCoinQuantity(2, trade.CoinSymbol, newQuantity)
			orderRepo.UpdateOrderStatus(trade.SellOrderID, sellStatus, trade.Quantity)

			// 오더북 스냅샷 브로드캐스트
			snapshot := me.GetOrderBookSnapshot()
			snapshotJSON, _ := json.Marshal(map[string]interface{}{
				"type": "orderbook",
				"data": snapshot,
			})
			hub.Broadcast <- snapshotJSON
		}
	}()

	r := gin.Default()
	r.GET("/ping", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"message": "pong",
		})
	})

	// WebSocket 라우터 등록
	r.GET("/ws", func(c *gin.Context) {
		ws.ServeWs(hub, c)
	})

	r.POST("/orders", orderHandler.CreateOrder)

	r.Run(":8080")
}
