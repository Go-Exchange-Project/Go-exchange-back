package service

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCancelCommandStore는 실제 DB 대신 command 상태를 메모리에 들고 있다.
// 이 테스트들이 보려는 것은 SQL이 아니라 worker의 상태 기계다.
type fakeCancelCommandStore struct {
	mu        sync.Mutex
	commands  map[uint64]*model.CancelCommand
	scanCount int
	attempts  map[uint64]int
}

func newFakeCancelCommandStore(commands ...*model.CancelCommand) *fakeCancelCommandStore {
	store := &fakeCancelCommandStore{
		commands: map[uint64]*model.CancelCommand{},
		attempts: map[uint64]int{},
	}
	for _, command := range commands {
		store.commands[command.ID] = command
	}
	return store
}

// FindPending은 실제 repository와 같이 ID 오름차순으로, 제외 목록을 LIMIT보다
// 먼저 적용해 돌려준다. 이 순서가 아니면 기아 시나리오가 재현되지 않는다.
func (f *fakeCancelCommandStore) FindPending(excluded []uint64, limit int) ([]model.CancelCommand, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scanCount++

	skip := make(map[uint64]struct{}, len(excluded))
	for _, id := range excluded {
		skip[id] = struct{}{}
	}

	var pending []model.CancelCommand
	for _, command := range f.commands {
		if command.Status != model.CancelCommandStatusPending {
			continue
		}
		if _, blocked := skip[command.ID]; blocked {
			continue
		}
		pending = append(pending, *command)
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].ID < pending[j].ID })
	if len(pending) > limit {
		pending = pending[:limit]
	}
	return pending, nil
}

func (f *fakeCancelCommandStore) FindStatuses(ids []uint64) ([]model.CancelCommand, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var found []model.CancelCommand
	for _, id := range ids {
		if command, ok := f.commands[id]; ok {
			found = append(found, *command)
		}
	}
	return found, nil
}

func (f *fakeCancelCommandStore) MarkNoop(id uint64) (*model.CancelCommand, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	command, ok := f.commands[id]
	if !ok || command.Status != model.CancelCommandStatusPending {
		return nil, nil
	}
	command.Status = model.CancelCommandStatusNoop
	command.UpdatedAt = time.Now()
	copied := *command
	return &copied, nil
}

func (f *fakeCancelCommandStore) RecordAttempt(id uint64, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts[id]++
	if command, ok := f.commands[id]; ok {
		command.LastError = message
	}
	return nil
}

func (f *fakeCancelCommandStore) CountPending() (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var count int64
	for _, command := range f.commands {
		if command.Status == model.CancelCommandStatusPending {
			count++
		}
	}
	return count, nil
}

// setStatus는 outbox 커밋이나 외부 정산이 상태를 바꾸는 것을 흉내낸다.
func (f *fakeCancelCommandStore) setStatus(id uint64, status model.CancelCommandStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if command, ok := f.commands[id]; ok {
		command.Status = status
	}
}

func (f *fakeCancelCommandStore) attemptCount(id uint64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts[id]
}

func (f *fakeCancelCommandStore) statusOf(id uint64) model.CancelCommandStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.commands[id].Status
}

type fakeOrderReader struct {
	mu     sync.Mutex
	orders map[uint]*model.Order
}

func newFakeOrderReader(orders ...*model.Order) *fakeOrderReader {
	reader := &fakeOrderReader{orders: map[uint]*model.Order{}}
	for _, order := range orders {
		reader.orders[order.ID] = order
	}
	return reader
}

func (f *fakeOrderReader) FindByID(orderID uint) (*model.Order, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	order, ok := f.orders[orderID]
	if !ok {
		return nil, errors.New("order not found")
	}
	copied := *order
	return &copied, nil
}

func (f *fakeOrderReader) setStatus(orderID uint, status model.OrderStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.orders[orderID].Status = status
}

// fakeCancelEngine은 CancelOrder 호출을 기록하고 응답을 제어한다.
type fakeCancelEngine struct {
	mu      sync.Mutex
	calls   []matching.CancelOrderCommand
	callAt  []time.Time
	result  matching.CancelOrderResult
	release chan struct{} // nil이 아니면 응답 전에 여기서 대기
}

func (f *fakeCancelEngine) CancelOrder(cmd matching.CancelOrderCommand) matching.CancelOrderResult {
	f.mu.Lock()
	f.calls = append(f.calls, cmd)
	f.callAt = append(f.callAt, time.Now())
	release := f.release
	result := f.result
	f.mu.Unlock()

	if release != nil {
		<-release
	}
	return result
}

