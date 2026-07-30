# A+C. per-order 런타임 fence와 terminal durable defer 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 정산 dispatcher의 파티션 전체 배리어를 **주문 단위 fence**로 좁히고, 실행할 수 없는 terminal을 **내구 기록으로 인계**해 온라인 복구가 가능하게 만든다.

**Architecture:** dispatcher는 `orderID → outstanding batch count`를 단독 소유하고, terminal은 자기 주문의 배치가 모두 retire된 뒤에만 공용 worker pool에 job으로 dispatch된다. 실행 불가 terminal(선행 정산 실패·실행 실패)은 `failed_market_completions` / 신규 `failed_order_cancellations`에 기록하고 원본 outbox를 `PROCESSED`로 닫는 **durable handoff**로 넘긴다. 기록조차 실패하면 outbox를 `PENDING`으로 남기고 boot 경로가 **fail-closed**로 받는다.

**Tech Stack:** Go 1.x, Gin, GORM, PostgreSQL, goose migrations, Prometheus client_golang, testify

**설계 문서:** [2026-07-30-per-order-fence-and-terminal-durable-defer-design.md](../specs/2026-07-30-per-order-fence-and-terminal-durable-defer-design.md)

## Global Constraints

- **정합성은 비협상.** 홀드는 늦게 풀릴 수는 있어도 **일찍 풀리면 안 된다**. 판단이 갈리면 항상 fail-closed.
- **`dependency failed ≠ dependency completed`** — 실패한 선행 정산은 terminal 실행 근거가 되지 않는다.
- **dependency 충족의 권위는 DB다.** 메모리 플래그를 판정에 쓰지 않는다. 예외는 `undurableOrderIDs` 하나뿐.
- **기존 메트릭의 의미·라벨을 바꾸지 않는다.** 의미가 달라지면 새 이름을 쓴다(`order_settlement_duration_seconds`는 절대 건드리지 않는다).
- **`failed_settlements` 테이블·제약은 건드리지 않는다.**
- **관련 없는 코드·주석·포맷팅을 손대지 않는다.** 변경된 모든 줄이 이 계획과 직접 연결돼야 한다.
- **`maxOutstanding = 2 * concurrency`**, **`cap(completions) = maxOutstanding`** — 두 값은 같은 변수에서 파생한다.
- 커밋 메시지는 **한글 subject+body**(type prefix는 영문), 커밋 전 **`commit-message` 스킬** 사용(사소한 오타 제외). `Co-Authored-By` 푸터는 넣지 않는다.
- 단위 테스트: `go test ./... -race`. 통합 테스트: `GOEXCHANGE_TEST_DATABASE_DSN` 설정 시에만 실행된다.

## File Structure

| 파일 | 책임 | 태스크 |
|---|---|---|
| `cmd/main.go` | outbox 이벤트 처리 분기, undurable 전파, terminal 실행·defer 정책, 배선 | 1, 2, 5 |
| `cmd/settlement_pipeline.go` | worker·dispatcher event loop | 1, 7 |
| `cmd/settlement_dependency.go` | **신규** — dispatcher dependency 상태 기계(순수 로직) | 6 |
| `cmd/settlement_dependency_test.go` | **신규** — 상태 기계 단위 테스트 | 6 |
| `internal/service/outbox_replayer.go` | boot replay fail-closed, ID 의미 분리 | 2 |
| `internal/model/failed_market_completion.go` | `retry_count` default 0 | 3 |
| `internal/repository/failed_market_completion_repository.go` | `EnsureDeferred` | 3 |
| `internal/service/failed_market_completion_service.go` | `EnsureDeferred` 서비스 계층 | 3 |
| `migrations/005_terminal_durable_defer.sql` | **신규** — retry_count 0 허용 + 신규 테이블 제약 | 3, 4 |
| `internal/model/failed_order_cancellation.go` | **신규** — cancel durable retry index | 4 |
| `internal/repository/failed_order_cancellation_repository.go` | **신규** | 4 |
| `internal/service/failed_order_cancellation_service.go` | **신규** | 4 |
| `internal/service/settlement_retry_worker.go` | cancel phase, terminal defer 메트릭 | 4 |
| `internal/metrics/metrics.go` | 신규 지표, 배리어 지표 제거 | 5, 7 |

---

## Task 1: undurable outcome 전파

정산도 실패하고 실패 기록도 실패한 trade의 주문 ID를 worker completion까지 흘린다. **dispatcher 동작은 바꾸지 않는다.**

**Files:**
- Modify: `cmd/main.go:497-522` (`processSingleOutboxEvent`), `cmd/main.go:553-588` (`settleTradeBatchWithFallback`)
- Modify: `cmd/settlement_pipeline.go:24-49` (`settlementResult`, `runSettlementWorker`)
- Test: `cmd/settlement_undurable_test.go` (신규)

**Interfaces:**
- Consumes: 없음 (첫 태스크)
- Produces:
  - `func processSingleOutboxEvent(...) bool` — `handled`. 기존 인자 순서 그대로, 반환값만 추가
  - `func settleTradeBatchWithFallback(...) []uint` — undurable 주문 ID(중복 제거되지 않은 원본, 호출자가 dedup)
  - `settleBatch func(batch []service.OutboxEvent, collect func(string, []byte)) []uint`
  - `settlementResult{seq uint64; messages []broadcastMessage; undurableOrderIDs []uint}`

- [x] **Step 1: 실패 테스트 작성** (`tradeOutboxEvent`가 이미 다른 시그니처로 존재해
  `tradeOutboxEventForOrders`로 이름을 바꿔 추가했다 — 아래 완료 보고 참고)

`cmd/settlement_undurable_test.go` 생성:

```go
package main

import (
	"errors"
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
)

type stubBatchSettler struct{ err error }

func (s stubBatchSettler) SettleTradeBatch(items []service.TradeBatchItem) ([]service.SettlementResult, error) {
	if s.err != nil {
		return nil, s.err
	}
	results := make([]service.SettlementResult, len(items))
	for i := range results {
		results[i] = service.SettlementResult{Applied: true}
	}
	return results, nil
}

type stubSettler struct{ err error }

func (s stubSettler) SettleTrade(trade *model.Trade, outboxEventID uint64) (service.SettlementResult, error) {
	if s.err != nil {
		return service.SettlementResult{}, s.err
	}
	return service.SettlementResult{Applied: true}, nil
}

// recordErr가 nil이 아니면 실패 기록 자체가 실패한다 = undurable.
type stubFailureRecorder struct{ recordErr error }

func (s stubFailureRecorder) RecordFailure(trade *model.Trade, settlementErr error) (*model.FailedSettlement, error) {
	if s.recordErr != nil {
		return nil, s.recordErr
	}
	return &model.FailedSettlement{}, nil
}

type stubOutboxMarker struct{ markErr error }

func (s stubOutboxMarker) MarkProcessed(id uint64) error { return s.markErr }

func tradeOutboxEvent(outboxID uint64, buyOrderID, sellOrderID uint) service.OutboxEvent {
	return service.OutboxEvent{
		OutboxID: outboxID,
		Event: matchingExecutionEventWithTrade(&model.Trade{
			CoinSymbol:  "BTC",
			BuyOrderID:  buyOrderID,
			SellOrderID: sellOrderID,
			Price:       decimal.NewFromInt(100),
			Quantity:    decimal.NewFromInt(1),
		}),
	}
}

func TestSettleTradeBatchWithFallbackReportsUndurableOrders(t *testing.T) {
	batch := []service.OutboxEvent{
		tradeOutboxEvent(1, 10, 20),
		tradeOutboxEvent(2, 30, 40),
	}

	// 배치 실패 → 폴백 단건 → 단건도 실패 → 기록도 실패 = undurable
	undurable := settleTradeBatchWithFallback(
		batch,
		stubBatchSettler{err: errors.New("batch boom")},
		stubSettler{err: errors.New("single boom")},
		stubFailureRecorder{recordErr: errors.New("record boom")},
		nil, nil, nil,
		func(string, []byte) {},
		stubOutboxMarker{},
		testLogger(),
	)

	assert.ElementsMatch(t, []uint{10, 20, 30, 40}, undurable)
}

func TestSettleTradeBatchWithFallbackNoUndurableWhenFailureRecorded(t *testing.T) {
	batch := []service.OutboxEvent{tradeOutboxEvent(1, 10, 20)}

	undurable := settleTradeBatchWithFallback(
		batch,
		stubBatchSettler{err: errors.New("batch boom")},
		stubSettler{err: errors.New("single boom")},
		stubFailureRecorder{}, // 기록은 성공 → durable
		nil, nil, nil,
		func(string, []byte) {},
		stubOutboxMarker{},
		testLogger(),
	)

	assert.Empty(t, undurable)
}

// 정산은 커밋됐고 outbox 마킹만 실패한 경우는 undurable이 아니다.
func TestSettleTradeBatchWithFallbackMarkProcessedFailureIsNotUndurable(t *testing.T) {
	batch := []service.OutboxEvent{tradeOutboxEvent(1, 10, 20)}

	undurable := settleTradeBatchWithFallback(
		batch,
		stubBatchSettler{err: errors.New("batch boom")},
		stubSettler{}, // 정산 성공
		stubFailureRecorder{},
		nil, nil, nil,
		func(string, []byte) {},
		stubOutboxMarker{markErr: errors.New("mark boom")},
		testLogger(),
	)

	assert.Empty(t, undurable)
}
```

같은 파일에 헬퍼를 둔다:

```go
func testLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func matchingExecutionEventWithTrade(trade *model.Trade) matching.ExecutionEvent {
	return matching.ExecutionEvent{Trade: trade}
}
```

(`log`, `io`, `matching` import 추가)

- [x] **Step 2: 테스트 실패 확인**

Run: `go test ./cmd/ -run TestSettleTradeBatchWithFallback -v`
Expected: FAIL — `settleTradeBatchWithFallback(...) used as value` (현재 반환값 없음)

