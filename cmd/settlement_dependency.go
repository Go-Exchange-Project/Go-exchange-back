package main

import (
	"fmt"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
)

// dependencyTracker는 dispatcher가 단독 소유하는 주문 단위 fence 상태다.
// worker는 이 상태를 읽지도 변경하지도 않는다 — completion은 job ID와 결과만 돌려준다.
// select loop와 분리해 상태 전이를 단위 테스트로 고정하기 위해 별도 타입으로 둔다.
type dependencyTracker struct {
	inFlight     map[uint]int      // orderID → 아직 retire되지 않은 배치 수
	dispatched   map[uint64][]uint // jobID → 그 배치가 건드린 주문(중복 제거됨)
	unsafeOrders map[uint]struct{} // 내구 기록조차 실패해 terminal 실행이 금지된 주문
	jobs         int
}

func newDependencyTracker() *dependencyTracker {
	return &dependencyTracker{
		inFlight:     map[uint]int{},
		dispatched:   map[uint64][]uint{},
		unsafeOrders: map[uint]struct{}{},
	}
}

// touchedOrderIDs는 배치가 건드리는 주문을 중복 없이 돌려준다. 배치당 1회만
// 카운트해야 retire 시 대칭이 맞는다(같은 주문이 배치 안에 여러 번 나와도 1).
func (d *dependencyTracker) touchedOrderIDs(batch []service.OutboxEvent) []uint {
	seen := make(map[uint]struct{}, len(batch)*2)
	orders := make([]uint, 0, len(batch)*2)
	for _, event := range batch {
		if event.Event.Trade == nil {
			continue
		}
		for _, id := range [2]uint{event.Event.Trade.BuyOrderID, event.Event.Trade.SellOrderID} {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			orders = append(orders, id)
		}
	}
	return orders
}

// register는 job 송신이 성공한 직후에만 호출한다 — select의 send case body에서
// 호출하므로 completion 처리가 등록을 앞지를 수 없다.
func (d *dependencyTracker) register(jobID uint64, orders []uint) {
	d.dispatched[jobID] = orders
	d.jobs++
	for _, id := range orders {
		d.inFlight[id]++
	}
}

// retire는 성공·실패 무관하게 자원을 반납한다. retire는 dependency 충족을
// 의미하지 않는다 — undurable로 보고된 주문은 quarantine돼 terminal이 금지된다.
func (d *dependencyTracker) retire(jobID uint64, undurable []uint) error {
	orders, ok := d.dispatched[jobID]
	if !ok {
		return fmt.Errorf("settlement dispatcher: completion for unknown job %d", jobID)
	}
	// quarantine 표시가 count 감소보다 먼저다 — 순서가 바뀌면 terminal이
	// quarantine을 보지 못한 채 ready로 판정될 수 있다.
	for _, id := range undurable {
		d.unsafeOrders[id] = struct{}{}
	}
	delete(d.dispatched, jobID)
	d.jobs--
	for _, id := range orders {
		d.inFlight[id]--
		if d.inFlight[id] <= 0 {
			delete(d.inFlight, id) // 누수 방지
		}
	}
	return nil
}

func (d *dependencyTracker) ready(orderID uint) bool { return d.inFlight[orderID] == 0 }

func (d *dependencyTracker) quarantined(orderID uint) bool {
	_, ok := d.unsafeOrders[orderID]
	return ok
}

func (d *dependencyTracker) clearQuarantine(orderID uint) { delete(d.unsafeOrders, orderID) }

func (d *dependencyTracker) outstanding() int { return d.jobs }

func (d *dependencyTracker) quarantinedCount() int { return len(d.unsafeOrders) }
