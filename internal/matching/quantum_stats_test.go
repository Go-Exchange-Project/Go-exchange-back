package matching

import (
	"math"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// latencySamples는 관측 표본과 censored(관측 창 안에 끝나지 않은) 개수를
// 함께 들고 있다. censored를 버리면 baseline이 실제보다 좋아 보이고
// 전후 비교가 무의미해진다.
type latencySamples struct {
	observed []time.Duration
	censored int
}

func (s latencySamples) p99Infinite() bool { return s.censored > 0 }

// percentile은 nearest-rank 정의를 쓴다: 정렬된 표본에서 ceil(q*n)번째 값.
// 0-based 인덱스로는 ceil(q*n)-1이다.
//
// int(q*n)을 쓰면 n=100에서 p50이 51번째, p99가 100번째가 되어 한 칸씩
// 밀린다. 꼬리를 보는 지표에서 이 한 칸은 판정을 뒤집을 수 있다.
func (s latencySamples) percentile(q float64) time.Duration {
	if len(s.observed) == 0 {
		return 0
	}
	sorted := append([]time.Duration{}, s.observed...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := int(math.Ceil(q * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

func medianDuration(runs []time.Duration) time.Duration {
	sorted := append([]time.Duration{}, runs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

func TestPercentileNearestRank(t *testing.T) {
	values := make([]time.Duration, 100)
	for i := range values {
		values[i] = time.Duration(i+1) * time.Millisecond
	}
	s := latencySamples{observed: values}
	require.False(t, s.p99Infinite())
	require.Equal(t, 99*time.Millisecond, s.percentile(0.99), "ceil(99)=99번째")
	require.Equal(t, 50*time.Millisecond, s.percentile(0.50), "ceil(50)=50번째")
	require.Equal(t, 100*time.Millisecond, s.percentile(1.0))
	require.Equal(t, time.Millisecond, s.percentile(0.0), "rank 0은 1로 클램프")

	odd := latencySamples{observed: []time.Duration{1, 2, 3}}
	require.Equal(t, time.Duration(1), odd.percentile(0.01), "ceil(0.03)=1번째")
	require.Equal(t, time.Duration(2), odd.percentile(0.50), "ceil(1.5)=2번째")
	require.Equal(t, time.Duration(3), odd.percentile(0.99), "ceil(2.97)=3번째")

	require.Equal(t, time.Duration(0), latencySamples{}.percentile(0.99))
	require.True(t, latencySamples{observed: []time.Duration{1}, censored: 1}.p99Infinite())
}

func TestMedianOfRuns(t *testing.T) {
	require.Equal(t, time.Duration(3), medianDuration([]time.Duration{5, 1, 3, 2, 4}))
	require.Equal(t, time.Duration(2), medianDuration([]time.Duration{3, 1, 2}))
}
