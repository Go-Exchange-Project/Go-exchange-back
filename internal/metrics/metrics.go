package metrics

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// matchLatencyBuckets extends the Prometheus default buckets (max 10s) with
// 15/20/30/45/60s so tail latencies above 10s are not all clipped into the
// +Inf bucket, which made histogram_quantile report a flat 10s ceiling.
var matchLatencyBuckets = append(append([]float64{}, prometheus.DefBuckets...), 15, 20, 30, 45, 60)

var (
	HTTPRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total HTTP requests processed, labeled by method, path, and status code.",
	}, []string{"method", "path", "status"})

	HTTPRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request duration in seconds, labeled by method, path, and status code.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path", "status"})

	OrderPipelineMatchLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "order_pipeline_match_latency_seconds",
		Help:    "Time from order enqueue into the matching engine to completion of matching for that order.",
		Buckets: matchLatencyBuckets,
	})

	OrderSettlementDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "order_settlement_duration_seconds",
		Help:    "Time to persist trade settlement (wallet/ledger updates) after a match event.",
		Buckets: matchLatencyBuckets,
	})

	ReconciliationViolations = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "reconciliation_violations",
		Help: "Violation count from the most recent reconciliation run, labeled by check name.",
	}, []string{"check"})

	ReconciliationLastRunTimestamp = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "reconciliation_last_run_timestamp_seconds",
		Help: "Unix timestamp of the most recent reconciliation run.",
	})

	ReconciliationCheckErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "reconciliation_check_errors_total",
		Help: "Total number of reconciliation check queries that failed to execute, labeled by check name.",
	}, []string{"check"})

	TradeOutboxFlushDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "trade_outbox_flush_seconds",
		Help:    "Time to commit one outbox batch INSERT.",
		Buckets: prometheus.DefBuckets,
	})

	TradeOutboxFlushBatchSize = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "trade_outbox_flush_batch_size",
		Help:    "Number of events per committed outbox batch (group commit efficiency).",
		Buckets: []float64{1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024},
	})

	TradeOutboxWriteErrorsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "trade_outbox_write_errors_total",
		Help: "Total outbox batch INSERT failures (each retried until success).",
	})

	// CancelCommandLatency measures the durable cancel path end to end. The
	// application does not promise an upper bound on it, so this histogram is
	// the only basis for discussing one later.
	CancelCommandLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "cancel_command_latency_seconds",
		Help:    "Time from durable cancel command creation to PROCESSED or NOOP commit.",
		Buckets: matchLatencyBuckets,
	})

	// A rising counter means the outbox commit is stalled, not that a command
	// was lost: the worker keeps holding it instead of re-dispatching.
	CancelCommandAwaitingOutboxDeadlineTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cancel_command_awaiting_outbox_deadline_total",
		Help: "Cancel commands still awaiting the atomic outbox commit after the warning deadline.",
	})

	// 보상 실패 후 UNKNOWN 기록에 성공한 건수. 실패해서 PENDING에 머문 경우는
	// 여기 잡히지 않으므로 아래 counter가 함께 필요하다.
	OrderIdempotencyUnknownTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "order_idempotency_unknown_total",
		Help: "Order idempotency records marked UNKNOWN after a failed compensation.",
	})

	OrderIdempotencyOutcomeUpdateFailuresTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "order_idempotency_outcome_update_failures_total",
		Help: "Failed attempts to record ACCEPTED/REJECTED/UNKNOWN on an idempotency record.",
	})

	// counter는 "그 순간 코드가 살아 있었다"를 전제한다. 프로세스가 hold 커밋 직후
	// 죽으면 아무 counter도 오르지 않으므로 gauge가 필요하다.
	OrderIdempotencyStalePending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "order_idempotency_stale_pending",
		Help: "Idempotency records still PENDING past the staleness threshold.",
	})

	OrderIdempotencyMonitorErrorsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "order_idempotency_monitor_errors_total",
		Help: "Failed stale-pending queries. The gauge keeps its last value on failure.",
	})

	SettlementBatchSize = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "settlement_batch_size",
		Help:    "Number of trades per committed settlement batch. Stuck at 1 indicates low load or drain not happening.",
		Buckets: []float64{1, 2, 4, 8, 16, 32},
	})

	SettlementBatchFallbacksTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "settlement_batch_fallbacks_total",
		Help: "Total number of times batch settlement failed and fell back to per-trade settlement.",
	})

	HoldBatchSize = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "hold_batch_size",
		Help:    "Number of orders per committed hold batch (group commit efficiency).",
		Buckets: []float64{1, 2, 4, 8, 16, 32, 64, 128},
	})

	HoldBatchFallbacksTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "hold_batch_fallbacks_total",
		Help: "Total number of times batch hold failed and fell back to per-order persist+hold.",
	})

	OrdersAdmissionRejectedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "orders_admission_rejected_total",
		Help: "Total orders fast-rejected by admission control (503), labeled by shedding stage.",
	}, []string{"stage"})

	// 차단된 polling "횟수"다 — 현재 차단된 주문 수가 아니다(rate/increase로 지속 차단 탐지).
	SettlementCompletionBlockedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "settlement_completion_blocked_total",
		Help: "Times a market-completion retry was skipped due to an OPEN failed settlement dependency.",
	})

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

	// 4차 축 1 관측성: DB 호출 1회(트랜잭션 시도) 단위. 기존 order_settlement_duration_seconds
	// (논리적 단건 정산 전체)와는 의미가 다른 별도 메트릭이다 — 기존 것은 그대로 보존한다.
	SettlementAttemptDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "settlement_attempt_duration_seconds",
		Help:    "Duration of one settlement DB call (transaction attempt).",
		Buckets: prometheus.DefBuckets,
	}, []string{"path"})

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

	// 같은 주문의 두 번째 terminal을 거부한 횟수. 주문당 terminal 1개는 엔진
	// 불변식이므로 정상값은 항상 0이다 — 실측의 비협상 무결성 게이트로 쓴다.
	// 종류는 오류 로그로 식별할 수 있어 라벨을 두지 않는다.
	SettlementDuplicateTerminalTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "settlement_duplicate_terminal_total",
		Help: "Second terminal events rejected for an order that already has one waiting (engine invariant violation).",
	})

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
	SettlementAttemptBatch  = SettlementAttemptDuration.WithLabelValues("batch")
	SettlementAttemptSingle = SettlementAttemptDuration.WithLabelValues("single")
	SettlementJobSuccess    = SettlementJobExecution.WithLabelValues("success")
	SettlementJobFallback   = SettlementJobExecution.WithLabelValues("fallback")
	SettlementJobFailed     = SettlementJobExecution.WithLabelValues("failed")
)

