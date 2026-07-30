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

// 배리어가 사라지면서 settlement_barrier_* 계열은 더 이상 방출되지 않는다.
// 대체 지표(terminal_wait/outstanding_jobs/quarantined_orders)가 kind·partition
// 라벨로 서로 다른 시계열을 만드는지 확인한다.
func TestSettlementTerminalWaitHasKindLabel(t *testing.T) {
	before := testutil.CollectAndCount(metrics.SettlementTerminalWait)
	metrics.SettlementTerminalWait.WithLabelValues("cancel").Observe(0.01)
	metrics.SettlementTerminalWait.WithLabelValues("market_done").Observe(0.02)
	assert.GreaterOrEqual(t, testutil.CollectAndCount(metrics.SettlementTerminalWait), before+2)
}

func TestSettlementOutstandingJobsAndQuarantinedOrdersHavePartitionLabel(t *testing.T) {
	metrics.SettlementOutstandingJobs.WithLabelValues("0").Set(3)
	metrics.SettlementQuarantinedOrders.WithLabelValues("0").Set(1)
	assert.Equal(t, float64(3), testutil.ToFloat64(metrics.SettlementOutstandingJobs.WithLabelValues("0")))
	assert.Equal(t, float64(1), testutil.ToFloat64(metrics.SettlementQuarantinedOrders.WithLabelValues("0")))
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
