package service

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
)

const (
	defaultCancelCommandPollInterval      = 50 * time.Millisecond
	defaultCancelCommandInitialBackoff    = 100 * time.Millisecond
	defaultCancelCommandMaxBackoff        = 5 * time.Second
	defaultCancelCommandAwaitingWarnAfter = 5 * time.Second
	defaultCancelCommandMaxDispatch       = 8
	defaultCancelCommandScanLimit         = 128
)

type cancelCommandStore interface {
	FindPending(excluded []uint64, limit int) ([]model.CancelCommand, error)
	FindStatuses(ids []uint64) ([]model.CancelCommand, error)
	MarkNoop(id uint64) (*model.CancelCommand, error)
	RecordAttempt(id uint64, message string) error
	CountPending() (int64, error)
}

type cancelCommandOrderReader interface {
	FindByID(orderID uint) (*model.Order, error)
}

type cancelCommandEngine interface {
	CancelOrder(matching.CancelOrderCommand) matching.CancelOrderResult
}

type cancelCommandPhase uint8

const (
	// dispatching: 엔진 호출이 진행 중이다.
	cancelCommandDispatching cancelCommandPhase = iota
	// awaitingOutbox: 엔진은 성공 반환했지만 outbox 커밋을 아직 못 봤다.
	// 엔진의 processCancel은 응답을 보낸 뒤에 이벤트를 방출하므로, 반환은 결말이
	// 아니다. 여기서 지우면 outbox 커밋 전에 재투입되고, 뒤늦게 도착한 첫 이벤트의
	// PROCESSED UPDATE가 0행이 되어 무관한 주문까지 든 배치가 통째로 rollback된다.
	cancelCommandAwaitingOutbox
	// backoff: 실패해서 nextAttemptAt까지 기다린다.
	cancelCommandBackoff
)

type cancelCommandInFlight struct {
	phase          cancelCommandPhase
	nextAttemptAt  time.Time
	backoff        time.Duration
	awaitingSince  time.Time
	warningEmitted bool
}

type cancelCommandResult struct {
	commandID uint64
	orderID   uint
	createdAt time.Time
	result    matching.CancelOrderResult
}

// CancelCommandWorker는 내구 기록된 취소 command를 매칭 엔진에 전달한다.
// in-flight 상태는 Run의 단일 goroutine만 소유하고, 엔진 호출만 별도 goroutine에서
// 수행한 뒤 채널로 결과를 돌려준다.
type CancelCommandWorker struct {
	commands cancelCommandStore
	orders   cancelCommandOrderReader
	engine   cancelCommandEngine

	PollInterval   time.Duration
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	// AwaitingWarnAfter가 지나면 경고와 counter를 한 번 남긴다. phase는 바꾸지
	// 않는다 — OutboxWriter가 커밋될 때까지 무한 재시도하므로 이벤트가 흘러
	// 없어지는 경로는 프로세스 죽음뿐이고, 그때는 in-flight도 함께 사라진다.
	AwaitingWarnAfter time.Duration
	MaxDispatch       int
	ScanLimit         int
	Logger            *log.Logger

	wake    chan struct{}
	results chan cancelCommandResult
	// queries는 Run goroutine이 소유한 in-flight 맵을 밖에서 안전하게 읽는 통로다.
	queries  chan chan int
	inFlight map[uint64]*cancelCommandInFlight
}

func NewCancelCommandWorker(commands cancelCommandStore, orders cancelCommandOrderReader, engine cancelCommandEngine) *CancelCommandWorker {
	return &CancelCommandWorker{
		commands: commands,
		orders:   orders,
		engine:   engine,
		wake:     make(chan struct{}, 1),
		results:  make(chan cancelCommandResult, defaultCancelCommandMaxDispatch),
		queries:  make(chan chan int),
		inFlight: map[uint64]*cancelCommandInFlight{},
	}
}

// Wake는 command 커밋 직후 호출된다. 버퍼 1에 non-blocking send이므로 신호가
// 뭉쳐도 유실이 아니다 — worker는 PENDING 전체를 다시 스캔한다.
func (w *CancelCommandWorker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// InFlightCount는 테스트·관측용이다. Run goroutine 밖에서 읽으므로 스냅샷이다.
func (w *CancelCommandWorker) InFlightCount() int {
	done := make(chan int, 1)
	select {
	case w.queries <- done:
		return <-done
	case <-time.After(time.Second):
		return -1
	}
}

func (w *CancelCommandWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.pollInterval())
	defer ticker.Stop()

	dispatching := 0
	for {
		select {
		case <-ctx.Done():
			// 이미 시작한 엔진 호출만 회수하고 새 dispatch는 하지 않는다.
			w.drainDispatches(dispatching)
			return
		case done := <-w.queries:
			done <- len(w.inFlight)
			continue
		case result := <-w.results:
			dispatching--
			w.applyResult(result)
		case <-w.wake:
		case <-ticker.C:
		}

		// ctx.Done()과 wake/ticker/result가 동시에 준비되면 select가 다른 case를
		// 고를 수 있다. 그대로 두면 종료 중에 새 엔진 호출이 시작된다.
		if ctx.Err() != nil {
			w.drainDispatches(dispatching)
			return
		}

		w.releaseCompleted()
		w.warnStaleAwaiting()
		dispatching += w.dispatchReady(dispatching)
	}
}

