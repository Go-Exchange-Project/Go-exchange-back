package main

import (
	"context"
	"fmt"
	"time"
)

// runCancelCommandStartupBarrier는 복구된 취소 command가 처리되기 전에 트래픽을
// 받지 않도록 부팅 순서를 고정한다.
//
// "bootstrap 후 worker 시작"만으로는 부족하다. bootstrap이 주문을 오더북에 다시
// 올린 직후 새 주문을 받으면 복구된 취소가 처리되기 전에 체결될 수 있다.
// drain이 끝나야만 반환하고, 실패하면 호출자가 HTTP를 열지 않는다 — 취소를 못
// 지키면서 주문을 받는 것보다 안 받는 것이 낫다.
func runCancelCommandStartupBarrier(
	ctx context.Context,
	bootstrap func(context.Context) error,
	startWorker func(),
	drain func(context.Context) error,
) error {
	if err := bootstrap(ctx); err != nil {
		return fmt.Errorf("matching bootstrap: %w", err)
	}
	startWorker()
	if err := drain(ctx); err != nil {
		return fmt.Errorf("recovered cancel command drain: %w", err)
	}
	return nil
}

// stopCancelCommandWorker는 worker context를 취소하고 done을 기다린다.
// 상한을 넘기면 error를 반환하지만, 그것은 "worker가 끝나지 않았다"는 사실이지
// "끝났다고 쳐도 된다"가 아니다 — 호출자는 stopCancelWorkerThenEngine처럼
// done을 계속 기다린 뒤에만 엔진을 정지해야 한다.
func stopCancelCommandWorker(cancel context.CancelFunc, done <-chan struct{}, timeout time.Duration) error {
	cancel()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-done:
		return nil
	case <-timer.C:
		return fmt.Errorf("cancel command worker did not stop within %s", timeout)
	}
}

// stopCancelWorkerThenEngine은 종료 순서의 핵심 계약이다: cancel worker가 실제로
// 끝난 뒤에만 엔진을 정지한다.
//
// 상한 초과는 경고로만 알리고 대기를 이어간다. 여기서 엔진 정지로 넘어가면
// worker가 아직 CancelOrder를 호출하고 있는 채로 엔진이 멈춰, 진행 중인 취소가
// 오더북에 반영되지 못한다. worker의 Run은 시작한 호출이 반환하면 반드시 끝나므로
// 이 대기는 무한이 아니다.
func stopCancelWorkerThenEngine(
	cancel context.CancelFunc,
	done <-chan struct{},
	timeout time.Duration,
	stopEngine func(),
	logf func(format string, args ...any),
) {
	for {
		err := stopCancelCommandWorker(cancel, done, timeout)
		if err == nil {
			break
		}
		logf("%v; still waiting before stopping the matching engine", err)
	}
	stopEngine()
}
