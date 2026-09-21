package main

import (
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func counterVecValue(t *testing.T, cv *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	c, err := cv.GetMetricWithLabelValues(labels...)
	require.NoError(t, err)
	m := &dto.Metric{}
	require.NoError(t, c.Write(m))
	return m.GetCounter().GetValue()
}

// 설계 §4.2: goexchange_db_timeout_total{sqlstate,path}는 55P03·57014 두
// SQLSTATE만 경로 라벨과 함께 센다. 실제 재시도 루프의 시도별 결과에서만
// 증가해야 한다 — attempt 수만큼(초기 1회 + 재시도 횟수) 늘어난다.
func TestDBTimeoutMetricIncrementsPerAttemptOnSettlementPath(t *testing.T) {
	withFastTransientRetries(t)
	before := counterVecValue(t, metrics.DBTimeoutTotal, "57014", "settlement")

	settler := &fakeTradeSettler{err: statementTimeoutError()}
	processTradeSettlement(testTrade(), 0, settler, &fakeFailureRecorder{}, func(string, []byte) {}, discardLogger())

	after := counterVecValue(t, metrics.DBTimeoutTotal, "57014", "settlement")
	assert.Equal(t, before+float64(1+len(transientRetryDelays)), after, "재시도 attempt마다 1씩 늘어야 한다")
}

func TestDBTimeoutMetricIncrementsPerAttemptOnMarketCompletionPath(t *testing.T) {
	withFastTransientRetries(t)
	before := counterVecValue(t, metrics.DBTimeoutTotal, "57014", "market_completion")

	completer := &fakeMarketCompleter{err: statementTimeoutError()}
	processMarketOrderDone(testMarketOrderDone(), completer, &fakeDependencyGuard{}, &fakeCompletionFailureRecorder{}, discardLogger())

	after := counterVecValue(t, metrics.DBTimeoutTotal, "57014", "market_completion")
	assert.Equal(t, before+float64(1+len(transientRetryDelays)), after)
}

func TestDBTimeoutMetricIncrementsPerAttemptOnCancellationPath(t *testing.T) {
	withFastTransientRetries(t)
	before := counterVecValue(t, metrics.DBTimeoutTotal, "57014", "cancellation")

	processor := &fakeCancelProcessor{err: statementTimeoutError()}
	processOrderCancellationEvent(
		&matching.OrderCancelled{OrderID: 1, CoinSymbol: "BTC"},
		1, processor, &fakeDependencyGuard{}, &fakeCancellationDeferStore{}, discardLogger(),
	)

	after := counterVecValue(t, metrics.DBTimeoutTotal, "57014", "cancellation")
	assert.Equal(t, before+float64(1+len(transientRetryDelays)), after)
}

// 대조군: 순수 분류 함수 호출만으로는 늘지 않는다(중복 계측 방지).
func TestDBTimeoutMetricDoesNotIncrementOnPureClassificationCall(t *testing.T) {
	before := counterVecValue(t, metrics.DBTimeoutTotal, "57014", "settlement")

	_ = service.IsTransientSettlementError(statementTimeoutError())
	_ = service.SettlementErrorSQLState(statementTimeoutError())

	after := counterVecValue(t, metrics.DBTimeoutTotal, "57014", "settlement")
	assert.Equal(t, before, after, "순수 분류 함수 호출은 카운터를 늘리면 안 된다")
}

// 대조군: deadlock(40P01)은 "DB 시간 상한" 카운터 대상이 아니다.
func TestDBTimeoutMetricIgnoresNonTimeoutTransientErrors(t *testing.T) {
	withFastTransientRetries(t)
	before := counterVecValue(t, metrics.DBTimeoutTotal, "40P01", "settlement")

	settler := &fakeTradeSettler{err: deadlockError()}
	processTradeSettlement(testTrade(), 0, settler, &fakeFailureRecorder{}, func(string, []byte) {}, discardLogger())

	after := counterVecValue(t, metrics.DBTimeoutTotal, "40P01", "settlement")
	assert.Equal(t, before, after, "deadlock(40P01)은 db_timeout 카운터 대상이 아니다")
}