- [x] **Step 3: `processSingleOutboxEvent`가 `handled`를 반환하도록 변경**

`cmd/main.go:497-522`:

```go
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
	broadcast func(coinSymbol string, payload []byte),
	outboxRepo outboxMarker,
	logger *log.Logger,
) bool {
	handled, markedInTx := processExecutionEvent(outboxEvent.Event, outboxEvent.OutboxID, settler, failureRecorder, marketCompleter, completionFailureRecorder, cancelProcessor, broadcast, logger)
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
```

- [x] **Step 4: `settleTradeBatchWithFallback`이 undurable 주문 ID를 반환하도록 변경**

`cmd/main.go:553-588`의 시그니처와 폴백 루프:

```go
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
			if processSingleOutboxEvent(event, settler, failureRecorder, marketCompleter, completionFailureRecorder, cancelProcessor, broadcast, outboxRepo, logger) {
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
```

- [x] **Step 5: worker가 undurable을 completion으로 전달**

`cmd/settlement_pipeline.go:24-49`:

```go
// settlementResult는 정산 완료 후 순서대로 방출할 메시지와, 내구 확정에 실패해
// terminal 실행이 금지된 주문 ID를 담는다.
type settlementResult struct {
	seq               uint64
	messages          []broadcastMessage
	undurableOrderIDs []uint
}

// runSettlementWorker는 전역 pool의 worker 1개다. 브로드캐스트는 하지 않고
// 수집 closure로 메시지를 모아 completion으로 돌려준다(순서 커밋은 dispatcher 몫).
// undurable 주문 ID는 판정하지 않고 그대로 전달만 한다(판정은 dispatcher 몫).
func runSettlementWorker(jobs <-chan settlementJob, settleBatch func(batch []service.OutboxEvent, collect func(string, []byte)) []uint) {
	for job := range jobs {
		metrics.SettlementJobDispatchWait.Observe(time.Since(job.dispatchAt).Seconds())
		execStart := time.Now()
		var messages []broadcastMessage
		var undurable []uint
		collect := func(symbol string, payload []byte) {
			messages = append(messages, broadcastMessage{coinSymbol: symbol, payload: payload})
		}
		if settleBatch != nil {
			undurable = settleBatch(job.batch, collect)
		}
		metrics.SettlementJobSuccess.Observe(time.Since(execStart).Seconds())
		job.done <- settlementResult{seq: job.seq, messages: messages, undurableOrderIDs: undurable}
	}
}
```

`settlement_pipeline.go:43-45`의 "settleBatch는 반환값이 없어 worker가 성공/폴백/실패를 알 수 없다" 주석은 **삭제한다**(더 이상 사실이 아니다).

- [x] **Step 6: 호출부 배선** (`settlement_pipeline_test.go`의 `okSettleBatch` 클로저도
  `[]uint` 반환으로 함께 수정 — 시그니처 변경의 직접 파급)

`cmd/main.go`에서 `settleTradeBatchWithFallback`을 `settleBatch`로 넘기는 closure의 반환값을 전달하도록 수정한다(현재 무반환 closure → `[]uint` 반환).

- [x] **Step 7: 테스트 통과 확인** (3 tests PASS, build/vet 클린, `-race` 포함 cmd 패키지 전체 그린)
- [x] **Step 8: 커밋** (`2183db4` — 설계+계획 문서 커밋에 실수로 합쳐짐, 완료 보고에 기록)

---

## Task 2: replayer fail-closed와 ID 의미 분리

boot replay가 앞 이벤트 실패에서 멈추게 하고, corrupted 행의 파괴적 마킹을 제거한다. **terminal guard는 아직 배선하지 않는다.**

**Files:**
- Modify: `internal/service/outbox_replayer.go`
- Modify: `cmd/main.go:472-493` (`processExecutionEvent` 시그니처), `cmd/main.go:131-147` (배선)
- Test: `internal/service/outbox_replayer_test.go`

**Interfaces:**
- Consumes: Task 1의 `processSingleOutboxEvent(...) bool`
- Produces:
  - `func processExecutionEvent(event matching.ExecutionEvent, transactionalOutboxID uint64, sourceOutboxID uint64, ...) (handled bool, markedInTx bool)`
  - `OutboxReplayer.Process func(sourceOutboxID uint64, event matching.ExecutionEvent) bool`
  - `OutboxReplayResult{Replayed, Deferred, Undurable, Corrupted int}`

- [x] **Step 1: 실패 테스트 작성**

`internal/service/outbox_replayer_test.go`에 추가:

```go
func TestReplayStopsOnUndurableEvent(t *testing.T) {
	src := &fakeOutboxReplaySource{rows: []model.TradeOutboxEvent{
		tradeOutboxRow(1), tradeOutboxRow(2), tradeOutboxRow(3),
	}}
	var seen []uint64
	r := &service.OutboxReplayer{
		Repo: src,
		Process: func(sourceOutboxID uint64, event matching.ExecutionEvent) bool {
			seen = append(seen, sourceOutboxID)
			return sourceOutboxID != 2 // 2번에서 내구 확정 실패
		},
	}

	result, err := r.Replay()

	require.Error(t, err)
	assert.Equal(t, []uint64{1, 2}, seen, "3번은 처리되면 안 된다")
	assert.Equal(t, 1, result.Undurable)
	assert.NotContains(t, src.marked, uint64(2), "undurable 행은 PENDING으로 남아야 한다")
}

func TestReplayStopsOnCorruptedEventWithoutMarking(t *testing.T) {
	src := &fakeOutboxReplaySource{rows: []model.TradeOutboxEvent{
		tradeOutboxRow(1), corruptedOutboxRow(2), tradeOutboxRow(3),
	}}
	var seen []uint64
	r := &service.OutboxReplayer{
		Repo: src,
		Process: func(sourceOutboxID uint64, event matching.ExecutionEvent) bool {
			seen = append(seen, sourceOutboxID)
			return true
		},
	}

	result, err := r.Replay()

	require.Error(t, err)
	assert.Equal(t, []uint64{1}, seen)
	assert.Equal(t, 1, result.Corrupted)
	assert.NotContains(t, src.marked, uint64(2),
		"corrupted 행을 PROCESSED로 마킹하면 처리되지 않은 금융 이벤트가 영구 소실된다")
}

func TestReplayContinuesWhenOnlyMarkProcessedFails(t *testing.T) {
	src := &fakeOutboxReplaySource{
		rows:      []model.TradeOutboxEvent{tradeOutboxRow(1), tradeOutboxRow(2)},
		markFails: map[uint64]bool{1: true},
	}
	var seen []uint64
	r := &service.OutboxReplayer{
		Repo: src,
		Process: func(sourceOutboxID uint64, event matching.ExecutionEvent) bool {
			seen = append(seen, sourceOutboxID)
			return true
		},
	}

	result, err := r.Replay()

	require.NoError(t, err)
	assert.Equal(t, []uint64{1, 2}, seen, "정산은 커밋됐으므로 계속 진행해야 한다")
	assert.Equal(t, 1, result.Deferred)
	assert.Equal(t, 1, result.Replayed)
}

func TestReplayPassesSourceOutboxIDToProcess(t *testing.T) {
	src := &fakeOutboxReplaySource{rows: []model.TradeOutboxEvent{tradeOutboxRow(7)}}
	var got uint64
	r := &service.OutboxReplayer{
		Repo: src,
		Process: func(sourceOutboxID uint64, event matching.ExecutionEvent) bool {
			got = sourceOutboxID
			return true
		},
	}

	_, err := r.Replay()

	require.NoError(t, err)
	assert.Equal(t, uint64(7), got)
}
```

기존 `fakeOutboxReplaySource`에 `marked []uint64`, `markFails map[uint64]bool` 필드를 추가하고 `MarkProcessed`가 이를 반영하게 한다. `tradeOutboxRow(id)`는 정상 역직렬화되는 행을, `corruptedOutboxRow(id)`는 payload를 깨뜨린 행을 만드는 헬퍼다.

- [x] **Step 2: 테스트 실패 확인**

Run: `go test ./internal/service/ -run TestReplay -v`
Expected: FAIL — `Process` 시그니처 불일치 컴파일 오류, `result.Undurable` 미정의

- [x] **Step 3: `OutboxReplayer` 구현**

`internal/service/outbox_replayer.go`:

```go
type OutboxReplayer struct {
	Repo OutboxReplaySource
	// Process는 이벤트를 정산 파이프라인과 동일한 로직으로 처리하고,
	// 처리 결과가 내구적으로 확정됐는지(정산 성공/멱등 no-op/실패의 내구 기록)를
	// 반환합니다. false면 PENDING으로 남기고 이번 부팅의 replay를 즉시 중단합니다 —
	// 뒤 이벤트를 계속 처리하면 미정산 trade 위에서 terminal이 실행될 수 있습니다.
	// sourceOutboxID는 원본 행 ID로, 실패 기록의 provenance에 쓰입니다.
	Process  func(sourceOutboxID uint64, event matching.ExecutionEvent) bool
	PageSize int
	Logger   *log.Logger
}

type OutboxReplayResult struct {
	Replayed  int // 처리 후 PROCESSED 마킹까지 끝난 이벤트
	Deferred  int // 도메인 처리는 확정됐으나 MarkProcessed만 실패(다음 부팅이 멱등 재처리)
	Undurable int // 내구 확정 실패 — PENDING 유지, replay 중단
	Corrupted int // 역직렬화 불가 — PENDING 유지, replay 중단(마킹하지 않는다)
}

func (r *OutboxReplayer) Replay() (OutboxReplayResult, error) {
	var result OutboxReplayResult
	if r.Repo == nil || r.Process == nil {
		return result, fmt.Errorf("outbox replayer requires repo and process func")
	}

	var afterID uint64
	for {
		rows, err := r.Repo.FindPendingAfter(afterID, r.pageSize())
		if err != nil {
			return result, fmt.Errorf("load pending outbox events: %w", err)
		}
		if len(rows) == 0 {
			return result, nil
		}
		for _, row := range rows {
			afterID = row.ID

			event, err := ExecutionEventFromOutbox(row)
			if err != nil {
				// 처리할 수 없는 금융 이벤트를 PROCESSED로 선언하지 않는다 —
				// 마킹하면 이벤트가 영구 소실되고 뒤 terminal이 그 위에서 실행된다.
				// 부팅을 막고 운영자가 복구하게 한다(runbook 참조).
				r.logf("outbox replay: CORRUPTED event %d, replay aborted: %v", row.ID, err)
				result.Corrupted++
				return result, fmt.Errorf("corrupted outbox event %d: %w", row.ID, err)
			}

			if !r.Process(row.ID, event) {
				// 내구 확정 실패(정산 실패의 기록조차 실패). 뒤 이벤트를 계속 처리하면
				// 미정산 trade 위에서 terminal이 실행될 수 있으므로 즉시 중단한다.
				r.logf("outbox replay: event %d not durably handled, replay aborted", row.ID)
				result.Undurable++
				return result, fmt.Errorf("outbox event %d not durably handled", row.ID)
			}
			if err := r.Repo.MarkProcessed(row.ID); err != nil {
				// 정산은 커밋됐다 — dependency는 충족이므로 계속 진행한다.
				// 다음 리플레이가 멱등 재처리한다.
				r.logf("outbox replay: mark event %d processed failed: %v", row.ID, err)
				result.Deferred++
				continue
			}
			result.Replayed++
		}
		if len(rows) < r.pageSize() {
			return result, nil
		}
	}
}
```

- [x] **Step 4: `processExecutionEvent`에 `sourceOutboxID` 추가**

`cmd/main.go:472-493`. **`transactionalOutboxID`는 기존 `outboxEventID`의 이름 변경이며 의미는 그대로다**(정산 트랜잭션 안에서 마킹할 ID, replay에서는 0). `sourceOutboxID`는 이번 태스크에서 아직 소비되지 않지만 시그니처에 넣어 Task 5가 바로 쓸 수 있게 한다.

```go
func processExecutionEvent(
	event matching.ExecutionEvent,
	transactionalOutboxID uint64,
	sourceOutboxID uint64,
	settler tradeSettler,
	failureRecorder settlementFailureRecorder,
	marketCompleter marketOrderCompleter,
	completionFailureRecorder marketCompletionFailureRecorder,
	cancelProcessor orderCancellationProcessor,
	broadcast func(coinSymbol string, payload []byte),
	logger *log.Logger,
) (handled bool, markedInTx bool) {
	if event.Trade != nil {
		return processTradeSettlement(event.Trade, transactionalOutboxID, settler, failureRecorder, broadcast, logger)
	}
	if event.MarketOrderDone != nil {
		return processMarketOrderDone(event.MarketOrderDone, marketCompleter, completionFailureRecorder, logger), false
	}
	if event.OrderCancelled != nil {
		return processOrderCancellationEvent(event.OrderCancelled, cancelProcessor, logger), false
	}
	return true, false
}
```

`processSingleOutboxEvent`의 호출을 `processExecutionEvent(outboxEvent.Event, outboxEvent.OutboxID, outboxEvent.OutboxID, ...)`로 바꾼다(live 경로는 두 ID가 동일).

- [x] **Step 5: 배선 수정**

`cmd/main.go:135-138`:

```go
		// 리플레이는 transactionalOutboxID=0으로 호출해 트랜잭션 흡수 마킹을 끄고,
		// 리플레이어가 직접 MarkProcessed한다(부팅 경로라 성능 무관, 순차 처리 로직을
		// 단순하게 유지). sourceOutboxID는 실제 행 ID를 그대로 넘겨 실패 기록의
		// provenance로 쓴다.
		Process: func(sourceOutboxID uint64, event matching.ExecutionEvent) bool {
			handled, _ := processExecutionEvent(event, 0, sourceOutboxID, settlementService, failedSettlementService, orderService, failedMarketCompletionService, orderService, broadcast, log.Default())
			return handled
		},
```

- [x] **Step 6: 테스트 통과 확인**

Run: `go test ./internal/service/ -run TestReplay -v`
Expected: PASS (4 tests + 기존 테스트)

Run: `go build ./... && go vet ./...`
Expected: 오류 없음

메모: 계획에는 없던 사전 존재 통합 테스트 위생 버그를 검증 과정에서 발견·수정했다
(`internal/service/order_cancellation_integration_test.go`의
`TestIntegrationCancelDuringInFlightPartialFillProducesNoFailedSettlements`가 실제
`OutboxWriter`로 커밋한 `ORDER_CANCELLED` outbox 행을 PROCESSED로 마킹하지 않고 끝나
공유 테스트 DB에 PENDING 행이 영구히 남던 문제). 옛 관대한 리플레이어는 이를
조용히 허용했지만 이번 fail-closed 계약 하에서 이후 `OutboxReplayer` 통합 테스트를
실패시켜, `t.Cleanup`으로 해당 행을 직접 삭제하도록 수정했다. `go test
./internal/repository/... ./internal/service/... -count=1`(DSN 설정, 완전히 새로
초기화한 DB)를 연속 2회 실행해 PENDING 잔재 없이 통과함을 확인했다.

- [x] **Step 7: 커밋**

`commit-message` 스킬 사용 후:

```bash
git add internal/service/outbox_replayer.go internal/service/outbox_replayer_test.go cmd/main.go
```

---

## Task 3: market completion `EnsureDeferred`와 retry count 0

dependency 차단이 retry budget을 소비하지 않도록 저장소 API를 두 의미로 분리하고, `retry_count = 0`을 허용한다.

**Files:**
- Modify: `internal/model/failed_market_completion.go:21`
- Modify: `internal/repository/failed_market_completion_repository.go`
- Modify: `internal/service/failed_market_completion_service.go`
- Create: `migrations/005_terminal_durable_defer.sql`
- Test: `internal/repository/failed_market_completion_repository_integration_test.go`

**Interfaces:**
- Consumes: 없음
- Produces:
  - `func (r *FailedMarketCompletionRepository) EnsureDeferred(failure *model.FailedMarketCompletion) (*model.FailedMarketCompletion, error)`
  - `func (s *FailedMarketCompletionService) EnsureDeferred(input CompleteMarketOrderInput, coinSymbol string, reason error) (*model.FailedMarketCompletion, error)`

- [x] **Step 1: 실패 테스트 작성**

`internal/repository/failed_market_completion_repository_integration_test.go`에 추가:

```go
func TestIntegrationFailedMarketCompletionEnsureDeferredDoesNotConsumeRetryBudget(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)
	repo := repository.NewFailedMarketCompletionRepository(db)

	deferred := failedMarketCompletionFixture(9001, "blocked by open failed settlement")
	deferred.RetryCount = 0

	first, err := repo.EnsureDeferred(deferred)
	require.NoError(t, err)
	assert.Equal(t, uint(0), first.RetryCount, "차단은 시도가 아니므로 0에서 시작한다")

	// 같은 주문에 반복 호출해도 증가하지 않는다(crash replay 멱등).
	again := *deferred
	again.ID = 0
	second, err := repo.EnsureDeferred(&again)
	require.NoError(t, err)
	assert.Equal(t, uint(0), second.RetryCount)

	// 실제 실행 실패는 RecordFailure로 0 → 1.
	actual := *deferred
	actual.ID = 0
	actual.ErrorMessage = "completion actually failed"
	third, err := repo.RecordFailure(&actual)
	require.NoError(t, err)
	assert.Equal(t, uint(1), third.RetryCount)
}

func TestIntegrationFailedMarketCompletionEnsureDeferredDoesNotReopenResolved(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)
	repo := repository.NewFailedMarketCompletionRepository(db)

	failure := failedMarketCompletionFixture(9002, "completion failed")
	persisted, err := repo.RecordFailure(failure)
	require.NoError(t, err)
	require.NoError(t, repo.MarkResolved(persisted.ID, "resolved by retry worker"))

	reopen := *failure
	reopen.ID = 0
	got, err := repo.EnsureDeferred(&reopen)
	require.NoError(t, err)

	assert.Equal(t, model.FailedSettlementStatusResolved, got.Status,
		"이미 실행된 terminal을 재실행 대상으로 되살리면 안 된다")
	assert.Equal(t, uint(1), got.RetryCount)
}
```

- [x] **Step 2: 테스트 실패 확인**

Run: `GOEXCHANGE_TEST_DATABASE_DSN=<dsn> go test ./internal/repository/ -run TestIntegrationFailedMarketCompletionEnsureDeferred -v`
Expected: FAIL — `repo.EnsureDeferred undefined`

- [x] **Step 3: 모델의 default 태그를 0으로 변경**

`internal/model/failed_market_completion.go:21`:

```go
	// dependency 차단으로 생성된 record는 시도 0회이므로 0에서 시작한다.
	// GORM에 default:1이 남아 있으면 Go의 0이 zero value로 INSERT에서 생략돼
	// DB default가 적용된다 — 모델과 DB를 모두 0으로 맞춰야 한다.
	RetryCount           uint                   `gorm:"not null;default:0;check:ck_failed_market_completions_retry_count_non_negative,retry_count >= 0"`
```

- [x] **Step 4: 마이그레이션 작성**

`migrations/005_terminal_durable_defer.sql`:

