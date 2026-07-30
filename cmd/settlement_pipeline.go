package main

import (
	"log"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
)

// broadcastMessage는 정산 worker가 모아둔(아직 방출하지 않은) WS 메시지다.
type broadcastMessage struct {
	coinSymbol string
	payload    []byte
}

type settlementJobKind uint8

const (
	jobKindTrade settlementJobKind = iota
	jobKindTerminal
)

// settlementJob은 worker에게 넘기는 작업 1개다. trade job은 배치를, terminal job은
// 단건 종결 이벤트를 싣는다. terminal은 WS 메시지를 만들지 않으므로 브로드캐스트
// 시퀀스(seq)를 소비하지 않는다.
type settlementJob struct {
	kind       settlementJobKind
	id         uint64
	seq        uint64 // jobKindTrade에서만 의미가 있다
	batch      []service.OutboxEvent
	terminal   service.OutboxEvent
	done       chan<- settlementResult
	dispatchAt time.Time
}

// settlementResult는 정산 완료 후 순서대로 방출할 메시지와, 내구 확정에 실패해
// terminal 실행이 금지된 주문 ID를 담는다.
type settlementResult struct {
	kind              settlementJobKind
	id                uint64
	seq               uint64
	messages          []broadcastMessage
	undurableOrderIDs []uint
}

// waitingTerminal은 자기 주문의 배치가 모두 retire되길 기다리는 종결 이벤트다.
type waitingTerminal struct {
	event     service.OutboxEvent
	orderID   uint
	kind      string // "cancel" | "market_done" — 메트릭 라벨
	arrivedAt time.Time
}

// runSettlementWorker는 전역 pool의 worker 1개다. trade job은 배치 정산 결과를 모아
// completion으로 돌려주고, terminal job은 settleTerminal로 단건 실행한다(브로드캐스트는
// 만들지 않음 — WS 메시지가 없으므로 순서 커밋과 무관하다).
func runSettlementWorker(
	jobs <-chan settlementJob,
	settleBatch func(batch []service.OutboxEvent, collect func(string, []byte)) []uint,
	settleTerminal func(event service.OutboxEvent),
) {
	for job := range jobs {
		metrics.SettlementJobDispatchWait.Observe(time.Since(job.dispatchAt).Seconds())
		execStart := time.Now()
		var messages []broadcastMessage
		var undurable []uint
		if job.kind == jobKindTerminal {
			if settleTerminal != nil {
				settleTerminal(job.terminal)
			}
		} else {
			collect := func(symbol string, payload []byte) {
				messages = append(messages, broadcastMessage{coinSymbol: symbol, payload: payload})
			}
			if settleBatch != nil {
				undurable = settleBatch(job.batch, collect)
			}
		}
		metrics.SettlementJobSuccess.Observe(time.Since(execStart).Seconds())
		job.done <- settlementResult{kind: job.kind, id: job.id, seq: job.seq, messages: messages, undurableOrderIDs: undurable}
	}
}

// terminalOrderID는 종결 이벤트의 주문 ID와 메트릭 라벨을 돌려준다.
func terminalOrderID(event service.OutboxEvent) (uint, string) {
	if event.Event.OrderCancelled != nil {
		return event.Event.OrderCancelled.OrderID, "cancel"
	}
	return event.Event.MarketOrderDone.OrderID, "market_done"
}

