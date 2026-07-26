# 3차 ②-수정 정산 병렬화 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:executing-plans로 태스크별 실행.
> Steps use checkbox (`- [ ]`). superpowers:test-driven-development로 RED→GREEN.

**Goal:** 정산 DB 작업을 전역 worker pool(N)로 병렬화하되, 브로드캐스트는 `batchSeq` 순서로 직렬
방출한다. 종결 이벤트는 앞선 배치 완료 배리어. 정산 도메인 로직 무변경.

**Architecture:** 해시 파티션 큐(기존 유지)마다 **dispatcher goroutine 1개**, 전역 **worker pool N개**
공유. dispatcher는 jobs 送信·completion 수신·입력 수집을 **하나의 select event loop**로 multiplex
(교착 방지). worker는 브로드캐스트 대신 **수집 closure**를 주입해 payload를 모으고, coordinator
(dispatcher와 같은 goroutine)가 `nextBroadcastSeq`부터 연속으로 실제 방출.

**Tech Stack:** Go(채널·select), 기존 `settleTradeBatchWithFallback`/`processSingleOutboxEvent`.

**스펙 문서:** `docs/superpowers/specs/2026-07-27-settlement-parallelization-design.md`

## Global Constraints

- **정산 도메인 로직(`SettleTradeBatch`·멱등·폴백 판단) 무변경.** `settleTradeBatchWithFallback`·
  `processSingleOutboxEvent`의 **시그니처·본문도 그대로** 두고, 이미 파라미터로 주입되는
  `broadcast func(coinSymbol string, payload []byte)` 자리에 **수집 closure**를 넣어 방출만 지연시킨다.
- **기존 `GOEXCHANGE_SETTLEMENT_WORKERS`(파티션 수 P) 의미 불변.** 동시 정산 수는 **신규
  `GOEXCHANGE_SETTLEMENT_CONCURRENCY`(N, 기본 4)**. **N=1이면 현재 동작과 동등**.
- dispatcher는 jobs send와 completion receive를 **절대 분리 블로킹하지 않는다**(항상 같은 select).
  배리어 중에는 입력·jobs 케이스를 nil로 비활성화.
- `AvgBuyPrice`는 `Equal()`이 아니라 **tolerance**로 비교(A′). "항상 1e-16" 단정 금지 —
  순열 테스트로 상한 측정 후 기록.
- 통합 테스트 DSN(포트 55432). 커밋은 태스크 단위, Conventional Commits·한글(스킬 불가 시 직접 작성).

---

### Task 1: `SETTLEMENT_CONCURRENCY` 설정 (TDD)

**Files:**
- Modify: `config/runtime.go`
- Test: `config/runtime_test.go`

**Interfaces:**
- Produces: `config.SettlementConcurrencyFromEnv() int` (기본 4)

- [x] **Step 1: 실패 테스트** — `config/runtime_test.go`에 추가(기존 `SettlementWorkersFromEnv`
  테스트 패턴 그대로):

```go
func TestSettlementConcurrencyFromEnvDefaultsWhenUnset(t *testing.T) {
	t.Setenv(EnvGOExchangeSettlementConcurrency, "")
	if got, want := SettlementConcurrencyFromEnv(), 4; got != want {
		t.Fatalf("SettlementConcurrencyFromEnv() = %d, want %d", got, want)
	}
}

func TestSettlementConcurrencyFromEnvUsesExplicitValue(t *testing.T) {
	t.Setenv(EnvGOExchangeSettlementConcurrency, "8")
	if got, want := SettlementConcurrencyFromEnv(), 8; got != want {
		t.Fatalf("SettlementConcurrencyFromEnv() = %d, want %d", got, want)
	}
}

func TestSettlementConcurrencyFromEnvFallsBackOnInvalid(t *testing.T) {
	t.Setenv(EnvGOExchangeSettlementConcurrency, "0")
	if got, want := SettlementConcurrencyFromEnv(), 4; got != want {
		t.Fatalf("SettlementConcurrencyFromEnv() = %d, want %d", got, want)
	}
}
```

Run: `go test ./config/... -run SettlementConcurrency -v` → FAIL(undefined).

- [x] **Step 2: 구현** — `config/runtime.go`:

```go
// EnvGOExchangeSettlementConcurrency는 전역 정산 worker pool 크기(동시 정산 트랜잭션 수)다.
// 기존 EnvGOExchangeSettlementWorkers(해시 파티션 수)와는 다른 축이다 — 파티션 수는 순서
// 보존 단위, 이 값은 DB 동시성 상한. DB 풀(GOEXCHANGE_DB_MAX_OPEN_CONNS, 기본 25)을 주문
// 홀드·아웃박스·리컨실리에이션과 공유하므로 보수적 기본값에서 시작한다.
const EnvGOExchangeSettlementConcurrency = "GOEXCHANGE_SETTLEMENT_CONCURRENCY"

const defaultSettlementConcurrency = 4

func SettlementConcurrencyFromEnv() int {
	return parsePositiveIntEnv(EnvGOExchangeSettlementConcurrency, defaultSettlementConcurrency)
}
```

Run: `go test ./config/... -count=1` → PASS.

- [x] **Step 3: Commit** — `feat(config): 정산 동시성 GOEXCHANGE_SETTLEMENT_CONCURRENCY 추가 (3차 ②-수정)`

---

### Task 2: 정산 파이프라인(dispatcher + worker pool + 순서 커밋) — TDD

**Files:**
- Create: `cmd/settlement_pipeline.go`
- Test: `cmd/settlement_pipeline_test.go`

**Interfaces:**
- Produces:
  - `type broadcastMessage struct { coinSymbol string; payload []byte }`
  - `type settlementJob struct { seq uint64; batch []service.OutboxEvent; done chan<- settlementResult }`
  - `type settlementResult struct { seq uint64; messages []broadcastMessage }`
  - `func runSettlementWorker(jobs <-chan settlementJob, settleBatch func(batch []service.OutboxEvent, collect func(string, []byte)))`
  - `func runPartitionDispatcher(queue <-chan service.OutboxEvent, jobs chan<- settlementJob, concurrency int, maxBatch int, settleSingle func(event service.OutboxEvent, collect func(string, []byte)), broadcast func(string, []byte))`
- Consumes: 기존 `collectTradeBatch`, `service.OutboxEvent`.

- [x] **Step 1: 순서 보존 실패 테스트** — 워커 완료를 일부러 뒤섞어도 방출은 디스패치 순서:

```go
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
```

Run: `go test ./cmd/... -run BroadcastsInDispatchOrder -v` → FAIL(컴파일: 미정의).

- [x] **Step 2: 종결 이벤트 배리어 실패 테스트** — 종결 이벤트는 앞선 배치 방출 뒤에 처리:

```go
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
```

Run: 위와 함께 → FAIL.

- [x] **Step 3: 구현** — `cmd/settlement_pipeline.go`:

```go
package main

import (
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
)

// broadcastMessage는 정산 worker가 모아둔(아직 방출하지 않은) WS 메시지다.
type broadcastMessage struct {
	coinSymbol string
	payload    []byte
}

// settlementJob은 worker에게 넘기는 배치 1개. done은 파티션별 completion 채널이다.
type settlementJob struct {
	seq   uint64
	batch []service.OutboxEvent
	done  chan<- settlementResult
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
		var messages []broadcastMessage
		collect := func(symbol string, payload []byte) {
			messages = append(messages, broadcastMessage{coinSymbol: symbol, payload: payload})
		}
		if settleBatch != nil {
			settleBatch(job.batch, collect)
		}
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
		nextSeq        uint64 = 1
		nextBroadcast  uint64 = 1
		inFlight       int
		pending        = map[uint64]settlementResult{}
		queueOpen      = true
		readyJob       *settlementJob // 디스패치 대기 중인 배치(없으면 nil)
		pendingTerminal *service.OutboxEvent
		carry          *service.OutboxEvent // collectTradeBatch가 돌려준 비-trade 이벤트
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
					continue
				}
				batch, next, open := collectTradeBatch(*first, queue, maxBatch)
				if next != nil {
					carry = next
				}
				if !open {
					queueOpen = false
				}
				job := settlementJob{seq: nextSeq, batch: batch, done: completions}
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
```

Run: Step 1·2 테스트 → PASS.

- [x] **Step 4: 종료 드레인 테스트** — 큐 close 후 잔여 배치가 전부 방출되고 함수가 반환:

```go
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
```

Run: `go test ./cmd/... -run PartitionDispatcher -v -race` → PASS(3종).

- [x] **Step 5: Commit** — `feat(settlement): 정산 병렬 파이프라인(dispatcher·worker·순서 커밋) 추가 (3차 ②-수정)`

---

### Task 3: main() 배선 (파티션 dispatcher + 전역 pool)

**Files:**
- Modify: `cmd/main.go` (worker 루프 166-201 교체, 기동 로그)

- [x] **Step 1: 배선 교체** — 기존 파티션별 인라인 워커 루프를 dispatcher + 전역 pool로:

```go
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
			runSettlementWorker(settlementJobs, func(batch []service.OutboxEvent, collect func(string, []byte)) {
				settleTradeBatchWithFallback(batch, settlementService, settlementService, failedSettlementService,
					orderService, failedMarketCompletionService, orderService, collect, outboxRepo, log.Default())
			})
		}()
	}
	log.Printf("settlement partitions=%d concurrency=%d", len(settlementQueues), concurrency)

	var settlementWg sync.WaitGroup
	for _, queue := range settlementQueues {
		settlementWg.Add(1)
		go func(queue chan service.OutboxEvent) {
			defer settlementWg.Done()
			runPartitionDispatcher(queue, settlementJobs, concurrency, settlementBatchMaxSize,
				func(event service.OutboxEvent, collect func(string, []byte)) {
					processSingleOutboxEvent(event, settlementService, failedSettlementService, orderService,
						failedMarketCompletionService, orderService, collect, outboxRepo, log.Default())
				}, broadcast)
		}(queue)
	}
```

- [x] **Step 2: 종료 순서** — 기존 `settlementWg.Wait()` 뒤에 **jobs 채널 close + worker 대기**를
  추가한다(모든 dispatcher가 반환한 뒤에만 close해야 send-on-closed가 없다):

```go
	settlementWg.Wait()
	close(settlementJobs)
	settlementWorkerWg.Wait()
```

- [x] **Step 3: 검증** — `go build ./...`; `go test ./cmd/... -count=1 -race` PASS(기존 outbox 도미노·
  graceful shutdown 테스트 무수정 그린). 기동 로그에 `settlement partitions=.. concurrency=..` 출력 확인.
- [x] **Step 4: Commit** — `refactor(settlement): 정산 파이프라인을 파티션 dispatcher+전역 풀로 배선 (3차 ②-수정)`

---

### Task 4: 등가성 + `AvgBuyPrice` 오차 상한(A′) 검증

**Files:**
- Test: `internal/service/settlement_batch_integration_test.go`(등가성) 또는 신규
  `cmd/settlement_equivalence_integration_test.go`
- Test: `internal/service/avg_buy_price_permutation_test.go`(순열 오차, DB 불필요)

- [x] **Step 1: 순열 오차 상한 측정 테스트(DB 불필요)** — 스펙 A′의 "상한은 측정으로":

```go
// 여러 체결 순열에 대해 러닝 가중평균의 최대 오차를 측정한다(balance.go의 산술과 동일).
// "항상 1e-16"이라 단정하지 않고, 관측된 최대 오차가 tolerance 이하임을 단언한다.
func TestAvgBuyPriceOrderDependenceStaysWithinTolerance(t *testing.T) {
	tolerance := decimal.RequireFromString("0.000001") // 표시 단위보다 충분히 작음
	fills := []struct{ qty, cost string }{
		{"0.11", "5500001.11"}, {"0.13", "6500003.33"},
		{"0.17", "8500007.77"}, {"0.19", "9500011.19"},
	}
	apply := func(order []int) decimal.Decimal {
		qty := decimal.RequireFromString("0.7")
		avg := decimal.RequireFromString("49999999.9999999999999999")
		for _, i := range order {
			q := decimal.RequireFromString(fills[i].qty)
			c := decimal.RequireFromString(fills[i].cost)
			newQty := qty.Add(q)
			avg = avg.Mul(qty).Add(c).Div(newQty)
			qty = newQty
		}
		return avg
	}
	base := apply([]int{0, 1, 2, 3})
	maxDiff := decimal.Zero
	for _, perm := range [][]int{{3, 2, 1, 0}, {1, 0, 3, 2}, {2, 3, 0, 1}, {0, 2, 1, 3}, {3, 0, 2, 1}} {
		diff := base.Sub(apply(perm)).Abs()
		if diff.GreaterThan(maxDiff) {
			maxDiff = diff
		}
	}
	t.Logf("관측된 최대 AvgBuyPrice 순서 오차 = %s", maxDiff) // 완료 문서에 이 값을 기록
	assert.True(t, maxDiff.LessThanOrEqual(tolerance),
		"순열 간 평균매입가 오차 %s가 허용치 %s를 초과", maxDiff, tolerance)
}
```

