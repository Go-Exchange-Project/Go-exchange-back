# 4차 축 1 — 정산 경로 관측성 패치 구현 계획 (관측성-only)

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:executing-plans로 태스크별 실행.
> Steps use checkbox (`- [ ]`). superpowers:test-driven-development로 RED→GREEN.

**Goal:** 27번이 남긴 두 원인(**batch 파편화** vs **barrier wait**)의 기여도를 구분할 계측 6종을
추가한다. **정산 동작은 한 줄도 바꾸지 않는다.**

**Architecture:** 신규 메트릭만 추가하고(기존 메트릭 의미 불변), 관측 지점은 ① 정산 DB 호출
(batch·single) ② dispatcher 배리어 ③ job 디스패치/실행 세 곳. 고정 라벨 collector는 초기화 시
1회 resolve해 hot path에서 map 조회를 피한다.

**Tech Stack:** Go, prometheus/client_golang.

**스펙 문서:** `docs/superpowers/specs/2026-07-28-settlement-observability-design.md`

## Global Constraints

- **정산 동작·순서 변경 금지** — 스케줄링·배리어·재시도·폴백·배치 구성 전부 그대로.
- **기존 메트릭 의미 변경 금지** — `order_settlement_duration_seconds`는 현재 관측 지점
  (`processTradeSettlement`, 재시도 루프 전체)을 **그대로 둔다**. 라벨도 추가하지 않는다.
- **신규 고카디널리티 라벨 금지** — `path`/`type`/`result`의 **고정값만**. `symbol`·`order_id`·
  `user_id`·`batch_seq` 금지.
- **batch/worker/concurrency/워터마크 변경 금지.** 축 2(fan-out·quantum) 계측도 이번에 섞지 않는다.
- **TDD는 시간 값이 아니라 histogram `_count`와 라벨 선택을 검증**(flaky 방지).
- **커밋 전 프로젝트의 `commit-message` 스킬 사용**(author→reviewer). 고장 시 `git diff --cached`로
  직접 확인해 작성하고 그 사실을 보고.
- 최종 검증은 `go test -race`와 **기존 benchmark 전후 비교** 포함. 통합 DSN 포트 55432.

---

### Task 1: 메트릭 선언 + 고정 라벨 collector 사전 resolve

**Files:**
- Modify: `internal/metrics/metrics.go`
- Test: `internal/metrics/settlement_observability_test.go`

**Interfaces:**
- Produces:
  - `metrics.SettlementAttemptDuration *prometheus.HistogramVec` (`path`)
  - `metrics.SettlementBarriersTotal *prometheus.CounterVec` (`type`)
  - `metrics.SettlementBarrierWait *prometheus.HistogramVec` (`type`)
  - `metrics.SettlementBarrierInflight *prometheus.HistogramVec` (`type`)
  - `metrics.SettlementJobDispatchWait prometheus.Histogram`
  - `metrics.SettlementJobExecution *prometheus.HistogramVec` (`result`)
  - **사전 resolve된 핸들**: `metrics.SettlementAttemptBatch`, `SettlementAttemptSingle`,
    `SettlementBarrierMarketDone`, `SettlementBarrierCancel`(각각 counter/histogram 묶음),
    `SettlementJobSuccess`, `SettlementJobFallback`, `SettlementJobFailed`

- [x] **Step 1: 실패 테스트** — `internal/metrics/settlement_observability_test.go`:

```go
package metrics_test

import (
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

func TestSettlementAttemptDurationHasFixedPathLabels(t *testing.T) {
	before := testutil.CollectAndCount(metrics.SettlementAttemptDuration)
	metrics.SettlementAttemptBatch.Observe(0.01)
	metrics.SettlementAttemptSingle.Observe(0.02)
	assert.GreaterOrEqual(t, testutil.CollectAndCount(metrics.SettlementAttemptDuration), before)
}

func TestSettlementBarrierCollectorsArePreResolvedPerType(t *testing.T) {
	// 사전 resolve된 핸들이 존재하고 서로 다른 시계열을 가리킨다(hot path에서 map 조회 금지).
	metrics.SettlementBarrierMarketDone.Inc()
	metrics.SettlementBarrierCancel.Inc()
	assert.NotEqual(t,
		testutil.ToFloat64(metrics.SettlementBarriersTotal.WithLabelValues("market_done")),
		-1.0)
	assert.NotEqual(t,
		testutil.ToFloat64(metrics.SettlementBarriersTotal.WithLabelValues("cancel")),
		-1.0)
}

func TestSettlementJobExecutionHasFixedResultLabels(t *testing.T) {
	metrics.SettlementJobSuccess.Observe(0.01)
	metrics.SettlementJobFallback.Observe(0.02)
	metrics.SettlementJobFailed.Observe(0.03)
	assert.GreaterOrEqual(t, testutil.CollectAndCount(metrics.SettlementJobExecution), 3)
}

// 기존 메트릭은 이번 패치에서 건드리지 않는다(의미·라벨 불변).
func TestOrderSettlementDurationRemainsUnlabeled(t *testing.T) {
	assert.Equal(t, 1, testutil.CollectAndCount(metrics.OrderSettlementDuration))
}
```

Run: `go test ./internal/metrics/... -run Settlement -v` → FAIL(undefined).

- [x] **Step 2: 구현** — `metrics.go`의 `var (...)` 블록에 추가(기존 항목은 손대지 않는다):

```go
	// 4차 축 1 관측성: DB 호출 1회(트랜잭션 시도) 단위. 기존 order_settlement_duration_seconds
	// (논리적 단건 정산 전체)와는 의미가 다른 별도 메트릭이다 — 기존 것은 그대로 보존한다.
	SettlementAttemptDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "settlement_attempt_duration_seconds",
		Help:    "Duration of one settlement DB call (transaction attempt).",
		Buckets: prometheus.DefBuckets,
	}, []string{"path"})

	SettlementBarriersTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "settlement_barriers_total",
		Help: "Terminal-event barrier entries in the partition dispatcher.",
	}, []string{"type"})

	SettlementBarrierWait = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "settlement_barrier_wait_seconds",
		Help:    "Time a terminal event waited for preceding in-flight batches.",
		Buckets: prometheus.DefBuckets,
	}, []string{"type"})

	SettlementBarrierInflight = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "settlement_barrier_inflight_batches",
		Help:    "In-flight batch count at barrier entry.",
		Buckets: []float64{0, 1, 2, 4, 8, 16},
	}, []string{"type"})

	// dispatcher가 job 송신을 "시도한" 시점부터 worker 실행 시작까지 — 채널 송신 대기·
	// 채널 내부 대기·worker 스케줄링 대기를 의도적으로 모두 포함한다.
	SettlementJobDispatchWait = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "settlement_job_dispatch_wait_seconds",
		Help:    "From dispatch attempt to worker execution start (includes channel send wait).",
		Buckets: prometheus.DefBuckets,
	})

	SettlementJobExecution = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "settlement_job_execution_seconds",
		Help:    "Worker start to logical job completion (includes retries and fallback).",
		Buckets: prometheus.DefBuckets,
	}, []string{"result"})
)

// hot path에서 라벨 map 조회를 피하기 위해 초기화 시 1회 resolve한다.
var (
	SettlementAttemptBatch      = SettlementAttemptDuration.WithLabelValues("batch")
	SettlementAttemptSingle     = SettlementAttemptDuration.WithLabelValues("single")
	SettlementBarrierMarketDone = SettlementBarriersTotal.WithLabelValues("market_done")
	SettlementBarrierCancel     = SettlementBarriersTotal.WithLabelValues("cancel")
	SettlementBarrierWaitDone   = SettlementBarrierWait.WithLabelValues("market_done")
	SettlementBarrierWaitCancel = SettlementBarrierWait.WithLabelValues("cancel")
	SettlementBarrierInflightDone   = SettlementBarrierInflight.WithLabelValues("market_done")
	SettlementBarrierInflightCancel = SettlementBarrierInflight.WithLabelValues("cancel")
	SettlementJobSuccess  = SettlementJobExecution.WithLabelValues("success")
	SettlementJobFallback = SettlementJobExecution.WithLabelValues("fallback")
	SettlementJobFailed   = SettlementJobExecution.WithLabelValues("failed")
)
```

