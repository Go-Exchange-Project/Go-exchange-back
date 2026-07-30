package main

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cancelOutboxEvent(outboxID uint64, orderID uint) service.OutboxEvent {
	return service.OutboxEvent{
		OutboxID: outboxID,
		Event:    matching.ExecutionEvent{OrderCancelled: &matching.OrderCancelled{OrderID: orderID, CoinSymbol: "BTC"}},
	}
}

func TestPartitionDispatcherBroadcastsInDispatchOrder(t *testing.T) {
	queue := make(chan service.OutboxEvent, 8)
	jobs := make(chan settlementJob, 4)

	// 배치를 강제로 1건씩 끊기 위해 maxBatch=1. trade 3건 주입.
	for i := 1; i <= 3; i++ {
		queue <- service.OutboxEvent{OutboxID: uint64(i), Event: matching.ExecutionEvent{
			Trade: &model.Trade{CoinSymbol: "BTC"}}}
	}
	close(queue)

	// worker: seq에 따라 완료를 뒤집는다(#1을 가장 늦게).
	go func() {
		for job := range jobs {
			job := job
			go func() {
				if job.seq == 1 {
					time.Sleep(50 * time.Millisecond)
				}
				msg := broadcastMessage{coinSymbol: "BTC",
					payload: []byte(fmt.Sprintf("seq-%d", job.seq))}
				job.done <- settlementResult{kind: jobKindTrade, id: job.id, seq: job.seq, messages: []broadcastMessage{msg}}
			}()
		}
	}()

	var mu sync.Mutex
	var got []string
	broadcast := func(symbol string, payload []byte) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, string(payload))
	}

	runPartitionDispatcher("0", queue, jobs, 3, 1, broadcast)

	assert.Equal(t, []string{"seq-1", "seq-2", "seq-3"}, got,
		"완료 순서와 무관하게 디스패치 순서로 방출돼야 한다")
}

func TestDispatcherAndWorkerRecordJobTimingMetrics(t *testing.T) {
	beforeWait := histogramSampleCount(t, metrics.SettlementJobDispatchWait)
	beforeExec := histogramVecSampleCount(t, metrics.SettlementJobExecution, "success")

	queue := make(chan service.OutboxEvent, 4)
	jobs := make(chan settlementJob, 4)
	queue <- service.OutboxEvent{OutboxID: 1, Event: matching.ExecutionEvent{
		Trade: &model.Trade{CoinSymbol: "BTC"}}}
	close(queue)

	okSettleBatch := func(batch []service.OutboxEvent, collect func(string, []byte)) []uint { return nil }

	// worker를 50ms 뒤에 기동해 디스패치 대기를 강제
	go func() { time.Sleep(50 * time.Millisecond); runSettlementWorker(jobs, okSettleBatch, nil) }()
	runPartitionDispatcher("0", queue, jobs, 2, 32, func(string, []byte) {})

	assert.Equal(t, beforeWait+1, histogramSampleCount(t, metrics.SettlementJobDispatchWait))
	assert.Equal(t, beforeExec+1, histogramVecSampleCount(t, metrics.SettlementJobExecution, "success"))
}

func TestPartitionDispatcherDrainsOnQueueClose(t *testing.T) {
	queue := make(chan service.OutboxEvent, 8)
	jobs := make(chan settlementJob, 4)
	for i := 1; i <= 5; i++ {
		queue <- service.OutboxEvent{OutboxID: uint64(i), Event: matching.ExecutionEvent{
			Trade: &model.Trade{CoinSymbol: "BTC"}}}
	}
	close(queue)
	go func() {
		for job := range jobs {
			job.done <- settlementResult{kind: jobKindTrade, id: job.id, seq: job.seq, messages: []broadcastMessage{
				{coinSymbol: "BTC", payload: []byte("t")}}}
		}
	}()
	var mu sync.Mutex
	count := 0
	broadcast := func(string, []byte) { mu.Lock(); count++; mu.Unlock() }

	done := make(chan struct{})
	go func() { runPartitionDispatcher("0", queue, jobs, 2, 1, broadcast); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("큐 close 후에도 dispatcher가 반환하지 않음")
	}
	assert.Equal(t, 5, count, "잔여 배치가 전부 방출돼야 한다")
}

