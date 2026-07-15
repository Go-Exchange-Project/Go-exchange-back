package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"sync"
	"syscall"
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
		&model.TradeOutboxEvent{},
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

	// 심볼을 태깅해 발행한다 — hub가 해당 심볼 구독자(또는 legacy full-feed
	// 클라이언트)에게만 전달한다(B-1b).
	broadcast := func(coinSymbol string, msg []byte) {
		hub.Broadcast <- ws.Message{CoinSymbol: coinSymbol, Payload: msg}
	}

	// A-3 write-ahead outbox: 정산은 outbox에 커밋된 이벤트만 처리한다.
	// outbox 커밋 이전 크래시는 매칭 자체가 롤백되고(자금 무변동, 부트스트랩이
	// 미체결 주문을 재투입), 이후 크래시는 아래 리플레이가 PENDING을 재처리한다.
	// 부팅 순서가 곧 정확성이다: ① 리플레이 → ② 시장가 파이널라이저 →
	// ③ 라이브 파이프라인 → ④ 부트스트랩 → ⑤ HTTP 개시.
	outboxRepo := repository.NewTradeOutboxRepository(config.DB)
	replayer := &service.OutboxReplayer{
		Repo: outboxRepo,
		// 리플레이는 outboxEventID=0으로 호출해 트랜잭션 흡수 마킹을 끄고, 리플레이어가
		// 직접 MarkProcessed한다(부팅 경로라 성능 무관, 순차 처리 로직을 단순하게 유지).
		Process: func(event matching.ExecutionEvent) bool {
			handled, _ := processExecutionEvent(event, 0, settlementService, failedSettlementService, orderService, failedMarketCompletionService, broadcast, log.Default())
			return handled
		},
	}
	replayResult, err := replayer.Replay()
	if err != nil {
		log.Fatal("trade outbox replay failed: ", err)
	}
	log.Printf(
		"trade outbox replay completed: replayed=%d deferred=%d corrupted=%d",
		replayResult.Replayed, replayResult.Deferred, replayResult.Corrupted,
	)

	// 리플레이 완료 시점에 PENDING/PARTIAL로 남은 시장가 주문은 엔진 메모리가
	// 사라졌으므로 더 이상 체결될 수 없다 — 잔여 hold를 해제해 영구 동결을 막는다.
	// 반드시 리플레이 뒤여야 한다(정산 완료 전 완료 시도는 filled 검증 conflict).
	finalizer := &service.StaleMarketOrderFinalizer{
		Orders:          orderRepo,
		Completer:       orderService,
		FailureRecorder: failedMarketCompletionService,
	}
	finalizeResult, err := finalizer.FinalizeAll()
	if err != nil {
		log.Fatal("stale market order finalize failed: ", err)
	}
	log.Printf("stale market orders finalized: finalized=%d failed=%d", finalizeResult.Finalized, finalizeResult.Failed)

	// 심볼 파티셔닝 정산 워커: 같은 심볼의 이벤트는 항상 같은 워커가 FIFO로 처리해
	// 엔진이 만든 순서(trade들 -> MarketOrderDone)를 보존한다. 워커를 채널 하나로
	// 경쟁 소비시키면 Done 이벤트가 trade 정산을 앞질러 완료가 유실될 수 있다.
	settlementQueues := make([]chan service.OutboxEvent, config.SettlementWorkersFromEnv())
	for i := range settlementQueues {
		settlementQueues[i] = make(chan service.OutboxEvent, settlementWorkerQueueSize)
	}
	metrics.RegisterSettlementWorkerQueueGauges(settlementQueueLenFns(settlementQueues))
	var settlementWg sync.WaitGroup
	for _, queue := range settlementQueues {
		settlementWg.Add(1)
		go func(queue chan service.OutboxEvent) {
			defer settlementWg.Done()
			var pending *service.OutboxEvent
			for {
				var event service.OutboxEvent
				if pending != nil {
					event, pending = *pending, nil
				} else {
					received, ok := <-queue
					if !ok {
						return
					}
					event = received
				}
				if event.Event.Trade == nil {
					processSingleOutboxEvent(event, settlementService, failedSettlementService, orderService, failedMarketCompletionService, broadcast, outboxRepo, log.Default())
					continue
				}
				batch, next, open := collectTradeBatch(event, queue, settlementBatchMaxSize)
				pending = next
				settleTradeBatchWithFallback(batch, settlementService, settlementService, failedSettlementService, orderService, failedMarketCompletionService, broadcast, outboxRepo, log.Default())
				if !open {
					// 채널 닫힘 — 잔여 배치는 방금 처리했고 pending은 nil.
					return
				}
			}
		}(queue)
	}

	// OutboxWriter는 ExecutionCh의 유일한 소비자: 배치 커밋(group commit) 후에만
	// 심볼 파티셔닝 큐로 전달한다. 엔진이 ExecutionCh를 닫으면(graceful shutdown)
	// 잔여 배치를 flush하고 큐를 닫아 워커 종료를 전파한다.
	outboxWriter := &service.OutboxWriter{
		Repo:   outboxRepo,
		Source: me.ExecutionCh,
		Forward: func(outboxEvent service.OutboxEvent) {
			forwardToSettlementQueue(settlementQueues, outboxEvent)
		},
	}
	outboxWriterDone := make(chan struct{})
	go func() {
		outboxWriter.Run()
		for _, queue := range settlementQueues {
			close(queue)
		}
		close(outboxWriterDone)
	}()

	backgroundCtx, cancelBackground := context.WithCancel(context.Background())
	defer cancelBackground()

	settlementRetryWorker := &service.SettlementRetryWorker{
		Settler:           settlementService,
		MarketCompleter:   orderService,
		FailedSettlements: failedSettlementService,
		FailedCompletions: failedMarketCompletionService,
	}
	go settlementRetryWorker.Run(backgroundCtx)

	reconciliationWorker := &service.ReconciliationWorker{
		Repository: repository.NewReconciliationRepository(config.DB),
		Interval:   config.ReconciliationIntervalFromEnv(),
	}
	go reconciliationWorker.Run(backgroundCtx)

	go func() {
		for snapshot := range me.SnapshotCh {
			snapshotJSON, _ := json.Marshal(map[string]interface{}{
				"type": "orderbook",
				"data": snapshot,
			})
			hub.Broadcast <- ws.Message{CoinSymbol: snapshot.CoinSymbol, Payload: snapshotJSON}
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
			// ticker는 소량이라 전역 발행 — 심볼 필터 없이 모든 클라이언트에게.
			hub.Broadcast <- ws.Message{Payload: []byte(msg)}
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

	// graceful shutdown 체인: HTTP 차단 → 엔진 드레인(ExecutionCh close) →
	// outbox writer flush(큐 close) → 정산 워커 드레인 → 백그라운드 워커 취소.
	// 상한 초과로 강제 종료돼도 outbox 덕에 유실은 없다 — 다음 부팅 리플레이가 처리한다.
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	srv := &http.Server{Addr: ":8080", Handler: r}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal("http server failed: ", err)
		}
	}()
	log.Println("server listening on :8080")

	<-signalCtx.Done()
	stopSignals()
	log.Println("shutdown: signal received, draining pipeline")

	httpCtx, cancelHTTP := context.WithTimeout(context.Background(), 10*time.Second)
	if err := srv.Shutdown(httpCtx); err != nil {
		log.Printf("shutdown: http server shutdown failed: %v", err)
	}
	cancelHTTP()

	drainDeadline := time.After(30 * time.Second)
	me.Stop()
	select {
	case <-me.Done():
	case <-drainDeadline:
		log.Println("shutdown: matching engine drain timed out")
	}
	select {
	case <-outboxWriterDone:
	case <-drainDeadline:
		log.Println("shutdown: outbox writer flush timed out")
	}
	settlementDrained := make(chan struct{})
	go func() {
		settlementWg.Wait()
		close(settlementDrained)
	}()
	select {
	case <-settlementDrained:
	case <-drainDeadline:
		log.Println("shutdown: settlement workers drain timed out, next boot replay will finish the rest")
	}

	cancelBackground()
	log.Println("shutdown complete")
}

