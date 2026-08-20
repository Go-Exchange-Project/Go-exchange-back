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
	"strconv"
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
		&model.FailedOrderCancellation{},
		&model.LedgerEntry{},
		&model.ReconciliationViolation{},
		&model.TradeOutboxEvent{},
	); err != nil {
		log.Fatal("auto migrate failed: ", err)
	}
	if err := dbmigration.Up(config.DB); err != nil {
		log.Fatal("db migration failed: ", err)
	}

	engineShards := config.EngineShardsFromEnv()
	me := matching.NewShardedEngine(engineShards)
	me.SetMatchLatencyObserver(func(d time.Duration) {
		metrics.OrderPipelineMatchLatency.Observe(d.Seconds())
	})
	metrics.RegisterMatchingEngineChannelLenGauges(
		me.OrderChannelLen,
		me.CancelChannelLen,
		me.ExecutionChannelLen,
		me.SnapshotChannelLen,
	)
	metrics.RegisterMatchingEngineShardOrderChannelLenGauges(me.ShardOrderChannelLens())
	me.Start()
	log.Printf("matching engine sharded: shards=%d", engineShards)

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
	orderService.AcceptanceTimeout = config.OrderAcceptanceTimeoutFromEnv()

	// [②] 자금 홀드 그룹커밋: CreateOrder의 persist+hold를 배치로 묶어 처리한다.
	// 종료 순서는 아래 graceful shutdown 체인 참고 — HTTP drain 이후에만 Shutdown().
	holdCoordinator := service.NewHoldCoordinator(config.DB, orderRepo, walletRepo, repository.NewLedgerRepository(config.DB), config.HoldBatchSizeFromEnv())
	go holdCoordinator.Run()
	orderService.HoldCoordinator = holdCoordinator

	// 취소는 엔진 호출 전에 DB에 먼저 기록된다. worker가 그 command를 엔진에
	// 전달하고, OutboxWriter가 실행 이벤트와 함께 PROCESSED로 커밋한다.
	cancelCommandRepo := repository.NewCancelCommandRepository(config.DB)
	cancelWorker := service.NewCancelCommandWorker(cancelCommandRepo, orderRepo, me)
	orderService.CancelCommandRepository = cancelCommandRepo
	orderService.CancelCommandWake = cancelWorker.Wake
	metrics.RegisterHoldCoordinatorInputGauge(func() int { return holdCoordinator.InputLen() })
	settlementService := service.NewSettlementService(config.DB, orderRepo, walletRepo)
	failedSettlementService := service.NewFailedSettlementService(repository.NewFailedSettlementRepository(config.DB))
	failedMarketCompletionService := service.NewFailedMarketCompletionService(repository.NewFailedMarketCompletionRepository(config.DB))
	failedOrderCancellationService := service.NewFailedOrderCancellationService(repository.NewFailedOrderCancellationRepository(config.DB))
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
		// 리플레이는 transactionalOutboxID=0으로 호출해 트랜잭션 흡수 마킹을 끄고,
		// 리플레이어가 직접 MarkProcessed한다(부팅 경로라 성능 무관, 순차 처리 로직을
		// 단순하게 유지). sourceOutboxID는 실제 행 ID를 그대로 넘겨 실패 기록의
		// provenance로 쓴다.
		Process: func(sourceOutboxID uint64, event matching.ExecutionEvent) bool {
			handled, _ := processExecutionEvent(event, 0, sourceOutboxID, settlementService, failedSettlementService, orderService, failedMarketCompletionService, orderService, failedSettlementService, failedOrderCancellationService, broadcast, log.Default())
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

	// 심볼 파티셔닝: 같은 심볼의 이벤트는 항상 같은 파티션 dispatcher가 소유해
	// 엔진이 만든 순서(trade들 -> MarketOrderDone)를 보존한다. 정산 DB 작업 자체는
	// 아래 전역 worker pool에서 병렬로 처리되지만, dispatcher가 배치 디스패치
	// 순서(seq)대로만 브로드캐스트를 커밋하고 종결 이벤트는 배리어로 지킨다 —
	// 파티션을 채널 하나로 합쳐 경쟁 소비시키면 이 순서 보장이 깨진다.
	settlementQueues := make([]chan service.OutboxEvent, config.SettlementWorkersFromEnv())
	for i := range settlementQueues {
		settlementQueues[i] = make(chan service.OutboxEvent, settlementWorkerQueueSize)
	}
	metrics.RegisterSettlementWorkerQueueGauges(settlementQueueLenFns(settlementQueues))

	concurrency := config.SettlementConcurrencyFromEnv()
	settlementJobs := make(chan settlementJob, concurrency)
	// 전역 worker pool: 정산만 하고 방출은 dispatcher가 순서대로.
	var settlementWorkerWg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		settlementWorkerWg.Add(1)
		go func() {
			defer settlementWorkerWg.Done()
			runSettlementWorker(settlementJobs, func(batch []service.OutboxEvent, collect func(string, []byte)) []uint {
				return settleTradeBatchWithFallback(batch, settlementService, settlementService, failedSettlementService,
					orderService, failedMarketCompletionService, orderService, failedSettlementService, failedOrderCancellationService,
					collect, outboxRepo, log.Default())
			}, func(event service.OutboxEvent) {
				processSingleOutboxEvent(event, settlementService, failedSettlementService, orderService,
					failedMarketCompletionService, orderService, failedSettlementService, failedOrderCancellationService,
					func(string, []byte) {}, outboxRepo, log.Default())
			})
		}()
	}
	log.Printf("settlement partitions=%d concurrency=%d", len(settlementQueues), concurrency)

	var settlementWg sync.WaitGroup
	for i, queue := range settlementQueues {
		settlementWg.Add(1)
		go func(partition string, queue chan service.OutboxEvent) {
			defer settlementWg.Done()
			runPartitionDispatcher(partition, queue, settlementJobs, concurrency, settlementBatchMaxSize, broadcast)
		}(strconv.Itoa(i), queue)
	}

	// OutboxWriter는 ExecutionCh의 유일한 소비자: 배치 커밋(group commit) 후에만
	// 심볼 파티셔닝 큐로 전달한다. 엔진이 ExecutionCh를 닫으면(graceful shutdown)
	// 잔여 배치를 flush하고 큐를 닫아 워커 종료를 전파한다.
	outboxWriter := &service.OutboxWriter{
		Repo:      outboxRepo,
		Source:    me.ExecutionCh,
		BatchSize: config.OutboxBatchSizeFromEnv(),
		Forward: func(outboxEvent service.OutboxEvent) {
			forwardToSettlementQueue(settlementQueues, outboxEvent)
		},
	}
	log.Printf("outbox writer batch size=%d", outboxWriter.BatchSize)
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
		Settler:             settlementService,
		MarketCompleter:     orderService,
		CancelProcessor:     orderService,
		FailedSettlements:   failedSettlementService,
		FailedCompletions:   failedMarketCompletionService,
		FailedCancellations: failedOrderCancellationService,
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

	// 부팅 장벽: outbox replay → matching bootstrap → cancel worker 시작·drain →
	// (아래) HTTP listen. bootstrap이 주문을 오더북에 다시 올린 직후 트래픽을 받으면
	// 복구된 취소가 처리되기 전에 체결될 수 있다.
	bootstrapService := service.NewMatchingBootstrapService(orderRepo, me)
	cancelWorkerCtx, cancelCancelWorker := context.WithCancel(context.Background())
	cancelWorkerDone := make(chan struct{})

	barrierCtx, cancelBarrier := context.WithTimeout(context.Background(), 30*time.Second)
	barrierErr := runCancelCommandStartupBarrier(
		barrierCtx,
		func(ctx context.Context) error {
			bootstrapResult, err := bootstrapService.BootstrapOpenOrders(ctx)
			if err != nil {
				return err
			}
			log.Printf(
				"matching bootstrap completed: loaded=%d submitted=%d skipped=%d pending=%d partial=%d",
				bootstrapResult.Loaded,
				bootstrapResult.Submitted,
				bootstrapResult.Skipped,
				bootstrapResult.StatusCounts[model.OrderStatusPending],
				bootstrapResult.StatusCounts[model.OrderStatusPartial],
			)
			return nil
		},
		func() {
			go func() {
				cancelWorker.Run(cancelWorkerCtx)
				close(cancelWorkerDone)
			}()
		},
		cancelWorker.WaitUntilDrained,
	)
	cancelBarrier()
	if barrierErr != nil {
		// 취소를 못 지키면서 주문을 받는 것보다 안 받는 것이 낫다.
		log.Fatal("startup barrier failed: ", barrierErr)
	}
	log.Println("startup barrier passed: recovered cancel commands drained")

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

	// HTTP가 in-flight CreateOrder 핸들러를 전부 드레인한 뒤에만 안전하다 — 그래야
	// 진행 중인 Submit()이 없어 input close와의 send-on-closed 경쟁이 없다. 반드시
	// 엔진 Stop() 앞: 엔진이 멈추면 이후 접수된 홀드가 매칭될 수 없다.
	holdCoordinator.Shutdown()

	drainDeadline := time.After(30 * time.Second)

	// cancel worker가 엔진보다 먼저 끝나야 drain 중 새 dispatch가 들어오지 않는다.
	// 상한을 넘겨도 엔진 정지로 넘어가지 않는다 — worker가 아직 CancelOrder를
	// 호출하고 있으면 진행 중인 취소가 오더북에 반영되지 못한다.
	stopCancelWorkerThenEngine(cancelCancelWorker, cancelWorkerDone, 10*time.Second, me.Stop, log.Printf)
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
		close(settlementJobs)
		settlementWorkerWg.Wait()
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