// 핵심 회귀: 무관한 주문의 terminal이 다른 배치를 막지 않는다(배리어였다면 막았을 것).
func TestDispatcherDoesNotBlockUnrelatedBatchesOnTerminal(t *testing.T) {
	queue := make(chan service.OutboxEvent, 8)
	jobs := make(chan settlementJob, 8)

	queue <- tradeOutboxEventForOrders(1, 10, 20) // 주문 10·20
	queue <- cancelOutboxEvent(2, 10)             // 주문 10의 terminal → 대기해야 한다
	queue <- tradeOutboxEventForOrders(3, 30, 40) // 무관한 주문 → 계속 진행해야 한다
	close(queue)

	done := make(chan struct{})
	go func() {
		runPartitionDispatcher("0", queue, jobs, 4, 32, func(string, []byte) {})
		close(done)
	}()

	first := <-jobs
	require.Equal(t, jobKindTrade, first.kind)
	require.Equal(t, uint(10), first.batch[0].Event.Trade.BuyOrderID)

	// 1번의 completion을 아직 보내지 않았는데도 3번이 dispatch되어야 한다.
	second := <-jobs
	assert.Equal(t, jobKindTrade, second.kind,
		"주문 10의 terminal이 대기 중이어도 무관한 주문 30·40의 배치는 진행해야 한다")
	assert.Equal(t, uint(30), second.batch[0].Event.Trade.BuyOrderID)

	// 이제 1번을 retire하면 terminal이 나온다.
	first.done <- settlementResult{kind: jobKindTrade, id: first.id, seq: first.seq}
	third := <-jobs
	assert.Equal(t, jobKindTerminal, third.kind)
	assert.Equal(t, uint(10), third.terminal.Event.OrderCancelled.OrderID)

	second.done <- settlementResult{kind: jobKindTrade, id: second.id, seq: second.seq}
	third.done <- settlementResult{kind: jobKindTerminal, id: third.id}
	<-done
}

// quarantine된 주문의 terminal은 실행되지 않는다(outbox PENDING으로 남는다).
func TestDispatcherSkipsTerminalForQuarantinedOrder(t *testing.T) {
	queue := make(chan service.OutboxEvent, 4)
	jobs := make(chan settlementJob, 4)

	queue <- tradeOutboxEventForOrders(1, 10, 20)
	queue <- cancelOutboxEvent(2, 10)
	close(queue)

	done := make(chan struct{})
	go func() {
		runPartitionDispatcher("0", queue, jobs, 4, 32, func(string, []byte) {})
		close(done)
	}()

	first := <-jobs
	// 주문 10의 실패 기록조차 실패했다고 보고한다.
	first.done <- settlementResult{kind: jobKindTrade, id: first.id, seq: first.seq, undurableOrderIDs: []uint{10, 20}}

	select {
	case job := <-jobs:
		t.Fatalf("quarantine된 주문의 terminal이 dispatch되면 안 된다: %+v", job)
	case <-done:
		// 정상 종료 — terminal은 실행되지 않고 outbox는 PENDING으로 남는다
	}
}

