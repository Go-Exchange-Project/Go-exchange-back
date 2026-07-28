package main

import (
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
)

// broadcastMessage는 정산 worker가 모아둔(아직 방출하지 않은) WS 메시지다.
type broadcastMessage struct {
	coinSymbol string
	payload    []byte
}

// settlementJob은 worker에게 넘기는 배치 1개. done은 파티션별 completion 채널이다.
type settlementJob struct {
	seq        uint64
	batch      []service.OutboxEvent
	done       chan<- settlementResult
	dispatchAt time.Time // 송신 시도 시점 — 채널 대기까지 포함해 측정(4차 축1 관측성)
}

// settlementResult는 정산 완료 후 순서대로 방출할 메시지를 담는다.
type settlementResult struct {
	seq      uint64
	messages []broadcastMessage
}

// runSettlementWorker는 전역 pool의 worker 1개다. 브로드캐스트는 하지 않고
// 수집 closure로 메시지를 모아 completion으로 돌려준다(순서 커밋은 dispatcher 몫).
func runSettlementWorker(jobs <-chan settlementJob, settleBatch func(batch []service.OutboxEvent, collect func(string, []byte))) {
	for job := range jobs {
		metrics.SettlementJobDispatchWait.Observe(time.Since(job.dispatchAt).Seconds())
		execStart := time.Now()
		var messages []broadcastMessage
		collect := func(symbol string, payload []byte) {
			messages = append(messages, broadcastMessage{coinSymbol: symbol, payload: payload})
		}
		if settleBatch != nil {
			settleBatch(job.batch, collect)
		}
		// settleBatch(=settleTradeBatchWithFallback)는 반환값이 없어 worker가 성공/폴백/실패를
		// 알 수 없다. 이번 패치는 success만 관측하고, fallback/failed 구분은 결과 전달 경로가
		// 필요하므로 범위 밖이다(다음 사이클).
		metrics.SettlementJobSuccess.Observe(time.Since(execStart).Seconds())
		job.done <- settlementResult{seq: job.seq, messages: messages}
	}
}

// runPartitionDispatcher는 파티션 큐 1개를 소유한다. 입력 수집·job 디스패치·completion
// 수신을 하나의 select로 multiplex한다(분리 블로킹 시 jobs/completion 교착이 가능하다).
// 종결 이벤트를 만나면 새 job을 던지지 않고 in-flight를 0까지 드레인·방출한 뒤 처리한다.
func runPartitionDispatcher(
	queue <-chan service.OutboxEvent,
	jobs chan<- settlementJob,
	concurrency int,
	maxBatch int,
	settleSingle func(event service.OutboxEvent, collect func(string, []byte)),
	broadcast func(string, []byte),
) {
	if concurrency < 1 {
		concurrency = 1
	}
	// completion 용량 = 최대 in-flight → worker의 done 送信은 절대 영구 블로킹하지 않는다.
	completions := make(chan settlementResult, concurrency)

	var (
		nextSeq         uint64 = 1
		nextBroadcast   uint64 = 1
		inFlight        int
		pending         = map[uint64]settlementResult{}
		queueOpen       = true
		readyJob        *settlementJob // 디스패치 대기 중인 배치(없으면 nil)
		pendingTerminal *service.OutboxEvent
		barrierStart    time.Time            // 배리어 진입 시각(4차 축1 관측성)
		carry           *service.OutboxEvent // collectTradeBatch가 돌려준 비-trade 이벤트
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

	runTerminal := func(event service.OutboxEvent) {
		if settleSingle == nil {
			return
		}
		settleSingle(event, broadcast) // 배리어 통과 후이므로 즉시 방출해도 순서가 보존된다
	}

	for {
		// 배리어: 종결 이벤트 대기 중이면 새 배치를 만들지도 디스패치하지도 않는다.
		barrier := pendingTerminal != nil
		if barrier && inFlight == 0 && readyJob == nil {
			flushBroadcasts()
			if pendingTerminal.Event.OrderCancelled != nil {
				metrics.SettlementBarrierWaitCancel.Observe(time.Since(barrierStart).Seconds())
			} else {
				metrics.SettlementBarrierWaitDone.Observe(time.Since(barrierStart).Seconds())
			}
			runTerminal(*pendingTerminal)
			pendingTerminal = nil
			continue
		}

		// 다음 배치 준비(배리어 중이 아니고, 대기 job이 없고, in-flight 여유가 있을 때만).
		if !barrier && readyJob == nil && inFlight < concurrency {
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
					pendingTerminal = first
					barrierStart = time.Now()
					if first.Event.OrderCancelled != nil {
						metrics.SettlementBarrierCancel.Inc()
						metrics.SettlementBarrierInflightCancel.Observe(float64(inFlight))
					} else {
						metrics.SettlementBarrierMarketDone.Inc()
						metrics.SettlementBarrierInflightDone.Observe(float64(inFlight))
					}
					continue
				}
				batch, next, open := collectTradeBatch(*first, queue, maxBatch)
				if next != nil {
					carry = next
				}
				if !open {
					queueOpen = false
				}
				job := settlementJob{seq: nextSeq, batch: batch, done: completions, dispatchAt: time.Now()}
				nextSeq++
				readyJob = &job
			}
		}

		// 종료 조건: 입력 닫힘 + 잔여 작업 없음.
		if !queueOpen && readyJob == nil && inFlight == 0 && carry == nil && pendingTerminal == nil {
			flushBroadcasts()
			return
		}

		// 디스패치 케이스는 대기 job이 있을 때만 활성(없으면 nil 채널 → 발화 안 함).
		var dispatchCh chan<- settlementJob
		var dispatchJob settlementJob
		if readyJob != nil {
			dispatchCh, dispatchJob = jobs, *readyJob
		}
		// 입력 케이스는 배리어 중이거나 닫혔으면 비활성.
		var inputCh <-chan service.OutboxEvent
		if !barrier && queueOpen && readyJob == nil && carry == nil && inFlight < concurrency {
			inputCh = queue
		}

		select {
		case dispatchCh <- dispatchJob:
			readyJob = nil
			inFlight++
		case res := <-completions:
			inFlight--
			pending[res.seq] = res
			flushBroadcasts()
		case ev, ok := <-inputCh:
			if !ok {
				queueOpen = false
				continue
			}
			carry = &ev
		}
	}
}
