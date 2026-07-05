package matching

import (
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
)

func drainEngineEvents(me *MatchingEngine, done <-chan struct{}) {
	for {
		select {
		case <-me.TradeCh:
		case <-me.ExecutionCh:
		case <-done:
			return
		}
	}
}

func BenchmarkMatch_ImmediateCross(b *testing.B) {
	me := NewMatchingEngine()
	me.Match(testOrder(1, "BTC", model.OrderSideSell, 50000, int64(b.N)+1))

	done := make(chan struct{})
	go drainEngineEvents(me, done)
	defer close(done)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		me.Match(testOrder(uint(i+2), "BTC", model.OrderSideBuy, 50000, 1))
	}
}