```sql
-- +goose Up
-- dependency 차단으로 생성되는 defer record는 실행 시도가 0회이므로 retry_count = 0에서
-- 시작해야 한다. 1에서 시작하면 SettlementRetryWorker의 RetryCount >= MaxRetryCount 검사에
-- 걸려 실제 시도 기회가 하나 줄어든다 — "차단은 retry budget을 소비하지 않는다"는 계약이
-- 저장소 계층에서 깨진다.
-- gorm AutoMigrate는 기존 CHECK를 갱신하지 않으므로 여기서 명시적으로 적용한다.

ALTER TABLE failed_market_completions
    ALTER COLUMN retry_count SET DEFAULT 0;

ALTER TABLE failed_market_completions
    DROP CONSTRAINT IF EXISTS ck_failed_market_completions_retry_count_positive;

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'failed_market_completions'::regclass
          AND conname = 'ck_failed_market_completions_retry_count_non_negative'
    ) THEN
        ALTER TABLE failed_market_completions
            ADD CONSTRAINT ck_failed_market_completions_retry_count_non_negative
            CHECK (retry_count >= 0);
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
-- no-op: retry_count = 0인 행이 존재하면 CHECK를 다시 좁힐 수 없다.
-- 001_constraints.sql의 방침대로 안전한 Down만 제공한다.
SELECT 1;
```

- [x] **Step 5: repository `EnsureDeferred` 구현**

`internal/repository/failed_market_completion_repository.go`에 추가:

```go
// EnsureDeferred는 terminal을 실행하지 않고 내구적으로 미룰 때 쓴다.
// RecordFailure와 달리 ON CONFLICT DO NOTHING 의미론이라 기존 행의 status·
// resolved_at·retry_count를 건드리지 않는다 — 특히 RESOLVED를 OPEN으로 되돌리지
// 않는다(이미 실행된 terminal을 재실행 대상으로 되살리게 된다).
func (r *FailedMarketCompletionRepository) EnsureDeferred(failure *model.FailedMarketCompletion) (*model.FailedMarketCompletion, error) {
	if failure == nil {
		return nil, fmt.Errorf("failed market completion is required")
	}
	if r == nil || r.DB == nil {
		return nil, fmt.Errorf("failed market completion repository DB is required")
	}

	if err := r.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "order_id"}},
		DoNothing: true,
	}).Create(failure).Error; err != nil {
		return nil, err
	}

	// DoNothing은 기존 행을 돌려주지 않으므로 조회가 필요하다.
	var persisted model.FailedMarketCompletion
	if err := r.DB.Where("order_id = ?", failure.OrderID).First(&persisted).Error; err != nil {
		return nil, err
	}
	return &persisted, nil
}
```

- [x] **Step 6: service `EnsureDeferred` 구현**

`internal/service/failed_market_completion_service.go`에 추가(기존 `RecordFailure`가 만드는 모델 조립 로직을 헬퍼로 공유하고, `EnsureDeferred`만 `RetryCount: 0`으로 둔다):

```go
// EnsureDeferred는 dependency 차단으로 terminal을 실행하지 않았을 때 쓴다.
// 실행 시도가 없었으므로 retry count를 소비하지 않는다.
func (s *FailedMarketCompletionService) EnsureDeferred(input CompleteMarketOrderInput, coinSymbol string, reason error) (*model.FailedMarketCompletion, error) {
	if s == nil || s.Repository == nil {
		return nil, fmt.Errorf("failed market completion repository is required")
	}
	failure, err := failedMarketCompletionFrom(input, coinSymbol, reason, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	failure.RetryCount = 0
	return s.Repository.EnsureDeferred(failure)
}
```

`failedMarketCompletionRepository` 서비스 인터페이스에 `EnsureDeferred(failure *model.FailedMarketCompletion) (*model.FailedMarketCompletion, error)`를 추가한다.

- [x] **Step 7: 테스트 통과 확인**

Run: `GOEXCHANGE_TEST_DATABASE_DSN=<dsn> go test ./internal/repository/ -run TestIntegrationFailedMarketCompletion -v`
Expected: PASS — 기존 `retry_count` 1→2 테스트도 그대로 통과(실제 실패 경로는 의미가 바뀌지 않았다)

Run: `go test ./... -race`
Expected: PASS

메모: 마이그레이션을 로컬 docker-compose 테스트 postgres(실제 DB)에 적용해
`005_terminal_durable_defer.sql` 성공(goose: successfully migrated to version 5)을
확인했다. 계획의 테스트 코드는 `failedMarketCompletionFixture`를 고정 주문 ID
(9001/9002)로 호출하지만, 이 파일의 기존 테스트(`TestIntegrationFailedMarketCompletionRecordFindResolve`)가
이미 `time.Now().UnixNano()` 기반 동적 orderID + `t.Cleanup` 삭제 패턴을 쓰고 있어
(order_id에 uniqueIndex가 걸려 있어 고정값은 반복 실행 시 공유 테스트 DB에 잔재를
남긴다 — AC-Task2에서 겪은 것과 같은 문제), 새 테스트 2개도 동일하게 동적 ID +
cleanup으로 작성했다. `go test ./internal/repository/... ./internal/service/... -count=1`
(DSN 설정)과 `go test ./... -race`(DSN 미설정, 통합 테스트 스킵) 모두 통과했다.

- [x] **Step 8: 커밋**

`commit-message` 스킬 사용 후:

```bash
git add internal/model/failed_market_completion.go internal/repository/failed_market_completion_repository.go internal/repository/failed_market_completion_repository_integration_test.go internal/service/failed_market_completion_service.go migrations/005_terminal_durable_defer.sql
```

---

## Task 4: cancellation durable retry subsystem

취소 terminal의 내구 기록 저장소와 온라인 복구 소비자를 함께 만든다.

**Files:**
- Create: `internal/model/failed_order_cancellation.go`
- Create: `internal/repository/failed_order_cancellation_repository.go`
- Create: `internal/service/failed_order_cancellation_service.go`
- Modify: `migrations/005_terminal_durable_defer.sql` (신규 테이블 제약 추가)
- Modify: `cmd/main.go:53` (`AutoMigrate` 목록), `internal/testdb/integration.go:31`
- Modify: `internal/service/settlement_retry_worker.go`
- Test: `internal/repository/failed_order_cancellation_repository_integration_test.go`, `internal/service/settlement_retry_worker_test.go`

**Interfaces:**
- Consumes: Task 3의 `EnsureDeferred` 패턴, B의 `HasOpenFailureForOrder(orderID uint) (bool, error)`
- Produces:
  - `model.FailedOrderCancellation`
  - `func (s *FailedOrderCancellationService) RecordFailure(cancelled matching.OrderCancelled, sourceOutboxID uint64, executionErr error) (*model.FailedOrderCancellation, error)`
  - `func (s *FailedOrderCancellationService) EnsureDeferred(cancelled matching.OrderCancelled, sourceOutboxID uint64, reason error) (*model.FailedOrderCancellation, error)`
  - `func (s *FailedOrderCancellationService) ListOpenFailures(limit int) ([]model.FailedOrderCancellation, error)`
  - `func (s *FailedOrderCancellationService) ResolveFailure(id uint, resolution string) error`
  - 워커가 보는 인터페이스(`retryFailedCompletionStore`와 동형, `internal/service/settlement_retry_worker.go`):

```go
type retryOrderCancellationProcessor interface {
	ProcessOrderCancellation(event matching.OrderCancelled) error
}

type retryFailedCancellationStore interface {
	ListOpenFailures(limit int) ([]model.FailedOrderCancellation, error)
	ResolveFailure(id uint, resolution string) error
	RecordFailure(cancelled matching.OrderCancelled, sourceOutboxID uint64, executionErr error) (*model.FailedOrderCancellation, error)
	EnsureDeferred(cancelled matching.OrderCancelled, sourceOutboxID uint64, reason error) (*model.FailedOrderCancellation, error)
}
```

  - `SettlementRetryWorker`에 필드 2개 추가: `CancelProcessor retryOrderCancellationProcessor`, `FailedCancellations retryFailedCancellationStore`
  - `func orderCancelledFromFailure(failure *model.FailedOrderCancellation) matching.OrderCancelled` — 저장된 필드에서 이벤트를 복원한다(`tradeFromFailedSettlement`와 같은 역할)

- [x] **Step 1: 모델 실패 테스트 작성**

`internal/repository/failed_order_cancellation_repository_integration_test.go` 생성:

```go
func TestIntegrationFailedOrderCancellationEnsureDeferredAndRecordFailure(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)
	repo := repository.NewFailedOrderCancellationRepository(db)

	deferred := &model.FailedOrderCancellation{
		OrderID:       7101,
		OutboxEventID: 5501,
		CoinSymbol:    "BTC",
		Side:          model.OrderSideBuy,
		EngineEventID: "evt-7101",
		ErrorMessage:  "blocked by open failed settlement",
		Status:        model.FailedSettlementStatusOpen,
		RetryCount:    0,
		OccurredAt:    time.Now().UTC(),
	}

	first, err := repo.EnsureDeferred(deferred)
	require.NoError(t, err)
	assert.Equal(t, uint(0), first.RetryCount)
	assert.Equal(t, uint64(5501), first.OutboxEventID)

	again := *deferred
	again.ID = 0
	second, err := repo.EnsureDeferred(&again)
	require.NoError(t, err)
	assert.Equal(t, uint(0), second.RetryCount, "차단 반복은 budget을 소비하지 않는다")

	actual := *deferred
	actual.ID = 0
	actual.ErrorMessage = "cancellation actually failed"
	third, err := repo.RecordFailure(&actual)
	require.NoError(t, err)
	assert.Equal(t, uint(1), third.RetryCount)

	fourth, err := repo.RecordFailure(&actual)
	require.NoError(t, err)
	assert.Equal(t, uint(2), fourth.RetryCount)
}
```

- [x] **Step 2: 테스트 실패 확인**

Run: `GOEXCHANGE_TEST_DATABASE_DSN=<dsn> go test ./internal/repository/ -run TestIntegrationFailedOrderCancellation -v`
Expected: FAIL — `model.FailedOrderCancellation undefined`

- [x] **Step 3: 모델 작성**

`internal/model/failed_order_cancellation.go`:

