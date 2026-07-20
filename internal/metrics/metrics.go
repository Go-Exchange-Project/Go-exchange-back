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
