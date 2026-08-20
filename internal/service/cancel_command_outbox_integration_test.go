package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"fmt"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// blockableOutboxRepo는 outbox 커밋을 테스트가 원하는 지점에서 멈춘다.
// "엔진은 제거했지만 outbox는 아직 커밋되지 않은" 창을 만들기 위한 유일한 방법이다.
type blockableOutboxRepo struct {
	inner *repository.TradeOutboxRepository

	mu      sync.Mutex
	gate    chan struct{} // nil이 아니면 커밋 직전 여기서 대기
	entered chan struct{} // 커밋 시도를 알린다
}

func (r *blockableOutboxRepo) InsertBatchAndMarkCancelCommands(events []*model.TradeOutboxEvent, commandIDs []uint64) error {
	r.mu.Lock()
	gate := r.gate
	entered := r.entered
	r.mu.Unlock()

	if gate != nil {
		if entered != nil {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
		<-gate
	}
	return r.inner.InsertBatchAndMarkCancelCommands(events, commandIDs)
}

func (r *blockableOutboxRepo) block() (release func(), entered <-chan struct{}) {
	gate := make(chan struct{})
	enteredCh := make(chan struct{}, 1)

	r.mu.Lock()
	r.gate = gate
	r.entered = enteredCh
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			r.gate = nil
			r.entered = nil
			r.mu.Unlock()
			close(gate)
		})
	}, enteredCh
}

// cancelPipelineHarness는 실제 Postgres·엔진·OutboxWriter·worker·OrderService를
// 한 런타임처럼 묶는다. 크래시 창은 "이 중 일부만 멈춘 상태"로 재현한다.
type cancelPipelineHarness struct {
	t  *testing.T
	db *gorm.DB

	engine       *matching.MatchingEngine
	orderService *OrderService
	settlement   *SettlementService
	outboxRepo   *blockableOutboxRepo
	commandRepo  *repository.CancelCommandRepository
	worker       *CancelCommandWorker

	forwarded    chan OutboxEvent
	writerDone   chan struct{}
	workerCancel context.CancelFunc
	workerDone   chan struct{}
}

func newCancelPipelineHarness(t *testing.T, db *gorm.DB) *cancelPipelineHarness {
	t.Helper()

	engine := matching.NewMatchingEngine()
	engine.Start()

	orderRepo := repository.NewOrderRepository(db)
	walletRepo := repository.NewWalletRepository(db)
	orderService := NewOrderService(orderRepo, walletRepo, engine)
	commandRepo := repository.NewCancelCommandRepository(db)
	orderService.CancelCommandRepository = commandRepo

	outboxRepo := &blockableOutboxRepo{inner: repository.NewTradeOutboxRepository(db)}
	forwarded := make(chan OutboxEvent, 32)
	writer := &OutboxWriter{
		Repo:          outboxRepo,
		Source:        engine.ExecutionCh,
		Forward:       func(event OutboxEvent) { forwarded <- event },
		BatchSize:     1,
		FlushInterval: 5 * time.Millisecond,
	}
	writerDone := make(chan struct{})
	go func() {
		writer.Run()
		close(writerDone)
	}()

	worker := NewCancelCommandWorker(commandRepo, orderRepo, engine)
	worker.PollInterval = 10 * time.Millisecond
	orderService.CancelCommandWake = worker.Wake

	harness := &cancelPipelineHarness{
		t:            t,
		db:           db,
		engine:       engine,
		orderService: orderService,
		settlement:   NewSettlementService(db, orderRepo, walletRepo),
		outboxRepo:   outboxRepo,
		commandRepo:  commandRepo,
		worker:       worker,
		forwarded:    forwarded,
		writerDone:   writerDone,
	}
	t.Cleanup(harness.stop)
	return harness
}

func (h *cancelPipelineHarness) startWorker() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.workerCancel = cancel
	h.workerDone = make(chan struct{})
	go func() {
		h.worker.Run(ctx)
		close(h.workerDone)
	}()
}

