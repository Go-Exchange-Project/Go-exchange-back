package matching

import (
	"fmt"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
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
	me := newTestEngine()
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

// buildSellWall seeds the book with `levels` resting sell orders at
// ascending prices (50000, 50001, ...), each with order ID i+1.
func buildSellWall(me *MatchingEngine, levels int) {
	for i := 0; i < levels; i++ {
		me.Match(testOrder(uint(i+1), "BTC", model.OrderSideSell, int64(50000+i), 1))
	}
}

func BenchmarkOrderBookDepth(b *testing.B) {
	depths := []int{100, 1000, 10000}
	for _, depth := range depths {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			me := newTestEngine()
			buildSellWall(me, depth)

			done := make(chan struct{})
			go drainEngineEvents(me, done)
			defer close(done)

			b.ReportAllocs()
			// The timed loop below only ever matches against the single
			// best-price (lowest) resting sell order at 50000, immediately
			// replenishing it each iteration. The rest of the sell wall
			// built above is never touched during timing, so ns/op is
			// expected to stay flat across depth=100/1000/10000 — this
			// benchmark measures top-of-book match cost, not depth-dependent
			// traversal cost.
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
		me := newTestEngine()
		buildSellWall(me, wallDepth)

		localDone := make(chan struct{})
		go drainEngineEvents(me, localDone)
		b.StartTimer()

		me.Match(testOrder(uint(wallDepth+1), "BTC", model.OrderSideBuy, int64(50000+wallDepth-1), int64(wallDepth)))

		b.StopTimer()
		close(localDone)
	}
}

func reportTPS(b *testing.B) {
	b.Helper()
	elapsed := b.Elapsed()
	if elapsed <= 0 || b.N == 0 {
		return
	}
	tps := float64(b.N) / elapsed.Seconds()
	b.ReportMetric(tps, "tps")
}

func BenchmarkTPS_LimitOrder(b *testing.B) {
	me := newTestEngine()
	me.Match(testOrder(1, "BTC", model.OrderSideSell, 50000, int64(b.N)+1))

	done := make(chan struct{})
	go drainEngineEvents(me, done)
	defer close(done)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		me.Match(testOrder(uint(i+2), "BTC", model.OrderSideBuy, 50000, 1))
	}
	b.StopTimer()
	reportTPS(b)
}

func BenchmarkTPS_MarketOrder(b *testing.B) {
	me := newTestEngine()

	done := make(chan struct{})
	go drainEngineEvents(me, done)
	defer close(done)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		orderID := uint(200000 + i*2)
		me.Match(testOrder(orderID, "BTC", model.OrderSideSell, 50000, 1))
		me.Match(&Order{
			ID:          orderID + 1,
			CoinSymbol:  "BTC",
			Side:        model.OrderSideBuy,
			OrderType:   model.OrderTypeMarket,
			QuoteAmount: decimal.NewFromInt(50000),
			CreatedAt:   time.Now(),
		})
	}
	b.StopTimer()
	reportTPS(b)
}

func BenchmarkTPS_MixedOrder(b *testing.B) {
	me := newTestEngine()

	done := make(chan struct{})
	go drainEngineEvents(me, done)
	defer close(done)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		orderID := uint(400000 + i*3)
		me.Match(testOrder(orderID, "BTC", model.OrderSideSell, 50000, 1))
		if i%2 == 0 {
			me.Match(testOrder(orderID+1, "BTC", model.OrderSideBuy, 50000, 1))
		} else {
			me.Match(&Order{
				ID:          orderID + 1,
				CoinSymbol:  "BTC",
				Side:        model.OrderSideBuy,
				OrderType:   model.OrderTypeMarket,
				QuoteAmount: decimal.NewFromInt(50000),
				CreatedAt:   time.Now(),
			})
		}
	}
	b.StopTimer()
	reportTPS(b)
}
