package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bootstrap이 주문을 오더북에 다시 올린 직후 새 주문을 받으면, 복구된 취소가
// 처리되기 전에 체결될 수 있다. 그래서 drain이 끝나야만 HTTP를 연다.
func TestCancelCommandLifecycleStartupBarrierOrder(t *testing.T) {
	var mu sync.Mutex
	var order []string
	record := func(step string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, step)
	}

	err := runCancelCommandStartupBarrier(
		context.Background(),
		func(context.Context) error { record("bootstrap"); return nil },
		func() { record("start_worker") },
		func(context.Context) error { record("drain"); return nil },
	)

	require.NoError(t, err)
	assert.Equal(t, []string{"bootstrap", "start_worker", "drain"}, order)
}

// drain이 끝나지 않으면 barrier도 반환하지 않는다 — 반환하면 곧 HTTP가 열린다.
func TestCancelCommandLifecycleStartupBarrierWaitsForDrain(t *testing.T) {
	release := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- runCancelCommandStartupBarrier(
			context.Background(),
			func(context.Context) error { return nil },
			func() {},
			func(context.Context) error { <-release; return nil },
		)
	}()

	select {
	case <-done:
		t.Fatal("drain이 끝나기 전에 barrier가 반환했다")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("drain이 끝났는데 barrier가 반환하지 않았다")
	}
}

// drain이 실패하면 서버를 열지 않는다. 취소를 못 지키면서 주문을 받는 것보다
// 안 받는 것이 낫다.
func TestCancelCommandLifecycleStartupBarrierPropagatesFailures(t *testing.T) {
	bootstrapErr := errors.New("bootstrap boom")
	err := runCancelCommandStartupBarrier(
		context.Background(),
		func(context.Context) error { return bootstrapErr },
		func() { t.Fatal("bootstrap이 실패했는데 worker가 시작됐다") },
		func(context.Context) error { t.Fatal("bootstrap이 실패했는데 drain이 실행됐다"); return nil },
	)
	require.ErrorIs(t, err, bootstrapErr)

	drainErr := errors.New("drain boom")
	err = runCancelCommandStartupBarrier(
		context.Background(),
		func(context.Context) error { return nil },
		func() {},
		func(context.Context) error { return drainErr },
	)
	require.ErrorIs(t, err, drainErr)
}

// 종료 1단계: 상한이 남아 있고 worker가 아직 안 끝났으면 helper는 기다린다.
func TestStopCancelCommandWorkerWaitsBeforeTimeout(t *testing.T) {
	done := make(chan struct{})
	result := make(chan error, 1)

	go func() {
		result <- stopCancelCommandWorker(func() {}, done, 2*time.Second)
	}()

	select {
	case <-result:
		t.Fatal("상한이 남았는데 helper가 먼저 반환했다")
	case <-time.After(150 * time.Millisecond):
	}

	close(done)
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("worker가 끝났는데 helper가 반환하지 않았다")
	}
}

// 종료 2단계: 상한을 넘기면 error다.
func TestStopCancelCommandWorkerReturnsErrorOnTimeout(t *testing.T) {
	var cancelled bool
	done := make(chan struct{})

	err := stopCancelCommandWorker(func() { cancelled = true }, done, 50*time.Millisecond)

	require.Error(t, err)
	assert.True(t, cancelled, "helper가 worker context를 취소하지 않았다")
}

// 종료 3단계: timeout error가 나도 엔진을 정지하지 않는다. worker가 아직 엔진을
// 호출하고 있을 수 있으므로, done이 닫힌 뒤에만 정지한다.
func TestShutdownCancelWorkerThenEngineNeverStopsEngineEarly(t *testing.T) {
	done := make(chan struct{})
	var mu sync.Mutex
	var engineStopped bool
	var warnings int

	finished := make(chan struct{})
	go func() {
		stopCancelWorkerThenEngine(
			func() {},
			done,
			50*time.Millisecond,
			func() {
				mu.Lock()
				defer mu.Unlock()
				engineStopped = true
			},
			func(string, ...any) {
				mu.Lock()
				defer mu.Unlock()
				warnings++
			},
		)
		close(finished)
	}()

	// 상한을 여러 번 넘겨도 엔진은 멈추지 않아야 한다.
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	stoppedEarly := engineStopped
	warned := warnings
	mu.Unlock()

	assert.False(t, stoppedEarly, "worker가 끝나기 전에 엔진이 정지했다")
	assert.Positive(t, warned, "상한 초과를 알리지 않았다")

	select {
	case <-finished:
		t.Fatal("worker가 끝나기 전에 종료 단계가 넘어갔다")
	default:
	}

	close(done)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("worker가 끝났는데 종료가 진행되지 않았다")
	}

	mu.Lock()
	defer mu.Unlock()
	assert.True(t, engineStopped, "worker가 끝났는데 엔진이 정지하지 않았다")
}

// 정상 종료에서는 경고 없이 곧바로 엔진을 정지한다.
func TestShutdownCancelWorkerThenEngineStopsAfterCleanExit(t *testing.T) {
	done := make(chan struct{})
	close(done)

	var engineStopped bool
	var warnings int
	stopCancelWorkerThenEngine(
		func() {},
		done,
		time.Second,
		func() { engineStopped = true },
		func(string, ...any) { warnings++ },
	)

	assert.True(t, engineStopped)
	assert.Zero(t, warnings)
}

// 종료 단계 세 개는 같은 deadline을 공유한다. one-shot 채널(time.After)을 쓰면
// 첫 단계가 유일한 값을 소비해 이후 단계가 영구 대기한다. 이미 만료된
// deadline이면 세 단계가 모두 즉시 빠져나와야 한다.
func TestWaitForShutdownStageAllStagesExitOnExpiredDeadline(t *testing.T) {
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancelDrain()
	<-drainCtx.Done()

	blocked := make(chan struct{}) // 어떤 단계도 끝나지 않았다
	var timeouts int
	logf := func(string, ...any) { timeouts++ }

	finished := make(chan struct{})
	go func() {
		for _, stage := range []string{"matching engine", "outbox writer flush", "settlement workers"} {
			if waitForShutdownStage(stage, blocked, drainCtx.Done(), logf) {
				t.Errorf("%s: 끝나지 않았는데 drained로 보고했다", stage)
			}
		}
		close(finished)
	}()

	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("만료된 deadline인데 종료 단계가 대기에 걸렸다")
	}
	assert.Equal(t, 3, timeouts, "세 단계 모두 timeout을 알려야 한다")
}

// 단계가 제때 끝나면 timeout 로그 없이 drained로 보고한다.
func TestWaitForShutdownStageReportsDrained(t *testing.T) {
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelDrain()

	done := make(chan struct{})
	close(done)

	var timeouts int
	drained := waitForShutdownStage("matching engine", done, drainCtx.Done(), func(string, ...any) { timeouts++ })

	assert.True(t, drained)
	assert.Zero(t, timeouts)
}
