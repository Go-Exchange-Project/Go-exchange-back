package matching

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestShardedEngine은 newTestEngine과 같은 이유로 코얼레싱 티커를 짧게 설정한다.
func newTestShardedEngine(shardCount int) *ShardedEngine {
	se := NewShardedEngine(shardCount)
	for _, shard := range se.shards {
		shard.snapshotInterval = 2 * time.Millisecond
	}
	return se
}

func waitForShardedSnapshot(t *testing.T, se *ShardedEngine, coinSymbol string) OrderBookSnapshot {
	t.Helper()
	for {
		select {
		case snap := <-se.SnapshotCh:
			if snap.CoinSymbol == coinSymbol {
				return snap
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for snapshot of %s", coinSymbol)
			return OrderBookSnapshot{}
		}
	}
}

func TestShardedEngineAssignsSymbolsRoundRobinAndStably(t *testing.T) {
	se := newTestShardedEngine(4)

	first := se.shardFor("BTC")
	second := se.shardFor("ETH")
	third := se.shardFor("XRP")
	fourth := se.shardFor("SOL")
	fifth := se.shardFor("ADA") // 5번째 신규 심볼은 라운드로빈으로 다시 shard 0

	assert.Same(t, first, se.shardFor("BTC"), "같은 심볼은 항상 같은 샤드")
	assert.NotSame(t, first, second)
	assert.NotSame(t, second, third)
	assert.NotSame(t, third, fourth)
	assert.NotSame(t, first, fourth)
	assert.Same(t, first, fifth, "4개 샤드에서 5번째 신규 심볼은 라운드로빈으로 순환")
}

func TestShardedEngineRoutesEmptySymbolToShardZero(t *testing.T) {
	se := newTestShardedEngine(4)
	assert.Same(t, se.shards[0], se.shardFor(""))
}

func TestShardedEnginePreservesPerSymbolEventOrder(t *testing.T) {
	se := newTestShardedEngine(4)
	se.Start()

	se.SubmitOrder(testOrder(1, "BTC", model.OrderSideSell, 100, 10))
	waitForShardedSnapshot(t, se, "BTC")
	for i := 0; i < 5; i++ {
		se.SubmitOrder(testOrder(uint(i+2), "BTC", model.OrderSideBuy, 100, 1))
	}

	var sequences []int64
	for i := 0; i < 5; i++ {
		select {
		case ev := <-se.ExecutionCh:
			require.NotNil(t, ev.Trade)
			sequences = append(sequences, ev.Trade.EngineSequence)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for trade")
		}
	}
	assert.Equal(t, []int64{1, 2, 3, 4, 5}, sequences, "제출 순서대로 이벤트가 머지 채널에 도착해야 한다")
}

func TestShardedEngineMatchesAcrossShardsIndependently(t *testing.T) {
	se := newTestShardedEngine(4)
	se.Start()

	symbols := []string{"BTC", "ETH", "XRP", "SOL"}
	for i, symbol := range symbols {
		se.SubmitOrder(testOrder(uint(i*2+1), symbol, model.OrderSideSell, 100, 1))
		se.SubmitOrder(testOrder(uint(i*2+2), symbol, model.OrderSideBuy, 100, 1))
	}

	seenSymbols := make(map[string]bool)
	for i := 0; i < len(symbols); i++ {
		select {
		case ev := <-se.ExecutionCh:
			require.NotNil(t, ev.Trade)
			seenSymbols[ev.Trade.CoinSymbol] = true
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for trade")
		}
	}
	for _, symbol := range symbols {
		assert.True(t, seenSymbols[symbol], "missing trade for %s", symbol)
	}
}

func TestShardedEngineCancelRoutesToOwningShard(t *testing.T) {
	se := newTestShardedEngine(4)
	se.Start()

	se.SubmitOrder(testOrder(1, "BTC", model.OrderSideBuy, 100, 5))
	snap := waitForShardedSnapshot(t, se, "BTC")
	require.Len(t, snap.Bids, 1)

	result := se.CancelOrder(CancelOrderCommand{
		CoinSymbol: "BTC",
		OrderID:    1,
		Side:       model.OrderSideBuy,
		Price:      decimal.NewFromInt(100),
	})
	assert.True(t, result.Removed)
	assert.NoError(t, result.Err)

	// 취소는 오더북에 즉시 반영되지만, 캐시는 다음 코얼레싱 티커가 flush해야
	// 갱신된다 — 그 스냅샷을 기다린 뒤 캐시를 읽는다.
	afterCancel := waitForShardedSnapshot(t, se, "BTC")
	assert.Empty(t, afterCancel.Bids, "취소가 제출과 같은 샤드로 라우팅돼 오더북에서 사라져야 한다")

	cached, err := se.RequestOrderBookSnapshot("BTC", 30)
	require.NoError(t, err)
	assert.Empty(t, cached.Bids)
}

func TestShardedEngineSnapshotReadsOwningShardCache(t *testing.T) {
	se := newTestShardedEngine(4)
	se.Start()

	se.SubmitOrder(testOrder(1, "ETH", model.OrderSideBuy, 200, 3))
	snap := waitForShardedSnapshot(t, se, "ETH")
	require.Len(t, snap.Bids, 1)
	assert.True(t, snap.Bids[0].Price.Equal(decimal.NewFromInt(200)))

	cached, err := se.RequestOrderBookSnapshot("ETH", 30)
	require.NoError(t, err)
	require.Len(t, cached.Bids, 1)
	assert.True(t, cached.Bids[0].Price.Equal(decimal.NewFromInt(200)), "스냅샷 조회가 소유 샤드 캐시에서 읽혀야 한다")
}

func TestShardedEngineStopDrainsAllShardsThenClosesMergedChannels(t *testing.T) {
	se := newTestShardedEngine(4)
	se.Start()

	symbols := []string{"BTC", "ETH", "XRP", "SOL"}
	for i, symbol := range symbols {
		se.SubmitOrder(testOrder(uint(i*2+1), symbol, model.OrderSideSell, 100, 1))
		se.SubmitOrder(testOrder(uint(i*2+2), symbol, model.OrderSideBuy, 100, 1))
	}

	se.Stop()
	<-se.Done()

	tradeCount := 0
	for {
		ev, ok := <-se.ExecutionCh
		if !ok {
			break
		}
		if ev.Trade != nil {
			tradeCount++
		}
	}
	assert.Equal(t, len(symbols), tradeCount, "접수된 주문에서 나온 체결은 전부 유실 없이 전달돼야 한다")

	for {
		if _, ok := <-se.SnapshotCh; !ok {
			break
		}
	}
}

// engine_concurrency_test.go의 Stop 배리어 패턴(waitWithTimeout)을 그대로 쓴다.
// 여러 심볼에 동시 다발 제출 → Stop()/Done()으로 처리 완료를 배리어 삼아
// 레이스 없이 각 심볼(=각 샤드) 오더북이 정확한 상태인지 확인한다.
func TestShardedEngineConcurrentMultiSymbolSubmission_NoRace(t *testing.T) {
	se := newTestShardedEngine(4)
	se.Start()

	symbols := []string{"BTC", "ETH", "XRP", "SOL", "ADA", "DOGE", "TRX", "DOT"}
	const ordersPerSymbol = 20
	var wg sync.WaitGroup
	wg.Add(len(symbols) * ordersPerSymbol)

	var nextOrderID uint32
	for _, symbol := range symbols {
		for i := 0; i < ordersPerSymbol; i++ {
			go func(symbol string, i int) {
				defer wg.Done()
				id := atomic.AddUint32(&nextOrderID, 1)
				se.SubmitOrder(testOrder(uint(id), symbol, model.OrderSideBuy, int64(1000+i), 1))
			}(symbol, i)
		}
	}

	waitWithTimeout(t, &wg, 5*time.Second)
	se.Stop()
	<-se.Done()

	for _, symbol := range symbols {
		snap, err := se.RequestOrderBookSnapshot(symbol, 50)
		require.NoError(t, err)
		total := 0
		for _, level := range snap.Bids {
			total += int(level.Quantity.IntPart())
		}
		assert.Equal(t, ordersPerSymbol, total, "symbol %s order count mismatch", symbol)
	}
}
