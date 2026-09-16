package matching

import "time"

// EmitKind는 ExecutionCh로 나가는 이벤트 종류다. emit 블로킹 시간을
// 종류별로 구분해 보기 위해 존재한다.
type EmitKind string

const (
	EmitTrade      EmitKind = "trade"
	EmitMarketDone EmitKind = "done"
	EmitCancelled  EmitKind = "cancelled"
)

// EngineObservers는 엔진 내부 계측을 밖으로 노출하는 콜백 묶음이다.
// internal/matching이 prometheus를 import하지 않도록 기존
// MatchLatencyObserver와 같은 방식을 따른다. 모든 필드는 nil 허용이다.
//
// Start() 전에 설정하고 실행 중 교체하지 않는다 — 재대입은 data race다.
type EngineObservers struct {
	// Turn은 turn 하나의 작업 구간 소요 시간이다(블로킹 대기 제외).
	Turn func(d time.Duration)
	// Slice는 조각 하나가 만든 체결 수와 그 조각의 emit 블로킹 누적 시간이다.
	Slice func(trades int, emitBlock time.Duration)
	// OrderAdmitted는 주문이 엔진 큐에서 꺼내진 시점의 대기 시간이다.
	OrderAdmitted func(queueWait time.Duration)
	// OrderDone은 주문이 완결될 때 그 주문이 만든 총 체결 수다.
	OrderDone func(trades int)
	// Cancel은 취소가 처리된 시점의 큐 대기 시간이다.
	Cancel func(queueWait time.Duration)
	// EmitBlock은 ExecutionCh send 1회가 블로킹된 시간이다.
	EmitBlock func(kind EmitKind, d time.Duration)
	// Yield는 조각이 예산 소진으로 반환될 때 1회 호출된다.
	Yield func()
	// ParkStarted는 runTurn 6단계의 첫 park 진입에서 1회 호출된다(중간
	// 깨어남에서는 다시 호출되지 않는다). 메트릭 없음 — 테스트 장벽용이다.
	ParkStarted func()
	// ParkDuration은 첫 park 진입부터 slice가 실제로 재개될 때까지의
	// 시간이다. finishPark가 park 중이었을 때만 1회 호출한다.
	ParkDuration func(d time.Duration)
	// CancelBackpressured는 cancel phase가 §4.1에 따라 취소를 거절할 때마다
	// 호출된다.
	CancelBackpressured func()
	// ShutdownLatched는 stop을 받는 세 경로(4단계 논블로킹 확인, 일반
	// blocking select, park select) 중 처음 한 번만 호출된다. 메트릭
	// 없음 — 테스트 관측용이다.
	ShutdownLatched func()
}

func (o EngineObservers) turn(d time.Duration) {
	if o.Turn != nil {
		o.Turn(d)
	}
}

func (o EngineObservers) slice(trades int, emitBlock time.Duration) {
	if o.Slice != nil {
		o.Slice(trades, emitBlock)
	}
}

func (o EngineObservers) orderAdmitted(queueWait time.Duration) {
	if o.OrderAdmitted != nil {
		o.OrderAdmitted(queueWait)
	}
}

func (o EngineObservers) orderDone(trades int) {
	if o.OrderDone != nil {
		o.OrderDone(trades)
	}
}

func (o EngineObservers) cancel(queueWait time.Duration) {
	if o.Cancel != nil {
		o.Cancel(queueWait)
	}
}

func (o EngineObservers) emitBlock(kind EmitKind, d time.Duration) {
	if o.EmitBlock != nil {
		o.EmitBlock(kind, d)
	}
}

func (o EngineObservers) yield() {
	if o.Yield != nil {
		o.Yield()
	}
}

func (o EngineObservers) parkStarted() {
	if o.ParkStarted != nil {
		o.ParkStarted()
	}
}

func (o EngineObservers) parkDuration(d time.Duration) {
	if o.ParkDuration != nil {
		o.ParkDuration(d)
	}
}

func (o EngineObservers) cancelBackpressured() {
	if o.CancelBackpressured != nil {
		o.CancelBackpressured()
	}
}

func (o EngineObservers) shutdownLatched() {
	if o.ShutdownLatched != nil {
		o.ShutdownLatched()
	}
}