func (h *cancelPipelineHarness) stopWorker() {
	h.t.Helper()
	if h.workerCancel == nil {
		return
	}
	h.workerCancel()
	select {
	case <-h.workerDone:
	case <-time.After(5 * time.Second):
		h.t.Fatal("cancel worker가 정지하지 않았다")
	}
	h.workerCancel = nil
}

func (h *cancelPipelineHarness) stop() {
	h.stopWorker()
	h.engine.Stop()
	select {
	case <-h.engine.Done():
	case <-time.After(5 * time.Second):
		h.t.Error("엔진이 드레인되지 않았다")
	}
	select {
	case <-h.writerDone:
	case <-time.After(5 * time.Second):
		h.t.Error("outbox writer가 종료되지 않았다")
	}
}

// settleForwarded는 정산 파이프라인이 하는 일을 테스트에서 직접 수행한다.
func (h *cancelPipelineHarness) settleForwarded(event OutboxEvent) {
	h.t.Helper()
	switch {
	case event.Event.OrderCancelled != nil:
		require.NoError(h.t, h.orderService.ProcessOrderCancellation(*event.Event.OrderCancelled))
	case event.Event.Trade != nil:
		_, err := h.settlement.SettleTrade(event.Event.Trade, event.OutboxID)
		require.NoError(h.t, err)
	}
	require.NoError(h.t, repository.NewTradeOutboxRepository(h.db).MarkProcessed(event.OutboxID))
}

func (h *cancelPipelineHarness) submitToEngine(order model.Order, remaining decimal.Decimal) {
	h.t.Helper()
	submitIntegrationEngineOrder(h.t, h.engine, order, remaining)
}

func (h *cancelPipelineHarness) commandStatus(id uint64) model.CancelCommandStatus {
	h.t.Helper()
	var command model.CancelCommand
	require.NoError(h.t, h.db.Where("id = ?", id).First(&command).Error)
	return command.Status
}

func (h *cancelPipelineHarness) orderStatus(orderID uint) model.OrderStatus {
	h.t.Helper()
	var order model.Order
	require.NoError(h.t, h.db.First(&order, orderID).Error)
	return order.Status
}

func (h *cancelPipelineHarness) releaseEntryCount(userID uint, orderID uint) int64 {
	h.t.Helper()
	var count int64
	require.NoError(h.t, h.db.Model(&model.LedgerEntry{}).
		Where("user_id = ? AND entry_type = ? AND reference_type = ? AND reference_id = ?",
			userID, model.LedgerEntryTypeOrderRelease, model.LedgerReferenceTypeOrder, orderID).
		Count(&count).Error)
	return count
}

// harnessSymbol은 테스트마다 고유한 심볼을 만든다. 공유 test DB에는 다른 테스트의
// outbox 행이 함께 있으므로, 공통 심볼("BTC")을 쓰면 정리와 replay가 남의 행까지
// 건드린다.
func harnessSymbol(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("CX%d", time.Now().UnixNano()%100_000_000)
}

func cleanupHarnessOutbox(t *testing.T, db *gorm.DB, symbol string) {
	t.Helper()
	t.Cleanup(func() {
		require.NoError(t, db.Where("coin_symbol = ?", symbol).Delete(&model.TradeOutboxEvent{}).Error)
	})
}

// symbolScopedReplaySource는 이 하니스가 만든 행만 replay 대상으로 노출한다.
// 감싸지 않으면 replayer가 DB의 모든 PENDING 행을 읽어 무관한 이벤트까지
// PROCESSED로 바꾼다.
type symbolScopedReplaySource struct {
	inner  *repository.TradeOutboxRepository
	symbol string
}

func (s *symbolScopedReplaySource) FindPendingAfter(afterID uint64, limit int) ([]model.TradeOutboxEvent, error) {
	var events []model.TradeOutboxEvent
	err := s.inner.DB.
		Where("status = ? AND coin_symbol = ? AND id > ?", model.TradeOutboxStatusPending, s.symbol, afterID).
		Order("id ASC").Limit(limit).Find(&events).Error
	return events, err
}

func (s *symbolScopedReplaySource) MarkProcessed(id uint64) error {
	return s.inner.MarkProcessed(id)
}