type orderCancellationProcessor interface {
	ProcessOrderCancellation(event matching.OrderCancelled) error
}

type marketCompletionFailureRecorder interface {
	RecordFailure(input service.CompleteMarketOrderInput, coinSymbol string, completionErr error) (*model.FailedMarketCompletion, error)
	EnsureDeferred(input service.CompleteMarketOrderInput, coinSymbol string, reason error) (*model.FailedMarketCompletion, error)
}

type outboxMarker interface {
	MarkProcessed(id uint64) error
}

// settlementDependencyGuard는 B의 HasOpenFailureForOrder를 live·replay 양쪽에서 공유한다.
type settlementDependencyGuard interface {
	HasOpenFailureForOrder(orderID uint) (bool, error)
}

type cancellationDeferStore interface {
	RecordFailure(cancelled matching.OrderCancelled, sourceOutboxID uint64, executionErr error) (*model.FailedOrderCancellation, error)
	EnsureDeferred(cancelled matching.OrderCancelled, sourceOutboxID uint64, reason error) (*model.FailedOrderCancellation, error)
}

// 차단 사유를 기록에 남기기 위한 고정 오류(실행 실패와 구분된다).
var errDependencyOpen = errors.New("terminal deferred: preceding settlement is still OPEN")

// guard가 없으면 dependency를 확인할 수 없으므로 fail-closed로 실행을 금지한다.
var errNoDependencyGuard = errors.New("dependency guard unavailable")

