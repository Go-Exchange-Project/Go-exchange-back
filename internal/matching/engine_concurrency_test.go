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

// 코얼레싱으로 "주문마다 스냅샷 1개"가 사라졌으므로, 동시 제출 테스트는 Stop()을
// 처리 배리어로 쓴다. Stop은 drainPendingWork로 OrderCh에 남은 주문을 모두 처리한
// 뒤 종료하므로, Done() 이후 오더북은 불변이고 엔진 goroutine과의 레이스가 없다.
func TestConcurrentOrderSubmission_NoRaceAndConsistentState(t *testing.T) {
	me := newTestEngine()
	me.Start()

	const numGoroutines = 50
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(i int) {
			defer wg.Done()
			me.OrderCh <- testOrder(uint(i+1), "BTC", model.OrderSideBuy, int64(50000+i), 1)
		}(i)
	}

	waitWithTimeout(t, &wg, 5*time.Second)
	me.Stop()
	<-me.Done()

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
	me := newTestEngine()
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
				me.OrderCh <- testOrder(uint(id), symbol, model.OrderSideBuy, int64(1000+i), 1)
			}(symbol, i)
		}
	}

	waitWithTimeout(t, &wg, 5*time.Second)
	me.Stop()
	<-me.Done()

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

func TestMultipleEngineInstances_Isolated(t *testing.T) {
	const numEngines = 5
	engines := make([]*MatchingEngine, numEngines)
	for i := range engines {
		engines[i] = newTestEngine()
		engines[i].Start()
	}

	var wg sync.WaitGroup
	wg.Add(numEngines)
	for i, me := range engines {
		go func(i int, me *MatchingEngine) {
			defer wg.Done()
			me.OrderCh <- testOrder(uint(i+1), "BTC", model.OrderSideSell, 50000, 1)
			me.OrderCh <- testOrder(uint(i+100), "BTC", model.OrderSideBuy, 50000, 1)
		}(i, me)
	}
	waitWithTimeout(t, &wg, 5*time.Second)

	for i, me := range engines {
		trade := requireNextTrade(t, me)
		assert.Equal(t, uint(i+1), trade.SellOrderID, "engine %d trade sell order id mismatch", i)
		assert.Equal(t, uint(i+100), trade.BuyOrderID, "engine %d trade buy order id mismatch", i)
		assert.Equal(t, int64(1), trade.EngineSequence, "engine %d should have independent trade sequence starting at 1", i)
		assertNoTrade(t, me)
		me.Stop()
		<-me.Done()
		assert.Equal(t, 0, me.GetOrderBook("BTC").BuyOrders.Len())
		assert.Equal(t, 0, me.GetOrderBook("BTC").SellOrders.Len())
	}
}
