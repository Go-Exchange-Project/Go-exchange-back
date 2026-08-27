package service

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
)

const (
	defaultStalePendingInterval  = 30 * time.Second
	defaultStalePendingThreshold = 5 * time.Minute
)

type stalePendingCounter interface {
	CountStalePending(olderThan time.Time) (int64, error)
}

// OrderIdempotencyMonitor는 stale PENDING 수를 gauge로 노출한다.
//
// 정산 worker에 얹지 않고 전용 컴포넌트로 둔다 — 책임이 섞이면 한쪽 장애가 다른 쪽
// 관측을 멈춘다.
type OrderIdempotencyMonitor struct {
	counter   stalePendingCounter
	Interval  time.Duration
	Threshold time.Duration
	Logger    *log.Logger

	lastValue atomic.Int64
	errors    atomic.Int64
}

func NewOrderIdempotencyMonitor(counter stalePendingCounter) *OrderIdempotencyMonitor {
	return &OrderIdempotencyMonitor{counter: counter}
}

func (m *OrderIdempotencyMonitor) LastValue() int64 { return m.lastValue.Load() }

func (m *OrderIdempotencyMonitor) ErrorCount() int64 { return m.errors.Load() }

// Run은 시작 직후 한 번 조회한 뒤 주기 ticker로 전환한다. 먼저 기다리면 재기동 직후
// 창에서 stale PENDING이 보이지 않는데, hold 커밋 직후 죽어서 생긴 레코드가 정확히
// 그 창에 있다.
func (m *OrderIdempotencyMonitor) Run(ctx context.Context) {
	m.observe()

	ticker := time.NewTicker(m.interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.observe()
		}
	}
}

func (m *OrderIdempotencyMonitor) observe() {
	count, err := m.counter.CountStalePending(time.Now().Add(-m.threshold()))
	if err != nil {
		// gauge를 0으로 덮지 않는다. DB가 불안정할 때 0으로 떨어지면 "문제가
		// 사라졌다"로 읽히지만 실제로는 관측이 사라진 것이다.
		m.errors.Add(1)
		metrics.OrderIdempotencyMonitorErrorsTotal.Inc()
		m.logf("order idempotency monitor: stale pending query failed: %v", err)
		return
	}
	m.lastValue.Store(count)
	metrics.OrderIdempotencyStalePending.Set(float64(count))
}

func (m *OrderIdempotencyMonitor) logf(format string, args ...any) {
	if m.Logger != nil {
		m.Logger.Printf(format, args...)
		return
	}
	log.Printf(format, args...)
}

func (m *OrderIdempotencyMonitor) interval() time.Duration {
	if m.Interval > 0 {
		return m.Interval
	}
	return defaultStalePendingInterval
}

func (m *OrderIdempotencyMonitor) threshold() time.Duration {
	if m.Threshold > 0 {
		return m.Threshold
	}
	return defaultStalePendingThreshold
}
