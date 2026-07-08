package metrics

import (
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
