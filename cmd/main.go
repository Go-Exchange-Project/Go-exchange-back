package main

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
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
		&model.FailedMarketCompletion{},
		&model.LedgerEntry{},
		&model.ReconciliationViolation{},
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
	failedMarketCompletionService := service.NewFailedMarketCompletionService(repository.NewFailedMarketCompletionRepository(config.DB))
	authHandler := handler.NewAuthHandler(authService)
	marketHandler := handler.NewMarketHandler(marketRulesRegistry)
	orderBookHandler := handler.NewOrderBookHandler(me)
	orderHandler := handler.NewOrderHandler(orderService)

	// 심볼 파티셔닝 정산 워커: 같은 심볼의 이벤트는 항상 같은 워커가 FIFO로 처리해
	// 엔진이 만든 순서(trade들 -> MarketOrderDone)를 보존한다. 워커를 채널 하나로
	// 경쟁 소비시키면 Done 이벤트가 trade 정산을 앞질러 완료가 유실될 수 있다.
	settlementQueues := make([]chan matching.ExecutionEvent, config.SettlementWorkersFromEnv())
	for i := range settlementQueues {
		settlementQueues[i] = make(chan matching.ExecutionEvent, settlementWorkerQueueSize)
	}
	metrics.RegisterSettlementWorkerQueueGauges(settlementQueueLenFns(settlementQueues))
	go dispatchExecutionEvents(me.ExecutionCh, settlementQueues)
	for _, queue := range settlementQueues {
		go func(queue chan matching.ExecutionEvent) {
			for event := range queue {
				processExecutionEvent(event, settlementService, failedSettlementService, orderService, failedMarketCompletionService, func(msg []byte) {
					hub.Broadcast <- msg
				}, log.Default())
			}
		}(queue)
	}

	settlementRetryWorker := &service.SettlementRetryWorker{
		Settler:           settlementService,
		MarketCompleter:   orderService,
		FailedSettlements: failedSettlementService,
		FailedCompletions: failedMarketCompletionService,
	}
	go settlementRetryWorker.Run(context.Background())

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

type marketCompletionFailureRecorder interface {
	RecordFailure(input service.CompleteMarketOrderInput, coinSymbol string, completionErr error) (*model.FailedMarketCompletion, error)
}

const settlementWorkerQueueSize = 256

// transientRetryDelays는 데드락 등 일시적 오류의 in-place 재시도 간격입니다.
// 여기서 못 잡은 실패는 SettlementRetryWorker(10초 주기)가 2차로 처리합니다.
var transientRetryDelays = []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond}

func dispatchExecutionEvents(events <-chan matching.ExecutionEvent, queues []chan matching.ExecutionEvent) {
	for event := range events {
		queues[settlementWorkerIndex(executionEventCoinSymbol(event), len(queues))] <- event
	}
}

func executionEventCoinSymbol(event matching.ExecutionEvent) string {
	if event.Trade != nil {
		return event.Trade.CoinSymbol
	}
	if event.MarketOrderDone != nil {
		return event.MarketOrderDone.CoinSymbol
	}
	return ""
}

func settlementWorkerIndex(coinSymbol string, workerCount int) int {
	if workerCount <= 1 {
		return 0
	}
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(coinSymbol))
	return int(hash.Sum32() % uint32(workerCount))
}

func settlementQueueLenFns(queues []chan matching.ExecutionEvent) []func() int {
	lenFns := make([]func() int, len(queues))
	for i, queue := range queues {
		queue := queue
		lenFns[i] = func() int { return len(queue) }
	}
	return lenFns
}

func processExecutionEvent(
	event matching.ExecutionEvent,
	settler tradeSettler,
	failureRecorder settlementFailureRecorder,
	marketCompleter marketOrderCompleter,
	completionFailureRecorder marketCompletionFailureRecorder,
	broadcast func([]byte),
	logger *log.Logger,
) {
	if event.Trade != nil {
		processTradeSettlement(event.Trade, settler, failureRecorder, broadcast, logger)
		return
	}
	if event.MarketOrderDone != nil {
		processMarketOrderDone(event.MarketOrderDone, marketCompleter, completionFailureRecorder, logger)
	}
}

func processMarketOrderDone(
	done *matching.MarketOrderDone,
	completer marketOrderCompleter,
	failureRecorder marketCompletionFailureRecorder,
	logger *log.Logger,
) {
	if logger == nil {
		logger = log.Default()
	}
	if completer == nil || done == nil {
		return
	}

	input := service.CompleteMarketOrderInput{
		OrderID:              done.OrderID,
		FilledAmount:         done.FilledAmount,
		FilledQuoteAmount:    done.FilledQuoteAmount,
		RemainingQuoteAmount: done.RemainingQuoteAmount,
	}
	err := completer.CompleteMarketOrder(input)
	for attempt := 0; err != nil && isRetryableCompletionError(err) && attempt < len(transientRetryDelays); attempt++ {
		time.Sleep(transientRetryDelays[attempt])
		err = completer.CompleteMarketOrder(input)
	}
	if err == nil {
		return
	}

	// Done 이벤트는 엔진 메모리에만 존재하므로, 여기서 버리면 시장가 주문의
	// 잔여 hold가 영구 동결된다. 내구 기록으로 남겨 재시도 워커에 넘긴다.
	if failureRecorder != nil {
		if _, recordErr := failureRecorder.RecordFailure(input, done.CoinSymbol, err); recordErr != nil {
			logger.Printf("record failed market completion failed: %v", recordErr)
		}
	}
	logger.Printf("complete market order failed: %v", err)
}

// isRetryableCompletionError: conflict는 같은 심볼의 trade 정산이 아직 안 끝났다는
// 뜻이고(정상 순서상 곧 끝남), transient는 DB 일시 오류라 둘 다 재시도 가치가 있다.
func isRetryableCompletionError(err error) bool {
	if service.IsTransientSettlementError(err) {
		return true
	}
	kind, ok := service.DomainErrorKind(err)
	return ok && kind == service.ErrorKindConflict
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
	for attempt := 0; err != nil && service.IsTransientSettlementError(err) && attempt < len(transientRetryDelays); attempt++ {
		time.Sleep(transientRetryDelays[attempt])
		result, err = settler.SettleTrade(trade)
	}
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
