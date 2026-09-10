package repository

import (
	"fmt"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func cleanupReconciliationViolations(t *testing.T, db *gorm.DB, subjectKeys ...string) {
	t.Helper()

	if len(subjectKeys) == 0 {
		return
	}
	require.NoError(t, db.Where("subject_key IN ?", subjectKeys).Delete(&model.ReconciliationViolation{}).Error)
}

func TestIntegrationCheckStaleMarketOrdersFindsOldPendingMarketOrder(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(820)
	defer cleanupRepositoryUsers(t, db, userID)

	staleOrder := model.Order{
		UserID:       userID,
		CoinSymbol:   "BTC",
		Side:         model.OrderSideBuy,
		OrderType:    model.OrderTypeMarket,
		Status:       model.OrderStatusPending,
		Price:        decimal.Zero,
		Amount:       decimal.Zero,
		QuoteAmount:  decimal.NewFromInt(100_000),
		FilledAmount: decimal.Zero,
		CreatedAt:    time.Now().UTC().Add(-10 * time.Minute),
	}
	require.NoError(t, db.Create(&staleOrder).Error)

	freshOrder := model.Order{
		UserID:       userID,
		CoinSymbol:   "BTC",
		Side:         model.OrderSideBuy,
		OrderType:    model.OrderTypeMarket,
		Status:       model.OrderStatusPending,
		Price:        decimal.Zero,
		Amount:       decimal.Zero,
		QuoteAmount:  decimal.NewFromInt(100_000),
		FilledAmount: decimal.Zero,
		CreatedAt:    time.Now().UTC(),
	}
	require.NoError(t, db.Create(&freshOrder).Error)

	repo := NewReconciliationRepository(db)
	rows, err := repo.CheckStaleMarketOrders(5 * time.Minute)
	require.NoError(t, err)

	assert.NotNil(t, findStaleMarketOrderRow(rows, staleOrder.ID), "10-minute-old pending market order must be flagged")
	assert.Nil(t, findStaleMarketOrderRow(rows, freshOrder.ID), "fresh order must not be flagged")
}

func findStaleMarketOrderRow(rows []StaleMarketOrderRow, orderID uint) *StaleMarketOrderRow {
	for i := range rows {
		if rows[i].OrderID == orderID {
			return &rows[i]
		}
	}
	return nil
}

func TestIntegrationCreateViolationsInsertsOnlyWhenNonEmpty(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewReconciliationRepository(db)

	require.NoError(t, repo.CreateViolations(nil))

	subjectKey := fmt.Sprintf("wallet:test-%d", time.Now().UnixNano())
	defer cleanupReconciliationViolations(t, db, subjectKey)

	violations := []model.ReconciliationViolation{{
		CheckName:  "ledger_wallet",
		SubjectKey: subjectKey,
		Detail:     "test detail",
		DetectedAt: time.Now().UTC(),
	}}
	require.NoError(t, repo.CreateViolations(violations))

	var count int64
	require.NoError(t, db.Model(&model.ReconciliationViolation{}).Where("subject_key = ?", subjectKey).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}
