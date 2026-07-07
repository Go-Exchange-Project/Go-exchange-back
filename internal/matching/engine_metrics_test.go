package matching

import (
	"sync"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMatchingEngineReportsMatchLatencyAfterProcessingOrder(t *testing.T) {
	me := NewMatchingEngine()

	var mu sync.Mutex
	var observed time.Duration
	done := make(chan struct{}, 1)
	me.MatchLatencyObserver = func(d time.Duration) {
		mu.Lock()
		observed = d
		mu.Unlock()
		done <- struct{}{}
	}
	me.Start()

	me.OrderCh <- &Order{
		ID:         1,
		UserID:     1,
		CoinSymbol: "BTC",
		Side:       model.OrderSideBuy,
		Price:      decimal.NewFromInt(100),
		Amount:     decimal.NewFromInt(1),
		OrderType:  model.OrderTypeLimit,
		EnqueuedAt: time.Now(),
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for match latency observation")
	}

	mu.Lock()
	defer mu.Unlock()
	assert.GreaterOrEqual(t, observed, time.Duration(0))
	require.Less(t, observed, 2*time.Second)
}

func TestMatchingEngineSkipsObserverWhenEnqueuedAtIsZero(t *testing.T) {
	me := NewMatchingEngine()

	called := make(chan struct{}, 1)
	me.MatchLatencyObserver = func(time.Duration) {
		called <- struct{}{}
	}
	me.Start()

	me.OrderCh <- &Order{
		ID:         2,
		UserID:     1,
		CoinSymbol: "BTC",
		Side:       model.OrderSideBuy,
		Price:      decimal.NewFromInt(100),
		Amount:     decimal.NewFromInt(1),
		OrderType:  model.OrderTypeLimit,
	}

	// Drain the snapshot channel so Match() completes without blocking, then
	// give the observer a short window to (not) fire.
	select {
	case <-me.SnapshotCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for snapshot")
	}

	select {
	case <-called:
		t.Fatal("observer should not be called when EnqueuedAt is zero")
	case <-time.After(200 * time.Millisecond):
	}
}
