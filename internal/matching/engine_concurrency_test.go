package matching

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
)

func waitWithTimeout(t *testing.T, wg *sync.WaitGroup, timeout time.Duration) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal("timed out waiting for goroutines to finish")
	}
}

func TestConcurrentOrderSubmission_NoRaceAndConsistentState(t *testing.T) {
	me := NewMatchingEngine()
	me.Start()

	const numGoroutines = 50
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(i int) {
			defer wg.Done()
			order := testOrder(uint(i+1), "BTC", model.OrderSideBuy, int64(50000+i), 1)
			submitAndWaitSnapshot(t, me, order)
		}(i)
	}

	waitWithTimeout(t, &wg, 5*time.Second)

	totalQty := decimal.Zero
	orderCount := 0
	me.GetOrderBook("BTC").BuyOrders.Ascend(func(level *PriceLevel) bool {
		for j := 0; j < level.Orders.Len(); j++ {
			totalQty = totalQty.Add(level.Orders.At(j).Amount)
			orderCount++
		}
		return true
	})

	assert.Equal(t, numGoroutines, orderCount)
	assert.True(t, totalQty.Equal(decimal.NewFromInt(numGoroutines)))
}

func TestConcurrentMultiSymbolAccess_NoRace(t *testing.T) {
	me := NewMatchingEngine()
	me.Start()

	symbols := []string{"BTC", "ETH", "AVAX", "SOL", "DOGE"}
	const ordersPerSymbol = 20
	var wg sync.WaitGroup
	wg.Add(len(symbols) * ordersPerSymbol)

	var nextOrderID uint32
	for _, symbol := range symbols {
		for i := 0; i < ordersPerSymbol; i++ {
			go func(symbol string, i int) {
				defer wg.Done()
				id := atomic.AddUint32(&nextOrderID, 1)
				order := testOrder(uint(id), symbol, model.OrderSideBuy, int64(1000+i), 1)
				submitAndWaitSnapshot(t, me, order)
			}(symbol, i)
		}
	}

	waitWithTimeout(t, &wg, 5*time.Second)

	for _, symbol := range symbols {
		book := me.GetOrderBook(symbol)
		count := 0
		book.BuyOrders.Ascend(func(level *PriceLevel) bool {
			for j := 0; j < level.Orders.Len(); j++ {
				if level.Orders.At(j).CoinSymbol != symbol {
					t.Fatalf("order from symbol %s found in %s book", level.Orders.At(j).CoinSymbol, symbol)
				}
				count++
			}
			return true
		})
		assert.Equal(t, ordersPerSymbol, count, "symbol %s order count mismatch", symbol)
	}
}