// runPartitionDispatcher는 파티션 큐 1개를 소유한다. per-order fence: terminal은
// 자기 주문을 건드린 배치가 모두 retire될 때까지만 대기하고, 무관한 주문의 배치는
// 계속 진행한다(파티션 전체를 막는 배리어는 없다). 입력 수집·job 디스패치·completion
// 수신을 하나의 select로 multiplex한다(분리 블로킹 시 jobs/completion 교착이 가능하다).
// terminal은 worker가 실행하므로 dispatcher는 더 이상 settleSingle을 받지 않는다.
func runPartitionDispatcher(
	partition string,
	queue <-chan service.OutboxEvent,
	jobs chan<- settlementJob,
	concurrency int,
	maxBatch int,
	broadcast func(string, []byte),
) {
	if concurrency < 1 {
		concurrency = 1
	}
	// 실행 중 최대 N + jobs 채널 대기 최대 N. completion 채널 용량을 같은 값으로
	// 묶어야 worker의 done 송신이 영구 블로킹하지 않는다(각 outstanding job이
	// 자기 슬롯을 이미 확보한 상태로만 dispatch된다).
	maxOutstanding := 2 * concurrency
	completions := make(chan settlementResult, maxOutstanding)

	dep := newDependencyTracker()
	outstandingGauge := metrics.SettlementOutstandingJobs.WithLabelValues(partition)
	quarantineGauge := metrics.SettlementQuarantinedOrders.WithLabelValues(partition)

	var (
		nextJobID     uint64 = 1
		nextSeq       uint64 = 1
		nextBroadcast uint64 = 1
		pending              = map[uint64]settlementResult{}
		waiting       []waitingTerminal
		readyJob      *settlementJob
		queueOpen     = true
		carry         *service.OutboxEvent
	)

	flushBroadcasts := func() {
		for {
			res, ok := pending[nextBroadcast]
			if !ok {
				return
			}
			delete(pending, nextBroadcast)
			for _, m := range res.messages {
				broadcast(m.coinSymbol, m.payload)
			}
			nextBroadcast++
		}
	}

	// takeReadyTerminal은 자기 주문의 배치가 모두 retire된 terminal을 waiting에서
	// 꺼낸다. quarantine된 주문은 꺼내되 dispatch하지 않고 버린다(outbox는 PENDING
	// 유지 — 부팅 replay가 받는다).
	takeReadyTerminal := func() *waitingTerminal {
		for i := range waiting {
			w := waiting[i]
			if !dep.ready(w.orderID) {
				continue
			}
			waiting = append(waiting[:i], waiting[i+1:]...)
			if dep.quarantined(w.orderID) {
				dep.clearQuarantine(w.orderID)
				quarantineGauge.Set(float64(dep.quarantinedCount()))
				metrics.SettlementTerminalDeferred.WithLabelValues(w.kind, "quarantine").Inc()
				return nil // 실행 금지 — 아무것도 하지 않는다
			}
			return &w
		}
		return nil
	}

	appendWaiting := func(event service.OutboxEvent) {
		orderID, kind := terminalOrderID(event)
		for _, w := range waiting {
			if w.orderID == orderID {
				// 주문당 terminal 1개는 엔진 불변식이다 — 조용히 덮어쓰지 않는다.
				log.Printf("settlement dispatcher: duplicate terminal for order %d (engine invariant violation)", orderID)
				return
			}
		}
		waiting = append(waiting, waitingTerminal{event: event, orderID: orderID, kind: kind, arrivedAt: time.Now()})
	}

	for {
		// (1) 실행 가능한 terminal을 먼저 꺼낸다 — 홀드가 잠긴 상태이므로 우선한다.
		if readyJob == nil && dep.outstanding() < maxOutstanding {
			if w := takeReadyTerminal(); w != nil {
				metrics.SettlementTerminalWait.WithLabelValues(w.kind).Observe(time.Since(w.arrivedAt).Seconds())
				job := settlementJob{kind: jobKindTerminal, id: nextJobID, terminal: w.event, done: completions, dispatchAt: time.Now()}
				nextJobID++
				readyJob = &job
			}
		}

		// (2) 다음 trade 배치 준비.
		if readyJob == nil && dep.outstanding() < maxOutstanding {
			var first *service.OutboxEvent
			if carry != nil {
				first, carry = carry, nil
			} else if queueOpen {
				select {
				case ev, ok := <-queue:
					if !ok {
						queueOpen = false
					} else {
						first = &ev
					}
				default:
				}
			}
			if first != nil {
				if first.Event.Trade == nil {
					appendWaiting(*first)
					continue
				}
				batch, next, open := collectTradeBatch(*first, queue, maxBatch)
				if next != nil {
					carry = next
				}
				if !open {
					queueOpen = false
				}
				job := settlementJob{kind: jobKindTrade, id: nextJobID, seq: nextSeq, batch: batch, done: completions, dispatchAt: time.Now()}
				nextJobID++
				nextSeq++
				readyJob = &job
			}
		}

		// (3) 종료: 입력이 닫혔고 남은 작업이 없을 때만. 이미 소비한 terminal은
		// dependency 해제 후 반드시 dispatch된다(graceful drain).
		if !queueOpen && readyJob == nil && dep.outstanding() == 0 && carry == nil && len(waiting) == 0 {
			flushBroadcasts()
			return
		}

		var dispatchCh chan<- settlementJob
		var dispatchJob settlementJob
		var dispatchOrders []uint
		if readyJob != nil {
			dispatchCh, dispatchJob = jobs, *readyJob
			if dispatchJob.kind == jobKindTrade {
				dispatchOrders = dep.touchedOrderIDs(dispatchJob.batch)
			}
		}
		var inputCh <-chan service.OutboxEvent
		if queueOpen && readyJob == nil && carry == nil && dep.outstanding() < maxOutstanding {
			inputCh = queue
		}

		select {
		case dispatchCh <- dispatchJob:
			// 등록은 송신 성공 후에만 — case body가 끝나기 전에는 completion을
			// 처리할 수 없으므로 등록이 completion에 추월당하지 않는다.
			dep.register(dispatchJob.id, dispatchOrders)
			outstandingGauge.Set(float64(dep.outstanding()))
			readyJob = nil
		case res := <-completions:
			// retire는 성공·실패 무관하게 수행한다(자원 반납). dependency 충족
			// 판정은 DB 권위이며, quarantine 표시는 retire 안에서 count 감소보다
			// 먼저 일어난다.
			if err := dep.retire(res.id, res.undurableOrderIDs); err != nil {
				log.Printf("settlement dispatcher: %v", err)
			}
			if len(res.undurableOrderIDs) > 0 {
				metrics.SettlementDependencyRecordFailedTotal.Inc()
			}
			outstandingGauge.Set(float64(dep.outstanding()))
			quarantineGauge.Set(float64(dep.quarantinedCount()))
			if res.kind == jobKindTrade {
				pending[res.seq] = res
				flushBroadcasts()
			}
		case ev, ok := <-inputCh:
			if !ok {
				queueOpen = false
				continue
			}
			carry = &ev
		}
	}
}
