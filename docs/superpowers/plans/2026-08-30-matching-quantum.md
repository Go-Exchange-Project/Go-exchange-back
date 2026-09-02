# 매칭 엔진 두 quantum 실행 계획 (B-3)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** 매칭 엔진 루프에 두 quantum(`maxMatchesPerTurn`, `maxConsecutiveCancels`)을 넣어 취소 홍수로 인한 신규 주문 기아와 대형 sweep으로 인한 취소 기아를 제거한다.

**Architecture:** `Match`를 `matchSlice`(최대 N체결) + `finishOrder`(마지막 조각 전용)로 쪼개고, 샤드당 하나의 `activeSweep` 슬롯에 진행 상태를 둔다. 엔진 루프는 blocking select가 작업을 실행하지 않고 latch만 하는 turn 상태 기계가 되며, 각 turn은 bounded cancel drain → due ticker → active slice → stop latch → admission 순으로 돈다.

**Tech Stack:** Go 1.25.7, Prometheus client_golang(promauto), testify, PostgreSQL(GORM). 측정은 Go 하니스(`-tags quantumharness`)와 k6.

**설계 문서:** [2026-08-30-matching-quantum-design.md](../specs/2026-08-30-matching-quantum-design.md)
**기준 HEAD:** 이 계획서 커밋

## Global Constraints

- 설계 문서의 계약이 이 계획의 암묵적 요구사항이다. 충돌하면 설계 문서가 이긴다.
- 커밋 메시지는 Conventional Commits, 제목·본문 한글(type 접두만 영문), `Co-Authored-By` 금지.
- 커밋 전 `commit-message` 스킬 사용. Bash 도구가 고장 나 있으므로 서브에이전트에는 `_workspace/ctx-log.txt`로 relay하고 커밋 후 삭제한다.
- 셸은 PowerShell. 기존 커밋 amend/rebase/force-push 금지.
- 중간 산출물은 `_workspace/`.
- **mutation 검증을 하지 않는다.** 파일을 변이했다 복구하는 절차는 과거에 미커밋 작업을 날린 사고의 원인이었다. 각 테스트가 계약을 직접 단언하도록 쓴다.
- **focused test는 해당 코드를 작성할 때만** 실행한다. 태스크별 전체 테스트·race·vet을 두지 않는다.
- `-tags quantumharness`는 baseline 수집과 후보 측정에서만. 일반 `go test`·CI에 포함하지 않는다.
- 모든 `go test`에 `-count=1`. 없으면 캐시된 결과로 조용히 통과한다.
- env 변수명: `GOEXCHANGE_MATCHING_MAX_MATCHES_PER_TURN`, `GOEXCHANGE_MATCHING_MAX_CONSECUTIVE_CANCELS`.
- 기존 `parsePositiveIntEnv`(config/database.go:68)의 동작을 바꾸지 않는다.
- 기존 무인자 생성자 `NewMatchingEngine()`·`NewShardedEngine(n)`은 테스트용 기본값을 유지한다.
- **GCP는 별도 유료 승인 후에만.** 500 VU 1회, 자동 재실행 금지.

## 체크포인트

| CP | 태스크 | 승인 |
|---|---|---|
| **A. 로컬 완료** | 1~8 | 중간 승인 없이 연속 진행. 측정 무효화·정확성 훼손 P1에서만 중단 |
| **B. GCP 완료** | 9~10 | 별도 유료 승인 후에만 시작 |

## 중단 조건

정확성 계약을 지킬 수 없는 구조적 P1 / baseline 또는 후보 run-set 무효(V1~V5) / 통과 후보 0개 / 전체 로컬 검증 또는 CI 실패 / GCP 유료 승인 시점 / runbook §7.5 시크릿 게이트 실패.

## 파일 구조

| 파일 | 책임 | 태스크 |
|---|---|---|
| `internal/matching/observers.go` (신규) | `EngineObservers` 7콜백, `EmitKind` | 1 |
| `internal/matching/engine.go` (수정) | 관측 훅, `CancelOrderCommand.EnqueuedAt`(engine.go:81) → `matchSlice`/`finishOrder`/turn 상태 기계/`crashHook` | 1, 4 |
| `internal/matching/sharded.go` (수정) | `SetObservers` → `NewShardedEngineWithQuantum` | 1, 5 |
| `internal/metrics/metrics.go` (수정) | 지표 8종 | 1 |
| `internal/metrics/matching_observers.go` (신규) | matching↔prometheus 어댑터 | 1 |
| `cmd/main.go` (수정) | 관측 배선 → quantum 설정 주입·기동 실패 | 1, 5 |
| `internal/matching/quantum_stats_test.go` (신규) | percentile·median 유틸 (태그 없음) | 2 |
| `internal/matching/quantum_harness_test.go` (신규) | H0/H1/H2/H4, watchdog·censored, JSON (`//go:build quantumharness`) | 2 |
| `internal/matching/quantum_config.go` (신규) | `QuantumConfig`, `Validate` | 5 |
| `config/runtime.go` (수정) | `strictPositiveEnv`, `MatchingQuantumFromEnv`, 기본값 상수 | 5 |
| `internal/matching/quantum_select.go` (신규) | `SelectQuantum` 순수 함수 | 6 |
| `internal/matching/quantum_aggregate.go` (신규) | `RunFile`, run-set 검증, 집계 | 6 |
| `cmd/quantumselect/main.go` (신규) | baseline+후보 JSON → 선택 | 6 |
| `internal/service/active_sweep_cancel_integration_test.go` (신규) | B′·A′ 의미 테스트 | 7 |

---

# CP A — 로컬 완료

## Task 1: 계측 (행동 변경 0)

**Files:**
- Create: `internal/matching/observers.go`, `internal/metrics/matching_observers.go`
- Modify: `internal/matching/engine.go`, `internal/matching/sharded.go`, `internal/metrics/metrics.go`, `cmd/main.go`
- Test: `internal/matching/engine_observers_test.go`, `internal/metrics/matching_observers_test.go`, `cmd/main_quantum_wiring_test.go`

**Produces:** `matching.EngineObservers`(7콜백), `matching.EmitKind`(`EmitTrade`/`EmitMarketDone`/`EmitCancelled`), `(*MatchingEngine).Observers`, `(*ShardedEngine).SetObservers`, `metrics.NewMatchingEngineObservers()`, 지표 8종.

- [ ] **Step 1: `observers.go`를 만든다**

```go
package matching

import "time"

// EmitKind는 ExecutionCh로 나가는 이벤트 종류다. emit 블로킹 시간을
// 종류별로 구분해 보기 위해 존재한다.
type EmitKind string

const (
	EmitTrade      EmitKind = "trade"
	EmitMarketDone EmitKind = "done"
	EmitCancelled  EmitKind = "cancelled"
)

// EngineObservers는 엔진 내부 계측을 밖으로 노출하는 콜백 묶음이다.
// internal/matching이 prometheus를 import하지 않도록 기존
// MatchLatencyObserver와 같은 방식을 따른다. 모든 필드는 nil 허용이다.
//
// Start() 전에 설정하고 실행 중 교체하지 않는다 — 재대입은 data race다.
type EngineObservers struct {
	Turn          func(d time.Duration)                     // turn 작업 구간 (블로킹 대기 제외)
	Slice         func(trades int, emitBlock time.Duration) // 조각 하나의 체결 수와 emit 블로킹 누적
	OrderAdmitted func(queueWait time.Duration)             // 큐에서 꺼내진 시점의 대기 시간
	OrderDone     func(trades int)                          // 주문 완결 시 총 체결 수
	Cancel        func(queueWait time.Duration)
	EmitBlock     func(kind EmitKind, d time.Duration) // ExecutionCh send 1회의 블로킹
	Yield         func()                               // 예산 소진으로 조각이 반환될 때
}

func (o EngineObservers) turn(d time.Duration) {
	if o.Turn != nil {
		o.Turn(d)
	}
}

func (o EngineObservers) slice(trades int, emitBlock time.Duration) {
	if o.Slice != nil {
		o.Slice(trades, emitBlock)
	}
}

func (o EngineObservers) orderAdmitted(queueWait time.Duration) {
	if o.OrderAdmitted != nil {
		o.OrderAdmitted(queueWait)
	}
}

func (o EngineObservers) orderDone(trades int) {
	if o.OrderDone != nil {
		o.OrderDone(trades)
	}
}

func (o EngineObservers) cancel(queueWait time.Duration) {
	if o.Cancel != nil {
		o.Cancel(queueWait)
	}
}

func (o EngineObservers) emitBlock(kind EmitKind, d time.Duration) {
	if o.EmitBlock != nil {
		o.EmitBlock(kind, d)
	}
}

func (o EngineObservers) yield() {
	if o.Yield != nil {
		o.Yield()
	}
}
```

- [ ] **Step 2: 엔진에 훅을 넣는다**

`internal/matching/engine.go`.

`CancelOrderCommand`(engine.go:81)의 `ResponseCh` 위에 필드를 추가한다:

```go
	// EnqueuedAt은 CancelOrder가 채운다. 제로값이면 큐 대기 관측을 건너뛴다 —
	// 테스트가 직접 구성한 command가 가짜 지연을 만들지 않게 하기 위해서다.
	EnqueuedAt time.Time
```

`MatchingEngine`의 `MatchLatencyObserver` 아래에 추가한다:

```go
	// Observers는 quantum 계측용 콜백 묶음이다. 제로값이면 전부 비활성이다.
	Observers EngineObservers

	// lastMatchTrades·sliceEmitBlock은 한 조각의 누적값이다.
	// Task 4에서 matchSlice가 값을 직접 반환하게 되면 lastMatchTrades는
	// 사라지고 sliceEmitBlock만 남는다.
	lastMatchTrades int
	sliceEmitBlock  time.Duration
```

`CancelOrder`(engine.go:350) 진입부:

```go
	if cmd.EnqueuedAt.IsZero() {
		cmd.EnqueuedAt = time.Now()
	}
```

`processCancel`:

```go
func (me *MatchingEngine) processCancel(cmd CancelOrderCommand) {
	if !cmd.EnqueuedAt.IsZero() {
		me.Observers.cancel(time.Since(cmd.EnqueuedAt))
	}
	result := me.handleCancel(cmd)
	// ... 기존 본문 그대로 ...
}
```

세 emit 함수의 `ExecutionCh` send를 감싼다:

```go
// sendExecution은 ExecutionCh로의 블로킹 send 시간을 관측한다.
// send 자체에는 timeout이 없다 — 하류가 멈추면 여기서 무기한 블로킹한다.
func (me *MatchingEngine) sendExecution(kind EmitKind, event ExecutionEvent) {
	start := time.Now()
	me.ExecutionCh <- event
	blocked := time.Since(start)
	me.Observers.emitBlock(kind, blocked)
	me.sliceEmitBlock += blocked
}
```

`emitTrade`는 `me.sendExecution(EmitTrade, ExecutionEvent{Trade: trade})` 뒤에 `me.lastMatchTrades++`, `emitMarketOrderDone`은 `EmitMarketDone`, `emitOrderCancelled`는 `EmitCancelled`를 쓴다.

`processOrder`를 바꾼다. **동작(체결·이벤트·dirty)은 그대로이고 관측만 추가된다.**

```go
func (me *MatchingEngine) processOrder(order *Order) {
	if order == nil {
		return
	}
	if !order.EnqueuedAt.IsZero() {
		me.Observers.orderAdmitted(time.Since(order.EnqueuedAt))
	}
	// 조각화 이전에는 "조각 = 주문 전체"다. Task 4와 같은 지표를 채우기 위해
	// Slice/OrderDone을 여기서 1회 낸다. 이 둘이 비면 matches_per_slice와
	// emit_block_per_slice의 baseline이 없어 전후 비교가 불가능하다.
	me.lastMatchTrades = 0
	me.sliceEmitBlock = 0
	admissible := me.orderIsAdmissible(order)
	me.Match(order)
	if me.MatchLatencyObserver != nil && !order.EnqueuedAt.IsZero() {
		me.MatchLatencyObserver(time.Since(order.EnqueuedAt))
	}
	// 즉시 완료 경로는 슬롯이 만들어지지 않으므로 셀 체결이 없다.
	// 여기서 OrderDone(0)/Slice(0)를 내면 executions_per_order 분포가
	// 0으로 오염되고, 설계 §2.3의 "OrderAdmitted만 발생" 계약과 어긋난다.
	if admissible {
		me.Observers.slice(me.lastMatchTrades, me.sliceEmitBlock)
		me.Observers.orderDone(me.lastMatchTrades)
	}
	me.markDirty(order.CoinSymbol)
}

// orderIsAdmissible은 Match가 실제 매칭을 수행할 주문인지를 Match와 같은
// 기준으로 판단한다. Task 4에서 admitOrder로 흡수된다.
func (me *MatchingEngine) orderIsAdmissible(order *Order) bool {
	switch order.Side {
	case model.OrderSideBuy:
		return order.OrderType == model.OrderTypeMarket || order.Amount.GreaterThan(decimal.Zero)
	case model.OrderSideSell:
		return order.Amount.GreaterThan(decimal.Zero)
	}
	return false
}
```

`Slice`는 `Match`(= `finishOrder` 포함) **뒤에** 부른다. 마지막 조각의 `MarketOrderDone` emit 블로킹이 `sliceEmitBlock`에 들어간 뒤여야 한다 — 순서가 뒤집히면 하류 포화 시 가장 오래 막히는 지점이 측정에서 빠진다.

`Start` 루프의 turn 관측은 넣지 않는다. Task 4에서 `runTurn`이 생기면 그때 한 곳에서 잰다. 지금 넣으면 Task 4에서 전부 지워야 한다.

- [ ] **Step 3: 지표 8종과 어댑터를 만든다**

`internal/metrics/metrics.go`의 `var (` 블록 끝에 추가한다.

```go
	MatchingEngineExecutionsPerOrder = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "matching_engine_executions_per_order",
		Help:    "Number of trades produced by a single order, observed when the order completes.",
		Buckets: []float64{1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 4096},
	})

	MatchingEngineMatchesPerSlice = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "matching_engine_matches_per_slice",
		Help:    "Number of trades produced by one matching slice (quantum unit).",
		Buckets: []float64{1, 2, 4, 8, 16, 32, 64, 128, 256},
	})

	MatchingEngineTurnDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "matching_engine_turn_duration_seconds",
		Help:    "Work duration of one scheduler turn, excluding time blocked waiting for input.",
		Buckets: prometheus.ExponentialBuckets(1e-5, 4, 8),
	})

	MatchingEngineOrderQueueWait = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "matching_engine_order_queue_wait_seconds",
		Help:    "Time an order waited in the engine queue before admission.",
		Buckets: prometheus.ExponentialBuckets(1e-4, 4, 9),
	})

	MatchingEngineCancelQueueWait = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "matching_engine_cancel_queue_wait_seconds",
		Help:    "Time a cancel command waited in the engine queue before processing.",
		Buckets: prometheus.ExponentialBuckets(1e-4, 4, 9),
	})

	MatchingEngineQuantumYields = promauto.NewCounter(prometheus.CounterOpts{
		Name: "matching_engine_quantum_yields_total",
		Help: "Number of matching slices that returned because the per-turn trade budget was exhausted.",
	})

	MatchingEngineEmitBlock = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "matching_engine_emit_block_seconds",
		Help:    "Duration of a single blocking send into ExecutionCh. This send has no timeout.",
		Buckets: prometheus.ExponentialBuckets(1e-6, 5, 9),
	}, []string{"event"})

	MatchingEngineEmitBlockPerSlice = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "matching_engine_emit_block_per_slice_seconds",
		Help:    "Total ExecutionCh blocking time accumulated within one matching slice.",
		Buckets: prometheus.ExponentialBuckets(1e-6, 5, 9),
	})
```

`matches_per_slice`의 상단 bucket이 후보 최대값(128)보다 한 칸 큰 것은 의도적이다 — 상한을 넘는 관측이 나오면 그 자체가 버그 신호이므로 bucket에 잡혀야 한다.

`internal/metrics/matching_observers.go`:

```go
package metrics

import (
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
)

// NewMatchingEngineObservers는 엔진 계측 콜백을 프로메테우스 지표에 연결한다.
// internal/matching이 prometheus를 import하지 않도록 어댑터를 여기에 둔다.
func NewMatchingEngineObservers() matching.EngineObservers {
	return matching.EngineObservers{
		Turn: func(d time.Duration) {
			MatchingEngineTurnDuration.Observe(d.Seconds())
		},
		Slice: func(trades int, emitBlock time.Duration) {
			MatchingEngineMatchesPerSlice.Observe(float64(trades))
			MatchingEngineEmitBlockPerSlice.Observe(emitBlock.Seconds())
		},
		OrderAdmitted: func(queueWait time.Duration) {
			MatchingEngineOrderQueueWait.Observe(queueWait.Seconds())
		},
		OrderDone: func(trades int) {
			MatchingEngineExecutionsPerOrder.Observe(float64(trades))
		},
		Cancel: func(queueWait time.Duration) {
			MatchingEngineCancelQueueWait.Observe(queueWait.Seconds())
		},
		EmitBlock: func(kind matching.EmitKind, d time.Duration) {
			MatchingEngineEmitBlock.WithLabelValues(string(kind)).Observe(d.Seconds())
		},
		Yield: func() {
			MatchingEngineQuantumYields.Inc()
		},
	}
}
```

`internal/matching/sharded.go`의 `SetMatchLatencyObserver` 아래:

```go
// SetObservers는 전 샤드에 같은 계측 콜백 묶음을 설정한다. Start() 전에만 부른다.
func (se *ShardedEngine) SetObservers(observers EngineObservers) {
	for _, shard := range se.shards {
		shard.Observers = observers
	}
}
```

`cmd/main.go`의 `me.SetMatchLatencyObserver(...)` 아래:

```go
	me.SetObservers(metrics.NewMatchingEngineObservers())
```

- [ ] **Step 4: 계측 테스트를 쓴다**

`internal/matching/engine_observers_test.go`:

```go
package matching

import (
	"sync"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

type sliceSample struct {
	trades    int
	emitBlock time.Duration
}

type recordedObservers struct {
	mu         sync.Mutex
	admitted   []time.Duration
	doneTrades []int
	cancels    []time.Duration
	emits      []EmitKind
	slices     []sliceSample
}

// install은 Start() 전에만 부른다. 실행 중 재대입은 data race다.
func (r *recordedObservers) install(me *MatchingEngine) {
	me.Observers = EngineObservers{
		Slice: func(n int, b time.Duration) {
			r.mu.Lock()
			r.slices = append(r.slices, sliceSample{trades: n, emitBlock: b})
			r.mu.Unlock()
		},
		OrderAdmitted: func(d time.Duration) {
			r.mu.Lock()
			r.admitted = append(r.admitted, d)
			r.mu.Unlock()
		},
		OrderDone: func(n int) {
			r.mu.Lock()
			r.doneTrades = append(r.doneTrades, n)
			r.mu.Unlock()
		},
		Cancel: func(d time.Duration) {
			r.mu.Lock()
			r.cancels = append(r.cancels, d)
			r.mu.Unlock()
		},
		EmitBlock: func(k EmitKind, _ time.Duration) {
			r.mu.Lock()
			r.emits = append(r.emits, k)
			r.mu.Unlock()
		},
	}
}

func (r *recordedObservers) counts() (admitted, done, cancels, emits, slices int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.admitted), len(r.doneTrades), len(r.cancels), len(r.emits), len(r.slices)
}

func TestObserversRecordAdmitSliceDoneAndEmit(t *testing.T) {
	me := newTestEngine()
	rec := &recordedObservers{}
	rec.install(me)
	drainAll(me)
	me.Start()

	sell := stopTestLimitOrder(1, model.OrderSideSell, 50000, 2)
	sell.EnqueuedAt = time.Now()
	me.OrderCh <- sell
	buy := stopTestLimitOrder(2, model.OrderSideBuy, 50000, 2)
	buy.EnqueuedAt = time.Now()
	me.OrderCh <- buy

	require.Eventually(t, func() bool {
		_, done, _, _, _ := rec.counts()
		return done == 2
	}, 5*time.Second, 5*time.Millisecond)

	me.Stop()
	waitEngineDone(t, me)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.Len(t, rec.admitted, 2, "admit된 주문마다 OrderAdmitted 1회")
	require.Equal(t, []int{0, 1}, rec.doneTrades, "첫 주문은 체결 0, 두 번째는 1")
	require.Len(t, rec.slices, 2, "계측 커밋도 주문당 Slice를 1회 내야 baseline이 비지 않는다")
	require.Equal(t, 0, rec.slices[0].trades)
	require.Equal(t, 1, rec.slices[1].trades)
	require.Contains(t, rec.emits, EmitTrade)
}

func TestCancelObserverUsesEnqueuedAt(t *testing.T) {
	me := newTestEngine()
	rec := &recordedObservers{}
	rec.install(me)
	drainAll(me)
	me.Start()

	resting := stopTestLimitOrder(7, model.OrderSideSell, 50000, 1)
	resting.EnqueuedAt = time.Now()
	me.OrderCh <- resting
	require.Eventually(t, func() bool {
		_, done, _, _, _ := rec.counts()
		return done == 1
	}, 5*time.Second, 5*time.Millisecond)

	result := me.CancelOrder(CancelOrderCommand{
		CoinSymbol: "BTC", OrderID: 7,
		Side: model.OrderSideSell, Price: resting.Price,
	})
	require.True(t, result.Removed)

	me.Stop()
	waitEngineDone(t, me)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.Len(t, rec.cancels, 1, "취소마다 Cancel 관측 1회")
	require.Greater(t, rec.cancels[0], time.Duration(0), "CancelOrder가 EnqueuedAt을 채워야 한다")
	require.Contains(t, rec.emits, EmitCancelled)
}

// 즉시 완료 경로는 OrderAdmitted만 낸다. 슬롯이 없어 셀 체결이 없으므로
// OrderDone(0)/Slice(0)를 내면 executions_per_order 분포가 오염된다.
func TestImmediateCompletionEmitsOnlyOrderAdmitted(t *testing.T) {
	me := newTestEngine()
	rec := &recordedObservers{}
	rec.install(me)
	drainAll(me)
	me.Start()

	zero := stopTestLimitOrder(1, model.OrderSideSell, 50000, 1)
	zero.Amount = decimal.Zero
	zero.EnqueuedAt = time.Now()
	me.OrderCh <- zero

	require.Eventually(t, func() bool {
		admitted, _, _, _, _ := rec.counts()
		return admitted == 1
	}, 5*time.Second, 5*time.Millisecond)

	me.Stop()
	waitEngineDone(t, me)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.Empty(t, rec.doneTrades, "즉시 완료 주문은 OrderDone을 내지 않는다")
	require.Empty(t, rec.slices, "즉시 완료 주문은 Slice를 내지 않는다")
}

// Slice의 emitBlock은 finishOrder의 MarketOrderDone 블로킹까지 포함해야 한다.
//
// 빈 capacity=1 채널의 첫 send는 즉시 성공하므로 block을 재지 못한다.
// 채널을 미리 채워두고, consumer가 정해진 시각까지 수신하지 않게 만들어
// MarketOrderDone send가 반드시 그만큼 막히게 한다.
func TestSliceEmitBlockIncludesMarketOrderDone(t *testing.T) {
	const hold = 200 * time.Millisecond

	me := newTestEngine()
	rec := &recordedObservers{}
	rec.install(me)
	me.ExecutionCh = make(chan ExecutionEvent, 1)
	me.ExecutionCh <- ExecutionEvent{} // 미리 채워 다음 send가 막히게 한다
	go func() {
		for range me.SnapshotCh {
		}
	}()

	release := make(chan struct{})
	go func() {
		<-release
		for range me.ExecutionCh {
		}
	}()

	me.Start()
	market := &Order{
		ID: 1, UserID: 1, CoinSymbol: "BTC", Side: model.OrderSideBuy,
		QuoteAmount: decimal.NewFromInt(50000),
		OrderType:   model.OrderTypeMarket, EnqueuedAt: time.Now(),
	}
	me.OrderCh <- market

	time.Sleep(hold)
	close(release)

	require.Eventually(t, func() bool {
		_, _, _, _, slices := rec.counts()
		return slices == 1
	}, 5*time.Second, 5*time.Millisecond)

	rec.mu.Lock()
	block := rec.slices[0].emitBlock
	rec.mu.Unlock()
	require.Greater(t, block, hold/2,
		"Slice가 finishOrder보다 먼저 불려 MarketOrderDone 블로킹이 빠졌다")

	me.Stop()
	waitEngineDone(t, me)
}

func TestNilObserversAreSafe(t *testing.T) {
	me := newTestEngine()
	drainAll(me)
	me.Start()
	order := stopTestLimitOrder(1, model.OrderSideSell, 50000, 1)
	me.OrderCh <- order
	me.Stop()
	waitEngineDone(t, me)
}

func drainAll(me *MatchingEngine) {
	go func() {
		for range me.ExecutionCh {
		}
	}()
	go func() {
		for range me.SnapshotCh {
		}
	}()
}
```

`internal/metrics/matching_observers_test.go`:

```go
package metrics

import (
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

// histSample은 histogram의 표본 수와 합계를 읽는다.
// CollectAndCount는 metric family의 존재만 확인하므로, 콜백이 값을 하나도
// 기록하지 않아도 1을 돌려준다 — 판별력이 없다.
func histSample(t *testing.T, c prometheus.Collector) (uint64, float64) {
	t.Helper()
	ch := make(chan prometheus.Metric, 8)
	c.Collect(ch)
	close(ch)
	var count uint64
	var sum float64
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		require.NotNil(t, pb.Histogram)
		count += pb.Histogram.GetSampleCount()
		sum += pb.Histogram.GetSampleSum()
	}
	return count, sum
}

// 전역 promauto 컬렉터를 공유하므로 histogram 6종은 이 테스트가 독점한다.
// EmitBlock은 라벨로 분리한다.
func TestNewMatchingEngineObserversFeedsAllMetrics(t *testing.T) {
	obs := NewMatchingEngineObservers()

	obs.Turn(3 * time.Millisecond)
	obs.Slice(5, 2*time.Millisecond)
	obs.OrderAdmitted(7 * time.Millisecond)
	obs.OrderDone(11)
	obs.Cancel(13 * time.Millisecond)
	obs.EmitBlock(matching.EmitTrade, time.Millisecond)
	obs.Yield()

	cases := []struct {
		name      string
		metric    string
		collector prometheus.Collector
		wantSum   float64
	}{
		{"turn", "matching_engine_turn_duration_seconds", MatchingEngineTurnDuration, 0.003},
		{"slice", "matching_engine_matches_per_slice", MatchingEngineMatchesPerSlice, 5},
		{"emit_per_slice", "matching_engine_emit_block_per_slice_seconds", MatchingEngineEmitBlockPerSlice, 0.002},
		{"order_wait", "matching_engine_order_queue_wait_seconds", MatchingEngineOrderQueueWait, 0.007},
		{"per_order", "matching_engine_executions_per_order", MatchingEngineExecutionsPerOrder, 11},
		{"cancel_wait", "matching_engine_cancel_queue_wait_seconds", MatchingEngineCancelQueueWait, 0.013},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			count, sum := histSample(t, tc.collector)
			require.Equal(t, uint64(1), count, "표본이 기록되지 않았다")
			require.InDelta(t, tc.wantSum, sum, 1e-9, "기록된 값이 다르다")
			require.Equal(t, 1, testutil.CollectAndCount(tc.collector, tc.metric), "지표 이름")
		})
	}

	require.Equal(t, float64(1), testutil.ToFloat64(MatchingEngineQuantumYields))

	tradeCount, tradeSum := histSample(t, MatchingEngineEmitBlock.WithLabelValues(string(matching.EmitTrade)))
	require.Equal(t, uint64(1), tradeCount)
	require.InDelta(t, 0.001, tradeSum, 1e-9)
}

func TestEmitBlockIsLabeledByKind(t *testing.T) {
	obs := NewMatchingEngineObservers()
	obs.EmitBlock(matching.EmitMarketDone, 2*time.Millisecond)
	obs.EmitBlock(matching.EmitCancelled, 4*time.Millisecond)

	doneCount, doneSum := histSample(t, MatchingEngineEmitBlock.WithLabelValues(string(matching.EmitMarketDone)))
	require.Equal(t, uint64(1), doneCount)
	require.InDelta(t, 0.002, doneSum, 1e-9)

	cancelCount, cancelSum := histSample(t, MatchingEngineEmitBlock.WithLabelValues(string(matching.EmitCancelled)))
	require.Equal(t, uint64(1), cancelCount)
	require.InDelta(t, 0.004, cancelSum, 1e-9)
}
```

`cmd/main_quantum_wiring_test.go`:

```go
package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// main()에서 SetObservers 호출이 지워지면 계측이 조용히 사라진다.
// 런타임 테스트로는 잡히지 않으므로 소스 계약으로 고정한다.
func TestMainWiresMatchingEngineObservers(t *testing.T) {
	source, err := os.ReadFile("main.go")
	require.NoError(t, err)
	require.Contains(t, string(source), "me.SetObservers(metrics.NewMatchingEngineObservers())")
}
```

- [ ] **Step 5: focused test를 돌린다**

```powershell
cd Go-exchange-back; go test -count=1 ./internal/matching -run 'TestObservers|TestCancelObserver|TestImmediateCompletionEmits|TestSliceEmitBlock|TestNilObservers' -v; go test -count=1 ./internal/metrics -run 'TestNewMatchingEngineObservers|TestEmitBlockIsLabeled' -v; go test -count=1 ./cmd -run TestMainWires -v
```

Expected: 전부 PASS. 기존 matching 테스트도 함께 확인한다:

```powershell
cd Go-exchange-back; go test -count=1 ./internal/matching
```

Expected: `ok`.

---

## Task 2: 통계 유틸과 opt-in 하니스 (H0/H1/H2-1/H2-5000/H4)

**Files:**
- Create: `internal/matching/quantum_stats_test.go` (빌드 태그 **없음** — 순수 함수라 CI에서 돌아도 안전)
- Create: `internal/matching/quantum_harness_test.go` (`//go:build quantumharness`)

**Produces:** `latencySamples`, `percentile`, `medianDuration`, 하니스 JSON 스키마(`scenarioResult`).

**하니스가 지켜야 할 규칙.** 하나라도 깨지면 나온 숫자를 비교에 쓸 수 없다.

| # | 규칙 | 깨졌을 때 |
|---|---|---|
| B1 | `Observers`는 `Start()` 전에 한 번만 설정 | data race |
| B2 | goroutine마다 자기 `*rand.Rand` | `rand.Rand`는 동시 사용 불가. 시드를 고정해도 재현 안 됨 |
| B3 | setup 주문이 **전부 처리된 뒤** 측정 시작 | setup 표본이 probe 표본에 섞임 |
| B4 | sweep은 **첫 trade 관측**을 시작 장벽으로 | `time.Sleep`은 sweep 시작 전이거나 이미 끝난 뒤를 잼 |
| B5 | 완료는 **producer 종료가 아니라 observer 관측**으로 판정 | 큐에 넣기만 하고 처리 전에 결과를 만듦 |
| B6 | 장벽 deadline 초과는 **censored** | 미완료를 성공으로 세거나 조용히 버림 |
| B7 | 취소 ID는 **중복 없는 실존 ID** | 복원추출하면 두 번째가 not-found라 successful cancel을 못 잼 |

- [ ] **Step 1: 통계 유틸을 쓴다**

`internal/matching/quantum_stats_test.go`:

```go
package matching

import (
	"math"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// latencySamples는 관측 표본과 censored(관측 창 안에 끝나지 않은) 개수를
// 함께 들고 있다. censored를 버리면 baseline이 실제보다 좋아 보이고
// 전후 비교가 무의미해진다.
type latencySamples struct {
	observed []time.Duration
	censored int
}

func (s latencySamples) p99Infinite() bool { return s.censored > 0 }

// percentile은 nearest-rank 정의를 쓴다: 정렬된 표본에서 ceil(q*n)번째 값.
// 0-based 인덱스로는 ceil(q*n)-1이다.
//
// int(q*n)을 쓰면 n=100에서 p50이 51번째, p99가 100번째가 되어 한 칸씩
// 밀린다. 꼬리를 보는 지표에서 이 한 칸은 판정을 뒤집을 수 있다.
func (s latencySamples) percentile(q float64) time.Duration {
	if len(s.observed) == 0 {
		return 0
	}
	sorted := append([]time.Duration{}, s.observed...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := int(math.Ceil(q * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

func medianDuration(runs []time.Duration) time.Duration {
	sorted := append([]time.Duration{}, runs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

func TestPercentileNearestRank(t *testing.T) {
	values := make([]time.Duration, 100)
	for i := range values {
		values[i] = time.Duration(i+1) * time.Millisecond
	}
	s := latencySamples{observed: values}
	require.False(t, s.p99Infinite())
	require.Equal(t, 99*time.Millisecond, s.percentile(0.99), "ceil(99)=99번째")
	require.Equal(t, 50*time.Millisecond, s.percentile(0.50), "ceil(50)=50번째")
	require.Equal(t, 100*time.Millisecond, s.percentile(1.0))
	require.Equal(t, time.Millisecond, s.percentile(0.0), "rank 0은 1로 클램프")

	odd := latencySamples{observed: []time.Duration{1, 2, 3}}
	require.Equal(t, time.Duration(1), odd.percentile(0.01), "ceil(0.03)=1번째")
	require.Equal(t, time.Duration(2), odd.percentile(0.50), "ceil(1.5)=2번째")
	require.Equal(t, time.Duration(3), odd.percentile(0.99), "ceil(2.97)=3번째")

	require.Equal(t, time.Duration(0), latencySamples{}.percentile(0.99))
	require.True(t, latencySamples{observed: []time.Duration{1}, censored: 1}.p99Infinite())
}

func TestMedianOfRuns(t *testing.T) {
	require.Equal(t, time.Duration(3), medianDuration([]time.Duration{5, 1, 3, 2, 4}))
	require.Equal(t, time.Duration(2), medianDuration([]time.Duration{3, 1, 2}))
}
```

- [ ] **Step 2: 하니스 골격을 쓴다**

`internal/matching/quantum_harness_test.go`:

```go
//go:build quantumharness

package matching

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

const (
	harnessSeedBase  = 20260830
	harnessWatchdog  = 30 * time.Second
	harnessOutputDir = "../../_workspace/quantum"
)

// harnessRuns는 GOEXCHANGE_QUANTUM_RUNS로 준다. baseline·1차 탐색은 3,
// 확증은 5다.
func harnessRuns(t *testing.T) int {
	t.Helper()
	n, err := strconv.Atoi(os.Getenv("GOEXCHANGE_QUANTUM_RUNS"))
	require.NoError(t, err, "GOEXCHANGE_QUANTUM_RUNS를 지정해야 한다")
	require.Greater(t, n, 0)
	return n
}

// InfNs는 censored 때문에 분위수가 정의되지 않음을 뜻한다.
// JSON에는 Infinity를 쓸 수 없다. 집계기가 이 값을 보고 에러를 낸다.
const InfNs int64 = -1

type scenarioResult struct {
	Scenario              string `json:"scenario"`
	Seed                  int64  `json:"seed"`
	MaxMatchesPerTurn     int    `json:"max_matches_per_turn"`
	MaxConsecutiveCancels int    `json:"max_consecutive_cancels"`
	OrderWaitP50Ns        int64  `json:"order_wait_p50_ns"`
	OrderWaitP95Ns        int64  `json:"order_wait_p95_ns"`
	OrderWaitP99Ns        int64  `json:"order_wait_p99_ns"`
	OrderWaitMaxNs        int64  `json:"order_wait_max_ns"`
	OrderCensored         int    `json:"order_censored"`
	CancelWaitP99Ns       int64  `json:"cancel_wait_p99_ns"`
	CancelCensored        int    `json:"cancel_censored"`
	EmitBlockP99Ns        int64  `json:"emit_block_p99_ns"`
	SweepTotalNs          int64  `json:"sweep_total_ns"`
	SweepCensored         int    `json:"sweep_censored"`
	MaxSnapshotGapNs      int64  `json:"max_snapshot_gap_ns"`
	OrderSamples          int    `json:"order_samples"`
	CancelSamples         int    `json:"cancel_samples"`
}

// harnessCollector는 관측 콜백을 모은다. measuring이 false인 동안의 표본은
// 버린다(B3). snapshot gap도 measuring 구간만 기록한다 — setup 구간의
// gap이 섞이면 H4가 실제보다 나빠 보인다.
type harnessCollector struct {
	mu        sync.Mutex
	measuring bool

	orderWaits  latencySamples
	cancelWaits latencySamples
	emitBlocks  latencySamples

	snapshotGaps []time.Duration
	lastSnapshot time.Time

	firstTrade   chan struct{}
	firstTradeOK bool
	doneOrders   chan int
	admitted     chan struct{}
	cancels      chan struct{}
}

func newHarnessCollector() *harnessCollector {
	return &harnessCollector{
		firstTrade: make(chan struct{}),
		doneOrders: make(chan int, 65536),
		admitted:   make(chan struct{}, 65536),
		cancels:    make(chan struct{}, 65536),
	}
}

// observers는 Start() 전에 한 번만 설정된다(B1).
func (c *harnessCollector) observers() EngineObservers {
	return EngineObservers{
		OrderAdmitted: func(d time.Duration) {
			c.mu.Lock()
			if c.measuring {
				c.orderWaits.observed = append(c.orderWaits.observed, d)
			}
			c.mu.Unlock()
			select {
			case c.admitted <- struct{}{}:
			default:
			}
		},
		OrderDone: func(trades int) {
			select {
			case c.doneOrders <- trades:
			default:
			}
		},
		Cancel: func(d time.Duration) {
			c.mu.Lock()
			if c.measuring {
				c.cancelWaits.observed = append(c.cancelWaits.observed, d)
			}
			c.mu.Unlock()
			select {
			case c.cancels <- struct{}{}:
			default:
			}
		},
		EmitBlock: func(kind EmitKind, d time.Duration) {
			c.mu.Lock()
			if c.measuring {
				c.emitBlocks.observed = append(c.emitBlocks.observed, d)
			}
			if kind == EmitTrade && !c.firstTradeOK {
				c.firstTradeOK = true
				close(c.firstTrade)
			}
			c.mu.Unlock()
		},
	}
}

// startMeasuring은 snapshot 상태도 초기화한다. setup 구간의 gap을 H4에
// 포함하면 안 된다.
func (c *harnessCollector) startMeasuring() {
	c.mu.Lock()
	c.measuring = true
	c.snapshotGaps = nil
	c.lastSnapshot = time.Now()
	c.mu.Unlock()
}

// closeSnapshotWindow는 마지막 스냅샷 이후 측정 종료까지의 간격을 닫아
// 기록한다. 이걸 빼면 sweep 끝 무렵의 긴 공백이 통째로 사라진다.
func (c *harnessCollector) closeSnapshotWindow() {
	c.mu.Lock()
	if c.measuring && !c.lastSnapshot.IsZero() {
		c.snapshotGaps = append(c.snapshotGaps, time.Since(c.lastSnapshot))
	}
	c.measuring = false
	c.mu.Unlock()
}

func (c *harnessCollector) censor(order, cancel int) {
	c.mu.Lock()
	c.orderWaits.censored += order
	c.cancelWaits.censored += cancel
	c.mu.Unlock()
}

// waitSignals는 n건의 관측을 기다린다. deadline을 넘기면 false를 반환한다(B5·B6).
func waitSignals(ch <-chan struct{}, n int, deadline time.Duration) (int, bool) {
	timeout := time.After(deadline)
	for got := 0; got < n; got++ {
		select {
		case <-ch:
		case <-timeout:
			return got, false
		}
	}
	return n, true
}

func waitDoneSignals(ch <-chan int, n int, deadline time.Duration) bool {
	timeout := time.After(deadline)
	for got := 0; got < n; got++ {
		select {
		case <-ch:
		case <-timeout:
			return false
		}
	}
	return true
}

func (c *harnessCollector) waitFirstTrade(deadline time.Duration) bool {
	select {
	case <-c.firstTrade:
		return true
	case <-time.After(deadline):
		return false
	}
}

func (c *harnessCollector) result(scenario string, seed int64, cfg QuantumConfig,
	sweepTotal time.Duration, sweepCensored int) scenarioResult {
	c.mu.Lock()
	defer c.mu.Unlock()

	orderP99 := InfNs
	if !c.orderWaits.p99Infinite() {
		orderP99 = int64(c.orderWaits.percentile(0.99))
	}
	cancelP99 := InfNs
	if !c.cancelWaits.p99Infinite() {
		cancelP99 = int64(c.cancelWaits.percentile(0.99))
	}
	sweepNs := int64(sweepTotal)
	if sweepCensored > 0 {
		sweepNs = InfNs
	}
	var maxGap time.Duration
	for _, g := range c.snapshotGaps {
		if g > maxGap {
			maxGap = g
		}
	}
	return scenarioResult{
		Scenario:              scenario,
		Seed:                  seed,
		MaxMatchesPerTurn:     cfg.MaxMatchesPerTurn,
		MaxConsecutiveCancels: cfg.MaxConsecutiveCancels,
		OrderWaitP50Ns:        int64(c.orderWaits.percentile(0.50)),
		OrderWaitP95Ns:        int64(c.orderWaits.percentile(0.95)),
		OrderWaitP99Ns:        orderP99,
		OrderWaitMaxNs:        int64(c.orderWaits.percentile(1.0)),
		OrderCensored:         c.orderWaits.censored,
		CancelWaitP99Ns:       cancelP99,
		CancelCensored:        c.cancelWaits.censored,
		EmitBlockP99Ns:        int64(c.emitBlocks.percentile(0.99)),
		SweepTotalNs:          sweepNs,
		SweepCensored:         sweepCensored,
		MaxSnapshotGapNs:      int64(maxGap),
		OrderSamples:          len(c.orderWaits.observed),
		CancelSamples:         len(c.cancelWaits.observed),
	}
}

// writeResults는 GOEXCHANGE_QUANTUM_OUTDIR 하위에 쓴다. baseline과 후보
// 산출물이 같은 디렉터리에 섞이면 baseline이 덮인다.
func writeResults(t *testing.T, name string, results []scenarioResult) {
	t.Helper()
	sub := os.Getenv("GOEXCHANGE_QUANTUM_OUTDIR")
	require.NotEmpty(t, sub, "GOEXCHANGE_QUANTUM_OUTDIR을 지정해야 한다")
	dir := filepath.Join(harnessOutputDir, sub)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, name+".json")
	require.NoFileExists(t, path, "같은 산출물을 덮어쓰려 한다 — 디렉터리를 확인하라")
	data, err := json.MarshalIndent(results, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))
	t.Logf("wrote %s (%d runs)", path, len(results))
}

func harnessQuantum(me *MatchingEngine) QuantumConfig {
	cfg := QuantumConfig{
		MaxMatchesPerTurn:     me.maxMatchesPerTurn,
		MaxConsecutiveCancels: me.maxConsecutiveCancels,
	}
	if v, err := strconv.Atoi(os.Getenv("GOEXCHANGE_MATCHING_MAX_MATCHES_PER_TURN")); err == nil && v > 0 {
		cfg.MaxMatchesPerTurn = v
	}
	if v, err := strconv.Atoi(os.Getenv("GOEXCHANGE_MATCHING_MAX_CONSECUTIVE_CANCELS")); err == nil && v > 0 {
		cfg.MaxConsecutiveCancels = v
	}
	return cfg
}

func harnessEngine(t *testing.T, c *harnessCollector) (*MatchingEngine, QuantumConfig, func()) {
	t.Helper()
	me := NewMatchingEngine()
	me.snapshotInterval = 100 * time.Millisecond
	cfg := harnessQuantum(me)
	me.maxMatchesPerTurn = cfg.MaxMatchesPerTurn
	me.maxConsecutiveCancels = cfg.MaxConsecutiveCancels
	me.Observers = c.observers() // B1

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range me.ExecutionCh {
		}
	}()
	go func() {
		defer wg.Done()
		for range me.SnapshotCh {
			c.mu.Lock()
			if c.measuring {
				c.snapshotGaps = append(c.snapshotGaps, time.Since(c.lastSnapshot))
				c.lastSnapshot = time.Now()
			}
			c.mu.Unlock()
		}
	}()

	me.Start()
	return me, cfg, func() {
		me.Stop()
		select {
		case <-me.Done():
		case <-time.After(60 * time.Second):
			t.Fatal("harness engine did not stop")
		}
		wg.Wait()
	}
}

func harnessLimitOrder(rng *rand.Rand, id uint, side model.OrderSide, price int64, amount int64) *Order {
	return &Order{
		ID:         id,
		UserID:     uint(rng.Intn(1000) + 1),
		CoinSymbol: "BTC",
		Side:       side,
		Price:      decimal.NewFromInt(price),
		Amount:     decimal.NewFromInt(amount),
		OrderType:  model.OrderTypeLimit,
		CreatedAt:  time.Now(),
		EnqueuedAt: time.Now(),
	}
}
```

- [ ] **Step 3: H0/H1 시나리오를 쓴다**

H0과 H1은 **주문 수·producer 수·consumer 속도가 반드시 같고 취소 수만 다르다.** C2의 상한이 H0에서 유도되므로 다른 파라미터가 다르면 비교가 성립하지 않는다.

취소는 **`CancelCh`에 직접 넣는다.** 동기 `CancelOrder`는 응답을 기다리므로 한 goroutine에서 부르면 큐 깊이가 1을 넘지 않아 flood가 되지 않는다.

취소 ID는 **중복 없는 실존 ID**를 쓴다(B7). 복원추출하면 두 번째부터 not-found가 되어 successful cancel 처리 비용을 재지 못한다.

```go
const (
	floodRestingOrders = 20000 // 취소 대상. 중복 없이 뽑으려면 취소 수 이상이어야 한다
	floodProbeOrders   = 500
	floodCancels       = 15000
)

// runFloodScenario는 H0(cancelCount=0)과 H1의 공통 골격이다.
func runFloodScenario(t *testing.T, seed int64, cancelCount int) scenarioResult {
	require.LessOrEqual(t, cancelCount, floodRestingOrders,
		"중복 없는 실존 ID를 쓰려면 resting 주문이 취소 수 이상이어야 한다")

	setupRNG := rand.New(rand.NewSource(seed))     // B2
	cancelRNG := rand.New(rand.NewSource(seed + 1))
	probeRNG := rand.New(rand.NewSource(seed + 2))

	c := newHarnessCollector()
	me, cfg, shutdown := harnessEngine(t, c)
	defer shutdown()

	for i := 0; i < floodRestingOrders; i++ {
		me.OrderCh <- harnessLimitOrder(setupRNG, uint(i+1), model.OrderSideSell, int64(60000+i), 1)
	}
	// B3: setup이 전부 처리된 뒤에 측정을 켠다.
	_, ok := waitSignals(c.admitted, floodRestingOrders, harnessWatchdog)
	require.True(t, ok, "setup 주문이 admit되지 않았다")
	require.True(t, waitDoneSignals(c.doneOrders, floodRestingOrders, harnessWatchdog),
		"setup 주문이 완결되지 않았다")
	c.startMeasuring()

	// B7: 중복 없는 실존 ID. 셔플로 앞 cancelCount개를 취한다.
	perm := cancelRNG.Perm(floodRestingOrders)[:cancelCount]

	var flood sync.WaitGroup
	if cancelCount > 0 {
		flood.Add(1)
		go func() {
			defer flood.Done()
			for _, idx := range perm {
				me.CancelCh <- CancelOrderCommand{
					CoinSymbol: "BTC",
					OrderID:    uint(idx + 1),
					Side:       model.OrderSideSell,
					Price:      decimal.NewFromInt(int64(60000 + idx)),
					EnqueuedAt: time.Now(),
				}
			}
		}()
	}

	start := time.Now()
	go func() {
		for i := 0; i < floodProbeOrders; i++ {
			// 어떤 것과도 체결되지 않는 가격 — 측정 대상은 큐 대기다.
			me.OrderCh <- harnessLimitOrder(probeRNG, uint(floodRestingOrders+i+1),
				model.OrderSideBuy, 100, 1)
		}
	}()

	// B5·B6: producer 종료가 아니라 observer 관측을 기다린다.
	got, ok := waitSignals(c.admitted, floodProbeOrders, harnessWatchdog)
	elapsed := time.Since(start)
	if !ok {
		c.censor(floodProbeOrders-got, 0)
		t.Logf("seed=%d cancels=%d: probe %d/%d censored",
			seed, cancelCount, floodProbeOrders-got, floodProbeOrders)
	}
	if cancelCount > 0 {
		if n, ok := waitSignals(c.cancels, cancelCount, harnessWatchdog); !ok {
			c.censor(0, cancelCount-n)
			t.Logf("seed=%d: 취소 %d/%d censored", seed, cancelCount-n, cancelCount)
		}
	}
	flood.Wait()
	c.closeSnapshotWindow()

	name := "H1"
	if cancelCount == 0 {
		name = "H0"
	}
	return c.result(name, seed, cfg, elapsed, 0)
}

func TestHarnessH0NoCancelControl(t *testing.T) {
	runs := harnessRuns(t)
	results := make([]scenarioResult, 0, runs)
	for i := 0; i < runs; i++ {
		results = append(results, runFloodScenario(t, harnessSeedBase+int64(i)*100, 0))
	}
	writeResults(t, "H0", results)
}

func TestHarnessH1CancelFlood(t *testing.T) {
	runs := harnessRuns(t)
	results := make([]scenarioResult, 0, runs)
	for i := 0; i < runs; i++ {
		results = append(results, runFloodScenario(t, harnessSeedBase+int64(i)*100, floodCancels))
	}
	writeResults(t, "H1", results)
}
```

- [ ] **Step 4: H2/H4 시나리오를 쓴다**

`SweepTotalNs`는 **taker의 `OrderDone`까지**의 시간이다(B5). 첫 취소 응답까지가 아니다. 취소는 **cancel observer 1건을 deadline 안에 확인한 뒤에** 결과를 만든다.

```go
// runSweepScenario는 makerCount개 maker를 쓸어가는 taker를 넣고,
// sweep이 실제로 시작된 뒤(B4) 취소 1건을 던져 그 취소가 언제 처리되는지 잰다.
func runSweepScenario(t *testing.T, scenario string, seed int64, makerCount int) scenarioResult {
	setupRNG := rand.New(rand.NewSource(seed))
	takerRNG := rand.New(rand.NewSource(seed + 1))

	c := newHarnessCollector()
	me, cfg, shutdown := harnessEngine(t, c)
	defer shutdown()

	for i := 0; i < makerCount; i++ {
		me.OrderCh <- harnessLimitOrder(setupRNG, uint(i+1), model.OrderSideSell, 50000, 1)
	}
	// sweep이 닿지 않는 가격의 취소 대상.
	victimID := uint(makerCount + 1)
	me.OrderCh <- harnessLimitOrder(setupRNG, victimID, model.OrderSideSell, 90000, 1)

	_, ok := waitSignals(c.admitted, makerCount+1, harnessWatchdog)
	require.True(t, ok, "maker setup 미완료")
	require.True(t, waitDoneSignals(c.doneOrders, makerCount+1, harnessWatchdog), "maker setup 미완결")
	c.startMeasuring()

	start := time.Now()
	me.OrderCh <- harnessLimitOrder(takerRNG, uint(makerCount+2), model.OrderSideBuy, 50000, int64(makerCount))

	// B4: 첫 trade를 관측해야 sweep이 실제로 진행 중이다.
	if !c.waitFirstTrade(harnessWatchdog) {
		c.closeSnapshotWindow()
		t.Logf("seed=%d makers=%d: sweep이 시작되지 않았다", seed, makerCount)
		return c.result(scenario, seed, cfg, 0, 1)
	}

	go func() {
		me.CancelCh <- CancelOrderCommand{
			CoinSymbol: "BTC", OrderID: victimID,
			Side: model.OrderSideSell, Price: decimal.NewFromInt(90000),
			EnqueuedAt: time.Now(),
		}
	}()

	// 취소 observer 1건을 deadline 안에 확인한다.
	if _, ok := waitSignals(c.cancels, 1, harnessWatchdog); !ok {
		c.censor(0, 1)
		t.Logf("seed=%d makers=%d: sweep 중 취소가 censored", seed, makerCount)
	}

	// B5: taker의 OrderDone이 sweep 완료다.
	sweepCensored := 0
	if !waitDoneSignals(c.doneOrders, 1, harnessWatchdog) {
		sweepCensored = 1
		t.Logf("seed=%d makers=%d: sweep이 완료되지 않았다", seed, makerCount)
	}
	elapsed := time.Since(start)
	c.closeSnapshotWindow()

	return c.result(scenario, seed, cfg, elapsed, sweepCensored)
}

func TestHarnessH2Small(t *testing.T) {
	runs := harnessRuns(t)
	results := make([]scenarioResult, 0, runs)
	for i := 0; i < runs; i++ {
		results = append(results, runSweepScenario(t, "H2-1", harnessSeedBase+int64(i)*100+1, 1))
	}
	writeResults(t, "H2-1", results)
}

func TestHarnessH2Large(t *testing.T) {
	runs := harnessRuns(t)
	results := make([]scenarioResult, 0, runs)
	for i := 0; i < runs; i++ {
		results = append(results, runSweepScenario(t, "H2-5000", harnessSeedBase+int64(i)*100+2, 5000))
	}
	writeResults(t, "H2-5000", results)
}

// H4는 H2-5000과 같은 부하이고 보는 것만 다르다(스냅샷 간격).
// 시드를 달리해 H2-5000과 독립 표본으로 만든다.
func TestHarnessH4SnapshotFreshness(t *testing.T) {
	runs := harnessRuns(t)
	results := make([]scenarioResult, 0, runs)
	for i := 0; i < runs; i++ {
		results = append(results, runSweepScenario(t, "H4", harnessSeedBase+int64(i)*100+3, 5000))
	}
	writeResults(t, "H4", results)
}
```

**하니스는 여기서 완성된다. 이후 태스크에서 하니스 코드를 수정하지 않는다** — baseline과 후보가 같은 코드에서 나와야 한다는 계약이 그렇지 않으면 깨진다.

- [ ] **Step 5: 통계 테스트와 컴파일을 확인한다**

```powershell
cd Go-exchange-back; go test -count=1 ./internal/matching -run 'TestPercentileNearestRank|TestMedianOfRuns' -v
go vet -tags quantumharness ./internal/matching
go test -count=1 ./internal/matching -run TestHarness
```

Expected: 통계 2건 PASS / vet 출력 없음 / 마지막은 `no tests to run` (태그 없이는 하니스가 컴파일되지 않는다).

- [ ] **Step 6: 커밋 1 — 축소된 설계·계획 문서**

이미 커밋돼 있어야 한다. 아니면 여기서 `commit-message` 스킬로 커밋한다.

---

## Task 3: baseline 수집

**Files:** `_workspace/quantum/baseline/*.json`, `_workspace/quantum/baseline-manifest.md`, `_workspace/quantum/bench-{before,after}.txt`

- [ ] **Step 1: 계측 전후 벤치를 잰다**

계측 오버헤드를 지금 기록하지 않으면 나중에 quantum 비용과 구분할 수 없다.

```powershell
cd Go-exchange-back
git worktree add ../_gx-baseline HEAD~1
Push-Location ..\_gx-baseline
go test -count=1 ./internal/matching -bench . -benchmem -benchtime 3s -run '^$' | Out-File -Encoding utf8 ..\Go-exchange-back\_workspace\quantum\bench-before.txt
Pop-Location
git worktree remove ../_gx-baseline
go test -count=1 ./internal/matching -bench . -benchmem -benchtime 3s -run '^$' | Out-File -Encoding utf8 _workspace\quantum\bench-after.txt
```

`HEAD~1`은 계측 커밋 직전이다. 커밋 순서가 다르면 SHA를 직접 지정한다.

- [ ] **Step 2: 계측 + 하니스를 커밋한다 (커밋 2)**

`commit-message` 스킬로 메시지를 만든 뒤:

```powershell
cd Go-exchange-back; git add internal/matching/observers.go internal/matching/engine.go internal/matching/sharded.go internal/matching/engine_observers_test.go internal/matching/quantum_stats_test.go internal/matching/quantum_harness_test.go internal/metrics/ cmd/main.go cmd/main_quantum_wiring_test.go
```

커밋 후 SHA를 기록한다 — 이것이 **baseline SHA**다.

```powershell
cd Go-exchange-back; git rev-parse HEAD
```

- [ ] **Step 3: 그 SHA에서 baseline을 3회 수집한다**

```powershell
cd Go-exchange-back
$env:GOEXCHANGE_QUANTUM_OUTDIR = "baseline"
$env:GOEXCHANGE_QUANTUM_RUNS = "3"
Remove-Item Env:GOEXCHANGE_MATCHING_MAX_MATCHES_PER_TURN -ErrorAction SilentlyContinue
Remove-Item Env:GOEXCHANGE_MATCHING_MAX_CONSECUTIVE_CANCELS -ErrorAction SilentlyContinue
go test -tags quantumharness -count=1 ./internal/matching -timeout 90m -v -run 'TestHarnessH0NoCancelControl|TestHarnessH1CancelFlood|TestHarnessH2Small|TestHarnessH2Large|TestHarnessH4SnapshotFreshness'
Remove-Item Env:GOEXCHANGE_QUANTUM_OUTDIR, Env:GOEXCHANGE_QUANTUM_RUNS
```

Expected: `_workspace/quantum/baseline/`에 **파일 5개** — `H0.json`, `H1.json`, `H2-1.json`, `H2-5000.json`, `H4.json`. 각 3회차.

```powershell
cd Go-exchange-back; (Get-ChildItem _workspace\quantum\baseline\*.json).Count
```

Expected: `5`.

H1에서 censored가 나오는 것이 **정상**이다 — 그것이 고치려는 기아의 크기다. 다만 **H0에서 censored가 나오면 P1 중단**이다. 취소가 없는데 주문이 admit되지 않는다면 하니스가 잘못됐거나 부하가 과도한 것이고, 어느 쪽이든 C2 상한을 유도할 수 없다.

- [ ] **Step 4: baseline manifest를 쓴다**

`_workspace/quantum/baseline-manifest.md`에 baseline SHA, 수집 일시, 시나리오별 censored, C1·C2·C3 상한 계산값, 계측 오버헤드(bench 전후 ns/op 변화율)를 **실제 값으로** 적는다. `<값>` 같은 자리표시자를 남기지 않는다.

---

## Task 4: 조각화와 turn 상태 기계

**Files:** `internal/matching/engine.go`, `internal/matching/engine_slice_test.go`(신규), `internal/matching/engine_scheduler_test.go`(신규)

**Produces:** `matchSlice(book, order, budget) (trades int, done bool)`, `finishOrder`, `admitOrder`, `activeSweep`, `runTurn`, `admitPhase`, `runSlice`, `latchOneNonBlocking`, `crashHook`.

**조각화와 스케줄러를 한 태스크로 합친다.** 나누면 `Slice` 지표가 사라지는 중간 상태가 생긴다 — Task 1의 `processOrder`가 `lastMatchTrades`로 세는데, `matchSlice`만 먼저 도입하면 그 카운터가 반환값과 이중으로 관리되고, `runSlice`가 없어 `Slice` 관측 지점이 없다.