// 검증 1: command만 커밋된 상태에서 죽어도(=outbox 커밋 전) 재기동한 worker가
// 취소를 완료한다.
func TestIntegrationCancelCommandCrashBeforeOutboxIsRecovered(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(40)
	defer cleanupServiceUsers(t, db, userID)

	symbol := harnessSymbol(t)
	order := seedCancelOrderRows(t, db, cancelOrderSeed{
		UserID:        userID,
		CoinSymbol:    symbol,
		Side:          model.OrderSideBuy,
		Status:        model.OrderStatusPending,
		Price:         decimal.NewFromInt(100),
		Amount:        decimal.NewFromInt(5),
		FilledAmount:  decimal.Zero,
		LockedBalance: decimal.RequireFromString("500.25"),
	})
	cleanupHarnessOutbox(t, db, symbol)

	// 1차 런타임: worker를 시작하지 않은 채 취소만 접수하고 "죽는다".
	first := newCancelPipelineHarness(t, db)
	result, err := first.orderService.CancelOrder(CancelOrderInput{UserID: userID, OrderID: order.ID})
	require.NoError(t, err)
	defer cleanupServiceCancelCommands(t, db, result.CommandID)

	require.Equal(t, model.CancelCommandStatusPending, first.commandStatus(result.CommandID))
	first.stop()

	// 2차 런타임: bootstrap이 주문을 다시 올리고 worker가 command를 재실행한다.
	second := newCancelPipelineHarness(t, db)
	second.submitToEngine(order, decimal.NewFromInt(5))
	second.startWorker()

	require.Eventually(t, func() bool {
		return second.commandStatus(result.CommandID) == model.CancelCommandStatusProcessed
	}, 5*time.Second, 20*time.Millisecond, "재기동 후 command가 완료되지 않았다")

	event := requireForwardedOutboxEvent(t, second.forwarded)
	require.NotNil(t, event.Event.OrderCancelled)
	assert.Equal(t, result.CommandID, event.Event.OrderCancelled.CommandID)
	second.settleForwarded(event)

	assert.Equal(t, model.OrderStatusCancelled, second.orderStatus(order.ID))
	assert.EqualValues(t, 1, second.releaseEntryCount(userID, order.ID))
}

// 검증 2: outbox는 커밋됐고 command는 PROCESSED인데 정산 전에 죽으면,
// 기존 outbox replay가 마무리한다.
func TestIntegrationCancelCommandCrashAfterOutboxIsFinishedByReplay(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(41)
	defer cleanupServiceUsers(t, db, userID)

	symbol := harnessSymbol(t)
	order := seedCancelOrderRows(t, db, cancelOrderSeed{
		UserID:        userID,
		CoinSymbol:    symbol,
		Side:          model.OrderSideBuy,
		Status:        model.OrderStatusPending,
		Price:         decimal.NewFromInt(100),
		Amount:        decimal.NewFromInt(5),
		FilledAmount:  decimal.Zero,
		LockedBalance: decimal.RequireFromString("500.25"),
	})
	cleanupHarnessOutbox(t, db, symbol)

	harness := newCancelPipelineHarness(t, db)
	harness.submitToEngine(order, decimal.NewFromInt(5))
	harness.startWorker()

	result, err := harness.orderService.CancelOrder(CancelOrderInput{UserID: userID, OrderID: order.ID})
	require.NoError(t, err)
	defer cleanupServiceCancelCommands(t, db, result.CommandID)

	// outbox까지 커밋되기를 기다린 뒤, 정산은 하지 않고 런타임을 멈춘다.
	require.Eventually(t, func() bool {
		return harness.commandStatus(result.CommandID) == model.CancelCommandStatusProcessed
	}, 5*time.Second, 20*time.Millisecond)
	requireForwardedOutboxEvent(t, harness.forwarded) // forward는 받았지만 정산하지 않는다
	harness.stop()

	assert.Equal(t, model.OrderStatusPending, harness.orderStatus(order.ID), "정산 전이므로 아직 PENDING이다")

	// 재기동: outbox replay가 남은 PENDING 이벤트를 마무리한다.
	replayer := &OutboxReplayer{
		Repo:     &symbolScopedReplaySource{inner: repository.NewTradeOutboxRepository(db), symbol: symbol},
		PageSize: 32,
		Process: func(_ uint64, event matching.ExecutionEvent) bool {
			if event.OrderCancelled == nil {
				t.Errorf("이 심볼에는 취소 이벤트만 있어야 한다: %+v", event)
				return false
			}
			return harness.orderService.ProcessOrderCancellation(*event.OrderCancelled) == nil
		},
	}
	_, err = replayer.Replay()
	require.NoError(t, err)

	assert.Equal(t, model.OrderStatusCancelled, harness.orderStatus(order.ID))
	assert.EqualValues(t, 1, harness.releaseEntryCount(userID, order.ID))
}

