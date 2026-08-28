package service

import (
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failFrom은 "그 호출부터 계속 실패한다"는 뜻이다. 한 번만 실패시키면 다음 조회가
// 값을 되돌려 놓아서, gauge를 0으로 덮는 회귀를 테스트가 놓친다(변이로 확인).
type fakeStaleCounter struct {
	mu       sync.Mutex
	counts   []int64
	failFrom int
	calls    int
	lastArgs []time.Time
}

func (f *fakeStaleCounter) CountStalePending(olderThan time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastArgs = append(f.lastArgs, olderThan)
	if f.failFrom > 0 && f.calls >= f.failFrom {
		return 0, errors.New("db down")
	}
	if len(f.counts) == 0 {
		return 0, nil
	}
	value := f.counts[0]
	if len(f.counts) > 1 {
		f.counts = f.counts[1:]
	}
	return value, nil
}

func (f *fakeStaleCounter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeStaleCounter) firstArg(t *testing.T) time.Time {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.lastArgs)
	return f.lastArgs[0]
}

// 30초를 먼저 기다리면 재기동 직후 창에서 stale PENDING이 보이지 않는다. hold 커밋
// 직후 죽어서 생긴 레코드가 정확히 그 창에 있다.
func TestOrderIdempotencyMonitorQueriesImmediately(t *testing.T) {
	counter := &fakeStaleCounter{counts: []int64{7}}
	monitor := NewOrderIdempotencyMonitor(counter)
	monitor.Interval = time.Hour // ticker가 돌기 전에 첫 조회가 있어야 한다

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { monitor.Run(ctx); close(done) }()

	// gauge Set이 observe()의 마지막 동작이므로, gauge가 7이면 조회도 저장도 끝났다.
	// 호출 수만 기다리면 monitor가 값을 쓰기 전에 단언이 실행될 수 있다.
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(metrics.OrderIdempotencyStalePending) == 7
	}, time.Second, 5*time.Millisecond, "조회 결과가 gauge에 반영되지 않았다")
	assert.EqualValues(t, 7, monitor.LastValue())
	assert.Equal(t, 1, testutil.CollectAndCount(metrics.OrderIdempotencyStalePending,
		"order_idempotency_stale_pending"), "지표 이름이 바뀌면 대시보드와 알람이 조용히 끊긴다")

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("context 취소로 정지하지 않았다")
	}
}

// 조회 실패 시 gauge를 0으로 덮으면 "문제가 사라졌다"로 읽힌다. 실제로는 관측이
// 사라진 것이다.
func TestOrderIdempotencyMonitorKeepsLastValueOnError(t *testing.T) {
	// 첫 조회만 성공하고 그 뒤로는 계속 실패한다. 값이 되돌아올 기회를 주지 않아야
	// "실패 시 0으로 덮는" 회귀가 드러난다.
	counter := &fakeStaleCounter{counts: []int64{5}, failFrom: 2}
	monitor := NewOrderIdempotencyMonitor(counter)
	monitor.Interval = 10 * time.Millisecond
	monitor.Logger = log.New(io.Discard, "", 0) // 10ms 주기 실패 로그가 출력을 뒤덮는다
	before := testutil.ToFloat64(metrics.OrderIdempotencyMonitorErrorsTotal)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go monitor.Run(ctx)

	// 실패 counter가 3 오를 때까지 기다린다 — 첫 성공 조회는 이미 지나갔다는 뜻이다.
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(metrics.OrderIdempotencyMonitorErrorsTotal) >= before+3
	}, time.Second, 5*time.Millisecond, "조회 실패가 counter에 반영되지 않았다")
	assert.EqualValues(t, 5, testutil.ToFloat64(metrics.OrderIdempotencyStalePending),
		"조회 실패가 gauge를 0으로 덮었다")
	assert.EqualValues(t, 5, monitor.LastValue())
	assert.Positive(t, monitor.ErrorCount())
	assert.Equal(t, 1, testutil.CollectAndCount(metrics.OrderIdempotencyMonitorErrorsTotal,
		"order_idempotency_monitor_errors_total"), "지표 이름이 바뀌면 대시보드와 알람이 조용히 끊긴다")
}

// 임계 이전 시각으로 조회해야 "오래된 PENDING"만 잡힌다. time.Now()를 그대로 넘기면
// 정상 진행 중인 레코드까지 세어 gauge가 항상 0이 아니게 된다.
func TestOrderIdempotencyMonitorQueriesOlderThanThreshold(t *testing.T) {
	counter := &fakeStaleCounter{counts: []int64{0}}
	monitor := NewOrderIdempotencyMonitor(counter)
	monitor.Interval = time.Hour
	monitor.Threshold = 10 * time.Minute

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go monitor.Run(ctx)

	require.Eventually(t, func() bool { return counter.callCount() >= 1 }, time.Second, 5*time.Millisecond)

	elapsed := time.Since(counter.firstArg(t))
	assert.Greater(t, elapsed, 9*time.Minute, "임계보다 최근 시각으로 조회했다")
	assert.Less(t, elapsed, 11*time.Minute)
}