type tradeSettler interface {
	SettleTrade(trade *model.Trade, outboxEventID uint64) (service.SettlementResult, error)
}

type tradeBatchSettler interface {
	SettleTradeBatch(items []service.TradeBatchItem) ([]service.SettlementResult, error)
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

type outboxMarker interface {
	MarkProcessed(id uint64) error
}

const settlementWorkerQueueSize = 256

// settlementBatchMaxSize는 collectTradeBatch가 한 번에 모으는 trade 상한이다.
const settlementBatchMaxSize = 32

// transientRetryDelays는 데드락 등 일시적 오류의 in-place 재시도 간격입니다.
// 여기서 못 잡은 실패는 SettlementRetryWorker(10초 주기)가 2차로 처리합니다.
var transientRetryDelays = []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond}

// forwardToSettlementQueue는 outbox에 커밋된 이벤트를 심볼 해시로 정해지는
// 워커 큐에 넣는다. 같은 심볼은 항상 같은 큐 — 엔진 방출 순서가 보존된다.
func forwardToSettlementQueue(queues []chan service.OutboxEvent, event service.OutboxEvent) {
	queues[settlementWorkerIndex(executionEventCoinSymbol(event.Event), len(queues))] <- event
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

func settlementQueueLenFns(queues []chan service.OutboxEvent) []func() int {
	lenFns := make([]func() int, len(queues))
	for i, queue := range queues {
		queue := queue
		lenFns[i] = func() int { return len(queue) }
	}
	return lenFns
}

// processExecutionEvent는 (handled, markedInTx)를 반환한다.
// handled: 처리가 내구적으로 확정됐는지(정산 성공, 멱등 no-op, 또는 실패의 내구
//
//	기록 완료). false면 outbox 행을 PENDING으로 남겨 다음 부팅 리플레이가 재시도한다.
//
// markedInTx: 정산과 같은 트랜잭션에서 outbox 행이 이미 PROCESSED로 마킹됐는지.
//
//	true면 호출자는 별도 MarkProcessed를 하지 않는다(왕복 절약). outboxEventID>0인
//	trade 성공 경로에서만 true다 — 리플레이(id=0)·실패기록·시장가 완료는 false.
func processExecutionEvent(
	event matching.ExecutionEvent,
	outboxEventID uint64,
	settler tradeSettler,
	failureRecorder settlementFailureRecorder,
	marketCompleter marketOrderCompleter,
	completionFailureRecorder marketCompletionFailureRecorder,
	broadcast func(coinSymbol string, payload []byte),
	logger *log.Logger,
) (handled bool, markedInTx bool) {
	if event.Trade != nil {
		return processTradeSettlement(event.Trade, outboxEventID, settler, failureRecorder, broadcast, logger)
	}
	if event.MarketOrderDone != nil {
		return processMarketOrderDone(event.MarketOrderDone, marketCompleter, completionFailureRecorder, logger), false
	}
	return true, false
}

// processSingleOutboxEvent는 outbox 이벤트 1건을 단건 경로로 처리한다 — 워커 루프의
// 비-trade(MarketOrderDone) 분기와, 배치 정산 실패 시 폴백 분기가 공유한다.
func processSingleOutboxEvent(
	outboxEvent service.OutboxEvent,
	settler tradeSettler,
	failureRecorder settlementFailureRecorder,
	marketCompleter marketOrderCompleter,
	completionFailureRecorder marketCompletionFailureRecorder,
	broadcast func(coinSymbol string, payload []byte),
	outboxRepo outboxMarker,
	logger *log.Logger,
) {
	handled, markedInTx := processExecutionEvent(outboxEvent.Event, outboxEvent.OutboxID, settler, failureRecorder, marketCompleter, completionFailureRecorder, broadcast, logger)
	if !handled {
		// 내구 확정 실패(정산 실패의 기록조차 실패) — PENDING으로 남겨
		// 다음 부팅 리플레이가 재시도한다.
		return
	}
	if markedInTx {
		// 정산 트랜잭션이 outbox 마킹까지 이미 커밋했다 — 별도 왕복 불필요.
		return
	}
	if err := outboxRepo.MarkProcessed(outboxEvent.OutboxID); err != nil {
		// 마킹 실패는 유실이 아니라 다음 리플레이의 멱등 재처리일 뿐.
		logger.Printf("mark outbox event %d processed failed: %v", outboxEvent.OutboxID, err)
	}
}

// collectTradeBatch는 first(반드시 trade)에 이어 큐에 이미 쌓인 trade를 논블로킹으로
// 최대 maxBatch까지 모은다. 티머 없음 — 이벤트는 이미 outbox에 커밋된 뒤라 모으려고
// 기다릴 이유가 없다(부하 낮으면 배치 1, 백로그가 있을 때만 커지는 적응형).
// 비-trade(MarketOrderDone)를 만나면 배치를 끊고 pending으로 돌려준다(순서 보존).
func collectTradeBatch(first service.OutboxEvent, queue <-chan service.OutboxEvent, maxBatch int) (batch []service.OutboxEvent, pending *service.OutboxEvent, open bool) {
	batch = append(batch, first)
	open = true
	for len(batch) < maxBatch {
		select {
		case event, ok := <-queue:
			if !ok {
				open = false
				return
			}
			if event.Event.Trade == nil {
				pending = &event
				return
			}
			batch = append(batch, event)
		default:
			return
		}
	}
	return
}

// settleTradeBatchWithFallback: 배치 성공 시 Applied trade만 브로드캐스트.
// 실패 시 전체 롤백된 상태이므로 기존 단건 경로로 건별 재처리 —
// 불량 trade만 실패 기록으로 빠지고 나머지는 정상 정산된다.
func settleTradeBatchWithFallback(
	batch []service.OutboxEvent,
	batchSettler tradeBatchSettler,
	settler tradeSettler,
	failureRecorder settlementFailureRecorder,
	marketCompleter marketOrderCompleter,
	completionFailureRecorder marketCompletionFailureRecorder,
	broadcast func(coinSymbol string, payload []byte),
	outboxRepo outboxMarker,
	logger *log.Logger,
) {
	items := make([]service.TradeBatchItem, len(batch))
	for i, event := range batch {
		items[i] = service.TradeBatchItem{Trade: event.Event.Trade, OutboxEventID: event.OutboxID}
	}
	results, err := batchSettler.SettleTradeBatch(items)
	if err != nil {
		metrics.SettlementBatchFallbacksTotal.Inc()
		logger.Printf("settle trade batch of %d failed, falling back to per-trade settlement: %v", len(batch), err)
		for _, event := range batch {
			processSingleOutboxEvent(event, settler, failureRecorder, marketCompleter, completionFailureRecorder, broadcast, outboxRepo, logger)
		}
		return
	}
	metrics.SettlementBatchSize.Observe(float64(len(batch)))
	for i, result := range results {
		if result.Applied {
			broadcastSettledTrade(batch[i].Event.Trade, broadcast, logger)
		}
	}
}

func processMarketOrderDone(
	done *matching.MarketOrderDone,
	completer marketOrderCompleter,
	failureRecorder marketCompletionFailureRecorder,
	logger *log.Logger,
) bool {
	if logger == nil {
		logger = log.Default()
	}
	if completer == nil || done == nil {
		return true
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
		return true
	}

	// Done 이벤트를 여기서 버리면 시장가 주문의 잔여 hold가 영구 동결된다.
	// 내구 기록으로 남겨 재시도 워커에 넘긴다.
	logger.Printf("complete market order failed: %v", err)
	if failureRecorder == nil {
		return false
	}
	if _, recordErr := failureRecorder.RecordFailure(input, done.CoinSymbol, err); recordErr != nil {
		logger.Printf("record failed market completion failed: %v", recordErr)
		return false
	}
	return true
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

// processTradeSettlement는 (handled, markedInTx)를 반환한다. outboxEventID>0이면
// SettleTrade가 정산 트랜잭션 안에서 outbox를 마킹하므로, 정산이 성공하는 즉시
// markedInTx=true다(실패 후 내구기록 경로는 트랜잭션이 롤백돼 markedInTx=false).
func processTradeSettlement(
	trade *model.Trade,
	outboxEventID uint64,
	settler tradeSettler,
	failureRecorder settlementFailureRecorder,
	broadcast func(coinSymbol string, payload []byte),
	logger *log.Logger,
) (handled bool, markedInTx bool) {
	if logger == nil {
		logger = log.Default()
	}

	settlementStart := time.Now()
	result, err := settler.SettleTrade(trade, outboxEventID)
	for attempt := 0; err != nil && service.IsTransientSettlementError(err) && attempt < len(transientRetryDelays); attempt++ {
		time.Sleep(transientRetryDelays[attempt])
		result, err = settler.SettleTrade(trade, outboxEventID)
	}
	metrics.OrderSettlementDuration.Observe(time.Since(settlementStart).Seconds())
	if err != nil {
		logger.Printf("settle trade failed: %v", err)
		if failureRecorder == nil {
			return false, false
		}
		if _, recordErr := failureRecorder.RecordFailure(trade, err); recordErr != nil {
			logger.Printf("record failed settlement failed: %v", recordErr)
			return false, false
		}
		return true, false
	}
	// 정산 성공: outboxEventID>0이면 SettleTrade가 같은 트랜잭션에서 마킹까지 커밋했다.
	markedInTx = outboxEventID > 0
	if !result.Applied {
		return true, markedInTx
	}

	broadcastSettledTrade(trade, broadcast, logger)
	return true, markedInTx
}

// broadcastSettledTrade는 이미 커밋된 정산의 trade를 JSON으로 마샬해 브로드캐스트한다.
// 마샬 실패는 정산 내구성과 무관하므로 로그만 남기고 조용히 건너뛴다.
func broadcastSettledTrade(trade *model.Trade, broadcast func(coinSymbol string, payload []byte), logger *log.Logger) {
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
	broadcast(trade.CoinSymbol, tradeJSON)
}