```go
package model

import "time"

// FailedOrderCancellation은 취소 terminal을 실행하지 못했을 때 남기는 내구 기록입니다.
// 진실의 원본은 outbox이며, 이 테이블은 온라인 복구를 위한 retry index입니다 —
// 기록이 커밋되면 원본 outbox는 PROCESSED로 닫히고(durable handoff) 이후 복구는
// SettlementRetryWorker가 담당합니다.
// OrderCancelled 이벤트는 엔진 메모리에만 존재하므로 여기 저장된 필드만으로
// 취소를 재시도할 수 있어야 합니다.
type FailedOrderCancellation struct {
	ID uint `gorm:"primaryKey"`
	// 주문당 record 1건으로 수렴시킨다 — replay·재시도가 같은 행을 멱등 재사용한다.
	OrderID uint `gorm:"not null;uniqueIndex:idx_failed_order_cancellations_order_id"`
	// 원본 추적·감사·1:1 연결용 provenance. 복구 시 마킹 키가 아니다.
	OutboxEventID uint64                 `gorm:"not null"`
	CoinSymbol    string                 `gorm:"not null"`
	Side          OrderSide              `gorm:"not null"`
	EngineEventID string                 `gorm:"type:text"`
	ErrorMessage  string                 `gorm:"type:text;not null;check:ck_failed_order_cancellations_error_message_not_empty,length(btrim(error_message)) > 0"`
	Status        FailedSettlementStatus `gorm:"not null;default:OPEN;check:ck_failed_order_cancellations_status_valid,status IN ('OPEN', 'RESOLVED')"`
	// dependency 차단으로 생성되면 0(실행 시도 없음), 실제 실패면 1부터.
	RetryCount uint      `gorm:"not null;default:0;check:ck_failed_order_cancellations_retry_count_non_negative,retry_count >= 0"`
	OccurredAt time.Time `gorm:"not null"`
	Resolution string    `gorm:"type:text"`
	ResolvedAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}
```

- [x] **Step 4: repository 작성**

`internal/repository/failed_order_cancellation_repository.go` — `FailedMarketCompletionRepository`와 **동형**으로 `RecordFailure`(`DoUpdates` + `retry_count + 1`), `EnsureDeferred`(`DoNothing` + 조회), `FindOpen`(`occurred_at ASC, id ASC`, `NormalizeFailedSettlementListLimit`), `MarkResolved`(`status = OPEN`인 행만, `RowsAffected == 0`이면 오류)를 구현한다. 충돌 컬럼은 `order_id`.

```go
func (r *FailedOrderCancellationRepository) RecordFailure(failure *model.FailedOrderCancellation) (*model.FailedOrderCancellation, error) {
	if failure == nil {
		return nil, fmt.Errorf("failed order cancellation is required")
	}
	if r == nil || r.DB == nil {
		return nil, fmt.Errorf("failed order cancellation repository DB is required")
	}

	now := time.Now().UTC()
	if err := r.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "order_id"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"error_message": failure.ErrorMessage,
			"status":        model.FailedSettlementStatusOpen,
			"retry_count":   gorm.Expr("failed_order_cancellations.retry_count + ?", 1),
			"resolution":    "",
			"resolved_at":   nil,
			"updated_at":    now,
		}),
	}).Create(failure).Error; err != nil {
		return nil, err
	}

	var persisted model.FailedOrderCancellation
	if err := r.DB.Where("order_id = ?", failure.OrderID).First(&persisted).Error; err != nil {
		return nil, err
	}
	return &persisted, nil
}

// EnsureDeferred는 실행하지 않고 미룰 때 쓴다 — 기존 행의 status·retry_count를
// 건드리지 않는다(RESOLVED를 OPEN으로 되돌리지 않는다).
func (r *FailedOrderCancellationRepository) EnsureDeferred(failure *model.FailedOrderCancellation) (*model.FailedOrderCancellation, error) {
	if failure == nil {
		return nil, fmt.Errorf("failed order cancellation is required")
	}
	if r == nil || r.DB == nil {
		return nil, fmt.Errorf("failed order cancellation repository DB is required")
	}

	if err := r.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "order_id"}},
		DoNothing: true,
	}).Create(failure).Error; err != nil {
		return nil, err
	}

	var persisted model.FailedOrderCancellation
	if err := r.DB.Where("order_id = ?", failure.OrderID).First(&persisted).Error; err != nil {
		return nil, err
	}
	return &persisted, nil
}
```

- [x] **Step 5: 마이그레이션·AutoMigrate 배선**

`migrations/005_terminal_durable_defer.sql`의 `Up` 끝에 신규 테이블 제약을 **`IF NOT EXISTS` 가드로** 추가한다(테이블 자체는 `AutoMigrate`가 만든다 — 001의 방침과 동일):

```sql
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_class WHERE relname = 'failed_order_cancellations') THEN
        ALTER TABLE failed_order_cancellations
            ALTER COLUMN retry_count SET DEFAULT 0;
    END IF;
END $$;
-- +goose StatementEnd
```

`cmd/main.go:53`의 `AutoMigrate` 목록과 `internal/testdb/integration.go:31`에 `&model.FailedOrderCancellation{}`를 추가한다.

- [x] **Step 6: service 작성**

`internal/service/failed_order_cancellation_service.go` — `matching.OrderCancelled`(`OrderID`/`CoinSymbol`/`Side`/`EngineEventID`, engine.go:112-117)에서 모델을 조립하는 헬퍼를 공유하고, `RecordFailure`는 `RetryCount: 1`, `EnsureDeferred`는 `RetryCount: 0`으로 둔다. 오류 메시지는 `settlementErrorMessage`와 동일하게 길이를 자르고 빈 문자열을 대체한다(CHECK 제약 위반 방지).

- [x] **Step 7: worker cancel phase 실패 테스트 작성**

`internal/service/settlement_retry_worker_test.go`에 추가:

```go
func TestRetryWorkerCancelPhaseRespectsDependencyAndRetryBudget(t *testing.T) {
	tests := []struct {
		name           string
		hasOpen        bool
		depErr         error
		cancelErr      error
		wantCancelled  bool
		wantRecordKind string // "" | "record" — 차단은 아무것도 기록하지 않는다
	}{
		{name: "OPEN dependency면 취소를 실행하지 않는다", hasOpen: true, wantCancelled: false},
		{name: "dependency 없으면 실행하고 resolve한다", wantCancelled: true},
		{name: "dependency 조회 오류는 fail-closed", depErr: errors.New("boom"), wantCancelled: false},
		{name: "실행 실패는 RecordFailure로 count 증가", cancelErr: errors.New("boom"), wantCancelled: true, wantRecordKind: "record"},
	}
	// ... 각 케이스에서 fake store의 recordCalls / ensureCalls / resolved를 검증한다.
	// 차단 케이스는 recordCalls == 0 && ensureCalls == 0 (worker는 이미 OPEN인 행을
	// 다시 기록하지 않는다) 를 고정한다.
}

func TestRetryWorkerCancelPhaseFailsClosedWithoutDependencyStore(t *testing.T) {
	w := &service.SettlementRetryWorker{
		CancelProcessor:     &fakeCancelProcessor{},
		FailedCancellations: &fakeFailedCancellationStore{open: []model.FailedOrderCancellation{{OrderID: 1}}},
		FailedSettlements:   nil, // dependency 확인 수단 없음
	}
	w.RunOnce()
	assert.Zero(t, w.CancelProcessor.(*fakeCancelProcessor).calls)
}
```

- [x] **Step 8: worker cancel phase 구현**

`internal/service/settlement_retry_worker.go` — `retryFailedCompletions()`와 **동일한 구조**로 `retryFailedCancellations()`를 추가하고 `RunOnce()`에 세 번째 phase로 붙인다:

```go
func (w *SettlementRetryWorker) RunOnce() {
	w.retryFailedSettlements()
	w.retryFailedCompletions()
	w.retryFailedCancellations()
}
```

```go
func (w *SettlementRetryWorker) retryFailedCancellations() {
	if w.CancelProcessor == nil || w.FailedCancellations == nil {
		return
	}
	// fail-closed: dependency를 확인할 수단이 없으면 terminal을 실행하지 않는다.
	if w.FailedSettlements == nil {
		w.logf("retry worker: dependency store unavailable, skipping cancellation phase")
		return
	}

	failures, err := w.FailedCancellations.ListOpenFailures(settlementRetryBatchLimit)
	if err != nil {
		w.logf("retry worker: list open failed order cancellations failed: %v", err)
		return
	}

	for i := range failures {
		failure := &failures[i]
		if failure.RetryCount >= w.maxRetryCount() {
			continue
		}

		hasOpen, depErr := w.FailedSettlements.HasOpenFailureForOrder(failure.OrderID)
		if depErr != nil {
			w.logf("retry worker: dependency check failed for order %d: %v", failure.OrderID, depErr)
			return
		}
		if hasOpen {
			metrics.SettlementCompletionBlockedTotal.Inc()
			continue // 차단은 정상 동작이라 로그를 남기지 않는다
		}

		cancelled := orderCancelledFromFailure(failure)
		if err := w.CancelProcessor.ProcessOrderCancellation(cancelled); err != nil {
			if _, recordErr := w.FailedCancellations.RecordFailure(cancelled, failure.OutboxEventID, err); recordErr != nil {
				w.logf("retry worker: record failed order cancellation failed: %v", recordErr)
			}
			w.logf("retry worker: process order cancellation %d failed: %v", failure.OrderID, err)
			continue
		}

		if err := w.FailedCancellations.ResolveFailure(failure.ID, "auto-retry: order cancellation succeeded"); err != nil {
			w.logf("retry worker: resolve failed order cancellation %d failed: %v", failure.ID, err)
		}
	}
}
```

- [x] **Step 9: 테스트 통과 확인**

Run: `go test ./internal/service/ -run TestRetryWorker -v`
Expected: PASS

