package main

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/stretchr/testify/assert"
)

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
				job.done <- settlementResult{seq: job.seq, messages: []broadcastMessage{msg}}
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

	runPartitionDispatcher(queue, jobs, 3, 1, nil, broadcast)

	assert.Equal(t, []string{"seq-1", "seq-2", "seq-3"}, got,
		"완료 순서와 무관하게 디스패치 순서로 방출돼야 한다")
}

func TestPartitionDispatcherProcessesTerminalEventAfterPrecedingBatches(t *testing.T) {
	queue := make(chan service.OutboxEvent, 8)
	jobs := make(chan settlementJob, 4)

	queue <- service.OutboxEvent{OutboxID: 1, Event: matching.ExecutionEvent{
		Trade: &model.Trade{CoinSymbol: "BTC"}}}
	queue <- service.OutboxEvent{OutboxID: 2, Event: matching.ExecutionEvent{
		OrderCancelled: &matching.OrderCancelled{CoinSymbol: "BTC", OrderID: 7}}}
	close(queue)

	go func() {
		for job := range jobs {
			job := job
			go func() {
				time.Sleep(30 * time.Millisecond) // 배치가 늦게 끝나도
				job.done <- settlementResult{seq: job.seq, messages: []broadcastMessage{
					{coinSymbol: "BTC", payload: []byte("trade")}}}
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
	// 종결 이벤트 처리도 방출로 관측(수집 closure가 broadcast로 흘러감)
	settleSingle := func(event service.OutboxEvent, collect func(string, []byte)) {
		collect("BTC", []byte("terminal"))
	}

	runPartitionDispatcher(queue, jobs, 3, 32, settleSingle, broadcast)

	assert.Equal(t, []string{"trade", "terminal"}, got,
		"종결 이벤트는 앞선 배치의 방출 뒤에 처리돼야 한다")
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
			job.done <- settlementResult{seq: job.seq, messages: []broadcastMessage{
				{coinSymbol: "BTC", payload: []byte("t")}}}
		}
	}()
	var mu sync.Mutex
	count := 0
	broadcast := func(string, []byte) { mu.Lock(); count++; mu.Unlock() }

	done := make(chan struct{})
	go func() { runPartitionDispatcher(queue, jobs, 2, 1, nil, broadcast); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("큐 close 후에도 dispatcher가 반환하지 않음")
	}
	assert.Equal(t, 5, count, "잔여 배치가 전부 방출돼야 한다")
}