// dependencyBlocked는 guard==nil이면 fail-closed로 (false, errNoDependencyGuard)를 돌려준다.
func dependencyBlocked(guard settlementDependencyGuard, orderID uint) (bool, error) {
	if guard == nil {
		return false, errNoDependencyGuard
	}
	return guard.HasOpenFailureForOrder(orderID)
}

// retryTransient는 defer 기록에만 쓰는 유한 백오프다 — worker job 안에서 실행되므로
// dispatcher를 막지 않는다. transient가 아니면 즉시 강등한다.
func retryTransient(fn func() error) error {
	err := fn()
	for attempt := 0; err != nil && service.IsTransientSettlementError(err) && attempt < len(transientRetryDelays); attempt++ {
		time.Sleep(transientRetryDelays[attempt])
		err = fn()
	}
	return err
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
	if event.OrderCancelled != nil {
		return event.OrderCancelled.CoinSymbol
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
//	true면 호출자는 별도 MarkProcessed를 하지 않는다(왕복 절약). transactionalOutboxID>0인
//	trade 성공 경로에서만 true다 — 리플레이(0)·실패기록·시장가 완료는 false.
//
// transactionalOutboxID는 정산 트랜잭션 안에서 PROCESSED 마킹할 ID(live: 행 ID,
// replay: 0 — 리플레이어가 직접 마킹한다). sourceOutboxID는 원본 이벤트 provenance로,
// live 경로는 둘이 같고 replay는 항상 실제 행 ID다 — 실패 기록의 출처 추적에 쓴다.
func processExecutionEvent(
	event matching.ExecutionEvent,
	transactionalOutboxID uint64,
	sourceOutboxID uint64,
	settler tradeSettler,
	failureRecorder settlementFailureRecorder,
	marketCompleter marketOrderCompleter,
	completionFailureRecorder marketCompletionFailureRecorder,
	cancelProcessor orderCancellationProcessor,
	guard settlementDependencyGuard,
	cancelDeferStore cancellationDeferStore,
	broadcast func(coinSymbol string, payload []byte),
	logger *log.Logger,
) (handled bool, markedInTx bool) {
	if event.Trade != nil {
		return processTradeSettlement(event.Trade, transactionalOutboxID, settler, failureRecorder, broadcast, logger)
	}
	if event.MarketOrderDone != nil {
		return processMarketOrderDone(event.MarketOrderDone, marketCompleter, guard, completionFailureRecorder, logger), false
	}
	if event.OrderCancelled != nil {
		return processOrderCancellationEvent(event.OrderCancelled, sourceOutboxID, cancelProcessor, guard, cancelDeferStore, logger), false
	}
	return true, false
}

// processSingleOutboxEvent는 outbox 이벤트 1건을 단건 경로로 처리하고, 처리가
// 내구적으로 확정됐는지(handled)를 반환한다. false면 outbox 행이 PENDING으로 남는다 —
// 호출자는 이를 dependency 미확정(undurable)으로 취급해야 한다.
func processSingleOutboxEvent(
	outboxEvent service.OutboxEvent,
	settler tradeSettler,
	failureRecorder settlementFailureRecorder,
	marketCompleter marketOrderCompleter,
	completionFailureRecorder marketCompletionFailureRecorder,
	cancelProcessor orderCancellationProcessor,
	guard settlementDependencyGuard,
	cancelDeferStore cancellationDeferStore,
	broadcast func(coinSymbol string, payload []byte),
	outboxRepo outboxMarker,
	logger *log.Logger,
) bool {
	handled, markedInTx := processExecutionEvent(outboxEvent.Event, outboxEvent.OutboxID, outboxEvent.OutboxID, settler, failureRecorder, marketCompleter, completionFailureRecorder, cancelProcessor, guard, cancelDeferStore, broadcast, logger)
	if !handled {
		// 내구 확정 실패(정산 실패의 기록조차 실패) — PENDING으로 남겨
		// 다음 부팅 리플레이가 재시도한다.
		return false
	}
	if markedInTx {
		// 정산 트랜잭션이 outbox 마킹까지 이미 커밋했다 — 별도 왕복 불필요.
		return true
	}
	if err := outboxRepo.MarkProcessed(outboxEvent.OutboxID); err != nil {
		// 마킹 실패는 유실이 아니라 다음 리플레이의 멱등 재처리일 뿐 — 정산은 커밋됐으므로
		// dependency는 충족이다(undurable이 아니다).
		logger.Printf("mark outbox event %d processed failed: %v", outboxEvent.OutboxID, err)
	}
	return true
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
// 반환값은 내구 확정에 실패한(handled=false) trade의 maker·taker 주문 ID다 —
// 이 주문들의 terminal은 실행하면 안 된다(dispatcher가 quarantine한다).
func settleTradeBatchWithFallback(
	batch []service.OutboxEvent,
	batchSettler tradeBatchSettler,
	settler tradeSettler,
	failureRecorder settlementFailureRecorder,
	marketCompleter marketOrderCompleter,
	completionFailureRecorder marketCompletionFailureRecorder,
	cancelProcessor orderCancellationProcessor,
	guard settlementDependencyGuard,
	cancelDeferStore cancellationDeferStore,
	broadcast func(coinSymbol string, payload []byte),
	outboxRepo outboxMarker,
	logger *log.Logger,
) []uint {
	items := make([]service.TradeBatchItem, len(batch))
	for i, event := range batch {
		items[i] = service.TradeBatchItem{Trade: event.Event.Trade, OutboxEventID: event.OutboxID}
	}
	attemptStart := time.Now()
	results, err := batchSettler.SettleTradeBatch(items)
	metrics.SettlementAttemptBatch.Observe(time.Since(attemptStart).Seconds())
	if err != nil {
		metrics.SettlementBatchFallbacksTotal.Inc()
		logger.Printf("settle trade batch of %d failed, falling back to per-trade settlement: %v", len(batch), err)
		var undurable []uint
		for _, event := range batch {
			if processSingleOutboxEvent(event, settler, failureRecorder, marketCompleter, completionFailureRecorder, cancelProcessor, guard, cancelDeferStore, broadcast, outboxRepo, logger) {
				continue
			}
			undurable = append(undurable, event.Event.Trade.BuyOrderID, event.Event.Trade.SellOrderID)
		}
		return undurable
	}
	metrics.SettlementBatchSize.Observe(float64(len(batch)))
	applied := make([]*model.Trade, 0, len(batch))
	for i, result := range results {
		if result.Applied {
			applied = append(applied, batch[i].Event.Trade)
		}
	}
	broadcastSettledTrades(applied, broadcast, logger)
	return nil
}

func processMarketOrderDone(
	done *matching.MarketOrderDone,
	completer marketOrderCompleter,
	guard settlementDependencyGuard,
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

	blocked, depErr := dependencyBlocked(guard, done.OrderID)
	if depErr != nil {
		// fail-closed: 확인하지 못하면 실행하지 않는다.
		logger.Printf("market order completion dependency check failed for order %d: %v", done.OrderID, depErr)
		return deferMarketOrderDone(input, done.CoinSymbol, failureRecorder, depErr, "dependency_open", logger)
	}
	if blocked {
		return deferMarketOrderDone(input, done.CoinSymbol, failureRecorder, errDependencyOpen, "dependency_open", logger)
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
	recordErr := retryTransient(func() error {
		_, e := failureRecorder.RecordFailure(input, done.CoinSymbol, err)
		return e
	})
	if recordErr != nil {
		logger.Printf("record failed market completion failed: %v", recordErr)
		metrics.SettlementTerminalDeferRecordFailed.WithLabelValues("market_done").Inc()
		return false
	}
	return true
}

func deferMarketOrderDone(
	input service.CompleteMarketOrderInput,
	coinSymbol string,
	failureRecorder marketCompletionFailureRecorder,
	reason error,
	reasonLabel string,
	logger *log.Logger,
) bool {
	if failureRecorder == nil {
		return false
	}
	if err := retryTransient(func() error {
		_, e := failureRecorder.EnsureDeferred(input, coinSymbol, reason)
		return e
	}); err != nil {
		logger.Printf("ensure deferred market order completion %d failed: %v", input.OrderID, err)
		metrics.SettlementTerminalDeferRecordFailed.WithLabelValues("market_done").Inc()
		return false
	}
	metrics.SettlementTerminalDeferred.WithLabelValues("market_done", reasonLabel).Inc()
	return true
}

// processOrderCancellationEvent는 OrderCancelled 실행 이벤트를 ProcessOrderCancellation으로
// 확정한다. 취소 terminal 실패 또는 dependency 차단은 failed_order_cancellations에
// 내구적으로 인계한다. 인계가 성공하면 원본 outbox를 PROCESSED로 마킹하고
// SettlementRetryWorker가 온라인으로 복구한다. 실패 기록까지 실패한 경우에만
// outbox PENDING 부팅 backstop으로 강등된다.
func processOrderCancellationEvent(
	cancelled *matching.OrderCancelled,
	sourceOutboxID uint64,
	processor orderCancellationProcessor,
	guard settlementDependencyGuard,
	deferStore cancellationDeferStore,
	logger *log.Logger,
) bool {
	if logger == nil {
		logger = log.Default()
	}
	if processor == nil || cancelled == nil {
		return true
	}

	blocked, depErr := dependencyBlocked(guard, cancelled.OrderID)
	if depErr != nil {
		// fail-closed: 확인하지 못하면 실행하지 않는다.
		logger.Printf("cancellation dependency check failed for order %d: %v", cancelled.OrderID, depErr)
		return deferCancellation(cancelled, sourceOutboxID, deferStore, depErr, "dependency_open", logger)
	}
	if blocked {
		return deferCancellation(cancelled, sourceOutboxID, deferStore, errDependencyOpen, "dependency_open", logger)
	}

	err := processor.ProcessOrderCancellation(*cancelled)
	for attempt := 0; err != nil && service.IsTransientSettlementError(err) && attempt < len(transientRetryDelays); attempt++ {
		time.Sleep(transientRetryDelays[attempt])
		err = processor.ProcessOrderCancellation(*cancelled)
	}
	if err == nil {
		return true
	}

	logger.Printf("process order cancellation failed: %v", err)
	if deferStore == nil {
		return false
	}
	recordErr := retryTransient(func() error {
		_, e := deferStore.RecordFailure(*cancelled, sourceOutboxID, err)
		return e
	})
	if recordErr != nil {
		logger.Printf("record failed order cancellation failed: %v", recordErr)
		metrics.SettlementTerminalDeferRecordFailed.WithLabelValues("cancel").Inc()
		return false
	}
	return true
}

func deferCancellation(
	cancelled *matching.OrderCancelled,
	sourceOutboxID uint64,
	deferStore cancellationDeferStore,
	reason error,
	reasonLabel string,
	logger *log.Logger,
) bool {
	if deferStore == nil {
		return false
	}
	if err := retryTransient(func() error {
		_, e := deferStore.EnsureDeferred(*cancelled, sourceOutboxID, reason)
		return e
	}); err != nil {
		logger.Printf("ensure deferred order cancellation %d failed: %v", cancelled.OrderID, err)
		metrics.SettlementTerminalDeferRecordFailed.WithLabelValues("cancel").Inc()
		return false
	}
	metrics.SettlementTerminalDeferred.WithLabelValues("cancel", reasonLabel).Inc()
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

	settlementStart := time.Now() // 기존 그대로(논리 전체)
	attemptStart := time.Now()
	result, err := settler.SettleTrade(trade, outboxEventID)
	metrics.SettlementAttemptSingle.Observe(time.Since(attemptStart).Seconds())
	for attempt := 0; err != nil && service.IsTransientSettlementError(err) && attempt < len(transientRetryDelays); attempt++ {
		time.Sleep(transientRetryDelays[attempt])
		attemptStart = time.Now()
		result, err = settler.SettleTrade(trade, outboxEventID)
		metrics.SettlementAttemptSingle.Observe(time.Since(attemptStart).Seconds())
	}
	metrics.OrderSettlementDuration.Observe(time.Since(settlementStart).Seconds()) // 기존 유지
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

	broadcastSettledTrades([]*model.Trade{trade}, broadcast, logger)
	return true, markedInTx
}

// broadcastSettledTrades는 이미 커밋된 정산의 trade들을 심볼별로 그룹핑해(등장 순서·
// 배치 내 순서 보존) 심볼당 "trades" 배열 메시지 1건으로 마샬·브로드캐스트한다. 단건
// 경로도 원소 1개짜리 배치로 호출해 와이어 형식을 하나로 유지한다. 마샬 실패는 정산
// 내구성과 무관하므로 로그만 남기고 조용히 건너뛴다.
func broadcastSettledTrades(trades []*model.Trade, broadcast func(coinSymbol string, payload []byte), logger *log.Logger) {
	if len(trades) == 0 {
		return
	}

	symbolOrder := make([]string, 0, len(trades))
	grouped := make(map[string][]map[string]interface{}, len(trades))
	for _, trade := range trades {
		if _, seen := grouped[trade.CoinSymbol]; !seen {
			symbolOrder = append(symbolOrder, trade.CoinSymbol)
		}
		grouped[trade.CoinSymbol] = append(grouped[trade.CoinSymbol], map[string]interface{}{
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
		})
	}

	for _, symbol := range symbolOrder {
		tradesJSON, err := json.Marshal(map[string]interface{}{
			"type": "trades",
			"data": grouped[symbol],
		})
		if err != nil {
			logger.Printf("marshal trades broadcast failed: %v", err)
			continue
		}
		broadcast(symbol, tradesJSON)
	}
}
