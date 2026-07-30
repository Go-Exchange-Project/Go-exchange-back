package repository

import (
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegrationFailedOrderCancellationEnsureDeferredAndRecordFailure(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)
	repo := NewFailedOrderCancellationRepository(db)

	orderID := uint(time.Now().UnixNano() % 1_000_000_000)
	t.Cleanup(func() {
		require.NoError(t, db.Where("order_id = ?", orderID).Delete(&model.FailedOrderCancellation{}).Error)
	})

	deferred := &model.FailedOrderCancellation{
		OrderID:       orderID,
		OutboxEventID: 5501,
		CoinSymbol:    "BTC",
		Side:          model.OrderSideBuy,
		EngineEventID: "evt-7101",
		ErrorMessage:  "blocked by open failed settlement",
		Status:        model.FailedSettlementStatusOpen,
		RetryCount:    0,
		OccurredAt:    time.Now().UTC(),
	}

	first, err := repo.EnsureDeferred(deferred)
	require.NoError(t, err)
	assert.Equal(t, uint(0), first.RetryCount)
	assert.Equal(t, uint64(5501), first.OutboxEventID)

	again := *deferred
	again.ID = 0
	second, err := repo.EnsureDeferred(&again)
	require.NoError(t, err)
	assert.Equal(t, uint(0), second.RetryCount, "차단 반복은 budget을 소비하지 않는다")

	actual := *deferred
	actual.ID = 0
	actual.ErrorMessage = "cancellation actually failed"
	third, err := repo.RecordFailure(&actual)
	require.NoError(t, err)
	assert.Equal(t, uint(1), third.RetryCount)

	fourth, err := repo.RecordFailure(&actual)
	require.NoError(t, err)
	assert.Equal(t, uint(2), fourth.RetryCount)
}

func TestIntegrationFailedOrderCancellationEnsureDeferredDoesNotReopenResolved(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)
	repo := NewFailedOrderCancellationRepository(db)

	orderID := uint(time.Now().UnixNano()%1_000_000_000) + 1
	t.Cleanup(func() {
		require.NoError(t, db.Where("order_id = ?", orderID).Delete(&model.FailedOrderCancellation{}).Error)
	})

	failure := &model.FailedOrderCancellation{
		OrderID:       orderID,
		OutboxEventID: 5502,
		CoinSymbol:    "BTC",
		Side:          model.OrderSideSell,
		EngineEventID: "evt-7102",
		ErrorMessage:  "cancellation failed",
		Status:        model.FailedSettlementStatusOpen,
		RetryCount:    1,
		OccurredAt:    time.Now().UTC(),
	}
	persisted, err := repo.RecordFailure(failure)
	require.NoError(t, err)
	require.NoError(t, repo.MarkResolved(persisted.ID, "resolved by retry worker"))

	reopen := *failure
	reopen.ID = 0
	got, err := repo.EnsureDeferred(&reopen)
	require.NoError(t, err)

	assert.Equal(t, model.FailedSettlementStatusResolved, got.Status,
		"이미 실행된 terminal을 재실행 대상으로 되살리면 안 된다")
	assert.Equal(t, uint(1), got.RetryCount)
}

func TestIntegrationFailedOrderCancellationFindOpenAndResolve(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)
	repo := NewFailedOrderCancellationRepository(db)

	orderID := uint(time.Now().UnixNano()%1_000_000_000) + 2
	t.Cleanup(func() {
		require.NoError(t, db.Where("order_id = ?", orderID).Delete(&model.FailedOrderCancellation{}).Error)
	})

	failure := &model.FailedOrderCancellation{
		OrderID:       orderID,
		OutboxEventID: 5503,
		CoinSymbol:    "BTC",
		Side:          model.OrderSideBuy,
		EngineEventID: "evt-7103",
		ErrorMessage:  "cancellation failed",
		Status:        model.FailedSettlementStatusOpen,
		RetryCount:    1,
		OccurredAt:    time.Now().UTC(),
	}
	persisted, err := repo.RecordFailure(failure)
	require.NoError(t, err)

	open, err := repo.FindOpen(0)
	require.NoError(t, err)
	found := false
	for _, f := range open {
		if f.OrderID == orderID {
			found = true
		}
	}
	assert.True(t, found, "recorded failure should appear in open list")

	require.NoError(t, repo.MarkResolved(persisted.ID, "auto-retry succeeded"))

	var resolved model.FailedOrderCancellation
	require.NoError(t, db.First(&resolved, persisted.ID).Error)
	assert.Equal(t, model.FailedSettlementStatusResolved, resolved.Status)
	assert.Equal(t, "auto-retry succeeded", resolved.Resolution)
	require.NotNil(t, resolved.ResolvedAt)

	err = repo.MarkResolved(persisted.ID, "again")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "affected no rows")
}