- [ ] **Step 1: 네 match 루프에 budget을 넣는다**

시그니처를 `(book *OrderBook, order *Order, budget int) (int, bool)`로 바꾸고, 네 함수 모두 같은 형태로 고친다. `matchBuy` 기준:

```go
// budget 0은 무제한(public Match 전용) sentinel이다. budget > 0이면
// trades가 budget에 닿을 때 yield한다. 예산 검사는 루프 조건 **뒤**에 있다 —
// 예산 경계에서 정확히 전량 체결된 sweep이 같은 조각에서 done=true로
// 끝나야 빈 조각이 생기지 않는다.
func (me *MatchingEngine) matchBuy(book *OrderBook, order *Order, budget int) (int, bool) {
	trades := 0
	for order.Amount.GreaterThan(decimal.Zero) {
		if budget > 0 && trades >= budget {
			return trades, false
		}
		sellLevel, orderIndex, ok := bestMatchableSellOrder(book, order)
		if !ok {
			return trades, true
		}
		sellOrder := sellLevel.Orders.At(orderIndex)
		tradeQty := decimal.Min(order.Amount, sellOrder.Amount)
		if !tradeQty.GreaterThan(decimal.Zero) {
			return trades, true
		}
		order.Amount = order.Amount.Sub(tradeQty)
		order.FilledAmount = order.FilledAmount.Add(tradeQty)
		sellOrder.Amount = sellOrder.Amount.Sub(tradeQty)
		sellOrder.FilledAmount = sellOrder.FilledAmount.Add(tradeQty)
		if !sellOrder.Amount.GreaterThan(decimal.Zero) {
			sellLevel.Orders.Remove(orderIndex)
		}
		if sellLevel.Orders.Len() == 0 {
			book.SellOrders.Delete(sellLevel)
		}
		me.emitTrade(me.newTrade(order.CoinSymbol, sellLevel.Price, tradeQty, order.ID, sellOrder.ID))
		trades++
	}
	return trades, true
}
```

`matchSell`·`matchMarketSell`은 루프 조건이 `order.Amount`, `matchMarketBuy`는 `order.QuoteAmount`인 것만 다르다. 기존 `return`을 전부 `return trades, true`로 바꾸고, 루프 끝에 `trades++`, 함수 끝에 `return trades, true`를 둔다.

`emitTrade`의 `me.lastMatchTrades++`를 **지운다.** `matchSlice`가 값을 직접 반환하므로 두 메커니즘이 같은 값을 관리할 이유가 없다. `MatchingEngine`의 `lastMatchTrades` 필드도 지운다.

- [ ] **Step 2: `matchSlice`/`finishOrder`/`admitOrder`를 만든다**

`Match`를 교체한다.

```go
// activeSweep은 조각 사이에 살아남는 유일한 sweep 상태다. 재개에 필요한 것은
// 주문 포인터뿐이다 — 네 match 루프는 반복 사이에 지역 상태를 들고 가지 않고
// 매번 bestMatchable*로 상대를 새로 찾는다. 그것이 조각 사이의 취소를
// 반영하는 유일하게 안전한 방법이다.
type activeSweep struct {
	order  *Order
	book   *OrderBook
	trades int
}

// matchSlice는 최대 budget개의 체결을 만들고 돌아온다.
// done=true는 "더 체결할 수 없다"이고, budget 소진(done=false)과 다르다.
func (me *MatchingEngine) matchSlice(book *OrderBook, order *Order, budget int) (int, bool) {
	switch order.Side {
	case model.OrderSideBuy:
		if order.OrderType == model.OrderTypeMarket {
			return me.matchMarketBuy(book, order, budget)
		}
		return me.matchBuy(book, order, budget)
	case model.OrderSideSell:
		if order.OrderType == model.OrderTypeMarket {
			return me.matchMarketSell(book, order, budget)
		}
		return me.matchSell(book, order, budget)
	}
	return 0, true
}

// finishOrder는 마지막 조각에서만 부른다.
func (me *MatchingEngine) finishOrder(book *OrderBook, order *Order) {
	if order.OrderType == model.OrderTypeMarket {
		me.emitMarketOrderDone(order)
		return
	}
	if order.Amount.GreaterThan(decimal.Zero) {
		book.AddOrder(order)
		me.markDirty(order.CoinSymbol)
	}
}

// admitOrder는 주문을 슬롯에 올린다. 슬롯을 만들 수 없는 즉시 완료 주문은
// nil을 반환하되 observer는 정확히 1회 호출한다(nil 주문 제외).
// 이 가드들은 조각화 전 Match 앞머리의 조기 반환과 의미가 같아야 한다.
func (me *MatchingEngine) admitOrder(order *Order) *activeSweep {
	if order == nil {
		return nil
	}
	if !order.EnqueuedAt.IsZero() {
		me.Observers.orderAdmitted(time.Since(order.EnqueuedAt))
	}
	if !me.orderIsAdmissible(order) {
		me.observeMatchLatency(order)
		return nil
	}
	return &activeSweep{order: order, book: me.GetOrderBook(order.CoinSymbol)}
}

func (me *MatchingEngine) observeMatchLatency(order *Order) {
	if me.MatchLatencyObserver != nil && !order.EnqueuedAt.IsZero() {
		me.MatchLatencyObserver(time.Since(order.EnqueuedAt))
	}
}

// Match는 조각화 없이 주문을 끝까지 처리한다. 테스트·벤치 약 30곳이 이
// 시그니처를 쓰므로 그대로 둔다.
func (me *MatchingEngine) Match(order *Order) {
	sweep := me.admitOrder(order)
	if sweep == nil {
		return
	}
	me.matchSlice(sweep.book, sweep.order, 0)
	me.finishOrder(sweep.book, sweep.order)
}
```

`processOrder`를 **삭제한다.** `runSlice`가 대신한다.

- [ ] **Step 3: turn 상태 기계를 만든다**

`MatchingEngine`에 필드를 추가한다.

```go
	// quantum 값. 0은 무제한 sentinel과 충돌하므로 항상 >= 1이어야 한다.
	// 여기 기본값은 개발·테스트용이며, production 값은 탐색 결과로 확정한다.
	maxMatchesPerTurn     int
	maxConsecutiveCancels int

	// 스케줄러 지속 상태.
	activeSweep          *activeSweep
	shuttingDown         bool
	cancelsSinceProgress int
	pendingCancel        *CancelOrderCommand
	pendingOrder         *Order
	tickerDue            bool

	// crashHook은 테스트 전용이다. nil이 아니고 true를 반환하면 엔진 루프가
	// drain·flush·채널 close 없이 즉시 반환한다 — 프로세스 크래시와 같은
	// 상태를 만든다. 프로덕션에서는 항상 nil이다.
	//
	// Start() 전에 설치하고, arm은 hook 내부의 atomic으로 한다. 실행 중
	// 함수 필드를 교체하면 data race다.
	crashHook func() bool
```

`NewMatchingEngine`에 `maxMatchesPerTurn: 64`, `maxConsecutiveCancels: 32`를 넣는다(개발용 임시값).

`drainPendingWork`를 삭제하고 `Start`를 교체한다.

```go
func (me *MatchingEngine) Start() {
	go func() {
		defer close(me.doneCh)
		ticker := time.NewTicker(me.interval())
		defer ticker.Stop()
		for {
			if me.runTurn(ticker) {
				return
			}
		}
	}()
}

// runTurn은 quantum 경계 하나를 실행한다. true를 반환하면 엔진이 종료됐다.
//
// 계약: blocking select는 작업을 실행하지 않고 latch만 한다. latch된 작업은
// 다음 turn에서 처리된다 — 그래야 모든 실제 작업이 계측 구간 안에서 일어나고
// turn_duration에 블로킹 대기가 섞이지 않는다.
func (me *MatchingEngine) runTurn(ticker *time.Ticker) bool {
	turnStart := time.Now()

	// 0. turn 시작 상태를 고정한다. 5단계가 3단계 실행 **후의** activeSweep을
	//    다시 읽으면, 마지막 조각이 끝난 turn에서 P-a와 P-b/P-c/P-d가 함께
	//    발생해 "turn당 progress 정확히 하나" 불변식이 깨진다.
	hadActive := me.activeSweep != nil

	// 1. cancel phase — 상한 안에서만 드레인한다.
	for me.cancelsSinceProgress < me.maxConsecutiveCancels {
		cmd, ok := me.takeCancel()
		if !ok {
			break
		}
		me.processCancel(cmd)
		me.cancelsSinceProgress++
	}

	// 2. ticker phase
	if me.tickerDue {
		me.tickerDue = false
		me.flushSnapshots()
	} else {
		select {
		case <-ticker.C:
			me.flushSnapshots()
		default:
		}
	}

	// 3. slice phase
	if me.activeSweep != nil {
		me.runSlice()
		me.cancelsSinceProgress = 0 // P-a
	}

	// 3.5 crash hook (테스트 전용). 조각 사이에서만 발동한다.
	if me.crashHook != nil && me.crashHook() {
		return true // drain·flush·close 없이 즉시 종료
	}

	// 4. stop latch
	if !me.shuttingDown {
		select {
		case <-me.stopCh:
			me.shuttingDown = true
		default:
		}
	}

	// 5. admission phase — turn 시작 시점에 슬롯이 있었으면 건너뛴다.
	if !hadActive {
		me.admitPhase()
	}

	me.Observers.turn(time.Since(turnStart))

	// 6. 남은 일이 있으면 다음 turn으로.
	if me.activeSweep != nil || me.pendingCancel != nil || me.pendingOrder != nil || me.tickerDue {
		return false
	}

	// 7. shutdown drain 완료 판정.
	if me.shuttingDown {
		if me.latchOneNonBlocking() {
			return false
		}
		me.flushSnapshots()
		if me.ExecutionCh != nil {
			close(me.ExecutionCh)
		}
		close(me.SnapshotCh)
		return true
	}

	// 8. blocking select — 실행하지 않고 latch만 한다.
	orderCh := me.OrderCh
	if me.emitBackpressured() {
		orderCh = nil // 하류 포화 — 신규 유입만 억제한다.
	}
	select {
	case cmd := <-me.CancelCh:
		me.pendingCancel = &cmd
	case order := <-orderCh:
		me.pendingOrder = order
	case <-ticker.C:
		me.tickerDue = true
	case <-me.stopCh:
		me.shuttingDown = true
	}
	return false
}

func (me *MatchingEngine) takeCancel() (CancelOrderCommand, bool) {
	if me.pendingCancel != nil {
		cmd := *me.pendingCancel
		me.pendingCancel = nil
		return cmd, true
	}
	select {
	case cmd := <-me.CancelCh:
		return cmd, true
	default:
		return CancelOrderCommand{}, false
	}
}

// admitPhase는 P-b/P-c/P-d 중 하나를 반드시 발생시킨다.
//
// latch된 pendingOrder는 emitBackpressured() 검사보다 **먼저** 처리한다.
// 게이트는 OrderCh에서의 신규 유입만 억제하는 장치이고, 이미 엔진 안으로
// 들어온 작업에는 적용되지 않는다. 순서가 뒤집히면 cancel phase가 만든
// backpressure에 latch된 주문이 걸려 무기한 park한다.
func (me *MatchingEngine) admitPhase() {
	if me.pendingOrder != nil {
		order := me.pendingOrder
		me.pendingOrder = nil
		me.admit(order)
		me.cancelsSinceProgress = 0 // P-b
		return
	}
	if !me.shuttingDown && me.emitBackpressured() {
		me.cancelsSinceProgress = 0 // P-d
		return
	}
	select {
	case order := <-me.OrderCh:
		me.admit(order)
	default:
	}
	me.cancelsSinceProgress = 0 // P-b 또는 P-c
}

func (me *MatchingEngine) admit(order *Order) {
	if sweep := me.admitOrder(order); sweep != nil {
		me.activeSweep = sweep
	}
}

// runSlice는 조각 하나를 실행하고, 마지막 조각이면 슬롯을 비운다.
// Slice 관측은 finishOrder 뒤다 — 마지막 조각의 MarketOrderDone emit
// 블로킹이 sliceEmitBlock에 들어간 뒤여야 한다.
func (me *MatchingEngine) runSlice() {
	sweep := me.activeSweep
	me.sliceEmitBlock = 0
	trades, done := me.matchSlice(sweep.book, sweep.order, me.maxMatchesPerTurn)
	sweep.trades += trades
	if trades >= 1 {
		me.markDirty(sweep.order.CoinSymbol)
	}
	if !done {
		me.Observers.slice(trades, me.sliceEmitBlock)
		me.Observers.yield()
		return
	}
	me.finishOrder(sweep.book, sweep.order)
	me.Observers.slice(trades, me.sliceEmitBlock)
	me.observeMatchLatency(sweep.order)
	me.Observers.orderDone(sweep.trades)
	me.activeSweep = nil
}

// latchOneNonBlocking은 shutdown drain 전용이다. OrderCh에 게이트를 걸지
// 않는다 — 이미 접수된 작업이기 때문이다. HTTP·hold coordinator·cancel
// worker가 먼저 종료돼 새 producer가 없다는 lifecycle을 전제로 한다.
// 채널 len()으로 판정하지 않는다.
func (me *MatchingEngine) latchOneNonBlocking() bool {
	select {
	case cmd := <-me.CancelCh:
		me.pendingCancel = &cmd
		return true
	default:
	}
	select {
	case order := <-me.OrderCh:
		me.pendingOrder = order
		return true
	default:
	}
	return false
}
```

- [ ] **Step 4: 조각화 테스트를 쓴다**

`internal/matching/engine_slice_test.go`:

```go
package matching

import (
	"fmt"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

type comparableTrade struct {
	Price       string
	Quantity    string
	BuyOrderID  uint
	SellOrderID uint
}

// 동적 필드(trade ID, 시각, 시퀀스, engine ID)는 실행마다 달라지므로
// 비교에서 뺀다. 구조체 리터럴로 비교해 새 필드 추가 시 컴파일이 깨지게 한다.
func collectTrades(events chan ExecutionEvent) []comparableTrade {
	close(events)
	var out []comparableTrade
	for e := range events {
		if e.Trade == nil {
			continue
		}
		out = append(out, comparableTrade{
			Price:       e.Trade.Price.String(),
			Quantity:    e.Trade.Quantity.String(),
			BuyOrderID:  e.Trade.BuyOrderID,
			SellOrderID: e.Trade.SellOrderID,
		})
	}
	return out
}

func runToCompletion(budget, makers int) []comparableTrade {
	me := NewMatchingEngine()
	events := make(chan ExecutionEvent, 8192)
	me.ExecutionCh = events
	book := me.GetOrderBook("BTC")
	for i := 0; i < makers; i++ {
		book.AddOrder(testOrder(uint(i+1), "BTC", model.OrderSideSell, int64(50000+i), 1))
	}
	taker := testOrder(9999, "BTC", model.OrderSideBuy, int64(50000+makers-1), int64(makers))
	taker.OrderType = model.OrderTypeLimit
	for {
		if _, done := me.matchSlice(book, taker, budget); done {
			me.finishOrder(book, taker)
			break
		}
	}
	return collectTrades(events)
}

// 조각화가 체결열을 바꾸지 않는다. budget=1(최대 조각화)과 budget=0(무제한),
// 그리고 예산 경계를 직접 비교한다.
func TestSlicingPreservesTradeSequence(t *testing.T) {
	for _, makers := range []int{1, 2, 3, 17, 64} {
		makers := makers
		t.Run(fmt.Sprintf("makers-%d", makers), func(t *testing.T) {
			unlimited := runToCompletion(0, makers)
			require.Len(t, unlimited, makers)
			require.Equal(t, unlimited, runToCompletion(1, makers), "budget=1")
			for _, b := range []int{makers - 1, makers, makers + 1} {
				if b < 1 {
					continue
				}
				require.Equal(t, unlimited, runToCompletion(b, makers), "budget=%d", b)
			}
		})
	}
}

// 예산 경계에서 정확히 전량 체결되면 같은 조각에서 done=true여야 한다.
// 예산 검사가 루프 조건보다 앞에 있으면 빈 조각이 하나 더 생긴다.
func TestSliceBudgetBoundaryDoneInSameSlice(t *testing.T) {
	me := NewMatchingEngine()
	me.ExecutionCh = make(chan ExecutionEvent, 16)
	book := me.GetOrderBook("BTC")
	book.AddOrder(testOrder(1, "BTC", model.OrderSideSell, 50000, 5))

	taker := testOrder(2, "BTC", model.OrderSideBuy, 50000, 5)
	taker.OrderType = model.OrderTypeLimit
	trades, done := me.matchSlice(book, taker, 1)
	require.Equal(t, 1, trades)
	require.True(t, done, "전량 체결됐으므로 같은 조각에서 done=true")
}

// 예산 소진은 완료가 아니다.
func TestSliceBudgetExhaustionIsNotDone(t *testing.T) {
	me := NewMatchingEngine()
	me.ExecutionCh = make(chan ExecutionEvent, 16)
	book := me.GetOrderBook("BTC")
	book.AddOrder(testOrder(1, "BTC", model.OrderSideSell, 50000, 2))
	book.AddOrder(testOrder(2, "BTC", model.OrderSideSell, 50001, 3))

	taker := testOrder(3, "BTC", model.OrderSideBuy, 50001, 5)
	taker.OrderType = model.OrderTypeLimit

	trades, done := me.matchSlice(book, taker, 1)
	require.Equal(t, 1, trades)
	require.False(t, done, "예산 소진이지 완료가 아니다")

	trades, done = me.matchSlice(book, taker, 1)
	require.Equal(t, 1, trades)
	require.True(t, done)
}

// 시장가는 마지막 조각에서만 MarketOrderDone을 정확히 1회 낸다.
func TestMarketOrderDoneExactlyOnceAcrossSlices(t *testing.T) {
	me := NewMatchingEngine()
	events := make(chan ExecutionEvent, 100)
	me.ExecutionCh = events
	book := me.GetOrderBook("BTC")
	for i := 0; i < 4; i++ {
		book.AddOrder(testOrder(uint(i+1), "BTC", model.OrderSideSell, 50000, 1))
	}
	taker := &Order{
		ID: 99, CoinSymbol: "BTC", Side: model.OrderSideBuy,
		QuoteAmount: decimal.NewFromInt(200000), OrderType: model.OrderTypeMarket,
	}
	for {
		if _, done := me.matchSlice(book, taker, 1); done {
			me.finishOrder(book, taker)
			break
		}
	}
	close(events)
	doneCount := 0
	for e := range events {
		if e.MarketOrderDone != nil {
			doneCount++
		}
	}
	require.Equal(t, 1, doneCount)
}

// 즉시 완료 경로는 슬롯을 만들지 않고, book을 바꾸지 않고,
// terminal event를 내지 않고, dirty도 찍지 않는다.
func TestImmediateCompletionPathsDoNotMutate(t *testing.T) {
	cases := []struct {
		name  string
		order *Order
	}{
		{"limit-zero-amount", &Order{
			ID: 1, CoinSymbol: "BTC", Side: model.OrderSideBuy,
			Amount: decimal.Zero, Price: decimal.NewFromInt(50000),
			OrderType: model.OrderTypeLimit,
		}},
		{"market-sell-zero-amount", &Order{
			ID: 2, CoinSymbol: "BTC", Side: model.OrderSideSell,
			Amount: decimal.Zero, OrderType: model.OrderTypeMarket,
		}},
		{"unknown-side", &Order{
			ID: 3, CoinSymbol: "BTC", Side: model.OrderSide("SIDEWAYS"),
			Amount: decimal.NewFromInt(1), Price: decimal.NewFromInt(50000),
			OrderType: model.OrderTypeLimit,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			me := NewMatchingEngine()
			events := make(chan ExecutionEvent, 10)
			me.ExecutionCh = events
			observed := 0
			me.MatchLatencyObserver = func(time.Duration) { observed++ }
			tc.order.EnqueuedAt = time.Now()

			// 엔진을 Start하지 않으므로 book 직접 조회가 안전하다.
			require.Nil(t, me.admitOrder(tc.order), "슬롯을 만들지 않는다")
			require.Equal(t, 1, observed, "observer는 정확히 1회")
			require.Len(t, events, 0, "terminal event 없음")
			require.Empty(t, me.dirtySymbols, "dirty 없음")
			snapshot := me.GetOrderBookSnapshot("BTC")
			require.Empty(t, snapshot.Bids)
			require.Empty(t, snapshot.Asks)
		})
	}
}

func TestNilOrderDoesNotObserve(t *testing.T) {
	me := NewMatchingEngine()
	observed := 0
	me.MatchLatencyObserver = func(time.Duration) { observed++ }
	require.Nil(t, me.admitOrder(nil))
	require.Equal(t, 0, observed, "nil 주문은 observer를 부르지 않는다")
}
```