// 검증 3: 202 직후 종료해도 주문이 부활하지 않는다. bootstrap으로 복원되더라도
// 부팅 장벽의 drain이 끝나기 전에는 트래픽을 받지 않으므로 live 주문과 체결되지 않는다.
func TestIntegrationCancelCommandRestartDoesNotResurrectOrder(t *testing.T) {
	db := openServiceIntegrationDB(t)
	makerID := serviceTestUserID(42)
	defer cleanupServiceUsers(t, db, makerID)

	symbol := harnessSymbol(t)
	order := seedCancelOrderRows(t, db, cancelOrderSeed{
		UserID:        makerID,
		CoinSymbol:    symbol,
		Side:          model.OrderSideBuy,
		Status:        model.OrderStatusPending,
		Price:         decimal.NewFromInt(100),
		Amount:        decimal.NewFromInt(5),
		FilledAmount:  decimal.Zero,
		LockedBalance: decimal.RequireFromString("500.25"),
	})
	cleanupHarnessOutbox(t, db, symbol)

	first := newCancelPipelineHarness(t, db)
	result, err := first.orderService.CancelOrder(CancelOrderInput{UserID: makerID, OrderID: order.ID})
	require.NoError(t, err)
	defer cleanupServiceCancelCommands(t, db, result.CommandID)
	first.stop()

	// 재기동: bootstrap이 그 주문을 오더북에 다시 올린다.
	second := newCancelPipelineHarness(t, db)
	second.submitToEngine(order, decimal.NewFromInt(5))

	// 부팅 장벽 — drain이 끝나야만 트래픽을 받는다.
	second.startWorker()
	barrierCtx, cancelBarrier := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelBarrier()
	require.NoError(t, second.worker.WaitUntilDrained(barrierCtx))

	// 장벽 통과 후 들어온 crossing 주문은 그 maker와 체결되면 안 된다.
	second.engine.OrderCh <- &matching.Order{
		ID:         order.ID + 900_000,
		UserID:     makerID + 1,
		CoinSymbol: symbol,
		Side:       model.OrderSideSell,
		Price:      decimal.NewFromInt(100),
		Amount:     decimal.NewFromInt(5),
		OrderType:  model.OrderTypeLimit,
		CreatedAt:  time.Now(),
	}

	// sentinel: 같은 오더북은 OrderCh를 FIFO로 처리하므로, 뒤에 넣은 이 주문이
	// 보이면 crossing 주문의 매칭도 이미 끝났다는 뜻이다. snapshot 채널을 그냥
	// 읽으면 앞서 남은 취소 snapshot을 집어 아직 처리 전에 판정할 수 있다.
	second.engine.OrderCh <- &matching.Order{
		ID:         order.ID + 900_001,
		UserID:     makerID + 2,
		CoinSymbol: symbol,
		Side:       model.OrderSideSell,
		Price:      decimal.NewFromInt(100_000),
		Amount:     decimal.NewFromInt(1),
		OrderType:  model.OrderTypeLimit,
		CreatedAt:  time.Now(),
	}
	// 살아 있는 엔진의 BTree를 테스트 goroutine에서 직접 순회하면 매칭 루프와
	// 데이터 레이스다. RequestOrderBookSnapshot은 캐시를 락 없이 읽어 매칭과
	// 경쟁하지 않는 유일한 조회 경로다.
	sentinelPrice := decimal.NewFromInt(100_000)
	require.Eventually(t, func() bool {
		snapshot, err := second.engine.RequestOrderBookSnapshot(symbol, matching.DefaultSnapshotDepth)
		return err == nil && snapshotHasLevel(snapshot.Asks, sentinelPrice)
	}, 5*time.Second, 20*time.Millisecond, "sentinel 주문이 스냅샷에 반영되지 않았다")

	// 판정: crossing 매도가 체결되지 않고 그대로 남고, 취소된 매수는 없어야 한다.
	//
	// trades 테이블로는 판정할 수 없다 — 그 행은 SettlementService가 만드는데
	// 이 하니스는 정산을 돌리지 않으므로 실제로 체결돼도 0이다.
	snapshot, err := second.engine.RequestOrderBookSnapshot(symbol, matching.DefaultSnapshotDepth)
	require.NoError(t, err)
	crossingPrice := decimal.NewFromInt(100)

	assert.True(t, snapshotHasLevel(snapshot.Asks, crossingPrice),
		"crossing 매도가 사라졌다 — 취소된 주문이 부활해 체결됐다: %+v", snapshot)
	assert.False(t, snapshotHasLevel(snapshot.Bids, crossingPrice),
		"취소된 매수 주문이 오더북에 남아 있다: %+v", snapshot)
}

