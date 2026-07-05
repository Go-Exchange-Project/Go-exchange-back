package matching

import (
	"fmt"
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

func BenchmarkOrderBookDepth(b *testing.B) {
	depths := []int{100, 1000, 10000}
	for _, depth := range depths {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			me := NewMatchingEngine()
			for i := 0; i < depth; i++ {
				me.Match(testOrder(uint(i+1), "BTC", model.OrderSideSell, int64(50000+i), 1))
			}

			done := make(chan struct{})
			go drainEngineEvents(me, done)
			defer close(done)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				orderID := uint(100000 + i*2)
				me.Match(testOrder(orderID, "BTC", model.OrderSideBuy, 50000, 1))
				me.Match(testOrder(orderID+1, "BTC", model.OrderSideSell, 50000, 1))
			}
		})
	}
}

func BenchmarkBulkFill(b *testing.B) {
	const wallDepth = 100

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		me := NewMatchingEngine()
		for lvl := 0; lvl < wallDepth; lvl++ {
			me.Match(testOrder(uint(lvl+1), "BTC", model.OrderSideSell, int64(50000+lvl), 1))
		}

		localDone := make(chan struct{})
		go drainEngineEvents(me, localDone)
		b.StartTimer()

		me.Match(testOrder(uint(wallDepth+1), "BTC", model.OrderSideBuy, int64(50000+wallDepth-1), int64(wallDepth)))

		b.StopTimer()
		close(localDone)
	}
}