// RegisterSettlementWorkerQueueGauges는 심볼 파티셔닝된 정산 워커 큐의 적체를
// 워커 인덱스 라벨로 노출합니다. 핫 심볼 쏠림을 관측하는 용도입니다.
func RegisterSettlementWorkerQueueGauges(queueLenFns []func() int) {
	for i, lenFn := range queueLenFns {
		lenFn := lenFn
		promauto.NewGaugeFunc(prometheus.GaugeOpts{
			Name:        "settlement_worker_queue_length",
			Help:        "Current number of buffered execution events in a settlement worker queue.",
			ConstLabels: prometheus.Labels{"worker": strconv.Itoa(i)},
		}, func() float64 { return float64(lenFn()) })
	}
}

func RegisterMatchingEngineChannelLenGauges(orderLen, cancelLen, executionLen, snapshotLen func() int) {
	gauges := []struct {
		channel string
		lenFn   func() int
	}{
		{"order", orderLen},
		{"cancel", cancelLen},
		{"execution", executionLen},
		{"snapshot", snapshotLen},
	}
	for _, g := range gauges {
		g := g
		promauto.NewGaugeFunc(prometheus.GaugeOpts{
			Name:        "matching_engine_channel_length",
			Help:        "Current number of buffered items in a matching engine channel.",
			ConstLabels: prometheus.Labels{"channel": g.channel},
		}, func() float64 { return float64(g.lenFn()) })
	}
}

// RegisterMatchingEngineShardOrderChannelLenGauges는 샤딩된 매칭 엔진(B-3)의
// 샤드별 order 채널 적체를 노출합니다. 20번 벤치마크가 단일 엔진의 채널 게이지로
// 병목을 잡아낸 선례를 따라, 샤딩 후에도 샤드별 불균형을 관측할 수 있게 합니다.
func RegisterMatchingEngineShardOrderChannelLenGauges(orderLenFns []func() int) {
	for i, lenFn := range orderLenFns {
		lenFn := lenFn
		promauto.NewGaugeFunc(prometheus.GaugeOpts{
			Name:        "matching_engine_shard_order_channel_length",
			Help:        "Current number of buffered items in a single shard's order channel (B-3).",
			ConstLabels: prometheus.Labels{"shard": strconv.Itoa(i)},
		}, func() float64 { return float64(lenFn()) })
	}
}

// RegisterHoldCoordinatorInputGauge는 홀드 코디네이터 입력 채널의 적체를 노출합니다.
func RegisterHoldCoordinatorInputGauge(lenFn func() int) {
	promauto.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "hold_coordinator_input_length",
		Help: "Current number of buffered requests in the hold coordinator's input channel.",
	}, func() float64 { return float64(lenFn()) })
}