Run: `GOEXCHANGE_TEST_DATABASE_DSN=<dsn> go test ./internal/repository/ -run TestIntegrationFailedOrderCancellation -v`
Expected: PASS

메모: 계획의 파일 목록에는 없었지만 `cmd/main.go`도 함께 수정했다 — Step 8이
`SettlementRetryWorker`에 `CancelProcessor`/`FailedCancellations` 필드를
추가했는데, `main()`에서 실제 구현체를 채워 넣지 않으면 이번에 만든 취소
재시도 phase 전체가 프로덕션에서 항상 조용히 no-op이 된다(두 필드 중 하나라도
nil이면 `retryFailedCancellations()`가 즉시 return). 기존
`MarketCompleter`/`FailedCompletions` 배선과 동일한 패턴으로
`NewFailedOrderCancellationService`를 생성해 연결했다. 또한 Step 1의 테스트
스니펫에는 없지만 `FindOpen`/`MarkResolved` 경로가 미검증 상태로 남는 것을
막기 위해 `TestIntegrationFailedOrderCancellationFindOpenAndResolve`를
추가했다(FailedMarketCompletion 쪽은 이전 사이클의 사전 존재 테스트가 이미
커버하고 있었지만, 이 테이블은 이번에 신설돼 그런 커버리지가 없었다).
`go test ./internal/repository/... ./internal/service/... -count=1`(DSN
설정)과 `go test ./... -race`(DSN 미설정) 모두 통과했고, 마이그레이션을 실제
로컬 postgres에 적용해 `\d failed_order_cancellations`로 CHECK 제약·default
값을 직접 확인했다.

- [x] **Step 10: 커밋**

`commit-message` 스킬 사용 후 신규·수정 파일을 모두 스테이지.

---

## Task 5: 공통 terminal 실행·defer 정책

live와 replay가 **같은 guard·같은 defer 경로**를 쓰게 만든다.

**Files:**
- Modify: `cmd/main.go` (`processMarketOrderDone`, `processOrderCancellationEvent`, `processExecutionEvent`)
- Modify: `internal/metrics/metrics.go`
- Test: `cmd/terminal_defer_test.go` (신규)

**Interfaces:**
- Consumes: Task 2의 `sourceOutboxID`, Task 3/4의 `EnsureDeferred`, B의 `HasOpenFailureForOrder`
- Produces:
  - `metrics.SettlementTerminalDeferRecordFailed` (`CounterVec{kind}`), `metrics.SettlementTerminalDeferred` (`CounterVec{kind,reason}`)
  - `processMarketOrderDone` / `processOrderCancellationEvent`가 dependency guard + durable defer를 수행하고 `handled bool`을 반환
  - `cmd/main.go`에 새 인터페이스 2개(기존 `orderCancellationProcessor` 등과 같은 자리에 선언):

```go
// B의 HasOpenFailureForOrder를 live·replay 양쪽에서 공유한다.
type settlementDependencyGuard interface {
	HasOpenFailureForOrder(orderID uint) (bool, error)
}

type cancellationDeferStore interface {
	RecordFailure(cancelled matching.OrderCancelled, sourceOutboxID uint64, executionErr error) (*model.FailedOrderCancellation, error)
	EnsureDeferred(cancelled matching.OrderCancelled, sourceOutboxID uint64, reason error) (*model.FailedOrderCancellation, error)
}

// 차단 사유를 기록에 남기기 위한 고정 오류(실행 실패와 구분된다).
var errDependencyOpen = errors.New("terminal deferred: preceding settlement is still OPEN")
```

  기존 `marketCompletionFailureRecorder`에는 `EnsureDeferred(input service.CompleteMarketOrderInput, coinSymbol string, reason error) (*model.FailedMarketCompletion, error)`를 추가한다.
  `dependencyBlocked(guard, orderID)`는 `guard == nil`이면 `(false, errNoDependencyGuard)`를 돌려주는 얇은 헬퍼다 — **guard가 없으면 실행하지 않는다**(fail-closed).

- [x] **Step 1: 실패 테스트 작성**

`cmd/terminal_defer_test.go`:

```go
func TestProcessOrderCancellationDefersWhenDependencyOpen(t *testing.T) {
	guard := &fakeDependencyGuard{hasOpen: true}
	store := &fakeCancellationDeferStore{}
	processor := &fakeCancelProcessor{}

	handled := processOrderCancellationEvent(
		&matching.OrderCancelled{OrderID: 42, CoinSymbol: "BTC"},
		77, // sourceOutboxID
		processor, guard, store, testLogger(),
	)

	assert.True(t, handled, "내구 인계에 성공하면 outbox를 PROCESSED로 닫는다")
	assert.Zero(t, processor.calls, "선행 정산이 미해결이면 취소를 실행하지 않는다")
	assert.Equal(t, 1, store.ensureCalls)
	assert.Zero(t, store.recordCalls, "차단은 실행 실패가 아니다")
}

func TestProcessOrderCancellationRecordsFailureWhenExecutionFails(t *testing.T) {
	guard := &fakeDependencyGuard{}
	store := &fakeCancellationDeferStore{}
	processor := &fakeCancelProcessor{err: errors.New("boom")}

	handled := processOrderCancellationEvent(
		&matching.OrderCancelled{OrderID: 42, CoinSymbol: "BTC"},
		77, processor, guard, store, testLogger(),
	)

	assert.True(t, handled)
	assert.Equal(t, 1, store.recordCalls, "실제 실행 실패는 RecordFailure다")
	assert.Zero(t, store.ensureCalls)
}

func TestProcessOrderCancellationLeavesPendingWhenRecordFails() // handled=false
func TestProcessOrderCancellationFailsClosedOnGuardError()      // 실행 금지 + defer 시도
func TestProcessMarketOrderDoneDefersWhenDependencyOpen()       // market done 동일 계약
```

- [x] **Step 2: 테스트 실패 확인**

Run: `go test ./cmd/ -run TestProcessOrderCancellation -v`
Expected: FAIL — 인자 개수 불일치 컴파일 오류

- [x] **Step 3: 메트릭 추가**

`internal/metrics/metrics.go`에 추가:

```go
	// terminal이 실행되지 않고 내구 defer된 횟수. reason=dependency_open|quarantine.
	SettlementTerminalDeferred = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "settlement_terminal_deferred_total",
		Help: "Terminal events durably deferred instead of executed.",
	}, []string{"kind", "reason"})

	// defer 기록 자체가 최종 실패한 횟수 — 온라인 복구가 부팅 replay로 강등된다.
	// trade의 실패 기록 실패(settlement_dependency_record_failed_total)와는 의미가 다르다.
	SettlementTerminalDeferRecordFailed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "settlement_terminal_defer_record_failed_total",
		Help: "Terminal defer records that could not be persisted (online recovery degraded).",
	}, []string{"kind"})
```

- [x] **Step 4: terminal 처리 구현**

`processOrderCancellationEvent`에 dependency guard와 durable defer를 넣는다. `processMarketOrderDone`도 **같은 순서**(guard → 실행 → 실패 시 `RecordFailure`)로 바꾸되, 차단 시에는 `EnsureDeferred`를 쓴다.

```go
// processOrderCancellationEvent는 OrderCancelled를 확정한다.
// 선행 정산이 미해결(OPEN)이면 실행하지 않고 내구 기록으로 미룬다 — 기록이 커밋되면
// 원본 outbox는 PROCESSED로 닫히고(durable handoff) 온라인 retry worker가 이어받는다.
// 기록조차 실패하면 false를 반환해 outbox를 PENDING으로 남긴다.
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

// retryTransient는 defer 기록에만 쓰는 유한 백오프다 — worker job 안에서 실행되므로
// dispatcher를 막지 않는다. transient가 아니면 즉시 강등한다.
// sleep 합은 유한하지만 wall clock은 DB 호출 시간에 좌우된다(현재 per-call timeout
// 계약이 없다 — 백로그).
func retryTransient(fn func() error) error {
	err := fn()
	for attempt := 0; err != nil && service.IsTransientSettlementError(err) && attempt < len(transientRetryDelays); attempt++ {
		time.Sleep(transientRetryDelays[attempt])
		err = fn()
	}
	return err
}
```

`processExecutionEvent`에서 두 terminal 분기에 `sourceOutboxID`·`guard`·`deferStore`를 전달한다.

- [x] **Step 5: 테스트 통과 확인**

Run: `go test ./cmd/ -race -v`
Expected: PASS

메모: `processExecutionEvent`/`processSingleOutboxEvent`/`settleTradeBatchWithFallback`
시그니처에 `guard`·`cancelDeferStore`를 추가로 꿰어야 두 terminal 분기까지
실제로 전달됐다(계획 문서엔 "processExecutionEvent에서 두 terminal 분기에
전달한다"고만 적혀 있었지만, 그 앞단의 세 함수도 같은 파라미터를 릴레이해야
컴파일이 성립해 기계적으로 확장했다). `processMarketOrderDone`은 사양대로
`sourceOutboxID`를 받지 않는다 — `marketCompletionFailureRecorder.RecordFailure/
EnsureDeferred`가 애초에 그 값을 받는 시그니처가 아니고(Task 3에서 확정),
설계 문서도 "저장하지 않고 provenance로만 쓴다"고 명시했으므로 쓸 곳 없는
파라미터를 새로 추가하지 않았다. 계획에는 없던 `main()`의 실제 배선(guard=
`failedSettlementService`, cancelDeferStore=`failedOrderCancellationService`)도
이 Step에서 함께 했다 — 안 그러면 새 defer 경로가 프로덕션에서 죽은 코드로
남는다. `go test ./cmd/ -race -v`(45개 테스트, 신규 5개 포함) 전부 PASS,
`go test ./... -count=1 -race` 전체도 PASS.

- [x] **Step 6: 커밋** (`commit-message` 스킬)

---

## Task 6: dispatcher dependency 상태 기계

select loop와 분리된 **순수 상태 기계**를 먼저 만들고 단위 테스트로 전이를 고정한다.

