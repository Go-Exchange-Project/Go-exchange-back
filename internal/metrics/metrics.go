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

func RegisterMatchingEngineChannelLenGauges(orderLen, cancelLen, snapshotReqLen, executionLen, snapshotLen func() int) {
	gauges := []struct {
		channel string
		lenFn   func() int
	}{
		{"order", orderLen},
		{"cancel", cancelLen},
		{"snapshot_request", snapshotReqLen},
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
