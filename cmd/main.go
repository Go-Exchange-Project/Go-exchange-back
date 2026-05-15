package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/config"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/handler"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/upbit"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/ws"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

func main() {
	config.ConnectDB()

	config.DB.AutoMigrate(
		&model.User{},
		&model.Order{},
		&model.Wallet{},
		&model.Trade{},
		&model.FailedSettlement{},
	)

	me := matching.NewMatchingEngine()
	me.Start()

	hub := ws.NewHub()
	go hub.Run()

	orderRepo := repository.NewOrderRepository(config.DB)
	walletRepo := repository.NewWalletRepository(config.DB)
	orderService := service.NewOrderService(orderRepo, walletRepo, me)
	settlementService := service.NewSettlementService(config.DB, orderRepo, walletRepo)
	failedSettlementService := service.NewFailedSettlementService(repository.NewFailedSettlementRepository(config.DB))
	orderHandler := handler.NewOrderHandler(orderService)

	go func() {
		for trade := range me.TradeCh {
			processTradeSettlement(trade, settlementService, failedSettlementService, func(msg []byte) {
				hub.Broadcast <- msg
			}, log.Default())
		}
	}()

	go func() {
		for snapshot := range me.SnapshotCh {
			snapshotJSON, _ := json.Marshal(map[string]interface{}{
				"type": "orderbook",
				"data": snapshot,
			})
			hub.Broadcast <- snapshotJSON
		}
	}()

	bootstrapService := service.NewMatchingBootstrapService(orderRepo, me)
	bootstrapCtx, cancelBootstrap := context.WithTimeout(context.Background(), 30*time.Second)
	bootstrapResult, err := bootstrapService.BootstrapOpenOrders(bootstrapCtx)
	cancelBootstrap()
	if err != nil {
		log.Fatal("matching bootstrap failed: ", err)
	}
	log.Printf(
		"matching bootstrap completed: loaded=%d submitted=%d skipped=%d pending=%d partial=%d",
		bootstrapResult.Loaded,
		bootstrapResult.Submitted,
		bootstrapResult.Skipped,
		bootstrapResult.StatusCounts[model.OrderStatusPending],
		bootstrapResult.StatusCounts[model.OrderStatusPartial],
	)

	upbitClient, err := upbit.NewUpbitClient()
	if err != nil {
		panic(err)
	}
	upbitClient.Subscribe([]string{
		"KRW-BTC", "KRW-ETH", "KRW-XRP", "KRW-SOL",
		"KRW-DOGE", "KRW-ADA", "KRW-DOT", "KRW-AVAX",
		"KRW-MATIC", "KRW-LINK", "KRW-ATOM", "KRW-UNI",
		"KRW-SHIB", "KRW-TRX",
	})

	go upbitClient.Listen(func(code string, price float64) {
		msg := fmt.Sprintf(`{"type":"ticker","code":"%s","price":%f}`, code, price)
		hub.Broadcast <- []byte(msg)
	})

	r := gin.Default()

	r.Use(cors.New(cors.Config{
		AllowOrigins: []string{"http://localhost:3000"},
		AllowMethods: []string{"GET", "POST", "PUT", "DELETE"},
		AllowHeaders: []string{"Content-Type"},
	}))

	r.GET("/ping", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"message": "pong",
		})
	})

	r.GET("/ws", func(c *gin.Context) {
		ws.ServeWs(hub, c)
	})

	r.POST("/orders", orderHandler.CreateOrder)
	r.DELETE("/orders/:id", orderHandler.CancelOrder)

	r.Run(":8080")
}

type tradeSettler interface {
	SettleTrade(trade *model.Trade) (service.SettlementResult, error)
}

type settlementFailureRecorder interface {
	RecordFailure(trade *model.Trade, settlementErr error) (*model.FailedSettlement, error)
}

func processTradeSettlement(
	trade *model.Trade,
	settler tradeSettler,
	failureRecorder settlementFailureRecorder,
	broadcast func([]byte),
	logger *log.Logger,
) {
	if logger == nil {
		logger = log.Default()
	}

	result, err := settler.SettleTrade(trade)
	if err != nil {
		if failureRecorder != nil {
			if _, recordErr := failureRecorder.RecordFailure(trade, err); recordErr != nil {
				logger.Printf("record failed settlement failed: %v", recordErr)
			}
		}
		logger.Printf("settle trade failed: %v", err)
		return
	}
	if !result.Applied {
		return
	}

	tradeJSON, err := json.Marshal(map[string]interface{}{
		"type": "trade",
		"data": map[string]interface{}{
			"price":    trade.Price,
			"quantity": trade.Quantity,
			"time":     trade.TradedAt,
		},
	})
	if err != nil {
		logger.Printf("marshal trade broadcast failed: %v", err)
		return
	}
	broadcast(tradeJSON)
}