**Files:**
- Create: `cmd/settlement_dependency.go`, `cmd/settlement_dependency_test.go`

**Interfaces:**
- Consumes: Task 1의 `settlementResult.undurableOrderIDs`
- Produces:

```go
type dependencyTracker struct { /* 비공개 필드 */ }

func newDependencyTracker() *dependencyTracker
func (d *dependencyTracker) touchedOrderIDs(batch []service.OutboxEvent) []uint // dedup
func (d *dependencyTracker) register(jobID uint64, orders []uint)               // 송신 성공 후에만
func (d *dependencyTracker) retire(jobID uint64, undurable []uint) error        // quarantine → count 감소
func (d *dependencyTracker) ready(orderID uint) bool                            // inFlight count == 0
func (d *dependencyTracker) quarantined(orderID uint) bool
func (d *dependencyTracker) clearQuarantine(orderID uint)
func (d *dependencyTracker) outstanding() int
func (d *dependencyTracker) quarantinedCount() int
```

- [x] **Step 1: 실패 테스트 작성**

`cmd/settlement_dependency_test.go`:

```go
func TestTouchedOrderIDsDedupsWithinBatch(t *testing.T) {
	d := newDependencyTracker()
	batch := []service.OutboxEvent{
		tradeOutboxEvent(1, 10, 20),
		tradeOutboxEvent(2, 10, 30), // 10이 두 배치에 등장
		tradeOutboxEvent(3, 40, 40), // 자기거래: maker == taker
	}
	assert.ElementsMatch(t, []uint{10, 20, 30, 40}, d.touchedOrderIDs(batch))
}

func TestRegisterAndRetireCountsOncePerBatch(t *testing.T) {
	d := newDependencyTracker()
	d.register(1, []uint{10, 20})
	d.register(2, []uint{10})

	assert.Equal(t, 2, d.outstanding())
	assert.False(t, d.ready(10), "두 배치가 10을 건드렸다")
	assert.False(t, d.ready(20))

	require.NoError(t, d.retire(1, nil))
	assert.True(t, d.ready(20), "20은 이제 대기할 배치가 없다")
	assert.False(t, d.ready(10))

	require.NoError(t, d.retire(2, nil))
	assert.True(t, d.ready(10))
	assert.Equal(t, 0, d.outstanding())
}

func TestRetireOnFailureStillReleasesSlots(t *testing.T) {
	d := newDependencyTracker()
	d.register(1, []uint{10})

	// undurable이 있어도 자원은 정상 retire된다 — 아니면 슬롯이 영구 점유된다.
	require.NoError(t, d.retire(1, []uint{10}))

	assert.Equal(t, 0, d.outstanding())
	assert.True(t, d.ready(10))
	assert.True(t, d.quarantined(10), "다만 terminal 실행은 금지된다")
}

func TestRetireUnknownJobIsInvariantViolation(t *testing.T) {
	d := newDependencyTracker()
	assert.Error(t, d.retire(99, nil))
}

func TestQuarantineClearedAfterTerminalConsumed(t *testing.T) {
	d := newDependencyTracker()
	d.register(1, []uint{10})
	require.NoError(t, d.retire(1, []uint{10}))

	require.True(t, d.quarantined(10))
	assert.Equal(t, 1, d.quarantinedCount())

	d.clearQuarantine(10)
	assert.False(t, d.quarantined(10))
	assert.Equal(t, 0, d.quarantinedCount())
}

func TestReadyIsTrueForUntouchedOrder(t *testing.T) {
	d := newDependencyTracker()
	assert.True(t, d.ready(999), "건드린 배치가 없으면 즉시 dispatch 가능하다")
}
```

- [x] **Step 2: 테스트 실패 확인**

Run: `go test ./cmd/ -run TestTouchedOrderIDs -v`
Expected: FAIL — `newDependencyTracker undefined`

- [x] **Step 3: 구현**

`cmd/settlement_dependency.go`:

```go
package main

import (
	"fmt"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
)

// dependencyTracker는 dispatcher가 단독 소유하는 주문 단위 fence 상태다.
// worker는 이 상태를 읽지도 변경하지도 않는다 — completion은 job ID와 결과만 돌려준다.
// select loop와 분리해 상태 전이를 단위 테스트로 고정하기 위해 별도 타입으로 둔다.
type dependencyTracker struct {
	inFlight     map[uint]int      // orderID → 아직 retire되지 않은 배치 수
	dispatched   map[uint64][]uint // jobID → 그 배치가 건드린 주문(중복 제거됨)
	unsafeOrders map[uint]struct{} // 내구 기록조차 실패해 terminal 실행이 금지된 주문
	jobs         int
}

func newDependencyTracker() *dependencyTracker {
	return &dependencyTracker{
		inFlight:     map[uint]int{},
		dispatched:   map[uint64][]uint{},
		unsafeOrders: map[uint]struct{}{},
	}
}

// touchedOrderIDs는 배치가 건드리는 주문을 중복 없이 돌려준다. 배치당 1회만
// 카운트해야 retire 시 대칭이 맞는다(같은 주문이 배치 안에 여러 번 나와도 1).
func (d *dependencyTracker) touchedOrderIDs(batch []service.OutboxEvent) []uint {
	seen := make(map[uint]struct{}, len(batch)*2)
	orders := make([]uint, 0, len(batch)*2)
	for _, event := range batch {
		if event.Event.Trade == nil {
			continue
		}
		for _, id := range [2]uint{event.Event.Trade.BuyOrderID, event.Event.Trade.SellOrderID} {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			orders = append(orders, id)
		}
	}
	return orders
}

// register는 job 송신이 성공한 직후에만 호출한다 — select의 send case body에서
// 호출하므로 completion 처리가 등록을 앞지를 수 없다.
func (d *dependencyTracker) register(jobID uint64, orders []uint) {
	d.dispatched[jobID] = orders
	d.jobs++
	for _, id := range orders {
		d.inFlight[id]++
	}
}

// retire는 성공·실패 무관하게 자원을 반납한다. retire는 dependency 충족을
// 의미하지 않는다 — undurable로 보고된 주문은 quarantine돼 terminal이 금지된다.
func (d *dependencyTracker) retire(jobID uint64, undurable []uint) error {
	orders, ok := d.dispatched[jobID]
	if !ok {
		return fmt.Errorf("settlement dispatcher: completion for unknown job %d", jobID)
	}
	// quarantine 표시가 count 감소보다 먼저다 — 순서가 바뀌면 terminal이
	// quarantine을 보지 못한 채 ready로 판정될 수 있다.
	for _, id := range undurable {
		d.unsafeOrders[id] = struct{}{}
	}
	delete(d.dispatched, jobID)
	d.jobs--
	for _, id := range orders {
		d.inFlight[id]--
		if d.inFlight[id] <= 0 {
			delete(d.inFlight, id) // 누수 방지
		}
	}
	return nil
}

func (d *dependencyTracker) ready(orderID uint) bool { return d.inFlight[orderID] == 0 }

func (d *dependencyTracker) quarantined(orderID uint) bool {
	_, ok := d.unsafeOrders[orderID]
	return ok
}

func (d *dependencyTracker) clearQuarantine(orderID uint) { delete(d.unsafeOrders, orderID) }

func (d *dependencyTracker) outstanding() int { return d.jobs }

func (d *dependencyTracker) quarantinedCount() int { return len(d.unsafeOrders) }
```

- [x] **Step 4: 테스트 통과 확인**

Run: `go test ./cmd/ -race -run 'TestTouchedOrderIDs|TestRegisterAndRetire|TestRetire|TestQuarantine|TestReady' -v`
Expected: PASS (6 tests)

메모: 계획의 테스트 코드가 `tradeOutboxEvent(1, 10, 20)`(outboxID, buyOrderID,
sellOrderID 3-인자 형태)를 호출하지만, `cmd/main_test.go`에 이미 있는
`tradeOutboxEvent(outboxID uint64, engineSequence int64)`는 시그니처가 다르고
BuyOrderID/SellOrderID를 설정하지 않는다(Task 1에서 겪은 것과 동일한 충돌).
Task 1에서 이미 만들어 둔 `tradeOutboxEventForOrders(outboxID, buyOrderID,
sellOrderID)` 헬퍼가 정확히 이 시그니처이므로 새로 만들지 않고 그대로
재사용했다. `go test ./cmd/ -race -run '...' -v`(6개 전부 PASS), `go test
./... -count=1 -race`(전체) 모두 통과했다. `dependencyTracker`는 아직
production 코드 어디에도 배선되지 않은 순수 상태 기계다(계획대로 — Task 7이
dispatcher에 연결한다).

- [x] **Step 5: 커밋** (`commit-message` 스킬)

---

## Task 7: dispatcher event loop 통합

배리어를 제거하고 terminal을 공용 pool에 dispatch한다.

**Files:**
- Modify: `cmd/settlement_pipeline.go` 전체
- Modify: `cmd/main.go` (worker·dispatcher 배선)
- Modify: `internal/metrics/metrics.go` (배리어 지표 제거, 신규 지표 추가)
- Modify: `internal/metrics/settlement_observability_test.go` (배리어 테스트 제거)
- Test: `cmd/settlement_pipeline_test.go`

**Interfaces:**
- Consumes: Task 6의 `dependencyTracker`, Task 5의 terminal 처리
- Produces:
  - `func runPartitionDispatcher(partition string, queue <-chan service.OutboxEvent, jobs chan<- settlementJob, concurrency int, maxBatch int, broadcast func(string, []byte))`
  - `func runSettlementWorker(jobs <-chan settlementJob, settleBatch func([]service.OutboxEvent, func(string, []byte)) []uint, settleTerminal func(service.OutboxEvent))`
  - `func terminalOrderID(event service.OutboxEvent) (uint, string)`

- [ ] **Step 1: 실패 테스트 작성**

