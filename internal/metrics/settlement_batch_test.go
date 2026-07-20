package metrics_test

import (
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

func TestSettlementBatchSizeHistogramObserveRecordsBatchSize(t *testing.T) {
	// Observe a batch size value - histogram should record it
	metrics.SettlementBatchSize.Observe(5)

	// Verify the histogram was registered and collected
	metricCount := testutil.CollectAndCount(metrics.SettlementBatchSize, "settlement_batch_size")
	assert.Greater(t, metricCount, 0, "histogram should be collected")
}

func TestSettlementBatchFallbacksTotalCounterIncrementsOnFallback(t *testing.T) {
	before := testutil.ToFloat64(metrics.SettlementBatchFallbacksTotal)
	metrics.SettlementBatchFallbacksTotal.Inc()
	after := testutil.ToFloat64(metrics.SettlementBatchFallbacksTotal)

	// The counter should increase by 1 after one increment
	assert.Equal(t, before+1, after)
}

func TestHoldBatchSizeHistogramObserveRecordsBatchSize(t *testing.T) {
	metrics.HoldBatchSize.Observe(5)

	metricCount := testutil.CollectAndCount(metrics.HoldBatchSize, "hold_batch_size")
	assert.Greater(t, metricCount, 0, "histogram should be collected")
}

func TestHoldBatchFallbacksTotalCounterIncrementsOnFallback(t *testing.T) {
	before := testutil.ToFloat64(metrics.HoldBatchFallbacksTotal)
	metrics.HoldBatchFallbacksTotal.Inc()
	after := testutil.ToFloat64(metrics.HoldBatchFallbacksTotal)

	assert.Equal(t, before+1, after)
}
