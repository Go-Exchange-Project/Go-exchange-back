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