`cmd/settlement_pipeline_test.go`에 추가:

```go
func cancelOutboxEvent(outboxID uint64, orderID uint) service.OutboxEvent {
	return service.OutboxEvent{
		OutboxID: outboxID,
		Event:    matching.ExecutionEvent{OrderCancelled: &matching.OrderCancelled{OrderID: orderID, CoinSymbol: "BTC"}},
	}
}

// 핵심 회귀: 무관한 주문의 terminal이 다른 배치를 막지 않는다(현재는 막는다).
func TestDispatcherDoesNotBlockUnrelatedBatchesOnTerminal(t *testing.T) {
	queue := make(chan service.OutboxEvent, 8)
	jobs := make(chan settlementJob, 8)

	queue <- tradeOutboxEvent(1, 10, 20) // 주문 10·20
	queue <- cancelOutboxEvent(2, 10)    // 주문 10의 terminal → 대기해야 한다
	queue <- tradeOutboxEvent(3, 30, 40) // 무관한 주문 → 계속 진행해야 한다
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

	queue <- tradeOutboxEvent(1, 10, 20)
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

위 두 테스트와 **같은 골격**(queue 채우기 → 고루틴으로 dispatcher 기동 → `jobs`에서 수신 →
`done`으로 completion 회신)으로 나머지 5종을 작성한다. 각각의 단정은 다음과 같다:

| 테스트 | 배치 | 단정 |
|---|---|---|
| `TestDispatcherHoldsTerminalUntilSameOrderBatchesRetire` | trade(10,20) 2건 → cancel(10) | 두 배치가 **모두** retire되기 전에는 terminal job이 `jobs`에 나타나지 않는다 |
| `TestDispatcherRespectsMaxOutstanding` | trade 배치 10건, `concurrency=2` | completion을 보내지 않고 수신되는 job 수가 **4(=2N)를 넘지 않는다** |
| `TestDispatcherGracefulShutdownDrainsWaitingTerminals` | trade(10,20) → cancel(10), queue close | queue가 닫혀도 dispatcher는 **terminal을 dispatch한 뒤에** 반환한다 |
| `TestDispatcherTerminalDoesNotConsumeBroadcastSequence` | trade → cancel → trade, 각 trade가 메시지 1건 | broadcast가 **두 번, 원래 순서대로** 호출되고 terminal 때문에 멈추지 않는다 |
| `TestDispatcherDuplicateTerminalIsInvariantViolation` | cancel(10) 2건 | 두 번째는 waiting에 **추가되지 않고**(terminal job이 1개만 나온다) 첫 번째를 덮어쓰지 않는다 |
```

- [ ] **Step 2: 테스트 실패 확인**

Run: `go test ./cmd/ -run TestDispatcher -v`
Expected: FAIL — 현재 dispatcher는 배리어에서 정지하므로 3번 배치가 dispatch되지 않는다

- [ ] **Step 3: 메트릭 교체**

`internal/metrics/metrics.go`에서 `SettlementBarriersTotal` / `SettlementBarrierWait` / `SettlementBarrierInflight`와 사전 resolve 핸들 6개(`SettlementBarrierMarketDone`·`SettlementBarrierCancel`·`SettlementBarrierWaitDone`·`SettlementBarrierWaitCancel`·`SettlementBarrierInflightDone`·`SettlementBarrierInflightCancel`)를 **삭제**하고 다음을 추가한다. 배리어가 사라졌으므로 같은 이름으로 다른 의미를 잇지 않는다.

```go
	// terminal 도착부터 worker 송신까지 — 배리어 대기의 대체 지표다.
	SettlementTerminalWait = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "settlement_terminal_wait_seconds",
		Help:    "From terminal event arrival to job dispatch (per-order fence wait).",
		Buckets: prometheus.DefBuckets,
	}, []string{"kind"})

	// 현재 outstanding job 수. "2N에 상시 붙어 있는가"를 판정해야 하므로 Gauge다
	// (dispatch 순간의 분포로는 유지 시간을 알 수 없다).
	SettlementOutstandingJobs = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "settlement_outstanding_jobs",
		Help: "Jobs sent to workers but not yet retired by the dispatcher.",
	}, []string{"partition"})

	// 내구 기록조차 실패해 terminal 실행이 금지된 주문 수. 무한 증가 감시용.
	SettlementQuarantinedOrders = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "settlement_quarantined_orders",
		Help: "Orders whose terminal is blocked because a trade failure could not be recorded.",
	}, []string{"partition"})

	// trade 정산 실패의 내구 기록 자체가 실패한 횟수(= quarantine 등록).
	SettlementDependencyRecordFailedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "settlement_dependency_record_failed_total",
		Help: "Trade settlement failures that could not be durably recorded.",
	})
```

`internal/metrics/settlement_observability_test.go:18-28`의 `TestSettlementBarrierCollectorsArePreResolvedPerType`을 삭제하고, 신규 지표의 라벨 고정 테스트로 대체한다.

- [ ] **Step 4: dispatcher 구현**

`cmd/settlement_pipeline.go`. job 타입에 kind와 dispatcher-고유 ID를 넣고, 배리어를 제거한다.

```go
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
```

dispatcher 골격:

```go
// terminal은 worker가 실행하므로 dispatcher는 더 이상 settleSingle을 받지 않는다
// (기존 runTerminal 인라인 실행은 제거된다).
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

// terminalOrderID는 종결 이벤트의 주문 ID와 메트릭 라벨을 돌려준다.
func terminalOrderID(event service.OutboxEvent) (uint, string) {
	if event.Event.OrderCancelled != nil {
		return event.Event.OrderCancelled.OrderID, "cancel"
	}
	return event.Event.MarketOrderDone.OrderID, "market_done"
}
```

**terminal job은 `seq`를 소비하지 않는다** — `nextSeq`는 trade 배치에서만 증가하므로
`flushBroadcasts`의 순번에 구멍이 생기지 않는다. terminal은 WS 메시지를 만들지 않으므로
reorder coordinator를 막을 수 없다.

- [ ] **Step 5: worker에 terminal 분기 추가**

```go
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
```

- [ ] **Step 6: 테스트 통과 확인**

Run: `go test ./cmd/ -race -v`
Expected: PASS

Run: `go test ./... -race`
Expected: PASS

- [ ] **Step 7: 커밋** (`commit-message` 스킬)

---

## Task 8: 전체 검증과 29번 runbook

**Files:**
- Create: `docs/superpowers/plans/2026-07-30-per-order-fence-gcp-remeasurement.md` (29번 runbook)
- Create: `docs/runbooks/corrupted-outbox-recovery.md`
- Modify: `README.md`, `docs/refactor/README.md`
- Create: `docs/refactor/22_4차_축1_per-order_fence_완료.md`

**Interfaces:**
- Consumes: Task 1-7 전부
- Produces: 없음(문서·검증)

- [ ] **Step 1: 전체 검증 실행**

```bash
go build ./... && go vet ./... && go test ./... -race
```
Expected: 전부 통과

```bash
GOEXCHANGE_TEST_DATABASE_DSN=<dsn> go test ./internal/repository/ ./internal/service/ -v
```
Expected: 통합 테스트 통과(마이그레이션 적용 후)

- [ ] **Step 2: 폐기 지표 확인**

```bash
git grep -n "SettlementBarrier" -- '*.go'
```
Expected: 결과 없음(전부 제거됨)

```bash
git grep -n "order_settlement_duration_seconds" -- '*.go'
```
Expected: `metrics.go` 정의와 `main.go:696` 호출만 — **의미가 바뀌지 않았음을 확인**

- [ ] **Step 3: corrupted outbox 수동 복구 runbook 작성**

`docs/runbooks/corrupted-outbox-recovery.md`. 자동 격리를 제거했으므로 **사람의 결정**이 그 자리를 대체한다:

1. 부팅 실패 로그에서 `outbox replay: CORRUPTED event <id>` 확인
2. `SELECT * FROM trade_outbox_events WHERE id = <id>` 로 payload 검토
3. 판단 후 처리 — 복구 가능하면 payload 수정, 불가능하면 사람이 명시적으로 `status = 'PROCESSED'` 처리하고 사유를 기록
4. **새 엔드포인트·스키마를 만들지 않는다** — 이 절차는 의도적으로 수동이다

`Process=false`(DB 이상)로 인한 부팅 실패는 **crash loop가 정상 동작**임을 함께 적는다 — DB 회복 전까지 서버가 뜨지 않는 것이 계약이다.

- [ ] **Step 4: 29번 재측정 runbook 작성**

26번과 동일 조건 same-session A/B(e2-highcpu-4 server+DB, e2-standard-8 load-gen 수평 증설, `LOAD_START_AT_MS` 배리어, `--summary-export`, 단계별 스냅샷). 판정표는 스펙의 "실측 (29번)" 절을 그대로 옮긴다.

**무결성 검사를 먼저 통과시킨다 — 통과 전 성능 수치는 읽지 않는다.**

- [ ] **Step 5: 완료 문서·README 갱신**

`docs/refactor/22_4차_축1_per-order_fence_완료.md`에 **왜 필요했는지(28번 판정) → 왜 이 선택인지(A/C 계약과 대안 기각 사유) → 결과**를 적는다. 대화 중 정정된 사항(happens-before의 조건부 성립, `OrderID` unique가 중복 terminal을 탐지하지 않는다는 점, retry_count 1 시작이 실제 예산 손실이라는 점)을 함께 남긴다.

- [ ] **Step 6: 관련 없는 변경 없음 확인**

```bash
git diff --stat main...HEAD
```
각 파일이 이 계획의 태스크와 연결되는지 확인한다. 무관한 리팩터링·포맷팅이 있으면 되돌린다.

- [ ] **Step 7: 커밋** (`commit-message` 스킬)

**29번 GCP 실행은 별도 측정 세션에서 수행한다** — 구현 커밋과 분리해 runbook과 로컬 검증을 먼저 닫는다.
