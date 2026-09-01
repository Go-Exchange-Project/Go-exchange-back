package metrics

import (
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

// histSample은 histogram의 표본 수와 합계를 읽는다.
// CollectAndCount는 metric family의 존재만 확인하므로, 콜백이 값을 하나도
// 기록하지 않아도 1을 돌려준다 — 판별력이 없다.
func histSample(t *testing.T, c prometheus.Collector) (uint64, float64) {
	t.Helper()
	ch := make(chan prometheus.Metric, 8)
	c.Collect(ch)
	close(ch)
	var count uint64
	var sum float64
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		require.NotNil(t, pb.Histogram)
		count += pb.Histogram.GetSampleCount()
		sum += pb.Histogram.GetSampleSum()
	}
	return count, sum
}

// histVecSample은 HistogramVec에서 라벨 하나에 해당하는 표본만 읽는다.
// WithLabelValues는 Observer를 돌려줄 뿐 Collector가 아니므로,
// Vec 전체를 수집한 뒤 라벨로 걸러야 한다.
func histVecSample(t *testing.T, vec *prometheus.HistogramVec, label, value string) (uint64, float64) {
	t.Helper()
	ch := make(chan prometheus.Metric, 16)
	vec.Collect(ch)
	close(ch)
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		for _, pair := range pb.GetLabel() {
			if pair.GetName() == label && pair.GetValue() == value {
				require.NotNil(t, pb.Histogram)
				return pb.Histogram.GetSampleCount(), pb.Histogram.GetSampleSum()
			}
		}
	}
	return 0, 0
}

// 전역 promauto 컬렉터를 공유하므로 histogram 6종은 이 테스트가 독점한다.
// EmitBlock은 라벨로 분리해 다른 테스트와 겹치지 않게 한다.
func TestNewMatchingEngineObserversFeedsAllMetrics(t *testing.T) {
	obs := NewMatchingEngineObservers()

	obs.Turn(3 * time.Millisecond)
	obs.Slice(5, 2*time.Millisecond)
	obs.OrderAdmitted(7 * time.Millisecond)
	obs.OrderDone(11)
	obs.Cancel(13 * time.Millisecond)
	obs.EmitBlock(matching.EmitTrade, time.Millisecond)
	obs.Yield()

	cases := []struct {
		name      string
		metric    string
		collector prometheus.Collector
		wantSum   float64
	}{
		{"turn", "matching_engine_turn_duration_seconds", MatchingEngineTurnDuration, 0.003},
		{"slice", "matching_engine_matches_per_slice", MatchingEngineMatchesPerSlice, 5},
		{"emit_per_slice", "matching_engine_emit_block_per_slice_seconds", MatchingEngineEmitBlockPerSlice, 0.002},
		{"order_wait", "matching_engine_order_queue_wait_seconds", MatchingEngineOrderQueueWait, 0.007},
		{"per_order", "matching_engine_executions_per_order", MatchingEngineExecutionsPerOrder, 11},
		{"cancel_wait", "matching_engine_cancel_queue_wait_seconds", MatchingEngineCancelQueueWait, 0.013},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			count, sum := histSample(t, tc.collector)
			require.Equal(t, uint64(1), count, "표본이 기록되지 않았다")
			require.InDelta(t, tc.wantSum, sum, 1e-9, "기록된 값이 다르다")
			require.Equal(t, 1, testutil.CollectAndCount(tc.collector, tc.metric), "지표 이름")
		})
	}

	require.Equal(t, float64(1), testutil.ToFloat64(MatchingEngineQuantumYields))

	tradeCount, tradeSum := histVecSample(t, MatchingEngineEmitBlock, "event", string(matching.EmitTrade))
	require.Equal(t, uint64(1), tradeCount)
	require.InDelta(t, 0.001, tradeSum, 1e-9)
}

func TestEmitBlockIsLabeledByKind(t *testing.T) {
	obs := NewMatchingEngineObservers()
	obs.EmitBlock(matching.EmitMarketDone, 2*time.Millisecond)
	obs.EmitBlock(matching.EmitCancelled, 4*time.Millisecond)

	doneCount, doneSum := histVecSample(t, MatchingEngineEmitBlock, "event", string(matching.EmitMarketDone))
	require.Equal(t, uint64(1), doneCount)
	require.InDelta(t, 0.002, doneSum, 1e-9)

	cancelCount, cancelSum := histVecSample(t, MatchingEngineEmitBlock, "event", string(matching.EmitCancelled))
	require.Equal(t, uint64(1), cancelCount)
	require.InDelta(t, 0.004, cancelSum, 1e-9)
}
