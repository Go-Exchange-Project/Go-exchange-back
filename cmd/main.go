package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/config"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/auth"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/dbmigration"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/handler"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/httpapi"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/middleware"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/upbit"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/ws"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	if err := config.LoadLocalEnvFiles(); err != nil {
		log.Fatal("load local env failed: ", err)
	}
	config.ConnectDB()

	if config.PprofEnabledFromEnv() {
		// Binds to all interfaces inside the container; external exposure is
		// prevented by docker-compose's host-side 127.0.0.1:6060:6060 mapping,
		// not by this bind address (binding to 127.0.0.1 here would make it
		// unreachable through Docker's port forwarding, which routes via the
		// container's eth0, not its loopback).
		go func() {
			log.Println("pprof listening on :6060:", http.ListenAndServe(":6060", nil))
		}()
	}

	if err := config.DB.AutoMigrate(
		&model.User{},
		&model.Order{},
		&model.Wallet{},
		&model.Trade{},
		&model.FailedSettlement{},
		&model.LedgerEntry{},
	); err != nil {
		log.Fatal("auto migrate failed: ", err)
	}
	if err := dbmigration.Up(config.DB); err != nil {
		log.Fatal("db migration failed: ", err)
	}

	me := matching.NewMatchingEngine()
	me.MatchLatencyObserver = func(d time.Duration) {
		metrics.OrderPipelineMatchLatency.Observe(d.Seconds())
	}
	metrics.RegisterMatchingEngineChannelLenGauges(
		func() int { return len(me.OrderCh) },
		func() int { return len(me.CancelCh) },
		func() int { return len(me.SnapshotReq) },
		func() int { return len(me.ExecutionCh) },
		func() int { return len(me.SnapshotCh) },
	)
	me.Start()

	hub := ws.NewHub()
	go hub.Run()

	marketRulesRegistry, err := service.NewMarketRulesRegistryFromEnv()
	if err != nil {
		log.Fatal("market rules registry failed: ", err)
	}

	orderRepo := repository.NewOrderRepository(config.DB)
	walletRepo := repository.NewWalletRepository(config.DB)
	userRepo := repository.NewUserRepository(config.DB)
	tokenManager, err := auth.NewTokenManagerFromEnv()
	if err != nil {
		log.Fatal("auth token manager failed: ", err)
	}
	authService := service.NewAuthService(userRepo, tokenManager)
	orderService := service.NewOrderService(orderRepo, walletRepo, me)
	orderService.MarketRules = marketRulesRegistry
	settlementService := service.NewSettlementService(config.DB, orderRepo, walletRepo)
	failedSettlementService := service.NewFailedSettlementService(repository.NewFailedSettlementRepository(config.DB))
	authHandler := handler.NewAuthHandler(authService)
	marketHandler := handler.NewMarketHandler(marketRulesRegistry)
	orderBookHandler := handler.NewOrderBookHandler(me)
	orderHandler := handler.NewOrderHandler(orderService)

	for i := 0; i < config.SettlementWorkersFromEnv(); i++ {
		go func() {
			for event := range me.ExecutionCh {
				processExecutionEvent(event, settlementService, failedSettlementService, orderService, func(msg []byte) {
					hub.Broadcast <- msg
				}, log.Default())
			}
		}()
	}

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

	if config.UpbitEnabledFromEnv() {
		upbitClient, err := upbit.NewUpbitClient()
		if err != nil {
			panic(err)
		}
		if err := upbitClient.Subscribe([]string{
			"KRW-BTC", "KRW-ETH", "KRW-XRP", "KRW-SOL",
			"KRW-DOGE", "KRW-ADA", "KRW-DOT", "KRW-AVAX",
			"KRW-MATIC", "KRW-LINK", "KRW-ATOM", "KRW-UNI",
			"KRW-SHIB", "KRW-TRX",
		}); err != nil {
			panic(err)
		}

		go upbitClient.Listen(func(code string, price float64) {
			msg := fmt.Sprintf(`{"type":"ticker","code":"%s","price":%f}`, code, price)
			hub.Broadcast <- []byte(msg)
		})
	} else {
		log.Println("upbit feed disabled by GOEXCHANGE_ENABLE_UPBIT")
	}

	r := gin.Default()

	r.Use(cors.New(cors.Config{
		AllowOrigins: config.CORSAllowedOriginsFromEnv(),
		AllowMethods: []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders: []string{"Content-Type", "Authorization", middleware.DevToolsTokenHeader},
	}))
	r.Use(metrics.HTTPMiddleware())

	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	r.GET("/ping", func(c *gin.Context) {
		httpapi.WriteData(c, http.StatusOK, gin.H{
			"message": "pong",
		})
	})

	r.GET("/ws", func(c *gin.Context) {
		ws.ServeWs(hub, c)
	})

	r.POST("/auth/register", authHandler.Register)
	r.POST("/auth/login", authHandler.Login)
	r.GET("/markets/rules", marketHandler.GetRules)
	r.GET("/orderbook", orderBookHandler.GetSnapshot)

	authenticated := r.Group("/")
	authenticated.Use(middleware.AuthRequired(tokenManager))
	authenticated.GET("/orders", orderHandler.ListOrders)
	authenticated.GET("/orders/:id", orderHandler.GetOrder)
	authenticated.POST("/orders", orderHandler.CreateOrder)
	authenticated.DELETE("/orders/:id", orderHandler.CancelOrder)
	authenticated.GET("/wallets", orderHandler.ListWallets)
	authenticated.GET("/trades", orderHandler.ListTrades)
	if config.DevToolsEnabledFromEnv() {
		devHandler := handler.NewDevHandler(service.NewDevWalletService(config.DB))
		dev := authenticated.Group("/dev")
		dev.Use(middleware.DevToolsRequired(config.DevToolsTokenFromEnv()))
		dev.POST("/wallets/fund", devHandler.FundWallet)
	}

	r.Run(":8080")
}

