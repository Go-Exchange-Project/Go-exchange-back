package matching

import (
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func stopTestLimitOrder(id uint, side model.OrderSide, price int64, amount int64) *Order {
	return &Order{
		ID:         id,
		UserID:     id,
		CoinSymbol: "BTC",
		Side:       side,
		Price:      decimal.NewFromInt(price),
		Amount:     decimal.NewFromInt(amount),
		OrderType:  model.OrderTypeLimit,
		CreatedAt:  time.Now(),
	}
}

func waitEngineDone(t *testing.T, me *MatchingEngine) {
	t.Helper()
	select {
	case <-me.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not stop in time")
	}
}

func TestEngineStopDrainsQueuedOrdersThenClosesChannels(t *testing.T) {
	me := NewMatchingEngine()

	// 스냅샷 소비자(main의 브로드캐스트 goroutine에 해당) — close로 종료돼야 한다.
	snapshotsDone := make(chan struct{})
	go func() {
		for range me.SnapshotCh {
		}
		close(snapshotsDone)
	}()

	me.Start()
	// 교차하는 지정가 쌍을 넣고 곧바로 Stop — 이미 접수된 주문은 드레인 중에
	// 매칭돼 체결이 방출된 뒤에야 채널이 닫혀야 한다.
	me.OrderCh <- stopTestLimitOrder(1, model.OrderSideSell, 100, 5)
	me.OrderCh <- stopTestLimitOrder(2, model.OrderSideBuy, 100, 5)
	me.Stop()
	waitEngineDone(t, me)

	var trades int
	for event := range me.ExecutionCh { // close까지 소비
		if event.Trade != nil {
			trades++
			assert.Equal(t, "BTC", event.Trade.CoinSymbol)
			assert.True(t, event.Trade.Quantity.Equal(decimal.NewFromInt(5)))
		}
	}
	require.Equal(t, 1, trades, "Stop 전에 접수된 주문의 체결이 유실되면 안 된다")

	select {
	case <-snapshotsDone:
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot channel was not closed")
	}
}

func TestEngineStopIsIdempotent(t *testing.T) {
	me := NewMatchingEngine()
	me.Start()
	go func() {
		for range me.SnapshotCh {
		}
	}()

	me.Stop()
	me.Stop() // 두 번 호출해도 panic 없이 동작해야 한다
	waitEngineDone(t, me)

	_, open := <-me.ExecutionCh
	assert.False(t, open, "정지 후 ExecutionCh는 닫혀 있어야 한다")
}
