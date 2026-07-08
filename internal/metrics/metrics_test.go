package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
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