- [ ] **Step 5: 스케줄러 핵심 계약 5개를 테스트로 고정한다**

**다섯 계약만 남긴다.** 각 테스트는 계약을 직접 단언해야 한다.

| # | 계약 |
|---|---|
| 1 | 취소 flood 중 주문이 유한 횟수 안에 admit됨 |
| 2 | 대형 sweep 조각 사이에 취소가 처리됨 |
| 3 | active sweep 중 뒤 주문이 추월하지 않음 |
| 4 | backpressure가 신규 `OrderCh` 유입만 막고 이미 latch된 주문은 막지 않음 |
| 5 | shutdown이 이미 접수된 주문과 취소를 모두 처리하고 출력 채널을 닫음 |

`internal/matching/engine_scheduler_test.go`:

```go
package matching

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// schedRecorder는 Start() 전에 한 번 설치되고 실행 중 교체되지 않는다.
type schedRecorder struct {
	mu     sync.Mutex
	events []string // "order" | "cancel" 순서열
	trades atomic.Int64
	done   chan int
	admit  chan struct{}
	cancel chan struct{}
	first  chan struct{}
	once   sync.Once

	// admitTradeCount는 각 admit 시점의 누적 trade 수다.
	// 추월 여부를 "언제 admit됐는가"로 직접 판정하는 데 쓴다.
	admitTradeCount []int64
}

func newSchedRecorder() *schedRecorder {
	return &schedRecorder{
		done:   make(chan int, 65536),
		admit:  make(chan struct{}, 65536),
		cancel: make(chan struct{}, 65536),
		first:  make(chan struct{}),
	}
}

func (r *schedRecorder) install(me *MatchingEngine) {
	me.Observers = EngineObservers{
		OrderAdmitted: func(time.Duration) {
			r.mu.Lock()
			r.events = append(r.events, "order")
			r.admitTradeCount = append(r.admitTradeCount, r.trades.Load())
			r.mu.Unlock()
			select {
			case r.admit <- struct{}{}:
			default:
			}
		},
		OrderDone: func(n int) {
			select {
			case r.done <- n:
			default:
			}
		},
		Cancel: func(time.Duration) {
			r.mu.Lock()
			r.events = append(r.events, "cancel")
			r.mu.Unlock()
			select {
			case r.cancel <- struct{}{}:
			default:
			}
		},
		EmitBlock: func(k EmitKind, _ time.Duration) {
			if k != EmitTrade {
				return
			}
			r.trades.Add(1)
			r.once.Do(func() { close(r.first) })
		},
	}
}

func (r *schedRecorder) seq() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.events...)
}

func (r *schedRecorder) admitCounts() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64{}, r.admitTradeCount...)
}

func waitN(t *testing.T, ch <-chan struct{}, n int, what string) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("%s: %d/%d만 도착했다", what, i, n)
		}
	}
}

func waitDoneN(t *testing.T, ch <-chan int, n int, what string) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("%s: %d/%d만 완결됐다", what, i, n)
		}
	}
}

// bookHasPrice는 캐시 스냅샷으로 조회한다. 엔진 goroutine이 소유한 book을
// 테스트 goroutine이 직접 읽으면 race다.
func bookHasPrice(t *testing.T, me *MatchingEngine, side model.OrderSide, price int64) bool {
	t.Helper()
	snap, err := me.RequestOrderBookSnapshot("BTC", DefaultSnapshotDepth)
	require.NoError(t, err)
	levels := snap.Asks
	if side == model.OrderSideBuy {
		levels = snap.Bids
	}
	for _, level := range levels {
		if level.Price.Equal(decimal.NewFromInt(price)) {
			return true
		}
	}
	return false
}

// 계약 1: 취소 flood 중 주문이 유한 횟수 안에 admit된다.
// **첫 주문 이전 구간부터 검사한다** — 그 구간을 빼면 무제한 드레인도 통과한다.
func TestCancelsBetweenAdmissionsAreBounded(t *testing.T) {
	const limit = 4
	me := newTestEngine()
	me.maxConsecutiveCancels = limit
	rec := newSchedRecorder()
	rec.install(me)
	drainAll(me)

	// 취소를 먼저 전부 큐에 넣고 그 다음 주문을 넣는다. 무제한 드레인이면
	// 200건이 한 번에 처리된 뒤에야 첫 주문이 admit된다.
	for i := 0; i < 200; i++ {
		me.CancelCh <- CancelOrderCommand{
			CoinSymbol: "BTC", OrderID: uint(9000 + i),
			Side: model.OrderSideSell, Price: decimal.NewFromInt(70000),
			EnqueuedAt: time.Now(),
		}
	}
	for i := 0; i < 20; i++ {
		o := stopTestLimitOrder(uint(i+1), model.OrderSideSell, int64(50000+i), 1)
		o.EnqueuedAt = time.Now()
		me.OrderCh <- o
	}
	me.Start()
	waitN(t, rec.admit, 20, "주문 admit")

	run := 0
	for i, e := range rec.seq() {
		if e == "cancel" {
			run++
			require.LessOrEqual(t, run, limit,
				"인덱스 %d: 연속 취소 %d개가 상한 %d을 넘었다", i, run, limit)
			continue
		}
		run = 0
	}

	me.Stop()
	waitEngineDone(t, me)
}

// 계약 2: 대형 sweep 조각 사이에 취소가 처리된다.
func TestCancelDuringLargeSweepIsProcessed(t *testing.T) {
	const makers = 5000
	me := newTestEngine()
	me.maxMatchesPerTurn = 8
	rec := newSchedRecorder()
	rec.install(me)
	drainAll(me)
	me.Start()

	for i := 0; i < makers; i++ {
		o := stopTestLimitOrder(uint(i+1), model.OrderSideSell, 50000, 1)
		o.EnqueuedAt = time.Now()
		me.OrderCh <- o
	}
	victim := stopTestLimitOrder(90001, model.OrderSideSell, 90000, 1)
	victim.EnqueuedAt = time.Now()
	me.OrderCh <- victim
	waitDoneN(t, rec.done, makers+1, "setup")

	taker := stopTestLimitOrder(90002, model.OrderSideBuy, 50000, makers)
	taker.EnqueuedAt = time.Now()
	me.OrderCh <- taker

	// 첫 trade를 봐야 sweep이 진행 중이다. Sleep으로는 보장되지 않는다.
	select {
	case <-rec.first:
	case <-time.After(20 * time.Second):
		t.Fatal("sweep이 시작되지 않았다")
	}

	result := me.CancelOrder(CancelOrderCommand{
		CoinSymbol: "BTC", OrderID: 90001,
		Side: model.OrderSideSell, Price: decimal.NewFromInt(90000),
	})
	require.True(t, result.Removed, "sweep 진행 중에도 취소가 처리돼야 한다")
	require.Less(t, rec.trades.Load(), int64(makers),
		"취소가 sweep 완료 후에 처리됐다 — 조각 사이에 끼어들지 못했다")

	me.Stop()
	waitEngineDone(t, me)
}

// 계약 3: active sweep 중 뒤 주문이 추월하지 않는다.
// later 주문이 **admit된 순간의 trade count**를 보고 판정한다.
// 최종 총수만 보면 추월해도 통과한다.
func TestNewOrderDoesNotOvertakeActiveSweep(t *testing.T) {
	const makers = 200
	me := newTestEngine()
	me.maxMatchesPerTurn = 1
	rec := newSchedRecorder()
	rec.install(me)
	drainAll(me)
	me.Start()

	for i := 0; i < makers; i++ {
		o := stopTestLimitOrder(uint(i+1), model.OrderSideSell, 50000, 1)
		o.EnqueuedAt = time.Now()
		me.OrderCh <- o
	}
	waitDoneN(t, rec.done, makers, "setup")
	admitsBefore := len(rec.admitCounts())

	taker := stopTestLimitOrder(1000, model.OrderSideBuy, 50000, makers)
	taker.EnqueuedAt = time.Now()
	me.OrderCh <- taker
	select {
	case <-rec.first:
	case <-time.After(20 * time.Second):
		t.Fatal("sweep이 시작되지 않았다")
	}

	later := stopTestLimitOrder(1001, model.OrderSideSell, 80000, 1)
	later.EnqueuedAt = time.Now()
	me.OrderCh <- later

	waitDoneN(t, rec.done, 2, "taker+later 완결")
	me.Stop()
	waitEngineDone(t, me)

	counts := rec.admitCounts()
	require.Len(t, counts, admitsBefore+2, "taker와 later가 admit돼야 한다")
	laterAdmitTrades := counts[admitsBefore+1]
	require.GreaterOrEqual(t, laterAdmitTrades, int64(makers),
		"later 주문이 trade %d개 시점에 admit됐다 — sweep %d건 완료 전에 추월했다",
		laterAdmitTrades, makers)
}

// 계약 4: backpressure는 신규 OrderCh 유입만 막고, 이미 latch된 주문은 막지 않는다.
//
// latch 완료를 pendingOrder 상태로 결정론적으로 확인한 뒤 취소가 watermark를
// 넘기게 한다. select 운에 맡기지 않는다.
func TestBackpressureBlocksIntakeNotLatchedOrder(t *testing.T) {
	me := newTestEngine()
	me.ExecutionCh = make(chan ExecutionEvent, 16) // watermark = 12
	me.maxConsecutiveCancels = 8
	rec := newSchedRecorder()
	rec.install(me)
	go func() {
		for range me.SnapshotCh {
		}
	}()
	me.Start()

	// 1) 취소 대상 resting 주문 4건.
	for i := 0; i < 4; i++ {
		o := stopTestLimitOrder(uint(i+1), model.OrderSideSell, int64(50000+i), 1)
		o.EnqueuedAt = time.Now()
		me.OrderCh <- o
	}
	waitDoneN(t, rec.done, 4, "resting setup")

	// 2) watermark 바로 아래(11건)까지 채운다. 아직 게이트는 열려 있다.
	for i := 0; i < 11; i++ {
		me.ExecutionCh <- ExecutionEvent{}
	}
	require.False(t, me.emitBackpressured(), "아직 게이트가 열려 있어야 한다")

	// 3) 체결 0건 지정가 probe. emit이 없어야 올바른 구현도 send에 막히지 않는다.
	probe := stopTestLimitOrder(100, model.OrderSideSell, 90000, 1)
	probe.EnqueuedAt = time.Now()
	me.OrderCh <- probe

	// probe가 8단계에서 latch되고 다음 turn에서 admit돼 book에 오를 때까지 기다린다.
	require.Eventually(t, func() bool {
		return bookHasPrice(t, me, model.OrderSideSell, 90000)
	}, 10*time.Second, 5*time.Millisecond, "probe가 처리되지 않았다")

	// 4) 이제 취소 4건으로 watermark를 넘긴다(11+4=15 >= 12).
	for i := 0; i < 4; i++ {
		me.CancelCh <- CancelOrderCommand{
			CoinSymbol: "BTC", OrderID: uint(i + 1),
			Side: model.OrderSideSell, Price: decimal.NewFromInt(int64(50000 + i)),
			EnqueuedAt: time.Now(),
		}
	}
	waitN(t, rec.cancel, 4, "취소 처리")
	require.True(t, me.emitBackpressured(), "취소 emit으로 watermark를 넘겼어야 한다")

	// 5) backpressure 상태에서 새 주문은 admit되지 않는다.
	admitsBefore := len(rec.admitCounts())
	blocked := stopTestLimitOrder(101, model.OrderSideSell, 95000, 1)
	blocked.EnqueuedAt = time.Now()
	me.OrderCh <- blocked
	time.Sleep(200 * time.Millisecond)
	require.Equal(t, admitsBefore, len(rec.admitCounts()),
		"backpressure 중에 신규 주문이 admit됐다")

	// 6) 소비자를 붙여 해소하면 그 주문이 처리된다 — 유실이 아니라 지연이다.
	go func() {
		for range me.ExecutionCh {
		}
	}()
	require.Eventually(t, func() bool {
		return bookHasPrice(t, me, model.OrderSideSell, 95000)
	}, 10*time.Second, 5*time.Millisecond, "backpressure 해소 후에도 주문이 유실됐다")

	me.Stop()
	waitEngineDone(t, me)
}

// 계약 5: shutdown이 이미 접수된 주문과 취소를 모두 처리하고 출력 채널을 닫는다.
//
// 소비자가 watermark를 해소한다 — 소비자가 없으면 올바른 구현도 blocking
// send에서 멈춰 판별력이 없다. 게이트 오적용은 deadlock이 아니라
// **주문 유실**로 나타난다: 7단계에서 OrderCh 수신이 "비었음"으로 관측되는
// 것이 아니라 건너뛰어져, 주문을 남긴 채 drain 완료로 판정된다.
func TestStopDrainsQueuedOrderAndCancel(t *testing.T) {
	me := newTestEngine()
	me.ExecutionCh = make(chan ExecutionEvent, 16)
	rec := newSchedRecorder()
	rec.install(me)
	release := make(chan struct{})
	execDone := make(chan struct{})
	go func() {
		<-release
		for range me.ExecutionCh {
		}
		close(execDone)
	}()
	go func() {
		for range me.SnapshotCh {
		}
	}()

	me.Start()
	resting := stopTestLimitOrder(1, model.OrderSideSell, 50000, 1)
	resting.EnqueuedAt = time.Now()
	me.OrderCh <- resting
	waitDoneN(t, rec.done, 1, "resting setup")

	// watermark를 넘긴다.
	for i := 0; i < 13; i++ {
		me.ExecutionCh <- ExecutionEvent{}
	}
	require.True(t, me.emitBackpressured())

	// Stop() 전에 주문과 취소를 둘 다 큐에 넣는다.
	queued := stopTestLimitOrder(2, model.OrderSideSell, 90000, 1)
	queued.EnqueuedAt = time.Now()
	me.OrderCh <- queued
	me.CancelCh <- CancelOrderCommand{
		CoinSymbol: "BTC", OrderID: 1,
		Side: model.OrderSideSell, Price: decimal.NewFromInt(50000),
		EnqueuedAt: time.Now(),
	}

	me.Stop()
	close(release)
	waitEngineDone(t, me)

	select {
	case <-execDone:
	case <-time.After(20 * time.Second):
		t.Fatal("ExecutionCh가 닫히지 않았다")
	}
	select {
	case _, ok := <-me.SnapshotCh:
		require.False(t, ok, "SnapshotCh가 닫혀야 한다")
	case <-time.After(time.Second):
		t.Fatal("SnapshotCh가 닫히지 않았다")
	}

	require.True(t, bookHasPrice(t, me, model.OrderSideSell, 90000),
		"drain 중 OrderCh에 게이트가 걸려 주문이 유실됐다")
	require.False(t, bookHasPrice(t, me, model.OrderSideSell, 50000),
		"drain 중 취소가 처리되지 않았다")
}

// shutdown은 진행 중 sweep을 선점하지 않는다. 시장가면 MarketOrderDone까지 낸다.
func TestStopFinishesActiveMarketSweep(t *testing.T) {
	const makers = 100
	me := newTestEngine()
	me.maxMatchesPerTurn = 1
	events := make(chan ExecutionEvent, 4096)
	me.ExecutionCh = events
	rec := newSchedRecorder()
	rec.install(me)
	go func() {
		for range me.SnapshotCh {
		}
	}()
	me.Start()

	for i := 0; i < makers; i++ {
		o := stopTestLimitOrder(uint(i+1), model.OrderSideSell, 50000, 1)
		o.EnqueuedAt = time.Now()
		me.OrderCh <- o
	}
	waitDoneN(t, rec.done, makers, "setup")

	market := &Order{
		ID: 2000, UserID: 2000, CoinSymbol: "BTC", Side: model.OrderSideBuy,
		QuoteAmount: decimal.NewFromInt(50000 * makers),
		OrderType:   model.OrderTypeMarket, EnqueuedAt: time.Now(),
	}
	me.OrderCh <- market
	select {
	case <-rec.first:
	case <-time.After(20 * time.Second):
		t.Fatal("시장가 sweep이 시작되지 않았다")
	}

	me.Stop()
	waitEngineDone(t, me)

	doneCount, tradeCount := 0, 0
	for e := range events {
		if e.MarketOrderDone != nil {
			doneCount++
		}
		if e.Trade != nil {
			tradeCount++
		}
	}
	require.Equal(t, 1, doneCount, "MarketOrderDone 정확히 1회")
	require.Equal(t, makers, tradeCount, "shutdown이 sweep을 선점해 체결이 잘렸다")
}
```

- [ ] **Step 6: focused test를 돌린다**

```powershell
cd Go-exchange-back; go test -count=1 -timeout 300s ./internal/matching -v -run 'TestSlicing|TestSliceBudget|TestMarketOrderDoneExactly|TestImmediateCompletion|TestNilOrder|TestCancelsBetween|TestCancelDuringLarge|TestNewOrderDoesNot|TestBackpressureBlocks|TestStopDrains|TestStopFinishes'
```

Expected: 전부 PASS. 기존 matching 테스트도 확인한다:

```powershell
cd Go-exchange-back; go test -count=1 ./internal/matching
```

Expected: `ok`.

---

## Task 5: strict env parser와 QuantumConfig 주입

**Files:** `internal/matching/quantum_config.go`(신규), `internal/matching/sharded.go`, `config/runtime.go`, `EnvGOExchange*` 상수 블록, `cmd/main.go`, `config/runtime_quantum_test.go`(신규), `internal/matching/quantum_config_test.go`(신규), `cmd/main_quantum_wiring_test.go`(추가)

**Produces:** `matching.QuantumConfig{MaxMatchesPerTurn, MaxConsecutiveCancels int}`, `(QuantumConfig).Validate() error`, `matching.NewShardedEngineWithQuantum(int, QuantumConfig) (*ShardedEngine, error)`, `config.MatchingQuantumFromEnv() (int, int, error)`, `config.strictPositiveEnv(string, int) (int, error)`.

- [ ] **Step 1: `QuantumConfig`와 주입 경로를 만든다**

`internal/matching/quantum_config.go`:

```go
package matching

import "fmt"

// QuantumConfig는 엔진 스케줄러의 두 상한이다. 0은 matchSlice의 무제한
// sentinel과 충돌하므로 반드시 1 이상이어야 한다.
type QuantumConfig struct {
	MaxMatchesPerTurn     int
	MaxConsecutiveCancels int
}

func (c QuantumConfig) Validate() error {
	if c.MaxMatchesPerTurn < 1 {
		return fmt.Errorf("maxMatchesPerTurn must be >= 1, got %d", c.MaxMatchesPerTurn)
	}
	if c.MaxConsecutiveCancels < 1 {
		return fmt.Errorf("maxConsecutiveCancels must be >= 1, got %d", c.MaxConsecutiveCancels)
	}
	return nil
}
```

`internal/matching/sharded.go`:

```go
// NewShardedEngineWithQuantum은 검증된 quantum 설정을 전 샤드에 주입한다.
// 무인자 NewShardedEngine은 테스트용 기본값을 유지하므로 그대로 둔다.
func NewShardedEngineWithQuantum(shardCount int, cfg QuantumConfig) (*ShardedEngine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	se := NewShardedEngine(shardCount)
	for _, shard := range se.shards {
		shard.maxMatchesPerTurn = cfg.MaxMatchesPerTurn
		shard.maxConsecutiveCancels = cfg.MaxConsecutiveCancels
	}
	return se, nil
}
```

- [ ] **Step 2: strict parser를 만든다**

`EnvGOExchange*` 상수 블록에 추가한다(실제 파일은 `Select-String -Path "config\*.go" -Pattern 'EnvGOExchangeEngineShards'`로 확인).

```go
	EnvGOExchangeMatchingMaxMatchesPerTurn     = "GOEXCHANGE_MATCHING_MAX_MATCHES_PER_TURN"
	EnvGOExchangeMatchingMaxConsecutiveCancels = "GOEXCHANGE_MATCHING_MAX_CONSECUTIVE_CANCELS"
```

`config/runtime.go`:

```go
// 탐색으로 확정할 때까지의 임시값이다.
const (
	defaultMaxMatchesPerTurn     = 64
	defaultMaxConsecutiveCancels = 32
)

// strictPositiveEnv는 기존 parsePositiveIntEnv(config/database.go:68)와 달리
// 조용히 fallback하지 않는다. quantum 값은 0이 무제한 sentinel과 충돌하므로,
// 잘못된 설정으로 뜬 서버가 부하를 받는 것보다 안 뜨는 편이 낫다.
//
// 미설정(LookupEnv ok=false)만 기본값을 쓴다. 빈 문자열로 설정된 것은
// 셸 변수 오타·치환 실패의 전형적 결과이므로 에러다.
func strictPositiveEnv(key string, def int) (int, error) {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return def, nil
	}
	if raw == "" {
		return 0, fmt.Errorf("%s is set but empty", key)
	}
	if raw != strings.TrimSpace(raw) {
		return 0, fmt.Errorf("%s has surrounding whitespace: %q", key, raw)
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s is not a decimal integer: %q", key, raw)
	}
	if strconv.Itoa(parsed) != raw {
		return 0, fmt.Errorf("%s is not in canonical decimal form: %q", key, raw)
	}
	if parsed < 1 {
		return 0, fmt.Errorf("%s must be >= 1, got %d", key, parsed)
	}
	return parsed, nil
}

// MatchingQuantumFromEnv는 두 quantum 값을 strict 파싱한다.
// matching 타입을 반환하지 않는 것은 config → matching 의존을 만들지
// 않기 위해서다. main이 두 값으로 QuantumConfig를 구성한다.
func MatchingQuantumFromEnv() (maxMatchesPerTurn int, maxConsecutiveCancels int, err error) {
	maxMatchesPerTurn, err = strictPositiveEnv(EnvGOExchangeMatchingMaxMatchesPerTurn, defaultMaxMatchesPerTurn)
	if err != nil {
		return 0, 0, err
	}
	maxConsecutiveCancels, err = strictPositiveEnv(EnvGOExchangeMatchingMaxConsecutiveCancels, defaultMaxConsecutiveCancels)
	if err != nil {
		return 0, 0, err
	}
	return maxMatchesPerTurn, maxConsecutiveCancels, nil
}
```

import에 `fmt`·`os`·`strconv`·`strings`가 있는지 확인한다.

- [ ] **Step 3: `cmd/main.go`를 바꾼다**

```go
	engineShards := config.EngineShardsFromEnv()
	maxMatchesPerTurn, maxConsecutiveCancels, quantumErr := config.MatchingQuantumFromEnv()
	if quantumErr != nil {
		log.Fatal("matching quantum config invalid: ", quantumErr)
	}
	me, engineErr := matching.NewShardedEngineWithQuantum(engineShards, matching.QuantumConfig{
		MaxMatchesPerTurn:     maxMatchesPerTurn,
		MaxConsecutiveCancels: maxConsecutiveCancels,
	})
	if engineErr != nil {
		log.Fatal("matching quantum config invalid: ", engineErr)
	}
```

이어지는 `log.Printf`도 값이 보이게 고친다. **GCP preflight가 이 로그를 읽는다.**

```go
	log.Printf("matching engine sharded: shards=%d maxMatchesPerTurn=%d maxConsecutiveCancels=%d",
		engineShards, maxMatchesPerTurn, maxConsecutiveCancels)
```

- [ ] **Step 4: 테스트를 쓴다**

`config/runtime_quantum_test.go`:

```go
package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStrictPositiveEnvContract(t *testing.T) {
	const key = "GOEXCHANGE_TEST_STRICT_INT"

	t.Run("unset uses default", func(t *testing.T) {
		require.NoError(t, os.Unsetenv(key))
		got, err := strictPositiveEnv(key, 7)
		require.NoError(t, err)
		require.Equal(t, 7, got, "미설정은 기본값을 쓰겠다는 명시적 선택이다")
	})

	for _, tc := range []struct{ name, value string }{
		{"empty string", ""},
		{"leading space", " 3"},
		{"trailing space", "3 "},
		{"plus sign", "+3"},
		{"leading zero", "03"},
		{"zero", "0"},
		{"negative", "-1"},
		{"not a number", "abc"},
		{"overflow", "99999999999999999999"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(key, tc.value)
			_, err := strictPositiveEnv(key, 7)
			require.Error(t, err, "%q는 에러여야 한다 (기본값 fallback 금지)", tc.value)
		})
	}

	t.Run("valid", func(t *testing.T) {
		t.Setenv(key, "3")
		got, err := strictPositiveEnv(key, 7)
		require.NoError(t, err)
		require.Equal(t, 3, got)
	})
}

func TestMatchingQuantumFromEnv(t *testing.T) {
	t.Run("both unset", func(t *testing.T) {
		require.NoError(t, os.Unsetenv(EnvGOExchangeMatchingMaxMatchesPerTurn))
		require.NoError(t, os.Unsetenv(EnvGOExchangeMatchingMaxConsecutiveCancels))
		matches, cancels, err := MatchingQuantumFromEnv()
		require.NoError(t, err)
		require.Equal(t, defaultMaxMatchesPerTurn, matches)
		require.Equal(t, defaultMaxConsecutiveCancels, cancels)
	})
	t.Run("one invalid fails", func(t *testing.T) {
		t.Setenv(EnvGOExchangeMatchingMaxMatchesPerTurn, "0")
		_, _, err := MatchingQuantumFromEnv()
		require.Error(t, err)
	})
}

// 기존 파서의 동작을 바꾸지 않았는지 고정한다.
func TestParsePositiveIntEnvStillFallsBackSilently(t *testing.T) {
	const key = "GOEXCHANGE_TEST_LEGACY_INT"
	t.Setenv(key, "0")
	require.Equal(t, 9, parsePositiveIntEnv(key, 9), "기존 파서는 조용한 fallback을 유지한다")
	t.Setenv(key, " 3")
	require.Equal(t, 3, parsePositiveIntEnv(key, 9), "기존 파서는 TrimSpace를 유지한다")
}
```

`internal/matching/quantum_config_test.go`:

```go
package matching

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestQuantumConfigValidate(t *testing.T) {
	require.NoError(t, QuantumConfig{MaxMatchesPerTurn: 1, MaxConsecutiveCancels: 1}.Validate())
	require.Error(t, QuantumConfig{MaxMatchesPerTurn: 0, MaxConsecutiveCancels: 1}.Validate())
	require.Error(t, QuantumConfig{MaxMatchesPerTurn: 1, MaxConsecutiveCancels: 0}.Validate())
	require.Error(t, QuantumConfig{MaxMatchesPerTurn: -1, MaxConsecutiveCancels: 1}.Validate())
}

func TestNewShardedEngineWithQuantumInjectsEveryShard(t *testing.T) {
	se, err := NewShardedEngineWithQuantum(4, QuantumConfig{MaxMatchesPerTurn: 17, MaxConsecutiveCancels: 5})
	require.NoError(t, err)
	require.Len(t, se.shards, 4)
	for i, shard := range se.shards {
		require.Equal(t, 17, shard.maxMatchesPerTurn, "shard %d", i)
		require.Equal(t, 5, shard.maxConsecutiveCancels, "shard %d", i)
	}
	_, err = NewShardedEngineWithQuantum(2, QuantumConfig{MaxMatchesPerTurn: 0, MaxConsecutiveCancels: 4})
	require.Error(t, err)
}
```

`cmd/main_quantum_wiring_test.go`에 추가:

```go
func TestMainFailsFastOnInvalidQuantumConfig(t *testing.T) {
	source, err := os.ReadFile("main.go")
	require.NoError(t, err)
	text := string(source)
	require.Contains(t, text, "config.MatchingQuantumFromEnv()")
	require.Contains(t, text, "matching.NewShardedEngineWithQuantum(")
	require.Contains(t, text, `log.Fatal("matching quantum config invalid: "`)
	// GCP preflight가 이 로그로 적용값을 확인한다.
	require.Contains(t, text, "maxMatchesPerTurn=%d maxConsecutiveCancels=%d")
}
```

- [ ] **Step 5: focused test를 돌린다**

```powershell
cd Go-exchange-back; go test -count=1 ./config -v -run 'TestStrictPositiveEnv|TestMatchingQuantumFromEnv|TestParsePositiveIntEnvStill'; go test -count=1 ./internal/matching -v -run 'TestQuantumConfigValidate|TestNewShardedEngineWithQuantum'; go test -count=1 ./cmd -v -run 'TestMainWires|TestMainFailsFast'
```

Expected: 전부 PASS.

---

## Task 6: 집계·선택 순수 함수와 selector 프로그램

**Files:** `internal/matching/quantum_aggregate.go`(신규), `internal/matching/quantum_select.go`(신규), 각 `_test.go`, `cmd/quantumselect/main.go`(신규)

**Produces:** `RunFile`, `BaselineStats`, `CandidateResult`, `QuantumChoice`, `ValidateRunSet`, `AggregateBaseline`, `AggregateCandidate`, `ExceedingRuns`, `SelectQuantum`.

- [ ] **Step 1: 집계기를 만든다**

**run-set 검증이 censored 조기 반환보다 먼저다.** censored를 이유로 일찍 빠져나가면 run-set이 깨진 것을 영영 못 본다.

`internal/matching/quantum_aggregate.go`:

```go
package matching

import (
	"fmt"
	"sort"
	"time"
)

// InfNs는 censored 때문에 분위수가 정의되지 않음을 뜻한다.
// JSON에 Infinity를 쓸 수 없어 -1로 표현한다.
const InfNs int64 = -1

// RunFile은 하니스가 쓴 JSON 배열의 원소 하나다. 필드명이 하니스의
// scenarioResult와 어긋나면 0으로 읽혀 조용히 통과하므로 ValidateRunSet이
// 설정값 일치를 확인한다.
type RunFile struct {
	Scenario              string `json:"scenario"`
	Seed                  int64  `json:"seed"`
	MaxMatchesPerTurn     int    `json:"max_matches_per_turn"`
	MaxConsecutiveCancels int    `json:"max_consecutive_cancels"`
	OrderWaitP99Ns        int64  `json:"order_wait_p99_ns"`
	OrderCensored         int    `json:"order_censored"`
	CancelWaitP99Ns       int64  `json:"cancel_wait_p99_ns"`
	CancelCensored        int    `json:"cancel_censored"`
	SweepTotalNs          int64  `json:"sweep_total_ns"`
	SweepCensored         int    `json:"sweep_censored"`
	MaxSnapshotGapNs      int64  `json:"max_snapshot_gap_ns"`
}

// requiredScenarios는 판정에 반드시 있어야 하는 다섯이다.
// H4가 빠지면 max gap이 0이 되어 C4를 조용히 통과하므로 에러여야 한다.
var requiredScenarios = []string{"H0", "H1", "H2-1", "H2-5000", "H4"}

// ValidateRunSet은 집계 전에 정확히 한 번 부른다.
// cfg가 제로값이면 설정 일치 검사를 건너뛴다(baseline은 기본값으로 돈다).
func ValidateRunSet(runs []RunFile, want int, cfg QuantumConfig, checkConfig bool) error {
	for _, scenario := range requiredScenarios {
		sel := pick(runs, scenario)
		if len(sel) != want {
			return fmt.Errorf("%s: %d runs, want %d", scenario, len(sel), want)
		}
		seen := map[int64]bool{}
		for _, r := range sel {
			if seen[r.Seed] {
				return fmt.Errorf("%s: duplicate seed %d", scenario, r.Seed)
			}
			seen[r.Seed] = true
			if !checkConfig {
				continue
			}
			if r.MaxMatchesPerTurn != cfg.MaxMatchesPerTurn ||
				r.MaxConsecutiveCancels != cfg.MaxConsecutiveCancels {
				return fmt.Errorf("%s seed %d: config m=%d c=%d, want m=%d c=%d",
					scenario, r.Seed, r.MaxMatchesPerTurn, r.MaxConsecutiveCancels,
					cfg.MaxMatchesPerTurn, cfg.MaxConsecutiveCancels)
			}
		}
	}
	return nil
}

func pick(runs []RunFile, scenario string) []RunFile {
	var out []RunFile
	for _, r := range runs {
		if r.Scenario == scenario {
			out = append(out, r)
		}
	}
	return out
}

// medianNs는 회차 값의 중앙값이다. InfNs가 하나라도 있으면 에러다 —
// 분위수가 정의되지 않은 회차를 섞어 중앙값을 내면 그 숫자는 거짓이다.
func medianNs(runs []RunFile, scenario string, get func(RunFile) int64) (time.Duration, error) {
	sel := pick(runs, scenario)
	values := make([]int64, 0, len(sel))
	for i, r := range sel {
		v := get(r)
		if v == InfNs {
			return 0, fmt.Errorf("%s run %d: censored (quantile undefined)", scenario, i)
		}
		values = append(values, v)
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return time.Duration(values[len(values)/2]), nil
}

func maxNs(runs []RunFile, scenario string, get func(RunFile) int64) time.Duration {
	var out int64
	for _, r := range pick(runs, scenario) {
		if v := get(r); v > out {
			out = v
		}
	}
	return time.Duration(out)
}

func sumCensored(runs []RunFile) int {
	total := 0
	for _, r := range runs {
		total += r.OrderCensored + r.CancelCensored + r.SweepCensored
	}
	return total
}

// AggregateBaseline은 C1·C2·C3의 기준값을 유도한다.
func AggregateBaseline(runs []RunFile, want int) (BaselineStats, error) {
	if err := ValidateRunSet(runs, want, QuantumConfig{}, false); err != nil {
		return BaselineStats{}, fmt.Errorf("baseline run-set invalid: %w", err)
	}
	h0, err := medianNs(runs, "H0", func(r RunFile) int64 { return r.OrderWaitP99Ns })
	if err != nil {
		return BaselineStats{}, err
	}
	small, err := medianNs(runs, "H2-1", func(r RunFile) int64 { return r.CancelWaitP99Ns })
	if err != nil {
		return BaselineStats{}, err
	}
	large, err := medianNs(runs, "H2-5000", func(r RunFile) int64 { return r.SweepTotalNs })
	if err != nil {
		return BaselineStats{}, err
	}
	return BaselineStats{H0OrderP99: h0, H2SmallCancelP99: small, H2LargeSweepTotal: large}, nil
}

// AggregateCandidate는 후보 조합 하나의 JSON을 판정 입력으로 바꾼다.
// run-set 검증이 censored 조기 반환보다 먼저다.
func AggregateCandidate(cfg QuantumConfig, runs []RunFile, want int, semanticPass bool) (CandidateResult, error) {
	if err := ValidateRunSet(runs, want, cfg, true); err != nil {
		return CandidateResult{}, fmt.Errorf("candidate run-set invalid: %w", err)
	}
	out := CandidateResult{
		Config:            cfg,
		Censored:          sumCensored(runs),
		MaxSnapshotGap:    maxNs(runs, "H4", func(r RunFile) int64 { return r.MaxSnapshotGapNs }),
		SemanticTestsPass: semanticPass,
	}
	if out.Censored > 0 {
		return out, nil // 어차피 C5에서 탈락한다. 분위수 계산은 건너뛴다.
	}
	var err error
	if out.H1OrderP99, err = medianNs(runs, "H1", func(r RunFile) int64 { return r.OrderWaitP99Ns }); err != nil {
		return CandidateResult{}, err
	}
	if out.H2LargeCancelP99, err = medianNs(runs, "H2-5000", func(r RunFile) int64 { return r.CancelWaitP99Ns }); err != nil {
		return CandidateResult{}, err
	}
	if out.H2LargeSweepTotal, err = medianNs(runs, "H2-5000", func(r RunFile) int64 { return r.SweepTotalNs }); err != nil {
		return CandidateResult{}, err
	}
	return out, nil
}

// ExceedingRuns는 상한을 넘은 회차의 인덱스를 돌려준다. 중앙값이 통과해도
// 개별 초과는 보고서의 "한계" 절에 그대로 적는다.
func ExceedingRuns(runs []RunFile, scenario string, get func(RunFile) int64, limitNs int64) []int {
	var out []int
	for i, r := range pick(runs, scenario) {
		if v := get(r); v == InfNs || v > limitNs {
			out = append(out, i)
		}
	}
	return out
}
```

- [ ] **Step 2: `SelectQuantum`을 만든다**

`internal/matching/quantum_select.go`:

```go
package matching

import (
	"errors"
	"time"
)

// snapshotFreshnessLimit는 C4의 상한이자 C1·C2 상한의 바닥이다.
// 이 파이프라인이 이미 감수하는 최소 관측 지연 단위가 스냅샷 코얼레싱
// 주기(100ms)이므로, 그보다 촘촘한 요구는 다른 부분과 정합하지 않는다.
const snapshotFreshnessLimit = 300 * time.Millisecond

const throughputAllowance = 1.05

// BaselineStats는 보존된 baseline JSON에서 유도한 기준값이다.
// 탐색 단계는 이 값을 다시 측정하지 않는다.
type BaselineStats struct {
	H2SmallCancelP99  time.Duration // H2-1 cancel p99 중앙값
	H0OrderP99        time.Duration // H0 order p99 중앙값
	H2LargeSweepTotal time.Duration // H2-5000 sweep 총 시간 중앙값
}

type CandidateResult struct {
	Config            QuantumConfig
	H2LargeCancelP99  time.Duration
	H1OrderP99        time.Duration
	H2LargeSweepTotal time.Duration
	MaxSnapshotGap    time.Duration
	Censored          int
	SemanticTestsPass bool
}

type QuantumChoice struct {
	Config QuantumConfig
	Result CandidateResult
}

// ErrNoCandidatePassed는 통과 조합이 0개일 때 반환된다. 이 경우 임계값을
// 완화하거나 격자를 넓히지 않는다 — quantum만으로 해결되지 않는다는
// 증거이므로 중단한다.
var ErrNoCandidatePassed = errors.New("no candidate passed the pre-registered gates")

func upperBound(controlP99 time.Duration) time.Duration {
	if bound := controlP99 * 3; bound > snapshotFreshnessLimit {
		return bound
	}
	return snapshotFreshnessLimit
}

func (c CandidateResult) passes(base BaselineStats) bool {
	switch {
	case c.Censored > 0: // C5
		return false
	case !c.SemanticTestsPass: // C6
		return false
	case c.H2LargeCancelP99 > upperBound(base.H2SmallCancelP99): // C1
		return false
	case c.H1OrderP99 > upperBound(base.H0OrderP99): // C2
		return false
	case float64(c.H2LargeSweepTotal) > float64(base.H2LargeSweepTotal)*throughputAllowance: // C3
		return false
	case c.MaxSnapshotGap > snapshotFreshnessLimit: // C4
		return false
	}
	return true
}

// RankCandidates는 통과 후보를 사전 등록된 선택 규칙 순으로 정렬해 돌려준다.
// 규칙: (1) MaxMatchesPerTurn 최대 → (2) 동률이면 MaxConsecutiveCancels
// 최소 → (3) 그래도 동률이면 입력 순서상 먼저.
//
// 1차 탐색에서 상위 2개를 고르는 데도 이 함수를 쓴다 — 상위 선정과 최종
// 선택이 같은 규칙이어야 탐색이 결과를 편향시키지 않는다.
func RankCandidates(candidates []CandidateResult, base BaselineStats) []CandidateResult {
	passed := make([]CandidateResult, 0, len(candidates))
	order := map[QuantumConfig]int{}
	for i, c := range candidates {
		if _, seen := order[c.Config]; !seen {
			order[c.Config] = i
		}
		if c.passes(base) {
			passed = append(passed, c)
		}
	}
	sort.SliceStable(passed, func(i, j int) bool {
		a, b := passed[i].Config, passed[j].Config
		if a.MaxMatchesPerTurn != b.MaxMatchesPerTurn {
			return a.MaxMatchesPerTurn > b.MaxMatchesPerTurn
		}
		if a.MaxConsecutiveCancels != b.MaxConsecutiveCancels {
			return a.MaxConsecutiveCancels < b.MaxConsecutiveCancels
		}
		return order[a] < order[b]
	})
	return passed
}

// SelectQuantum은 통과 후보 중 1등을 고른다. 부작용이 없다 — 측정 없이
// 판정 로직을 검증할 수 있어야 튜닝 단계에서 규칙을 슬쩍 바꾸는 일이 없다.
func SelectQuantum(candidates []CandidateResult, base BaselineStats) (QuantumChoice, error) {
	ranked := RankCandidates(candidates, base)
	if len(ranked) == 0 {
		return QuantumChoice{}, ErrNoCandidatePassed
	}
	return QuantumChoice{Config: ranked[0].Config, Result: ranked[0]}, nil
}
```

import에 `"sort"`를 추가한다.

- [ ] **Step 3: 집계·선택 테스트를 쓴다**

`internal/matching/quantum_select_test.go` — **세 개만.**

```go
package matching

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testBaseline() BaselineStats {
	return BaselineStats{
		H2SmallCancelP99:  10 * time.Millisecond,
		H0OrderP99:        10 * time.Millisecond,
		H2LargeSweepTotal: 1000 * time.Millisecond,
	}
}

func passingCandidate(matches, cancels int) CandidateResult {
	return CandidateResult{
		Config:            QuantumConfig{MaxMatchesPerTurn: matches, MaxConsecutiveCancels: cancels},
		H2LargeCancelP99:  20 * time.Millisecond,
		H1OrderP99:        20 * time.Millisecond,
		H2LargeSweepTotal: 1020 * time.Millisecond,
		MaxSnapshotGap:    100 * time.Millisecond,
		SemanticTestsPass: true,
	}
}

// 규칙 1(matches 최대) → 2(cancels 최소) → 3(입력 순서).
func TestSelectQuantumAppliesRankingRules(t *testing.T) {
	got, err := SelectQuantum([]CandidateResult{
		passingCandidate(16, 8), passingCandidate(128, 32), passingCandidate(128, 8), passingCandidate(64, 8),
	}, testBaseline())
	require.NoError(t, err)
	require.Equal(t, QuantumConfig{MaxMatchesPerTurn: 128, MaxConsecutiveCancels: 8}, got.Config)

	// 규칙 3: 두 규칙으로도 동률이면 먼저 온 것.
	first := passingCandidate(64, 16)
	first.H1OrderP99 = 19 * time.Millisecond
	ranked := RankCandidates([]CandidateResult{first, passingCandidate(64, 16)}, testBaseline())
	require.Equal(t, 19*time.Millisecond, ranked[0].H1OrderP99)

	// 상한 바닥: control p99가 마이크로초여도 300ms 안이면 통과한다.
	tiny := BaselineStats{H2SmallCancelP99: time.Microsecond, H0OrderP99: time.Microsecond, H2LargeSweepTotal: time.Second}
	c := passingCandidate(64, 8)
	c.H2LargeCancelP99, c.H1OrderP99 = 250*time.Millisecond, 250*time.Millisecond
	_, err = SelectQuantum([]CandidateResult{c}, tiny)
	require.NoError(t, err, "300ms 바닥 안이면 통과해야 한다")
}

func TestSelectQuantumRejectsCensoredAndSemanticFailure(t *testing.T) {
	censored := passingCandidate(128, 8)
	censored.Censored = 1
	semantic := passingCandidate(128, 32)
	semantic.SemanticTestsPass = false

	got, err := SelectQuantum([]CandidateResult{censored, semantic, passingCandidate(16, 8)}, testBaseline())
	require.NoError(t, err)
	require.Equal(t, 16, got.Config.MaxMatchesPerTurn, "censored·semantic 실패는 탈락")
}

func TestSelectQuantumErrorsWhenNonePass(t *testing.T) {
	slow := passingCandidate(64, 8)
	slow.H2LargeCancelP99 = 10 * time.Second
	_, err := SelectQuantum([]CandidateResult{slow}, testBaseline())
	require.ErrorIs(t, err, ErrNoCandidatePassed,
		"통과 조합이 0개면 에러다 — 임계값을 완화하지 않는다")
}
```

`internal/matching/quantum_aggregate_test.go`:

```go
package matching

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func runFile(scenario string, seed int64, cfg QuantumConfig) RunFile {
	return RunFile{
		Scenario: scenario, Seed: seed,
		MaxMatchesPerTurn: cfg.MaxMatchesPerTurn, MaxConsecutiveCancels: cfg.MaxConsecutiveCancels,
	}
}

// 다섯 시나리오 × want회를 채운 최소 run-set.
func fullRunSet(want int, cfg QuantumConfig) []RunFile {
	var out []RunFile
	seed := int64(1)
	for _, s := range requiredScenarios {
		for i := 0; i < want; i++ {
			out = append(out, runFile(s, seed, cfg))
			seed++
		}
	}
	return out
}

func TestValidateRunSetCatchesStructuralProblems(t *testing.T) {
	cfg := QuantumConfig{MaxMatchesPerTurn: 32, MaxConsecutiveCancels: 8}

	require.NoError(t, ValidateRunSet(fullRunSet(3, cfg), 3, cfg, true))

	// 회차 부족
	short := fullRunSet(3, cfg)[:14]
	require.Error(t, ValidateRunSet(short, 3, cfg, true))

	// H4 누락 — max gap 0으로 조용히 통과하면 안 된다
	var noH4 []RunFile
	for _, r := range fullRunSet(3, cfg) {
		if r.Scenario != "H4" {
			noH4 = append(noH4, r)
		}
	}
	err := ValidateRunSet(noH4, 3, cfg, true)
	require.Error(t, err)
	require.Contains(t, err.Error(), "H4")

	// 중복 seed
	dup := fullRunSet(3, cfg)
	dup[1].Seed = dup[0].Seed
	require.ErrorContains(t, ValidateRunSet(dup, 3, cfg, true), "duplicate seed")

	// 설정 불일치 — 디렉터리가 말하는 후보와 JSON이 다르다
	mismatch := fullRunSet(3, cfg)
	mismatch[5].MaxMatchesPerTurn = 999
	require.ErrorContains(t, ValidateRunSet(mismatch, 3, cfg, true), "want m=32")
}

func TestAggregateBaselineTakesMedian(t *testing.T) {
	cfg := QuantumConfig{}
	runs := fullRunSet(3, cfg)
	set := func(scenario string, field func(*RunFile, int64), values ...int64) {
		i := 0
		for idx := range runs {
			if runs[idx].Scenario == scenario {
				field(&runs[idx], values[i])
				i++
			}
		}
	}
	set("H0", func(r *RunFile, v int64) { r.OrderWaitP99Ns = v }, 5, 1, 3)
	set("H2-1", func(r *RunFile, v int64) { r.CancelWaitP99Ns = v }, 30, 10, 20)
	set("H2-5000", func(r *RunFile, v int64) { r.SweepTotalNs = v }, 300, 100, 200)

	base, err := AggregateBaseline(runs, 3)
	require.NoError(t, err)
	require.Equal(t, time.Duration(3), base.H0OrderP99)
	require.Equal(t, time.Duration(20), base.H2SmallCancelP99)
	require.Equal(t, time.Duration(200), base.H2LargeSweepTotal)
}

// baseline 분위수가 +Inf면 상한을 유도할 수 없다.
func TestAggregateBaselineRejectsInfiniteMedian(t *testing.T) {
	runs := fullRunSet(3, QuantumConfig{})
	for idx := range runs {
		if runs[idx].Scenario == "H0" {
			runs[idx].OrderWaitP99Ns = InfNs
			runs[idx].OrderCensored = 1
		}
	}
	_, err := AggregateBaseline(runs, 3)
	require.ErrorContains(t, err, "H0")
}

// run-set 검증이 censored 조기 반환보다 먼저다.
func TestAggregateCandidateValidatesBeforeCensoredShortcut(t *testing.T) {
	cfg := QuantumConfig{MaxMatchesPerTurn: 32, MaxConsecutiveCancels: 8}
	runs := fullRunSet(3, cfg)
	runs[0].OrderCensored = 1     // censored가 있어도
	runs[1].MaxMatchesPerTurn = 9 // run-set 문제를 먼저 잡아야 한다
	_, err := AggregateCandidate(cfg, runs, 3, true)
	require.ErrorContains(t, err, "run-set invalid")
}

func TestAggregateCandidateSumsCensoredAndTakesMaxGap(t *testing.T) {
	cfg := QuantumConfig{MaxMatchesPerTurn: 32, MaxConsecutiveCancels: 8}
	runs := fullRunSet(3, cfg)
	n := 0
	for idx := range runs {
		switch runs[idx].Scenario {
		case "H1":
			runs[idx].OrderWaitP99Ns = 20
			runs[idx].OrderCensored = n
			n++
		case "H2-5000":
			runs[idx].CancelWaitP99Ns = 40
			runs[idx].SweepTotalNs = 500
		case "H4":
			runs[idx].MaxSnapshotGapNs = int64(50 + 20*n)
		}
	}
	got, err := AggregateCandidate(cfg, runs, 3, true)
	require.NoError(t, err)
	require.Equal(t, 3, got.Censored, "censored는 회차 합계 (0+1+2)")
	require.Equal(t, time.Duration(90), got.MaxSnapshotGap, "snapshot gap은 H4의 최댓값")
}

func TestExceedingRunsReportsPerRunOverruns(t *testing.T) {
	cfg := QuantumConfig{}
	runs := fullRunSet(3, cfg)
	values := []int64{100, 500, 100}
	i := 0
	for idx := range runs {
		if runs[idx].Scenario == "H1" {
			runs[idx].OrderWaitP99Ns = values[i]
			i++
		}
	}
	over := ExceedingRuns(runs, "H1", func(r RunFile) int64 { return r.OrderWaitP99Ns }, 200)
	require.Equal(t, []int{1}, over)
}
```

- [ ] **Step 4: selector 프로그램을 만든다**

일회성 스크립트가 아니라 커밋되는 프로그램이어야 판정이 재현된다.

`cmd/quantumselect/main.go`:

```go
// quantumselect는 보존된 baseline과 후보 측정 JSON을 읽어 사전 등록된
// 규칙으로 quantum 값을 고른다.
//
//	go run ./cmd/quantumselect -baseline _workspace/quantum/baseline -baseline-runs 3 \
//	    -candidates _workspace/quantum/explore -runs 3 \
//	    -semantic _workspace/quantum/semantic.json -top 2
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
)

func loadRuns(dir string) ([]matching.RunFile, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var all []matching.RunFile
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var runs []matching.RunFile
		if err := json.Unmarshal(data, &runs); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		all = append(all, runs...)
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("%s: no run files", dir)
	}
	return all, nil
}

// parseComboDir는 "m32-c8" 형태의 디렉터리명에서 설정을 읽는다.
func parseComboDir(name string) (matching.QuantumConfig, error) {
	parts := strings.Split(name, "-")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "m") || !strings.HasPrefix(parts[1], "c") {
		return matching.QuantumConfig{}, fmt.Errorf("bad combo dir %q, want m<N>-c<N>", name)
	}
	m, err := strconv.Atoi(parts[0][1:])
	if err != nil {
		return matching.QuantumConfig{}, err
	}
	c, err := strconv.Atoi(parts[1][1:])
	if err != nil {
		return matching.QuantumConfig{}, err
	}
	return matching.QuantumConfig{MaxMatchesPerTurn: m, MaxConsecutiveCancels: c}, nil
}

func main() {
	baselineDir := flag.String("baseline", "", "보존된 baseline JSON 디렉터리")
	baselineRuns := flag.Int("baseline-runs", 3, "baseline 시나리오당 회차 수")
	candidatesDir := flag.String("candidates", "", "후보별 하위 디렉터리를 담은 디렉터리")
	runs := flag.Int("runs", 3, "후보 시나리오당 회차 수")
	semanticPath := flag.String("semantic", "", "조합별 의미 테스트 통과 여부 JSON")
	top := flag.Int("top", 1, "상위 N개만 출력(1차 탐색은 2, 확증은 1)")
	flag.Parse()

	// baseline도 후보 선택 전에 실제 parser로 읽어 검증한다.
	baseRuns, err := loadRuns(*baselineDir)
	must(err)
	base, err := matching.AggregateBaseline(baseRuns, *baselineRuns)
	must(err)
	fmt.Printf("baseline: H0=%v H2-1=%v H2-5000=%v\n\n",
		base.H0OrderP99, base.H2SmallCancelP99, base.H2LargeSweepTotal)

	semanticData, err := os.ReadFile(*semanticPath)
	must(err)
	var semantic map[string]bool
	must(json.Unmarshal(semanticData, &semantic))

	entries, err := os.ReadDir(*candidatesDir)
	must(err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	// 격자 순서를 결정론적으로 만든다 — 규칙 3이 재현되려면 디렉터리
	// 나열 순서에 의존하면 안 된다.
	sort.Slice(names, func(i, j int) bool {
		a, _ := parseComboDir(names[i])
		b, _ := parseComboDir(names[j])
		if a.MaxMatchesPerTurn != b.MaxMatchesPerTurn {
			return a.MaxMatchesPerTurn < b.MaxMatchesPerTurn
		}
		return a.MaxConsecutiveCancels < b.MaxConsecutiveCancels
	})

	candidates := make([]matching.CandidateResult, 0, len(names))
	for _, name := range names {
		cfg, err := parseComboDir(name)
		must(err)
		runFiles, err := loadRuns(filepath.Join(*candidatesDir, name))
		must(err)
		result, err := matching.AggregateCandidate(cfg, runFiles, *runs, semantic[name])
		must(err)
		candidates = append(candidates, result)
		fmt.Printf("%-10s censored=%-3d semantic=%-5v cancelP99=%-12v orderP99=%-12v sweep=%-12v gap=%v\n",
			name, result.Censored, semantic[name],
			result.H2LargeCancelP99, result.H1OrderP99, result.H2LargeSweepTotal, result.MaxSnapshotGap)
	}

	ranked := matching.RankCandidates(candidates, base)
	if len(ranked) == 0 {
		fmt.Fprintf(os.Stderr, "\nSELECTION FAILED: %v\n", matching.ErrNoCandidatePassed)
		fmt.Fprintln(os.Stderr, "임계값을 완화하거나 격자를 넓히지 말고 P1으로 중단할 것.")
		os.Exit(2)
	}
	fmt.Printf("\nPASSED %d/%d\n", len(ranked), len(candidates))
	for i, c := range ranked {
		if i >= *top {
			break
		}
		fmt.Printf("RANK%d m%d-c%d\n", i+1, c.Config.MaxMatchesPerTurn, c.Config.MaxConsecutiveCancels)
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
```

- [ ] **Step 5: focused test를 돌린다**

```powershell
cd Go-exchange-back; go test -count=1 ./internal/matching -v -run 'TestSelectQuantum|TestValidateRunSet|TestAggregate|TestExceedingRuns'; go build ./cmd/quantumselect
```

Expected: 테스트 8건 PASS, build 성공.

---

## Task 7: B′·A′ 의미 테스트 (C6의 실체)

**Files:** `internal/service/active_sweep_cancel_integration_test.go`(신규)

**이미 있는 것과 없는 것.** `internal/service/cancel_command_worker_test.go`에 worker 쪽 절반이 이미 있다 — `TestCancelCommandWorkerRetriesNotFoundWhileOrderIsOpen`(309행), `TestCancelCommandWorkerMarksNoopWhenOrderIsTerminal`(325행). **없는 것은 엔진 쪽 절반**이다: active sweep 중인 주문의 취소가 실제로 not-found를 내고, sweep이 끝난 뒤 잔량이 book에 올라가 다음 retry가 제거한다는 것.

- [ ] **Step 1: B′ 의미 테스트 4개를 쓴다**

| # | 테스트 함수명 | 무엇을 고정하는가 |
|---|---|---|
| B-1 | `TestActiveSweepCancelReturnsNotFoundThenRemovesRemainder` | sweep 중 taker 취소 → `Removed=false`, `ErrCancelOrderNotFound`. sweep 완료 후 잔량이 book에 있고 재시도 취소가 `Removed=true`. `OrderCancelled` 정확히 1회 |
| B-2 | `TestActiveSweepFullFillLeavesNothingToCancel` | 전량 체결되는 taker를 sweep 중 취소 → not-found. 완료 후 book에 없고 재시도도 not-found. `ORDER_RELEASE` 원장 0건 |
| B-3 | `TestActiveMarketSweepCancelIsNotPreempted` | 시장가 sweep 중 취소 → not-found, sweep 완주, `MarketOrderDone` 1회 |
| B-4 | `TestMakerCancelledBetweenSlicesEscapesFill` | `maxMatchesPerTurn=1`, 조각 사이 maker 취소 → 그 maker는 이후 조각에서 체결되지 않는다 (**의도된 체결 회피**) |

골격. `cancel_command_worker_test.go`의 `testOpenOrder`(230행)·`testCancelCommand`(217행) 헬퍼와 `internal/testdb`를 그대로 쓴다.

```go
// 조각화를 최대로 만들어 sweep 중간 시점을 확실히 잡는다.
func semanticQuantum(t *testing.T) matching.QuantumConfig {
	t.Helper()
	cfg := matching.QuantumConfig{MaxMatchesPerTurn: 1, MaxConsecutiveCancels: 32}
	if v, err := strconv.Atoi(os.Getenv("GOEXCHANGE_MATCHING_MAX_MATCHES_PER_TURN")); err == nil && v > 0 {
		cfg.MaxMatchesPerTurn = v
	}
	if v, err := strconv.Atoi(os.Getenv("GOEXCHANGE_MATCHING_MAX_CONSECUTIVE_CANCELS")); err == nil && v > 0 {
		cfg.MaxConsecutiveCancels = v
	}
	return cfg
}
```

**env를 우선하게 만드는 것이 핵심이다.** 하드코딩된 `QuantumConfig{1, 32}`만 쓰면 조합별 C6 검증이 실제로는 항상 같은 값으로 돌아 아무것도 검증하지 않는다.

B-1의 단언 순서: (1) sweep 중 `CancelOrder` → `Removed=false`, `errors.Is(result.Err, matching.ErrCancelOrderNotFound)`. (2) sweep 완료 대기. (3) 잔량이 book에 있음. (4) 재시도 `CancelOrder` → `Removed=true`. (5) `ExecutionCh`에서 수집한 `OrderCancelled` 개수 == 1.

- [ ] **Step 2: crash hook을 arm 가능하게 만든다**

`crashHook`은 **Start 전에 설치하고, arm은 hook 내부의 atomic으로 한다.** 실행 중 함수 필드를 교체하면 data race다.

`internal/matching/engine.go`에 헬퍼를 추가한다(테스트 파일이 아니라 프로덕션 파일에 둔다 — `internal/service` 테스트가 써야 하므로).

```go
// CrashArmer는 테스트가 크래시 시점을 제어하는 손잡이다.
// Start 전에 InstallCrashHook으로 설치하고, setup이 끝난 뒤 Arm을 부른다.
type CrashArmer struct {
	armed     atomic.Bool
	remaining atomic.Int64
}

// Arm은 이 시점 이후 n번째 조각이 끝난 직후 엔진을 죽이도록 한다.
// n=1이면 arm 이후 첫 조각 직후다.
func (a *CrashArmer) Arm(n int) {
	a.remaining.Store(int64(n))
	a.armed.Store(true)
}

func (a *CrashArmer) shouldCrash() bool {
	if !a.armed.Load() {
		return false
	}
	// n번째 조각 **직후** 발동한다. Add가 0을 반환하는 그 호출이 n번째다.
	return a.remaining.Add(-1) <= 0
}

// InstallCrashHook은 Start 전에만 부른다. 테스트 전용이다 —
// 프로덕션에서는 crashHook이 항상 nil이다.
func (me *MatchingEngine) InstallCrashHook(a *CrashArmer) {
	me.crashHook = a.shouldCrash
}
```

`ShardedEngine`에도 샤드 0을 겨냥한 헬퍼를 둔다(테스트는 단일 심볼만 쓴다):

```go
// InstallCrashHookOnShardFor는 그 심볼을 소유한 샤드에 hook을 설치한다.
func (se *ShardedEngine) InstallCrashHookOnShardFor(coinSymbol string, a *CrashArmer) {
	se.shardFor(coinSymbol).InstallCrashHook(a)
}
```

- [ ] **Step 3: crash 통합 테스트 2개를 쓴다**

**두 개만 둔다.** 별도 crash hook 단위 테스트를 만들지 않고, 두 통합 테스트가 hook의 정확성까지 함께 증명한다.

| # | 테스트 함수명 | 크래시 지점 |
|---|---|---|
| A-1 | `TestCrashBetweenSlicesBeforeTradeOutboxCommit` | outbox flush 전 |
| A-2 | `TestCrashBetweenSlicesAfterPartialOutboxCommit` | 일부 trade가 outbox에 커밋된 뒤 |

**두 테스트가 함께 증명해야 하는 것:**

1. setup maker 처리가 **완료된 뒤** hook이 arm된다 (setup 조각에 걸리면 taker sweep을 못 잰다)
2. 크래시는 실제 taker sweep 중 **`0 < trades < makers`** 인 시점
3. 기존 엔진 goroutine 종료 확인 (`<-me.Done()`)
4. 크래시 후 기존 엔진이 **추가 write를 하지 않음**
5. 재기동 시 시장가 sweep **재개 없음**
6. 커밋된 체결까지만 인정되고 잔량 hold가 정확히 처리됨

공통 골격:

```go
	armer := &matching.CrashArmer{}
	se.InstallCrashHookOnShardFor("BTC", armer) // Start 전

	// ... maker setup, OrderDone 장벽으로 완료 확인 ...

	armer.Arm(crashAfterSlices) // ① setup 완료 뒤에 arm

	// ... taker 투입 ...

	select {
	case <-se.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("crash hook이 엔진을 멈추지 않았다")
	}

	// ② 크래시 시점이 sweep 중간이었는지
	require.Greater(t, tradesObserved.Load(), int64(0), "체결 전에 크래시했다")
	require.Less(t, tradesObserved.Load(), int64(makers), "sweep이 끝난 뒤 크래시했다")

	// ③ 죽은 엔진이 더 이상 쓰지 않는지
	before := tradesObserved.Load()
	time.Sleep(200 * time.Millisecond)
	require.Equal(t, before, tradesObserved.Load(), "죽은 엔진이 계속 쓰고 있다")

	// ④ 크래시는 채널을 닫지 않는다 — graceful shutdown이 아니다
	select {
	case _, ok := <-execCh:
		require.True(t, ok, "크래시가 ExecutionCh를 닫았다")
	default:
	}
```

A-1은 outbox writer를 붙이지 않고 크래시시킨 뒤, 재기동해서 **그 체결이 DB에 없고 홀드가 크래시 전 값 그대로**임을 단언한다.

A-2는 outbox flush를 확인한 뒤 크래시시키고, replay + 새 엔진 bootstrap 후 **`FilledAmount`가 커밋된 체결 합과 일치**하고 `StaleMarketOrderFinalizer`가 잔량을 정확히 해제하며 **시장가 taker가 book에 없음**(sweep 재개 없음)을 단언한다.

- [ ] **Step 4: focused test를 돌린다**

테스트 이름을 하나씩 다 적는다. **정규식이 어긋나 0건 매치가 되면 `go test`는 성공으로 끝난다.**

```powershell
cd Go-exchange-back; go test -count=1 -timeout 600s ./internal/service -v -run 'TestActiveSweepCancelReturnsNotFoundThenRemovesRemainder|TestActiveSweepFullFillLeavesNothingToCancel|TestActiveMarketSweepCancelIsNotPreempted|TestMakerCancelledBetweenSlicesEscapesFill|TestCrashBetweenSlicesBeforeTradeOutboxCommit|TestCrashBetweenSlicesAfterPartialOutboxCommit'
```

Expected: `--- PASS` **정확히 6줄.** 6줄이 아니면 정규식이나 테스트명이 어긋난 것이다.

기존 worker 테스트도 확인한다:

```powershell
cd Go-exchange-back; go test -count=1 ./internal/service -run 'TestCancelCommandWorker|TestStaleMarket'
```

Expected: `ok`.

- [ ] **Step 5: 커밋 3의 앞부분 — quantum 구현을 스테이징한다**

아직 커밋하지 않는다. Task 8에서 선택값을 넣은 뒤 함께 커밋한다.

---

## Task 8: 선택값 확정과 focused 검증

> **로컬 wall-clock C3는 `LOCAL_NOT_MEASURABLE`로 종료됐다**(설계 §8.4).
> 단일 sweep(±9~11%), 묶음 64(32.55%), 묶음 128(13.34%) 모두 ±5% 구분력 확보에 실패했다.
> 작업량 불일치와 0ns는 제거됐지만 실행 간 시간 변동이 남았고, **그 근본 원인은 미확정**이다.
> 시간 격자 탐색을 다시 돌리지 않는다.

**Files:** `internal/matching/quantum_config.go`, `config/runtime.go`,
`internal/matching/quantum_contract_test.go`(신규),
`internal/matching/quantum_selected_gates_test.go`(신규)

**선택값은 사전 등록 규칙으로 결정된다. 시간 측정값은 순위에 넣지 않는다.**

1. `maxMatchesPerTurn`이 큰 값 우선 → 128
2. 같으면 `maxConsecutiveCancels`가 작은 값 우선 → 8

→ **`m128-c8`**. "가장 빠른 값"이 아니라 **격자 중 추가 yield가 가장 적고(5,000 체결당 39회)
progress 전 연속 취소 상한이 가장 작은** 값이다.

- [ ] **Step 1: 시간 기반 C3 경로를 제거한다**

다음을 삭제한다. 방치하지 않는다.

- `quantum_precision_test.go` (precision 64/128 실행기)
- `quantum_batch_test.go`, `quantum_batch_contract_test.go` (묶음 타이밍 하니스)
- `quantum_aggregate.go`, `quantum_select.go`와 각 테스트 (시간 비율 C3 게이트)
- `cmd/quantumselect` (시간 기반 C3 적용)
- `quantum_scenarios_test.go`, `quantum_harness_test.go`, `quantum_diagnostic_test.go`
  (무효화된 자료의 생성기·진단기)

산출물 JSON은 `_workspace/quantum/`에 **전부 보존**한다. `precision/`은 실패 증거다.

- [ ] **Step 2: 결정적 계수 계약을 추가한다**

`internal/matching/quantum_config.go`에 순수 함수를 둔다.

```go
func ExpectedSlices(trades, maxMatchesPerTurn int) int  // ceil, budget<=0이면 1
func ExpectedYields(trades, maxMatchesPerTurn int) int  // slices - 1
```

**이 값은 처리량 손실률이 아니다.** sweep을 몇 조각으로 나눴고 제어점으로 몇 번 더 돌아왔는지만
뜻한다.

- [ ] **Step 3: 선택값을 두 곳에 확정한다**

`internal/matching/quantum_config.go`와 `config/runtime.go`의 기본값을 **같은 값으로**
128 / 8로 바꾼다. 두 곳이 어긋나면 테스트와 프로덕션이 다른 값으로 돈다.

- [ ] **Step 4: 결정적 계약 focused 테스트 (빌드 태그 없음)**

`quantum_contract_test.go` — 시계와 무관하므로 일반 `go test`에서 돈다.

- `ExpectedSlices`/`ExpectedYields` 순수 함수 11 케이스
- `m128-c8`에서 5,000 체결 sweep의 실제 yield == 39, 체결 수 == 5,000, 잔량 == 0
- 취소 상한이 설정값(8, 32)에 따라 실제로 움직임

- [ ] **Step 5: 안전 상한 focused 측정 (빌드 태그 `quantumharness`)**

`quantum_selected_gates_test.go` — 시간 단언이 있어 CI에서 흔들릴 수 있으므로 태그를 건다.

**후보 간 미세 비교가 아니라 300ms 안전 상한 확인이다.** 선택값 하나만 돌린다.

- C1: **첫 trade 관측 뒤** 취소를 넣고 그 취소 대기를 측정 (순서가 계약이다)
- C4: 같은 sweep 도중의 스냅샷 최대 간격
- C2: 취소 홍수 중 신규 주문 큐 대기
- C5: 표본 누락 0

측정값이 시계 해상도 이하면 **"0초"라고 단정하지 않고 "측정 해상도 이하"로 기록**한다.

- [ ] **Step 6: C6 의미 테스트를 선택값으로 확인한다**

B′ 4건 + crash hook 1건 + 스케줄러 계약 5건 + 조각화 2건.

**하나라도 실패하면 다음 후보로 자동 이동하지 말고 중단·보고한다.**

- [ ] **Step 7: 문서에 판정을 남긴다**

- 취소 기아 방지 구조: 검증됨
- `m128-c8` 결정적 스케줄링 계약: 검증됨
- 로컬 ±5% 처리량 보존: **판정 불가**
- 실제 처리량·500 VU 회귀: **최종 통합 GCP까지 보류**
- 이전 "5.11ms → 0s": 정확한 성능 비교값으로 사용하지 않음
- GCP: 지금 실행하지 않음

---

## Task 9: 전체 로컬 검증과 CI

**여기서 처음이자 마지막으로 전체 검증을 돌린다.**

- [ ] **Step 1: 네 명령을 정확히 한 번씩 돌린다**

```powershell
cd Go-exchange-back
go test -count=1 ./...
go test -count=1 -race ./internal/matching ./internal/service
go vet ./...
go build -trimpath ./cmd
```

Expected: 전 패키지 `ok` / race 없음 / vet 출력 없음 / build 성공.

하니스가 CI에 들어가지 않는지도 확인한다:

```powershell
cd Go-exchange-back; go test -count=1 ./internal/matching -run TestHarness
```

Expected: `no tests to run`.

- [ ] **Step 2: 커밋 3 — quantum 구현·선택값·로컬 검증**

`commit-message` 스킬로 메시지를 만든 뒤 스테이징한다.

```powershell
cd Go-exchange-back; git add internal/matching/ internal/service/ config/ cmd/ _workspace/quantum/
```

- [ ] **Step 3: 푸시하고 CI를 확인한다**

```powershell
cd Go-exchange-back; git push
```

CI가 success여야 한다. 실패하면 **P1 중단**.

- [ ] **Step 4: 로컬 완료 보고 (CP A)**

한 번에 보고한다: 문서 SHA와 구현 SHA / 선택된 quantum 값 / baseline 3회 및 6조합 탐색 결과 / 상위 2개 확증 결과 / 핵심 테스트 결과 / 전체 test·race·vet·build 결과 / CI 결과 / 누적 P2 / **GCP 유료 승인 요청**.

---

# CP B — GCP 완료

## Task 10: GCP 500 VU 회귀 게이트

**별도 유료 승인 없이 시작하지 않는다.**

- [ ] **Step 1: 유료 승인을 받는다**

기존 4 VM, 기존 토폴로지·machine type, ramp 30s + hold 10m, 500 VU **정확히 1회**. 신규 VM·방화벽·IAM·machine type 변경 없음. 자동 재실행 금지.

- [ ] **Step 2: 측정용 compose override에 선택값을 명시한다**

**미설정 기본값에 의존하지 않는다.** 기본값에 기대면 코드가 바뀌었을 때 측정이 조용히 다른 값으로 돈다.

```yaml
# docker-compose.quantum.yml
services:
  server:
    environment:
      GOEXCHANGE_MATCHING_MAX_MATCHES_PER_TURN: "<선택값>"
      GOEXCHANGE_MATCHING_MAX_CONSECUTIVE_CANCELS: "<선택값>"
```

- [ ] **Step 3: preflight를 통과시킨다**

[gcp-stress-test-runbook.md](../../gcp-stress-test-runbook.md) 1~6절을 따른다.

| 항목 | 기준 |
|---|---|
| concurrency 값 | 3곳 일치 (8) |
| dev token | 인증 200 + 구토큰 403. **값·hash·fingerprint 미출력** |
| load-gen linger | `enable-linger` 완료 |
| SSH 경로 | server·db는 `--tunnel-through-iap`, load-gen은 집 IP 직접 SSH |
| 기준 SHA | Task 9에서 푸시한 커밋과 일치 |
| **quantum 값** | 아래 세 곳이 모두 선택값과 일치 |

```powershell
docker compose -f docker-compose.yml -f docker-compose.quantum.yml config | Select-String 'GOEXCHANGE_MATCHING_MAX'
docker compose exec -T server env | Select-String 'MATCHING'
docker compose logs server | Select-String 'matching engine sharded'
```

Expected: 셋 다 `maxMatchesPerTurn=<선택값> maxConsecutiveCancels=<선택값>`과 일치.

**preflight 실패 = 부하 실행 금지. P1 중단.**

- [ ] **Step 4: 500 VU 1회를 실행한다**

runbook 6절. ramp 30s + hold 10m. **실행이 무효가 되어도 자동 재실행하지 않는다.**

- [ ] **Step 5: VM을 정지하고 TERMINATED를 조회한다 — 분석보다 먼저**

```powershell
gcloud compute instances stop goexchange-stress-server goexchange-stress-db --zone <zone>
gcloud compute instances stop goexchange-stress-load-gen goexchange-stress-load-gen-b --zone <zone>
gcloud compute instances list --format="table(name,status)"
```

Expected: 4대 모두 `TERMINATED`. **정지 명령을 실행했다는 사실은 판정 근거가 아니다. 조회 결과가 판정이다.** VM·디스크를 삭제하지 않는다.

- [ ] **Step 6: 36번의 정확한 게이트로 판정한다**

k6 측 (A·B 각각): 주문 응답 가용성 100.00%/fail 0, 주문 업무 성공 100.00%/fail 0, 1초 계약 초과 0건, HTTP 실패 0, k6 checks 실패 0, 취소 성공률 100.00%, 멱등성 계약 위반(400·409) 0, 202 PENDING 0.

서버·DB 측: `failed_settlements`·`failed_market_completions`·`failed_order_cancellations`·`reconciliation_violations` 각 0 / 주문 수 = 키 수 = `ORDER_HOLD` = k6 iteration 합계 완전 일치 / outcome `ACCEPTED` 100%, PENDING·REJECTED·UNKNOWN 0 / 키 1건 다중 주문 0 / 주문 1건 다중 키 0 / 주문 1건 hold 2건 이상 0 / 키 없는 주문 0 / stale PENDING(5분 초과) 0 / `POST /orders` 200만.

이번 변경 고유:

| 항목 | 기준 |
|---|---|
| 신규 지표 8종 — family 존재 | 8종 전부 `/metrics`에 노출 |
| 표본 수 | `turn_duration`·`order_queue_wait`·`executions_per_order`·`matches_per_slice`·`emit_block{event="trade"}`·**`emit_block_per_slice_seconds`** 의 `_count` > 0 |
| `emit_block_per_slice_seconds` | 주문 slice가 존재하는 한 **`_count` 0은 배선 실패다.** `_sum` 0은 허용 — 하류가 한 번도 안 막혔을 수 있다 |
| 0이어도 정상 | `quantum_yields_total`, `cancel_queue_wait_count`, `emit_block{event="done"}`. 0이면 워크로드 구성으로 설명해 보고서에 적는다 |
| p95 | **참고값으로만. quantum 효과로 귀속하지 않는다** |

- [ ] **Step 7: 산출물 시크릿 게이트를 통과시킨다**

[runbook §7.5](../../gcp-stress-test-runbook.md)의 7단계를 **그대로** 따른다. 줄이지 않는다.

```powershell
python _workspace/loadtest/redact_summary.py <phase>-summary-a.json <phase>-summary-b.json
```

3단계(metrics 불변) 또는 5단계(패턴 히트) 실패 = **P1 중단**. 스캔 보고는 **파일별 hit 개수와 종류만** 남긴다. 값·해시·fingerprint를 출력하지 않는다. 발견해도 즉시 수정·삭제하지 않고 먼저 보고한다.

---

## Task 11: 보고서와 문서 완료

- [ ] **Step 1: 37번 보고서를 쓴다**

`docs/benchmarks/37-YYYY-MM-DD-matching-quantum.md`. 구성: 왜 필요했는지 / 무엇을 바꿨는지 / preflight 결과 / 하드 게이트 결과(Task 10 Step 6 전 항목) / baseline 대비 전후 비교(**sweep 크기별로 나눠서**) / 선택된 quantum 값과 근거 / 계측 오버헤드 / p95 비귀속 명시 / 한계와 남은 부채.

**개별 회차가 상한을 넘었으나 중앙값이 통과한 경우, 회차별 값과 초과 폭을 그대로 기록한다.** 숨기지 않는다.

`MatchLatencyObserver` 값이 늘어난 것은 회귀가 아니라 설계된 교환이다 — **sweep 크기별로 나눠 비교하고 전체 평균으로 비교하지 않는다.**

- [ ] **Step 2: 기존 문서 3건을 갱신한다**

새 보고서만 쓰고 기존 문서를 두면, 저장소가 "quantum 미구현"이라고 말하는 상태로 남는다.

| 파일 | 무엇을 |
|---|---|
| `docs/ENGINEERING-SUMMARY.md` | **185행 `### 축 2 — 매칭 quantum (**미구현**)`을 구현 완료로.** 확정된 두 값, `selection.md` 링크, 37번 링크, §14 R1(`ExecutionCh` 무timeout으로 wall-clock 진행성 미보장)이 남은 한계임을 적는다. 249행의 "축 2 계약 6개 확정 후 quantum 구현 여부 결정"도 결정 결과로 |
| `docs/refactor/README.md` | B-3 항목 완료 표시, 다음 우선순위(B 잔여 4·5·6) |
| `docs/benchmarks/README.md` | 37번 항목 추가. **19~35번 목록 공백은 범위 밖 — 채우지 않는다** |

```powershell
cd Go-exchange-back; Select-String -Path docs\ENGINEERING-SUMMARY.md -Pattern '미구현'
```

Expected: 갱신 후 매칭 quantum 관련 `미구현` 표기 없음.

- [ ] **Step 3: 계획서 체크박스를 완료 처리한다**

모든 `- [ ]`를 `- [x]`로 바꾸고, 문서 머리에 완료 배너(최종 SHA, baseline SHA, 확정된 quantum 두 값, 37번 링크)를 넣는다.

- [ ] **Step 4: 커밋 4 — GCP 보고서·최종 문서**

```powershell
cd Go-exchange-back; git add docs/ _workspace/
```

- [ ] **Step 5: CP B 보고**

전체 게이트 결과 / 시크릿 게이트 결과(hit 개수·종류만) / 4 VM TERMINATED 조회 결과 / 37번 링크 / 누적 P2 전체.