func snapshotHasLevel(levels []matching.PriceLevelData, price decimal.Decimal) bool {
	for _, level := range levels {
		if level.Price.Equal(price) && level.Quantity.IsPositive() {
			return true
		}
	}
	return false
}

// 검증 4: 동시 100회 취소에도 hold 해제는 한 번뿐이다. 판정 기준은 상태가 아니라
// ORDER_RELEASE 원장 건수다 — ProcessOrderCancellation은 no-op일 때도 성공한다.
func TestIntegrationCancelCommandConcurrentRequestsReleaseHoldOnce(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(43)
	defer cleanupServiceUsers(t, db, userID)

	symbol := harnessSymbol(t)
	order := seedCancelOrderRows(t, db, cancelOrderSeed{
		UserID:        userID,
		CoinSymbol:    symbol,
		Side:          model.OrderSideBuy,
		Status:        model.OrderStatusPending,
		Price:         decimal.NewFromInt(100),
		Amount:        decimal.NewFromInt(5),
		FilledAmount:  decimal.Zero,
		LockedBalance: decimal.RequireFromString("500.25"),
	})
	cleanupHarnessOutbox(t, db, symbol)

	harness := newCancelPipelineHarness(t, db)
	harness.submitToEngine(order, decimal.NewFromInt(5))
	harness.startWorker()

	const concurrency = 100
	ids := make([]uint64, concurrency)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			result, err := harness.orderService.CancelOrder(CancelOrderInput{UserID: userID, OrderID: order.ID})
			if err == nil {
				ids[i] = result.CommandID
			}
		}(i)
	}
	close(start)
	wg.Wait()

	require.NotZero(t, ids[0])
	defer cleanupServiceCancelCommands(t, db, ids[0])
	for i, id := range ids {
		assert.Equal(t, ids[0], id, "goroutine %d가 다른 command를 받았다", i)
	}

	require.Eventually(t, func() bool {
		return harness.commandStatus(ids[0]) == model.CancelCommandStatusProcessed
	}, 5*time.Second, 20*time.Millisecond)

	event := requireForwardedOutboxEvent(t, harness.forwarded)
	require.NotNil(t, event.Event.OrderCancelled)
	harness.settleForwarded(event)

	assert.EqualValues(t, 1, harness.releaseEntryCount(userID, order.ID), "hold가 두 번 해제됐다")

	var commandCount int64
	require.NoError(t, db.Model(&model.CancelCommand{}).
		Where("order_id = ?", order.ID).Count(&commandCount).Error)
	assert.EqualValues(t, 1, commandCount)
}