// 같은 주문을 건드리는 배치가 둘이면, 둘 다 retire돼야만 terminal이 나온다.
func TestDispatcherHoldsTerminalUntilSameOrderBatchesRetire(t *testing.T) {
	queue := make(chan service.OutboxEvent, 8)
	jobs := make(chan settlementJob, 8)

	queue <- tradeOutboxEventForOrders(1, 10, 20) // A: 주문10 포함
	queue <- tradeOutboxEventForOrders(2, 10, 30) // B: 주문10을 건드리는 두 번째 배치
	queue <- cancelOutboxEvent(3, 10)             // 주문10의 terminal — A·B 모두 retire돼야 dispatch
	queue <- tradeOutboxEventForOrders(4, 40, 50) // C: 무관한 배치 — A retire 전에도 나와야 한다
	queue <- tradeOutboxEventForOrders(5, 60, 70) // D: 무관한 배치 — A만 retire된 상태에서도 나와야 한다
	close(queue)

	done := make(chan struct{})
	go func() {
		runPartitionDispatcher("0", queue, jobs, 4, 1, func(string, []byte) {})
		close(done)
	}()

	a := <-jobs
	require.Equal(t, jobKindTrade, a.kind)
	b := <-jobs
	require.Equal(t, jobKindTrade, b.kind)
	c := <-jobs
	require.Equal(t, jobKindTrade, c.kind, "무관한 배치 C는 terminal 대기와 무관하게 dispatch돼야 한다")

	a.done <- settlementResult{kind: jobKindTrade, id: a.id, seq: a.seq}

	d := <-jobs
	assert.Equal(t, jobKindTrade, d.kind,
		"A만 retire된 상태다 — 주문10을 건드린 B가 아직 남아 있어 terminal은 여전히 막혀야 한다")

	b.done <- settlementResult{kind: jobKindTrade, id: b.id, seq: b.seq}

	terminal := <-jobs
	assert.Equal(t, jobKindTerminal, terminal.kind, "A와 B가 모두 retire된 뒤에야 terminal이 dispatch된다")
	assert.Equal(t, uint(10), terminal.terminal.Event.OrderCancelled.OrderID)

	c.done <- settlementResult{kind: jobKindTrade, id: c.id, seq: c.seq}
	d.done <- settlementResult{kind: jobKindTrade, id: d.id, seq: d.seq}
	terminal.done <- settlementResult{kind: jobKindTerminal, id: terminal.id}
	<-done
}

// completion 없이는 outstanding job 수가 2*concurrency를 넘지 않는다.
func TestDispatcherRespectsMaxOutstanding(t *testing.T) {
	queue := make(chan service.OutboxEvent, 16)
	jobs := make(chan settlementJob, 16)

	for i := 1; i <= 10; i++ {
		queue <- tradeOutboxEventForOrders(uint64(i), uint(i*10), uint(i*10+1))
	}
	close(queue)

	done := make(chan struct{})
	go func() {
		runPartitionDispatcher("0", queue, jobs, 2, 1, func(string, []byte) {})
		close(done)
	}()

	var received []settlementJob
	for i := 0; i < 4; i++ {
		received = append(received, <-jobs)
	}

	// maxOutstanding=2*concurrency=4에 도달했다 — completion 없이는 5번째가 나오면 안 된다.
	select {
	case job := <-jobs:
		t.Fatalf("maxOutstanding(4)을 초과해 dispatch되면 안 된다: %+v", job)
	case <-time.After(100 * time.Millisecond):
	}

	for _, job := range received {
		job.done <- settlementResult{kind: job.kind, id: job.id, seq: job.seq}
	}
	for i := 0; i < 6; i++ {
		job := <-jobs
		job.done <- settlementResult{kind: job.kind, id: job.id, seq: job.seq}
	}
	<-done
}

// queue가 닫혀도 dispatcher는 waiting 중인 terminal을 dispatch한 뒤에야 반환한다.
func TestDispatcherGracefulShutdownDrainsWaitingTerminals(t *testing.T) {
	queue := make(chan service.OutboxEvent, 4)
	jobs := make(chan settlementJob, 4)

	queue <- tradeOutboxEventForOrders(1, 10, 20)
	queue <- cancelOutboxEvent(2, 10)
	close(queue)

	done := make(chan struct{})
	go func() {
		runPartitionDispatcher("0", queue, jobs, 4, 32, func(string, []byte) {})
		close(done)
	}()

	trade := <-jobs
	require.Equal(t, jobKindTrade, trade.kind)
	trade.done <- settlementResult{kind: jobKindTrade, id: trade.id, seq: trade.seq}

	terminal := <-jobs
	assert.Equal(t, jobKindTerminal, terminal.kind, "queue가 닫혀도 waiting 중인 terminal은 dispatch돼야 한다")
	terminal.done <- settlementResult{kind: jobKindTerminal, id: terminal.id}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("terminal을 dispatch한 뒤에도 dispatcher가 반환하지 않음")
	}
}

