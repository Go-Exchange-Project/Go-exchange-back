package metrics

import (
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
)

// NewMatchingEngineObservers는 엔진 계측 콜백을 프로메테우스 지표에 연결한다.
// internal/matching이 prometheus를 import하지 않도록 어댑터를 여기에 둔다
// (기존 MatchLatencyObserver와 같은 방식).
func NewMatchingEngineObservers() matching.EngineObservers {
	return matching.EngineObservers{
		Turn: func(d time.Duration) {
			MatchingEngineTurnDuration.Observe(d.Seconds())
		},
		Slice: func(trades int, emitBlock time.Duration) {
			MatchingEngineMatchesPerSlice.Observe(float64(trades))
			MatchingEngineEmitBlockPerSlice.Observe(emitBlock.Seconds())
		},
		OrderAdmitted: func(queueWait time.Duration) {
			MatchingEngineOrderQueueWait.Observe(queueWait.Seconds())
		},
		OrderDone: func(trades int) {
			MatchingEngineExecutionsPerOrder.Observe(float64(trades))
		},
		Cancel: func(queueWait time.Duration) {
			MatchingEngineCancelQueueWait.Observe(queueWait.Seconds())
		},
		EmitBlock: func(kind matching.EmitKind, d time.Duration) {
			MatchingEngineEmitBlock.WithLabelValues(string(kind)).Observe(d.Seconds())
		},
		Yield: func() {
			MatchingEngineQuantumYields.Inc()
		},
		ParkDuration: func(d time.Duration) {
			MatchingEngineExecutionCapacityParkDuration.Observe(d.Seconds())
		},
		CancelBackpressured: func() {
			MatchingEngineCancelBackpressured.Inc()
		},
		// ParkStarted·ShutdownLatched는 메트릭 없음 — 테스트 장벽·관측용이다
		// (설계 §8.1). 여기서는 배선하지 않는다.
	}
}