Run: `go test ./internal/metrics/... -count=1` → PASS.

- [x] **Step 3: Commit** — 초안: `feat(metrics): 정산 진단용 관측 메트릭 6종 추가 (4차 축1)` (커밋 `6ae5097`)

---

### Task 2: DB attempt 관측을 batch·single 경로에 연결

**Files:**
- Modify: `cmd/main.go`(`settleTradeBatchWithFallback`의 `SettleTradeBatch` 호출, `processTradeSettlement`의 재시도 루프)
- Test: `cmd/settlement_observability_test.go`

- [x] **Step 1: 실패 테스트** — 배치 경로 1회 호출에 attempt 샘플 1개, 단건 재시도 N회에 샘플 N개:

```go
func TestSettleTradeBatchObservesOneAttemptSample(t *testing.T) {
	before := histogramVecSampleCount(t, metrics.SettlementAttemptDuration, "batch")
	// 성공하는 fake batchSettler로 settleTradeBatchWithFallback 1회 호출
	// (기존 cmd/main_test.go의 fakeTradeBatchSettler 재사용)
	settleTradeBatchWithFallback(batch, okBatchSettler, settler, failureRecorder,
		marketCompleter, completionFailureRecorder, cancelProcessor, noopBroadcast, outboxRepo, logger)
	after := histogramVecSampleCount(t, metrics.SettlementAttemptDuration, "batch")
	assert.Equal(t, before+1, after, "배치 DB 호출 1회당 attempt 샘플 1개")
}

func TestProcessTradeSettlementObservesEachRetryAttempt(t *testing.T) {
	before := histogramVecSampleCount(t, metrics.SettlementAttemptDuration, "single")
	beforeLegacy := histogramSampleCount(t, metrics.OrderSettlementDuration)
	// 2회 transient 실패 후 3회째 성공하는 settler
	processTradeSettlement(trade, 0, flakySettler, failureRecorder, noopBroadcast, logger)
	after := histogramVecSampleCount(t, metrics.SettlementAttemptDuration, "single")
	afterLegacy := histogramSampleCount(t, metrics.OrderSettlementDuration)
	assert.Equal(t, before+3, after, "재시도 3회 = attempt 샘플 3개")
	assert.Equal(t, beforeLegacy+1, afterLegacy, "기존 메트릭은 논리 1건 그대로(의미 불변)")
}
```

Run: `go test ./cmd/... -run Attempt -v` → FAIL.

- [x] **Step 2: 구현** — 두 지점에 관측 추가. **기존 `OrderSettlementDuration.Observe`는 그대로 둔다**:

```go
	// settleTradeBatchWithFallback 내부 — SettleTradeBatch 호출 감싸기
	attemptStart := time.Now()
	results, err := batchSettler.SettleTradeBatch(items)
	metrics.SettlementAttemptBatch.Observe(time.Since(attemptStart).Seconds())
```

```go
	// processTradeSettlement 내부 — 재시도 "각 시도"를 감싼다(기존 전체 관측은 유지)
	settlementStart := time.Now()          // 기존 그대로(논리 전체)
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
```

Run: 위 테스트 → PASS.

- [x] **Step 3: Commit** — 초안: `feat(settlement): DB 시도 단위 정산 지연 관측 추가 (4차 축1)` (커밋 `607da72`)

---

### Task 3: 배리어 counter·wait·in-flight 관측

**Files:**
- Modify: `cmd/settlement_pipeline.go`(`runPartitionDispatcher`의 배리어 경로)
- Test: `cmd/settlement_pipeline_test.go`

- [x] **Step 1: 실패 테스트** — Done/Cancel이 각각 올바른 타입으로 계수되고, in-flight가 있으면
  wait 샘플이 남는다(**시간 값이 아니라 count·라벨만 단언**):