// terminal은 브로드캐스트 시퀀스를 소비하지 않으므로, 완료 순서가 뒤바뀌어도
// trade 브로드캐스트는 원래 디스패치 순서를 유지하고 terminal 때문에 멈추지 않는다.
func TestDispatcherTerminalDoesNotConsumeBroadcastSequence(t *testing.T) {
	queue := make(chan service.OutboxEvent, 4)
	jobs := make(chan settlementJob, 4)

	queue <- tradeOutboxEventForOrders(1, 10, 20)
	queue <- cancelOutboxEvent(2, 10)
	queue <- tradeOutboxEventForOrders(3, 30, 40)
	close(queue)

	var mu sync.Mutex
	var got []string
	broadcast := func(symbol string, payload []byte) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, string(payload))
	}

	done := make(chan struct{})
	go func() {
		runPartitionDispatcher("0", queue, jobs, 4, 1, broadcast)
		close(done)
	}()

	first := <-jobs
	require.Equal(t, jobKindTrade, first.kind)
	second := <-jobs
	require.Equal(t, jobKindTrade, second.kind)

	// 완료 순서를 뒤집는다: 두 번째가 먼저 끝난다.
	second.done <- settlementResult{kind: jobKindTrade, id: second.id, seq: second.seq,
		messages: []broadcastMessage{{coinSymbol: "BTC", payload: []byte("trade-2")}}}
	first.done <- settlementResult{kind: jobKindTrade, id: first.id, seq: first.seq,
		messages: []broadcastMessage{{coinSymbol: "BTC", payload: []byte("trade-1")}}}

	terminal := <-jobs
	assert.Equal(t, jobKindTerminal, terminal.kind)
	terminal.done <- settlementResult{kind: jobKindTerminal, id: terminal.id}

	<-done
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"trade-1", "trade-2"}, got,
		"terminal은 seq를 소비하지 않으므로 브로드캐스트 순서는 완료 순서와 무관하게 디스패치 순서를 유지한다")
}

// 주문당 terminal 1개는 엔진 불변식이다 — 위반 시 두 번째는 조용히 무시되고
// 첫 번째를 덮어쓰지 않는다.
func TestDispatcherDuplicateTerminalIsInvariantViolation(t *testing.T) {
	queue := make(chan service.OutboxEvent, 4)
	jobs := make(chan settlementJob, 4)

	// 주문10을 inflight로 유지해 첫 terminal이 waiting에 머무르게 한다 —
	// 그래야 두 번째 cancel이 도착했을 때 중복 검사가 실제로 발동한다.
	queue <- tradeOutboxEventForOrders(1, 10, 20)
	queue <- cancelOutboxEvent(2, 10)
	queue <- cancelOutboxEvent(3, 10) // 엔진 불변식 위반 — waiting에 이미 order10이 있으므로 무시된다
	close(queue)

	// 전역 counter라 절대값이 아니라 delta를 본다(다른 테스트가 먼저 증가시킬 수 있다).
	duplicatesBefore := testutil.ToFloat64(metrics.SettlementDuplicateTerminalTotal)

	done := make(chan struct{})
	go func() {
		runPartitionDispatcher("0", queue, jobs, 4, 32, func(string, []byte) {})
		close(done)
	}()

	trade := <-jobs
	require.Equal(t, jobKindTrade, trade.kind)
	trade.done <- settlementResult{kind: jobKindTrade, id: trade.id, seq: trade.seq}

	terminal := <-jobs
	assert.Equal(t, jobKindTerminal, terminal.kind)
	assert.Equal(t, uint64(2), terminal.terminal.OutboxID, "첫 번째(outboxID=2) terminal이 유지돼야 한다")
	terminal.done <- settlementResult{kind: jobKindTerminal, id: terminal.id}

	select {
	case job := <-jobs:
		t.Fatalf("중복 terminal은 waiting에 추가되지 않으므로 두 번째 job이 나오면 안 된다: %+v", job)
	case <-done:
		// 정상 종료 — terminal job은 1개만 나왔다
	}

	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.SettlementDuplicateTerminalTotal)-duplicatesBefore,
		"불변식 위반은 로그뿐 아니라 counter로도 드러나야 한다(29번 무결성 게이트)")
}
