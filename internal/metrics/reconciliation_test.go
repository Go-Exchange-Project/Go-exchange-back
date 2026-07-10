package metrics_test

import (
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

func TestReconciliationViolationsGaugeVecSetsPerCheckLabel(t *testing.T) {
	metrics.ReconciliationViolations.WithLabelValues("ledger_wallet").Set(3)
	metrics.ReconciliationViolations.WithLabelValues("asset_conservation").Set(0)

	assert.Equal(t, float64(3), testutil.ToFloat64(metrics.ReconciliationViolations.WithLabelValues("ledger_wallet")))
	assert.Equal(t, float64(0), testutil.ToFloat64(metrics.ReconciliationViolations.WithLabelValues("asset_conservation")))
}

func TestReconciliationLastRunTimestampGaugeIsSettable(t *testing.T) {
	metrics.ReconciliationLastRunTimestamp.Set(1234567890)
	assert.Equal(t, float64(1234567890), testutil.ToFloat64(metrics.ReconciliationLastRunTimestamp))
}

func TestReconciliationCheckErrorsTotalCounterIncrementsPerCheckLabel(t *testing.T) {
	before := testutil.ToFloat64(metrics.ReconciliationCheckErrorsTotal.WithLabelValues("stale_market_order"))
	metrics.ReconciliationCheckErrorsTotal.WithLabelValues("stale_market_order").Inc()
	after := testutil.ToFloat64(metrics.ReconciliationCheckErrorsTotal.WithLabelValues("stale_market_order"))
	assert.Equal(t, before+1, after)
}