// releaseCompleted는 awaiting_outbox 엔트리의 결말을 DB에서 확인한다.
// PENDING 스캔에서 사라졌는지로 판단하지 않는다 — 스캔에 LIMIT이 있으면
// "안 보임"과 "완료됨"이 구분되지 않는다.
func (w *CancelCommandWorker) releaseCompleted() {
	ids := make([]uint64, 0, len(w.inFlight))
	for id, entry := range w.inFlight {
		if entry.phase == cancelCommandAwaitingOutbox {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}

	statuses, err := w.commands.FindStatuses(ids)
	if err != nil {
		w.logf("cancel worker: awaiting status lookup failed: %v", err)
		return
	}
	for _, command := range statuses {
		if command.Status == model.CancelCommandStatusPending {
			continue
		}
		delete(w.inFlight, command.ID)
		metrics.CancelCommandLatency.Observe(command.UpdatedAt.Sub(command.CreatedAt).Seconds())
	}
}

func (w *CancelCommandWorker) warnStaleAwaiting() {
	deadline := w.awaitingWarnAfter()
	now := time.Now()
	for id, entry := range w.inFlight {
		if entry.phase != cancelCommandAwaitingOutbox || entry.warningEmitted {
			continue
		}
		if now.Sub(entry.awaitingSince) < deadline {
			continue
		}
		entry.warningEmitted = true
		metrics.CancelCommandAwaitingOutboxDeadlineTotal.Inc()
		w.logf("cancel worker: command %d still awaiting outbox commit after %s", id, deadline)
	}
}

func (w *CancelCommandWorker) dispatchReady(dispatching int) int {
	free := w.maxDispatch() - dispatching
	if free <= 0 {
		return 0
	}

	// 아직 때가 아닌 command를 SQL에서 먼저 뺀다. 조회한 뒤 애플리케이션에서
	// 빼면 앞선 LIMIT개가 전부 in-flight일 때 그 뒤가 영영 조회되지 않는다.
	pending, err := w.commands.FindPending(w.blockedIDs(), w.scanLimit())
	if err != nil {
		w.logf("cancel worker: pending scan failed: %v", err)
		return 0
	}

	started := 0
	for _, command := range pending {
		if started >= free {
			break
		}
		w.inFlight[command.ID] = w.markDispatching(command.ID)
		go w.dispatch(command)
		started++
	}
	return started
}

// blockedIDs는 지금 다시 투입하면 안 되는 command다: 엔진 호출 중, outbox 커밋
// 대기 중, 그리고 아직 nextAttemptAt에 도달하지 않은 backoff.
func (w *CancelCommandWorker) blockedIDs() []uint64 {
	now := time.Now()
	blocked := make([]uint64, 0, len(w.inFlight))
	for id, entry := range w.inFlight {
		if entry.phase == cancelCommandBackoff && !now.Before(entry.nextAttemptAt) {
			continue
		}
		blocked = append(blocked, id)
	}
	return blocked
}

func (w *CancelCommandWorker) markDispatching(id uint64) *cancelCommandInFlight {
	entry, ok := w.inFlight[id]
	if !ok {
		entry = &cancelCommandInFlight{}
	}
	entry.phase = cancelCommandDispatching
	return entry
}

func (w *CancelCommandWorker) dispatch(command model.CancelCommand) {
	result := w.engine.CancelOrder(matching.CancelOrderCommand{
		CommandID:  command.ID,
		CoinSymbol: command.CoinSymbol,
		OrderID:    command.OrderID,
		Side:       command.Side,
		Price:      command.Price,
	})
	w.results <- cancelCommandResult{
		commandID: command.ID,
		orderID:   command.OrderID,
		createdAt: command.CreatedAt,
		result:    result,
	}
}

func (w *CancelCommandWorker) applyResult(result cancelCommandResult) {
	entry, ok := w.inFlight[result.commandID]
	if !ok {
		return
	}

	switch {
	case result.result.Err == nil:
		// 성공 반환은 완료가 아니다. outbox 커밋을 DB에서 확인할 때까지 붙잡는다.
		entry.phase = cancelCommandAwaitingOutbox
		entry.awaitingSince = time.Now()
		entry.warningEmitted = false
	case errors.Is(result.result.Err, matching.ErrCancelOrderNotFound):
		w.applyNotFound(result, entry)
	default:
		w.scheduleRetry(result.commandID, entry, result.result.Err)
	}
}

// applyNotFound는 "엔진에 없음"을 DB 주문 상태로 해석한다. 주문이 아직 open이면
// 엔진과 DB가 어긋난 것이므로 조용히 종결하면 취소가 유실된다.
func (w *CancelCommandWorker) applyNotFound(result cancelCommandResult, entry *cancelCommandInFlight) {
	order, err := w.orders.FindByID(result.orderID)
	if err != nil {
		w.scheduleRetry(result.commandID, entry, err)
		return
	}
	if isCancellableOrderStatus(order.Status) {
		w.scheduleRetry(result.commandID, entry, matching.ErrCancelOrderNotFound)
		return
	}

	command, err := w.commands.MarkNoop(result.commandID)
	if err != nil {
		w.scheduleRetry(result.commandID, entry, err)
		return
	}
	delete(w.inFlight, result.commandID)
	if command != nil {
		metrics.CancelCommandLatency.Observe(command.UpdatedAt.Sub(command.CreatedAt).Seconds())
	}
}

func (w *CancelCommandWorker) scheduleRetry(id uint64, entry *cancelCommandInFlight, cause error) {
	if err := w.commands.RecordAttempt(id, cause.Error()); err != nil {
		w.logf("cancel worker: attempt record failed for command %d: %v", id, err)
	}

	entry.backoff = w.nextBackoff(entry.backoff)
	entry.phase = cancelCommandBackoff
	entry.nextAttemptAt = time.Now().Add(entry.backoff)
}

func (w *CancelCommandWorker) nextBackoff(current time.Duration) time.Duration {
	if current <= 0 {
		return w.initialBackoff()
	}
	next := current * 2
	if next > w.maxBackoff() {
		return w.maxBackoff()
	}
	return next
}

// drainDispatches는 이미 시작한 엔진 호출이 전부 반환할 때까지 기다린다.
// 여기서 자체 상한을 두면 안 된다 — 엔진의 CancelOrder는 enqueue 1초와 response
// 1초를 순차로 기다려 한 호출만으로 약 2초가 걸릴 수 있고, 그 호출이 살아 있는
// 채로 Run이 반환하면 종료 순서상 뒤에 오는 엔진 정지와 경쟁한다.
// 종료 상한은 이 worker를 정지시키는 lifecycle이 소유한다.
//
// 종료 시 새 dispatch는 하지 않으므로 in-flight 상태는 프로세스와 함께 사라지고,
// 남은 PENDING command는 재기동 후 다시 실행된다.
func (w *CancelCommandWorker) drainDispatches(dispatching int) {
	for dispatching > 0 {
		select {
		case <-w.results:
			dispatching--
		case done := <-w.queries:
			done <- len(w.inFlight)
		}
	}
}

// WaitUntilDrained는 DB의 PENDING command가 0이 될 때까지 기다린다.
// 부팅 장벽이 이 결과로 readiness 개방 여부를 정한다.
func (w *CancelCommandWorker) WaitUntilDrained(ctx context.Context) error {
	w.Wake()

	ticker := time.NewTicker(w.pollInterval())
	defer ticker.Stop()

	for {
		count, err := w.commands.CountPending()
		if err != nil {
			return err
		}
		if count == 0 {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w *CancelCommandWorker) logf(format string, args ...any) {
	if w.Logger != nil {
		w.Logger.Printf(format, args...)
		return
	}
	log.Printf(format, args...)
}

func (w *CancelCommandWorker) pollInterval() time.Duration {
	if w.PollInterval > 0 {
		return w.PollInterval
	}
	return defaultCancelCommandPollInterval
}

func (w *CancelCommandWorker) initialBackoff() time.Duration {
	if w.InitialBackoff > 0 {
		return w.InitialBackoff
	}
	return defaultCancelCommandInitialBackoff
}

func (w *CancelCommandWorker) maxBackoff() time.Duration {
	if w.MaxBackoff > 0 {
		return w.MaxBackoff
	}
	return defaultCancelCommandMaxBackoff
}

func (w *CancelCommandWorker) awaitingWarnAfter() time.Duration {
	if w.AwaitingWarnAfter > 0 {
		return w.AwaitingWarnAfter
	}
	return defaultCancelCommandAwaitingWarnAfter
}

func (w *CancelCommandWorker) maxDispatch() int {
	if w.MaxDispatch > 0 {
		return w.MaxDispatch
	}
	return defaultCancelCommandMaxDispatch
}

func (w *CancelCommandWorker) scanLimit() int {
	if w.ScanLimit > 0 {
		return w.ScanLimit
	}
	return defaultCancelCommandScanLimit
}