func (f *fakeCancelEngine) callsFor(commandID uint64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, call := range f.calls {
		if call.CommandID == commandID {
			count++
		}
	}
	return count
}

// callGaps는 연속된 호출 사이의 간격이다. backoff가 실제로 늘어나는지 본다.
func (f *fakeCancelEngine) callGaps() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.callAt) < 2 {
		return nil
	}
	gaps := make([]time.Duration, 0, len(f.callAt)-1)
	for i := 1; i < len(f.callAt); i++ {
		gaps = append(gaps, f.callAt[i].Sub(f.callAt[i-1]))
	}
	return gaps
}

func testCancelCommand(id uint64, orderID uint) *model.CancelCommand {
	return &model.CancelCommand{
		ID:         id,
		OrderID:    orderID,
		UserID:     7,
		CoinSymbol: "BTC",
		Side:       model.OrderSideBuy,
		Price:      decimal.NewFromInt(100),
		Status:     model.CancelCommandStatusPending,
		CreatedAt:  time.Now(),
	}
}

func testOpenOrder(orderID uint) *model.Order {
	return &model.Order{ID: orderID, Status: model.OrderStatusPending}
}

func startTestWorker(t *testing.T, worker *CancelCommandWorker) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		worker.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("worker가 정지하지 않았다")
		}
	})
	return cancel
}

// 검증 8: wake 신호가 합쳐지거나 유실돼도 polling이 다음 조회 시도를 시작한다.
func TestCancelCommandWorkerPollsWithoutWake(t *testing.T) {
	store := newFakeCancelCommandStore(testCancelCommand(1, 100))
	engine := &fakeCancelEngine{result: matching.CancelOrderResult{Removed: true}}
	worker := NewCancelCommandWorker(store, newFakeOrderReader(testOpenOrder(100)), engine)
	worker.PollInterval = 5 * time.Millisecond

	startTestWorker(t, worker)

	require.Eventually(t, func() bool { return engine.callsFor(1) == 1 }, time.Second, 5*time.Millisecond,
		"wake 없이도 polling이 command를 찾아야 한다")
}

// 검증 8b: 엔진 응답이 느려도 같은 command가 두 번 투입되지 않는다.
func TestCancelCommandWorkerDoesNotDispatchTwiceWhileEngineIsSlow(t *testing.T) {
	store := newFakeCancelCommandStore(testCancelCommand(1, 100))
	release := make(chan struct{})
	engine := &fakeCancelEngine{result: matching.CancelOrderResult{Removed: true}, release: release}
	worker := NewCancelCommandWorker(store, newFakeOrderReader(testOpenOrder(100)), engine)
	worker.PollInterval = 5 * time.Millisecond

	startTestWorker(t, worker)

	require.Eventually(t, func() bool { return engine.callsFor(1) == 1 }, time.Second, 5*time.Millisecond)
	time.Sleep(100 * time.Millisecond) // polling tick이 여러 번 지나가도록
	assert.Equal(t, 1, engine.callsFor(1), "엔진 응답 대기 중에 재투입됐다")

	close(release)
}

// 검증 8d: 엔진이 성공 반환해도 outbox 커밋 전까지는 완료가 아니다.
// deadline을 넘겨도 재투입하지 않고, DB에서 PROCESSED가 확인돼야 해제된다.
func TestCancelCommandWorkerHoldsAwaitingOutboxUntilStatusChanges(t *testing.T) {
	store := newFakeCancelCommandStore(testCancelCommand(1, 100))
	engine := &fakeCancelEngine{result: matching.CancelOrderResult{Removed: true}}
	worker := NewCancelCommandWorker(store, newFakeOrderReader(testOpenOrder(100)), engine)
	worker.PollInterval = 5 * time.Millisecond
	worker.AwaitingWarnAfter = 20 * time.Millisecond // deadline을 짧게 만든다

	startTestWorker(t, worker)

	require.Eventually(t, func() bool { return engine.callsFor(1) == 1 }, time.Second, 5*time.Millisecond)

	// command는 아직 PENDING이다(outbox가 커밋되지 않았다). deadline을 훌쩍 넘긴다.
	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, 1, engine.callsFor(1), "deadline 만료가 재투입을 만들었다")
	assert.True(t, worker.InFlightCount() > 0, "awaiting_outbox 엔트리가 사라졌다")

	// outbox가 커밋되면 그때 해제된다.
	store.setStatus(1, model.CancelCommandStatusProcessed)
	require.Eventually(t, func() bool { return worker.InFlightCount() == 0 }, time.Second, 5*time.Millisecond,
		"PROCESSED가 확인됐는데 in-flight에서 해제되지 않았다")
}

