package main

import (
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func histogramVecSampleCount(t *testing.T, hv *prometheus.HistogramVec, label string) uint64 {
	t.Helper()
	h, ok := hv.WithLabelValues(label).(prometheus.Histogram)
	require.True(t, ok, "HistogramVec.WithLabelValues는 prometheus.Histogram을 구현해야 한다")
	m := &dto.Metric{}
	require.NoError(t, h.Write(m))
	return m.GetHistogram().GetSampleCount()
}

func TestSettleTradeBatchObservesOneAttemptSample(t *testing.T) {
	before := histogramVecSampleCount(t, metrics.SettlementAttemptDuration, "batch")

	batchSettler := &fakeTradeBatchSettler{results: []service.SettlementResult{{Applied: true, TradeID: 1}}}
	settler := &fakeTradeSettler{}
	marker := &fakeOutboxMarker{}
	batch := []service.OutboxEvent{tradeOutboxEvent(1, 1)}

	settleTradeBatchWithFallback(batch, batchSettler, settler, nil, nil, nil, nil, nil, nil,
		func(string, []byte) {}, marker, discardLogger())

	after := histogramVecSampleCount(t, metrics.SettlementAttemptDuration, "batch")
	assert.Equal(t, before+1, after, "배치 DB 호출 1회당 attempt 샘플 1개")
}

func TestProcessTradeSettlementObservesEachRetryAttempt(t *testing.T) {
	withFastTransientRetries(t)
	before := histogramVecSampleCount(t, metrics.SettlementAttemptDuration, "single")
	beforeLegacy := histogramSampleCount(t, metrics.OrderSettlementDuration)

	settler := &fakeTradeSettler{
		errs:   []error{deadlockError(), deadlockError(), nil},
		result: service.SettlementResult{Applied: true, TradeID: 1},
	}

	processTradeSettlement(testTrade(), 0, settler, nil, func(string, []byte) {}, discardLogger())

	after := histogramVecSampleCount(t, metrics.SettlementAttemptDuration, "single")
	afterLegacy := histogramSampleCount(t, metrics.OrderSettlementDuration)
	assert.Equal(t, before+3, after, "재시도 3회 = attempt 샘플 3개")
	assert.Equal(t, beforeLegacy+1, afterLegacy, "기존 메트릭은 논리 1건 그대로(의미 불변)")
}
