package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestMatchLatencyBucketsExtendDefaultUpTo60Seconds(t *testing.T) {
	want := append(append([]float64{}, prometheus.DefBuckets...), 15, 20, 30, 45, 60)

	if len(matchLatencyBuckets) != len(want) {
		t.Fatalf("len(matchLatencyBuckets) = %d, want %d", len(matchLatencyBuckets), len(want))
	}
	for i, v := range want {
		if matchLatencyBuckets[i] != v {
			t.Fatalf("matchLatencyBuckets[%d] = %v, want %v", i, matchLatencyBuckets[i], v)
		}
	}
}

func TestRegisterMatchingEngineShardOrderChannelLenGaugesExposesPerShardValue(t *testing.T) {
	RegisterMatchingEngineShardOrderChannelLenGauges([]func() int{
		func() int { return 3 },
		func() int { return 7 },
	})

	expected := `
# HELP matching_engine_shard_order_channel_length Current number of buffered items in a single shard's order channel (B-3).
# TYPE matching_engine_shard_order_channel_length gauge
matching_engine_shard_order_channel_length{shard="0"} 3
matching_engine_shard_order_channel_length{shard="1"} 7
`
	err := testutil.GatherAndCompare(prometheus.DefaultGatherer, strings.NewReader(expected), "matching_engine_shard_order_channel_length")
	if err != nil {
		t.Fatal(err)
	}
}

func TestRegisterHoldCoordinatorInputGaugeExposesInputLength(t *testing.T) {
	RegisterHoldCoordinatorInputGauge(func() int { return 9 })

	expected := `
# HELP hold_coordinator_input_length Current number of buffered requests in the hold coordinator's input channel.
# TYPE hold_coordinator_input_length gauge
hold_coordinator_input_length 9
`
	err := testutil.GatherAndCompare(prometheus.DefaultGatherer, strings.NewReader(expected), "hold_coordinator_input_length")
	if err != nil {
		t.Fatal(err)
	}
}

// 두 지표는 취소 계약의 관측 기반이다. 이름이 바뀌면 대시보드와 35번 기준선이
// 조용히 비어 버리므로 이름 자체를 고정한다.
func TestCancelCommandMetricsAreRegisteredUnderStableNames(t *testing.T) {
	CancelCommandLatency.Observe(0.25)
	CancelCommandAwaitingOutboxDeadlineTotal.Inc()

	count, err := testutil.GatherAndCount(prometheus.DefaultGatherer,
		"cancel_command_latency_seconds",
		"cancel_command_awaiting_outbox_deadline_total",
	)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("두 지표가 모두 등록돼야 한다: got %d", count)
	}
}