// 검증 8e: 엔진 not-found의 해석은 DB 주문 상태가 정한다.
// 주문이 open이면 NOOP이 아니라 재시도다 — 조용히 종결하면 취소가 유실된다.
func TestCancelCommandWorkerRetriesNotFoundWhileOrderIsOpen(t *testing.T) {
	store := newFakeCancelCommandStore(testCancelCommand(1, 100))
	engine := &fakeCancelEngine{result: matching.CancelOrderResult{Err: matching.ErrCancelOrderNotFound}}
	reader := newFakeOrderReader(testOpenOrder(100))
	worker := NewCancelCommandWorker(store, reader, engine)
	worker.PollInterval = 5 * time.Millisecond
	worker.InitialBackoff = 5 * time.Millisecond

	startTestWorker(t, worker)

	require.Eventually(t, func() bool { return store.attemptCount(1) >= 2 }, time.Second, 5*time.Millisecond,
		"open 주문의 not-found가 재시도되지 않았다")
	assert.Equal(t, model.CancelCommandStatusPending, store.statusOf(1), "open 주문인데 NOOP으로 종결됐다")
}

// 검증 8e(계속): 주문이 이미 종결됐으면 할 일이 없다 — NOOP이고 실패가 아니다.
func TestCancelCommandWorkerMarksNoopWhenOrderIsTerminal(t *testing.T) {
	store := newFakeCancelCommandStore(testCancelCommand(1, 100))
	engine := &fakeCancelEngine{result: matching.CancelOrderResult{Err: matching.ErrCancelOrderNotFound}}
	order := testOpenOrder(100)
	order.Status = model.OrderStatusFilled
	worker := NewCancelCommandWorker(store, newFakeOrderReader(order), engine)
	worker.PollInterval = 5 * time.Millisecond

	startTestWorker(t, worker)

	require.Eventually(t, func() bool { return store.statusOf(1) == model.CancelCommandStatusNoop },
		time.Second, 5*time.Millisecond, "종결된 주문의 command가 NOOP이 되지 않았다")
	require.Eventually(t, func() bool { return worker.InFlightCount() == 0 }, time.Second, 5*time.Millisecond)

	// NOOP 이후에는 재시도 루프가 돌지 않아야 한다.
	calls := engine.callsFor(1)
	time.Sleep(60 * time.Millisecond)
	assert.Equal(t, calls, engine.callsFor(1), "NOOP 이후에도 재투입됐다")
}

// 검증 8c: 반복 실패의 재투입 간격이 지수적으로 늘어야 한다. nextAttemptAt이 없으면
// polling 간격으로 고정돼 backoff가 무의미해진다.
func TestCancelCommandWorkerBacksOffOnRepeatedEngineErrors(t *testing.T) {
	store := newFakeCancelCommandStore(testCancelCommand(1, 100))
	engine := &fakeCancelEngine{result: matching.CancelOrderResult{Err: matching.ErrCancelOrderTimedOut}}
	worker := NewCancelCommandWorker(store, newFakeOrderReader(testOpenOrder(100)), engine)
	worker.PollInterval = 2 * time.Millisecond
	worker.InitialBackoff = 20 * time.Millisecond
	worker.MaxBackoff = 200 * time.Millisecond

	startTestWorker(t, worker)

	require.Eventually(t, func() bool { return engine.callsFor(1) >= 4 }, 3*time.Second, 5*time.Millisecond)

	gaps := engine.callGaps()
	require.GreaterOrEqual(t, len(gaps), 3)
	// 첫 간격은 20ms 근처, 세 번째는 80ms 근처여야 한다. 타이밍 흔들림을 감안해
	// "뒤 간격이 앞 간격보다 확실히 크다"만 본다.
	assert.Greater(t, gaps[2], gaps[0]*2, "간격이 늘지 않았다: %v", gaps)
}

// WaitUntilDrained는 PENDING이 0이 될 때까지 기다린다. 부팅 장벽이 이걸 쓴다.
func TestCancelCommandWorkerWaitUntilDrained(t *testing.T) {
	store := newFakeCancelCommandStore(testCancelCommand(1, 100))
	engine := &fakeCancelEngine{result: matching.CancelOrderResult{Removed: true}}
	worker := NewCancelCommandWorker(store, newFakeOrderReader(testOpenOrder(100)), engine)
	worker.PollInterval = 5 * time.Millisecond

	startTestWorker(t, worker)

	// 엔진이 제거해도 outbox가 커밋하기 전에는 PENDING이다.
	shortCtx, cancelShort := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelShort()
	require.Error(t, worker.WaitUntilDrained(shortCtx), "PENDING이 남았는데 drain이 성공했다")

	store.setStatus(1, model.CancelCommandStatusProcessed)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, worker.WaitUntilDrained(ctx))
}