// 검증 4b: command가 PROCESSED이고 정산은 아직인 창에서 재요청해도
// ORDER_RELEASE는 여전히 1건이다.
func TestIntegrationCancelCommandRepeatBeforeSettlementReleasesOnce(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(44)
	defer cleanupServiceUsers(t, db, userID)

	symbol := harnessSymbol(t)
	order := seedCancelOrderRows(t, db, cancelOrderSeed{
		UserID:        userID,
		CoinSymbol:    symbol,
		Side:          model.OrderSideBuy,
		Status:        model.OrderStatusPending,
		Price:         decimal.NewFromInt(100),
		Amount:        decimal.NewFromInt(5),
		FilledAmount:  decimal.Zero,
		LockedBalance: decimal.RequireFromString("500.25"),
	})
	cleanupHarnessOutbox(t, db, symbol)

	harness := newCancelPipelineHarness(t, db)
	harness.submitToEngine(order, decimal.NewFromInt(5))
	harness.startWorker()

	first, err := harness.orderService.CancelOrder(CancelOrderInput{UserID: userID, OrderID: order.ID})
	require.NoError(t, err)
	defer cleanupServiceCancelCommands(t, db, first.CommandID)

	require.Eventually(t, func() bool {
		return harness.commandStatus(first.CommandID) == model.CancelCommandStatusProcessed
	}, 5*time.Second, 20*time.Millisecond)

	// 아직 정산하지 않은 채 재요청 — 여기가 부분 UNIQUE였다면 열렸을 창이다.
	second, err := harness.orderService.CancelOrder(CancelOrderInput{UserID: userID, OrderID: order.ID})
	require.NoError(t, err)
	assert.Equal(t, first.CommandID, second.CommandID)

	event := requireForwardedOutboxEvent(t, harness.forwarded)
	harness.settleForwarded(event)

	assert.EqualValues(t, 1, harness.releaseEntryCount(userID, order.ID))
}

// 검증 5: 이미 체결된 주문의 취소는 NOOP이고 실패가 아니다. hold 해제도 없다.
func TestIntegrationCancelCommandOnFilledOrderBecomesNoop(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(45)
	defer cleanupServiceUsers(t, db, userID)

	symbol := harnessSymbol(t)
	order := seedCancelOrderRows(t, db, cancelOrderSeed{
		UserID:        userID,
		CoinSymbol:    symbol,
		Side:          model.OrderSideBuy,
		Status:        model.OrderStatusPending,
		Price:         decimal.NewFromInt(100),
		Amount:        decimal.NewFromInt(5),
		FilledAmount:  decimal.Zero,
		LockedBalance: decimal.RequireFromString("500.25"),
	})
	cleanupHarnessOutbox(t, db, symbol)

	harness := newCancelPipelineHarness(t, db)
	result, err := harness.orderService.CancelOrder(CancelOrderInput{UserID: userID, OrderID: order.ID})
	require.NoError(t, err)
	defer cleanupServiceCancelCommands(t, db, result.CommandID)

	// 취소 접수 후, worker가 돌기 전에 주문이 체결됐다.
	require.NoError(t, db.Model(&model.Order{}).Where("id = ?", order.ID).
		Updates(map[string]any{
			"status":        model.OrderStatusFilled,
			"filled_amount": decimal.NewFromInt(5),
		}).Error)

	harness.startWorker()

	require.Eventually(t, func() bool {
		return harness.commandStatus(result.CommandID) == model.CancelCommandStatusNoop
	}, 5*time.Second, 20*time.Millisecond, "체결된 주문의 command가 NOOP이 되지 않았다")

	// NOOP은 종결이므로 재시도 루프가 돌지 않고, hold 해제도 없다.
	assert.Zero(t, harness.releaseEntryCount(userID, order.ID))
	assert.Equal(t, model.OrderStatusFilled, harness.orderStatus(order.ID))
}

