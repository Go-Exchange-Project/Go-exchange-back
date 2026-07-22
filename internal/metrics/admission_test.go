package metrics_test

import (
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

func TestOrdersAdmissionRejectedTotalIncrementsPerStage(t *testing.T) {
	before := testutil.ToFloat64(metrics.OrdersAdmissionRejectedTotal.WithLabelValues("engine_gate"))
	metrics.OrdersAdmissionRejectedTotal.WithLabelValues("engine_gate").Inc()
	after := testutil.ToFloat64(metrics.OrdersAdmissionRejectedTotal.WithLabelValues("engine_gate"))
	assert.Equal(t, before+1, after)
}
