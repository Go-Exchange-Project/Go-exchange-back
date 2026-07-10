package repository

import (
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/testdb"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegrationFailedMarketCompletionRecordFindResolve(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)
	repo := NewFailedMarketCompletionRepository(db)

	orderID := uint(time.Now().UnixNano() % 1_000_000_000)
	t.Cleanup(func() {
		require.NoError(t, db.Where("order_id = ?", orderID).Delete(&model.FailedMarketCompletion{}).Error)
	})

	failure := &model.FailedMarketCompletion{
		OrderID:              orderID,
		CoinSymbol:           "BTC",
		FilledAmount:         decimal.NewFromInt(1),
		FilledQuoteAmount:    decimal.NewFromInt(100),
		RemainingQuoteAmount: decimal.NewFromInt(5),
		ErrorMessage:         "market order settlement is not complete",
		Status:               model.FailedSettlementStatusOpen,
		RetryCount:           1,
		OccurredAt:           time.Now().UTC(),
	}

	persisted, err := repo.RecordFailure(failure)
	require.NoError(t, err)
	assert.Equal(t, uint(1), persisted.RetryCount)
	assert.Equal(t, model.FailedSettlementStatusOpen, persisted.Status)
	assert.True(t, persisted.RemainingQuoteAmount.Equal(decimal.NewFromInt(5)))

	// 같은 order_id로 다시 기록하면 retry_count가 증가하고 OPEN으로 유지된다.
	duplicate := *failure
	duplicate.ID = 0
	duplicate.ErrorMessage = "retry also failed"
	persisted, err = repo.RecordFailure(&duplicate)
	require.NoError(t, err)
	assert.Equal(t, uint(2), persisted.RetryCount)
	assert.Equal(t, "retry also failed", persisted.ErrorMessage)

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

	var resolved model.FailedMarketCompletion
	require.NoError(t, db.First(&resolved, persisted.ID).Error)
	assert.Equal(t, model.FailedSettlementStatusResolved, resolved.Status)
	assert.Equal(t, "auto-retry succeeded", resolved.Resolution)
	require.NotNil(t, resolved.ResolvedAt)

	// 이미 RESOLVED면 다시 resolve할 수 없다.
	err = repo.MarkResolved(persisted.ID, "again")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "affected no rows")
}