type tradeSettler interface {
	SettleTrade(trade *model.Trade) (service.SettlementResult, error)
}

type settlementFailureRecorder interface {
	RecordFailure(trade *model.Trade, settlementErr error) (*model.FailedSettlement, error)
}

type marketOrderCompleter interface {
	CompleteMarketOrder(input service.CompleteMarketOrderInput) error
}

func processExecutionEvent(
	event matching.ExecutionEvent,
	settler tradeSettler,
	failureRecorder settlementFailureRecorder,
	marketCompleter marketOrderCompleter,
	broadcast func([]byte),
	logger *log.Logger,
) {
	if event.Trade != nil {
		processTradeSettlement(event.Trade, settler, failureRecorder, broadcast, logger)
		return
	}
	if event.MarketOrderDone != nil {
		processMarketOrderDone(event.MarketOrderDone, marketCompleter, logger)
	}
}

func processMarketOrderDone(
	done *matching.MarketOrderDone,
	completer marketOrderCompleter,
	logger *log.Logger,
) {
	if logger == nil {
		logger = log.Default()
	}
	if completer == nil || done == nil {
		return
	}
	if err := completer.CompleteMarketOrder(service.CompleteMarketOrderInput{
		OrderID:              done.OrderID,
		FilledAmount:         done.FilledAmount,
		FilledQuoteAmount:    done.FilledQuoteAmount,
		RemainingQuoteAmount: done.RemainingQuoteAmount,
	}); err != nil {
		logger.Printf("complete market order failed: %v", err)
	}
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

	settlementStart := time.Now()
	result, err := settler.SettleTrade(trade)
	metrics.OrderSettlementDuration.Observe(time.Since(settlementStart).Seconds())
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
			"coin_symbol":      trade.CoinSymbol,
			"engine_sequence":  trade.EngineSequence,
			"engine_event_id":  trade.EngineEventID,
			"idempotency_key":  trade.IdempotencyKey,
			"price":            trade.Price,
			"quantity":         trade.Quantity,
			"fee_rate":         trade.FeeRate,
			"buyer_fee":        trade.BuyerFee,
			"buyer_fee_asset":  trade.BuyerFeeAsset,
			"seller_fee":       trade.SellerFee,
			"seller_fee_asset": trade.SellerFeeAsset,
			"time":             trade.TradedAt,
		},
	})
	if err != nil {
		logger.Printf("marshal trade broadcast failed: %v", err)
		return
	}
	broadcast(tradeJSON)
}