// Wake는 신호가 뭉쳐도 유실이 아니다 — worker가 PENDING 전체를 다시 스캔한다.
func TestCancelCommandWorkerWakeIsNonBlocking(t *testing.T) {
	store := newFakeCancelCommandStore()
	engine := &fakeCancelEngine{result: matching.CancelOrderResult{Removed: true}}
	worker := NewCancelCommandWorker(store, newFakeOrderReader(), engine)

	// Run 전에도, 연속 호출에도 블록되면 안 된다.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			worker.Wake()
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Wake가 블록됐다")
	}
}

// 앞선 ScanLimit개가 전부 in-flight여도 그 뒤의 command가 dispatch돼야 한다.
// 제외를 SQL LIMIT 이후에 적용하면 129번째는 조회조차 되지 않아 영구히 굶는다.
func TestCancelCommandWorkerDoesNotStarveCommandsBeyondScanLimit(t *testing.T) {
	const scanLimit = 8
	var commands []*model.CancelCommand
	var orders []*model.Order
	for id := uint64(1); id <= scanLimit+1; id++ {
		commands = append(commands, testCancelCommand(id, uint(100+id)))
		orders = append(orders, testOpenOrder(uint(100+id)))
	}

	store := newFakeCancelCommandStore(commands...)
	// 엔진은 성공 반환하지만 store가 PROCESSED로 바뀌지 않으므로 모두
	// awaiting_outbox에 쌓인다.
	engine := &fakeCancelEngine{result: matching.CancelOrderResult{Removed: true}}
	worker := NewCancelCommandWorker(store, newFakeOrderReader(orders...), engine)
	worker.PollInterval = 5 * time.Millisecond
	worker.ScanLimit = scanLimit
	worker.MaxDispatch = 2

	startTestWorker(t, worker)

	last := uint64(scanLimit + 1)
	require.Eventually(t, func() bool { return engine.callsFor(last) == 1 }, 3*time.Second, 5*time.Millisecond,
		"앞선 %d개가 in-flight라 %d번 command가 조회되지 않았다", scanLimit, last)
}

// 종료 상한은 lifecycle이 소유한다. Run은 시작한 엔진 호출이 반환하기 전에
// 종료 완료를 보고하면 안 된다 — 그러면 뒤이은 엔진 정지와 경쟁한다.
func TestCancelCommandWorkerRunBlocksUntilInFlightDispatchReturns(t *testing.T) {
	store := newFakeCancelCommandStore(testCancelCommand(1, 100))
	release := make(chan struct{})
	engine := &fakeCancelEngine{result: matching.CancelOrderResult{Removed: true}, release: release}
	worker := NewCancelCommandWorker(store, newFakeOrderReader(testOpenOrder(100)), engine)
	worker.PollInterval = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		worker.Run(ctx)
		close(done)
	}()

	require.Eventually(t, func() bool { return engine.callsFor(1) == 1 }, time.Second, 5*time.Millisecond)
	cancel()

	// 엔진의 CancelOrder는 enqueue 1초 + response 1초로 약 2초가 걸릴 수 있다.
	// 그보다 짧게 기다리면 "worker가 자체 상한을 두고 먼저 포기하는" 결함을
	// 구분하지 못한다.
	select {
	case <-done:
		t.Fatal("진행 중인 엔진 호출을 남겨둔 채 Run이 반환했다")
	case <-time.After(2500 * time.Millisecond):
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("호출이 끝났는데 Run이 반환하지 않았다")
	}
}

// 이미 취소된 context로 시작하면 엔진 호출을 한 번도 하지 않아야 한다.
func TestCancelCommandWorkerStartsNoDispatchAfterCancel(t *testing.T) {
	store := newFakeCancelCommandStore(testCancelCommand(1, 100))
	engine := &fakeCancelEngine{result: matching.CancelOrderResult{Removed: true}}
	worker := NewCancelCommandWorker(store, newFakeOrderReader(testOpenOrder(100)), engine)
	worker.PollInterval = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// wake 신호까지 미리 넣어 select가 다른 case를 고를 여지를 만든다.
	worker.Wake()

	done := make(chan struct{})
	go func() {
		worker.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("취소된 context인데 Run이 반환하지 않았다")
	}
	assert.Zero(t, engine.callsFor(1), "취소된 context에서 새 dispatch가 시작됐다")
}
