package main

import (
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTouchedOrderIDsDedupsWithinBatch(t *testing.T) {
	d := newDependencyTracker()
	batch := []service.OutboxEvent{
		tradeOutboxEventForOrders(1, 10, 20),
		tradeOutboxEventForOrders(2, 10, 30), // 10이 두 배치에 등장
		tradeOutboxEventForOrders(3, 40, 40), // 자기거래: maker == taker
	}
	assert.ElementsMatch(t, []uint{10, 20, 30, 40}, d.touchedOrderIDs(batch))
}

func TestRegisterAndRetireCountsOncePerBatch(t *testing.T) {
	d := newDependencyTracker()
	d.register(1, []uint{10, 20})
	d.register(2, []uint{10})

	assert.Equal(t, 2, d.outstanding())
	assert.False(t, d.ready(10), "두 배치가 10을 건드렸다")
	assert.False(t, d.ready(20))

	require.NoError(t, d.retire(1, nil))
	assert.True(t, d.ready(20), "20은 이제 대기할 배치가 없다")
	assert.False(t, d.ready(10))

	require.NoError(t, d.retire(2, nil))
	assert.True(t, d.ready(10))
	assert.Equal(t, 0, d.outstanding())
}

func TestRetireOnFailureStillReleasesSlots(t *testing.T) {
	d := newDependencyTracker()
	d.register(1, []uint{10})

	// undurable이 있어도 자원은 정상 retire된다 — 아니면 슬롯이 영구 점유된다.
	require.NoError(t, d.retire(1, []uint{10}))

	assert.Equal(t, 0, d.outstanding())
	assert.True(t, d.ready(10))
	assert.True(t, d.quarantined(10), "다만 terminal 실행은 금지된다")
}

func TestRetireUnknownJobIsInvariantViolation(t *testing.T) {
	d := newDependencyTracker()
	assert.Error(t, d.retire(99, nil))
}

func TestQuarantineClearedAfterTerminalConsumed(t *testing.T) {
	d := newDependencyTracker()
	d.register(1, []uint{10})
	require.NoError(t, d.retire(1, []uint{10}))

	require.True(t, d.quarantined(10))
	assert.Equal(t, 1, d.quarantinedCount())

	d.clearQuarantine(10)
	assert.False(t, d.quarantined(10))
	assert.Equal(t, 0, d.quarantinedCount())
}

func TestReadyIsTrueForUntouchedOrder(t *testing.T) {
	d := newDependencyTracker()
	assert.True(t, d.ready(999), "건드린 배치가 없으면 즉시 dispatch 가능하다")
}