```go
func TestDispatcherRecordsBarrierMetricsPerTerminalType(t *testing.T) {
	beforeCancel := testutil.ToFloat64(metrics.SettlementBarriersTotal.WithLabelValues("cancel"))
	beforeDone := testutil.ToFloat64(metrics.SettlementBarriersTotal.WithLabelValues("market_done"))
	beforeWait := histogramVecSampleCount(t, metrics.SettlementBarrierWait, "cancel")

	// trade 1건(지연 완료) → OrderCancelled → MarketOrderDone 순으로 주입
	// (기존 TestPartitionDispatcherProcessesTerminalEventAfterPrecedingBatches 픽스처 확장)
	runPartitionDispatcher(queue, jobs, 3, 32, settleSingle, broadcast)

	assert.Equal(t, beforeCancel+1,
		testutil.ToFloat64(metrics.SettlementBarriersTotal.WithLabelValues("cancel")))
	assert.Equal(t, beforeDone+1,
		testutil.ToFloat64(metrics.SettlementBarriersTotal.WithLabelValues("market_done")))
	assert.Equal(t, beforeWait+1, histogramVecSampleCount(t, metrics.SettlementBarrierWait, "cancel"),
		"in-flight가 있던 배리어는 wait 샘플을 남긴다")
}
```

Run: `go test ./cmd/... -run BarrierMetrics -v` → FAIL.

- [x] **Step 2: 구현** — 배리어 진입 시점(= `pendingTerminal` 설정 시) 시각·in-flight를 기록하고,
  해제 시점(= `runTerminal` 직전)에 observe. **배리어 로직 자체는 변경하지 않는다**:

```go
	// 종결 이벤트를 만나 배리어에 진입할 때
	if first.Event.Trade == nil {
		pendingTerminal = first
		barrierStart = time.Now()
		barrierInflight = inFlight
		switch {
		case first.Event.OrderCancelled != nil:
			metrics.SettlementBarrierCancel.Inc()
			metrics.SettlementBarrierInflightCancel.Observe(float64(barrierInflight))
		default: // MarketOrderDone
			metrics.SettlementBarrierMarketDone.Inc()
			metrics.SettlementBarrierInflightDone.Observe(float64(barrierInflight))
		}
		continue
	}
```

```go
	// 배리어 해제(in-flight 0 도달) — runTerminal 직전
	if pendingTerminal.Event.OrderCancelled != nil {
		metrics.SettlementBarrierWaitCancel.Observe(time.Since(barrierStart).Seconds())
	} else {
		metrics.SettlementBarrierWaitDone.Observe(time.Since(barrierStart).Seconds())
	}
```

Run: 위 테스트 + 기존 dispatcher 3종(순서·배리어·드레인) → 전부 PASS.

- [x] **Step 3: Commit** — 초안: `feat(settlement): 종결 이벤트 배리어 진입·대기·in-flight 관측 추가 (4차 축1)` (커밋 `123e784`)

---

### Task 4: job dispatch wait · logical execution 관측

**Files:**
- Modify: `cmd/settlement_pipeline.go`(`settlementJob`에 디스패치 시각, `runSettlementWorker`)
- Test: `cmd/settlement_pipeline_test.go`

- [x] **Step 1: 실패 테스트** — worker를 의도적으로 늦게 기동해 dispatch wait를 만들고, 완료 시
  execution 샘플이 남는지 확인:

```go
func TestDispatcherAndWorkerRecordJobTimingMetrics(t *testing.T) {
	beforeWait := histogramSampleCount(t, metrics.SettlementJobDispatchWait)
	beforeExec := histogramVecSampleCount(t, metrics.SettlementJobExecution, "success")

	// worker를 50ms 뒤에 기동해 디스패치 대기를 강제
	go func() { time.Sleep(50 * time.Millisecond); runSettlementWorker(jobs, okSettleBatch) }()
	runPartitionDispatcher(queue, jobs, 2, 32, nil, broadcast)

	assert.Equal(t, beforeWait+1, histogramSampleCount(t, metrics.SettlementJobDispatchWait))
	assert.Equal(t, beforeExec+1, histogramVecSampleCount(t, metrics.SettlementJobExecution, "success"))
}
```

Run: `go test ./cmd/... -run JobTiming -v` → FAIL.

- [x] **Step 2: 구현** — `settlementJob`에 `dispatchAt time.Time` 추가. **dispatcher가 송신을 시도하기
  직전**에 찍고(송신 대기 포함), worker가 실행 시작 시 observe:

```go
type settlementJob struct {
	seq        uint64
	batch      []service.OutboxEvent
	done       chan<- settlementResult
	dispatchAt time.Time // 송신 시도 시점 — 채널 대기까지 포함해 측정
}
```

```go
	// dispatcher: readyJob 구성 직후(= 송신 시도 시작)
	job := settlementJob{seq: nextSeq, batch: batch, done: completions, dispatchAt: time.Now()}
```

```go
	// worker: 실행 시작·완료
	for job := range jobs {
		metrics.SettlementJobDispatchWait.Observe(time.Since(job.dispatchAt).Seconds())
		execStart := time.Now()
		var messages []broadcastMessage
		collect := func(symbol string, payload []byte) { /* 기존 그대로 */ }
		if settleBatch != nil {
			settleBatch(job.batch, collect)
		}
		metrics.SettlementJobSuccess.Observe(time.Since(execStart).Seconds())
		job.done <- settlementResult{seq: job.seq, messages: messages}
	}
```

> **`result` 라벨 주의**: 현재 `settleBatch`(=`settleTradeBatchWithFallback`)는 반환값이 없어
> worker가 성공/폴백/실패를 알 수 없다. **이번 패치에서는 `success`만 관측**하고,
> `fallback`/`failed` 구분은 **결과 전달 경로가 필요하므로 범위 밖**(다음 사이클)임을 코드 주석과
> 완료 문서에 명시한다. — 동작을 바꾸지 않기 위한 의도적 제한.

Run: 위 테스트 → PASS.

- [x] **Step 3: Commit** — 초안: `feat(settlement): job 디스패치 대기·실행 시간 관측 추가 (4차 축1)` (커밋 `393af51`)

---

### Task 5: 전체 검증 + 통제 부하 + 문서

- [x] **Step 1: 전체 검증** — `go build ./...` + `go vet` + `go test ./... -count=1`(통합 SKIP 0,
  DSN 55432) + `go test ./cmd/... ./internal/service/... ./internal/matching/... -race -count=1`.
  **기존 순서·정산·종료·배리어 테스트 무수정 그린**(동작 불변의 증거).
- [x] **Step 2: 성능 회귀 확인** — 기존 matching/settlement 벤치마크를 **패치 전후로** 실행해
  (`go test -bench . -benchmem`) **유의미한 할당·시간 증가가 없음**을 확인하고 수치를 기록.
  hot path에서 라벨 map 조회가 없는지(사전 resolve 사용) 코드로 재확인.
- [x] **Step 3: 짧은 통제 부하 검증** — 로컬에서 종결 이벤트가 섞인 부하를 짧게 돌려:
  1. 신규 메트릭이 **전부 0이 아님**
  2. `settlement_barriers_total`이 실제 Done/Cancel 처리 건수와 **대략 일치**
  3. 완료 job 수와 `job_dispatch_wait`·`job_execution` histogram count **일치**
  4. **정합성·fallback 결과가 패치 전과 동일**
  5. **판정표 네 분기 중 하나를 실제로 선택 가능**
- [x] **Step 4: 완료 문서 + README** — `docs/refactor/20_4차축1_정산_관측성_완료.md`:
  왜(27번의 두 원인 분리) / 어떻게(신규 6종, 기존 메트릭 불변, 사전 resolve) / 결과(테스트·벤치마크·
  통제 부하 5확인) / **판정 결과(어느 분기인지)** / **`result` 라벨 제한**과 다음 사이클 예고.
  README 4차 현재 단계 갱신.
- [ ] **Step 5: Commit + 푸시 + CI** — `gh run watch` 그린.

---

## 다음 (범위 밖)

Task 5 Step 3의 판정 결과에 따른 **수정 설계**(주문별 dependency fence / batch scheduling /
트랜잭션·SQL 최적화 / pool·dispatcher 조정 중 하나) — 별도 스펙. 축 2(`executions_per_order`·
매칭 quantum) 계측. `settlement_job_execution_seconds`의 `fallback`/`failed` 라벨 구분.