Run: `go test ./internal/service/... -run AvgBuyPriceOrderDependence -v` → PASS + 로그의 최대 오차 기록.

- [x] **Step 2: 등가성 통합 테스트** — 같은 이벤트 열을 `concurrency=4`와 `1`로 처리 후 비교.
  **동일 단언**: 지갑 `available/locked/quantity/krw`, 주문 `FilledAmount`·상태, 원장 행 **개수와
  delta 합계**, `failed_settlements`/`failed_market_completions` = 0.
  **비-단언(명시)**: 원장 행별 `BalanceAfter`, `AvgBuyPrice`는 `Equal` 대신 위 tolerance 비교.
  (`openServiceIntegrationDB` + 기존 시드 헬퍼 재사용, `cleanupServiceUsers`로 격리.)
- [x] **Step 3: 전체 검증** — `go build ./...` + `go vet` + `go test ./... -count=1`(통합 SKIP 0) +
  `go test ./cmd/... ./internal/service/... -race -count=1` → 전부 PASS. 기존 정산·폴백·멱등·
  부트스트랩 테스트 무수정 그린.
- [x] **Step 4: Commit** — `test(settlement): 병렬 정산 등가성과 평균매입가 오차 상한 검증 추가 (3차 ②-수정)`

---

### Task 5: 프런트 평균매수가 포맷 (회귀 방지, `Go-exchange-front`)

**Files:**
- Modify: `Go-exchange-front/src/components/trading/AuthPanel.tsx:278`

- [x] **Step 1: 포맷 적용** — 현재 `{wallet.avg_buy_price}` 원문 출력을 바로 아래 평가액과 동일하게
  `formatKRWAmount(...)`로 포맷(값이 없거나 파싱 불가면 기존 fallback 유지). 백엔드 저장값은 불변.
- [x] **Step 2: 검증** — 프런트 테스트 스위트 그린(`npm test` 또는 리포 관례). `50000010.9999999999999999`
  같은 값이 포맷되어 노출되지 않음을 단언하는 테스트 1개 추가(`data-testid="balance-avg-buy-BTC"`).
- [x] **Step 3: Commit** — `fix(trading): 평균매수가를 시장 가격 정밀도로 포맷 (3차 ②-수정 동반)`

---

### Task 6: 완료 문서 + README

- [x] **Step 1: 완료 문서** — `docs/refactor/18_3차②_정산_병렬화_완료.md`: 왜(24번이 확정한 정산 워커
  바인딩) / 어떻게(파티션 dispatcher + 전역 pool, event loop 교착 방지, 수집 closure로 방출 지연,
  순서 커밋, 종결 배리어) / 결과(테스트, 회귀 그린, **측정된 AvgBuyPrice 최대 오차 기록**).
  **A′ 결정**(허용 + 포맷 + tolerance)과 **처리량 실증은 24번 재실행(1/2/4/8 스윕)** 임을 명기 —
  수치 주장 금지.
- [x] **Step 2: README** — 3차 표 ②-수정 🔨→✅ + 완료 문서 링크.
- [x] **Step 3: Commit + 푸시 + CI** — `gh run watch` 그린.

---

## 다음 (범위 밖)

**24번 재실행**(같은 진단 하니스로 `SETTLEMENT_CONCURRENCY` 1/2/4/8 스윕 —
`settlement_worker_queue_length` 포화 해소·처리량 변화 판정). 관측성 후속: 라이브 배치 정산 경로를
관측하지 못하는 `order_settlement_duration_seconds_count`(24번 발견). 파티션 수 P 재조정,
conflict-aware scheduling(필요해지면).