// 검증 6: DB는 open인데 엔진이 못 찾으면 command를 지우지 않고 재시도한다.
func TestIntegrationCancelCommandStaysPendingWhenEngineMissesOpenOrder(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(46)
	defer cleanupServiceUsers(t, db, userID)

	symbol := harnessSymbol(t)
	order := seedCancelOrderRows(t, db, cancelOrderSeed{
		UserID:        userID,
		CoinSymbol:    symbol,
		Side:          model.OrderSideBuy,
		Status:        model.OrderStatusPending,
		Price:         decimal.NewFromInt(100),
		Amount:        decimal.NewFromInt(5),
		FilledAmount:  decimal.Zero,
		LockedBalance: decimal.RequireFromString("500.25"),
	})
	cleanupHarnessOutbox(t, db, symbol)

	// 주문을 엔진에 올리지 않는다 — 엔진 관점에서는 "없음"이다.
	harness := newCancelPipelineHarness(t, db)
	result, err := harness.orderService.CancelOrder(CancelOrderInput{UserID: userID, OrderID: order.ID})
	require.NoError(t, err)
	defer cleanupServiceCancelCommands(t, db, result.CommandID)

	harness.startWorker()

	require.Eventually(t, func() bool {
		var command model.CancelCommand
		require.NoError(t, db.Where("id = ?", result.CommandID).First(&command).Error)
		return command.AttemptCount >= 2
	}, 5*time.Second, 20*time.Millisecond, "재시도가 기록되지 않았다")

	assert.Equal(t, model.CancelCommandStatusPending, harness.commandStatus(result.CommandID),
		"DB가 open인데 command가 종결됐다")
	assert.Zero(t, harness.releaseEntryCount(userID, order.ID))
}

// 검증 8d(통합): outbox 커밋을 막아 두면 worker는 같은 command를 재투입하지 않는다.
// 재투입되면 엔진은 이미 제거된 주문을 못 찾고(not-found), DB 주문은 아직 open이라
// worker는 NOOP이 아니라 RecordAttempt + backoff로 간다.
func TestIntegrationCancelCommandDoesNotRedispatchWhileOutboxIsBlocked(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(47)
	defer cleanupServiceUsers(t, db, userID)

	symbol := harnessSymbol(t)
	order := seedCancelOrderRows(t, db, cancelOrderSeed{
		UserID:        userID,
		CoinSymbol:    symbol,
		Side:          model.OrderSideBuy,
		Status:        model.OrderStatusPending,
		Price:         decimal.NewFromInt(100),
		Amount:        decimal.NewFromInt(5),
		FilledAmount:  decimal.Zero,
		LockedBalance: decimal.RequireFromString("500.25"),
	})
	cleanupHarnessOutbox(t, db, symbol)

	harness := newCancelPipelineHarness(t, db)
	harness.submitToEngine(order, decimal.NewFromInt(5))

	release, entered := harness.outboxRepo.block()
	harness.startWorker()

	result, err := harness.orderService.CancelOrder(CancelOrderInput{UserID: userID, OrderID: order.ID})
	require.NoError(t, err)
	defer cleanupServiceCancelCommands(t, db, result.CommandID)

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("outbox 커밋 시도가 관측되지 않았다")
	}

	// polling tick이 여러 번 지나가도 재투입이 없어야 한다.
	//
	// 판정 기준은 상태가 아니라 attempt_count다. 재투입되면 엔진은 이미 제거된
	// 주문을 못 찾고(not-found), 주문은 아직 open이므로 worker는 NOOP이 아니라
	// RecordAttempt + backoff로 간다 — 상태는 그대로 PENDING이라 구분되지 않는다.
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, model.CancelCommandStatusPending, harness.commandStatus(result.CommandID))

	var blocked model.CancelCommand
	require.NoError(t, db.Where("id = ?", result.CommandID).First(&blocked).Error)
	assert.Zero(t, blocked.AttemptCount, "outbox 커밋 전에 같은 command가 재투입됐다")

	release()

	require.Eventually(t, func() bool {
		return harness.commandStatus(result.CommandID) == model.CancelCommandStatusProcessed
	}, 5*time.Second, 20*time.Millisecond, "차단을 풀었는데 command가 완료되지 않았다")

	event := requireForwardedOutboxEvent(t, harness.forwarded)
	require.NotNil(t, event.Event.OrderCancelled)
	harness.settleForwarded(event)

	assert.EqualValues(t, 1, harness.releaseEntryCount(userID, order.ID))
	assert.Equal(t, model.OrderStatusCancelled, harness.orderStatus(order.ID))
}
