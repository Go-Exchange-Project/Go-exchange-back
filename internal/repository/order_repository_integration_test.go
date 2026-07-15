package repository

import (
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestIntegrationFindOpenOrdersForBootstrapFiltersAndOrders(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(30)
	coinSymbol := "BOOT-FILTER"
	base := time.Now().UTC().Add(-time.Hour)

	partial := bootstrapRepositoryOrder(userID, coinSymbol, model.OrderStatusPartial, base.Add(time.Minute), decimal.NewFromInt(10), decimal.NewFromInt(4))
	sameTimeFirst := bootstrapRepositoryOrder(userID, coinSymbol, model.OrderStatusPending, base.Add(2*time.Minute), decimal.NewFromInt(5), decimal.Zero)
	sameTimeSecond := bootstrapRepositoryOrder(userID, coinSymbol, model.OrderStatusPending, base.Add(2*time.Minute), decimal.NewFromInt(6), decimal.Zero)
	pending := bootstrapRepositoryOrder(userID, coinSymbol, model.OrderStatusPending, base.Add(3*time.Minute), decimal.NewFromInt(7), decimal.Zero)
	noRemaining := bootstrapRepositoryOrder(userID, coinSymbol, model.OrderStatusPending, base.Add(4*time.Minute), decimal.NewFromInt(5), decimal.NewFromInt(5))
	filled := bootstrapRepositoryOrder(userID, coinSymbol, model.OrderStatusFilled, base.Add(5*time.Minute), decimal.NewFromInt(5), decimal.NewFromInt(5))
	cancelled := bootstrapRepositoryOrder(userID, coinSymbol, model.OrderStatusCancelled, base.Add(6*time.Minute), decimal.NewFromInt(5), decimal.Zero)

	orders := []*model.Order{&partial, &sameTimeFirst, &sameTimeSecond, &pending, &noRemaining, &filled, &cancelled}
	for _, order := range orders {
		require.NoError(t, db.Create(order).Error)
	}
	defer cleanupBootstrapRepositoryOrders(t, db, userID)

	got, err := NewOrderRepository(db).FindOpenOrdersForBootstrap()
	require.NoError(t, err)

	targetIDs := map[uint]bool{
		partial.ID:        true,
		sameTimeFirst.ID:  true,
		sameTimeSecond.ID: true,
		pending.ID:        true,
		noRemaining.ID:    true,
		filled.ID:         true,
		cancelled.ID:      true,
	}
	var found []uint
	for _, order := range got {
		if targetIDs[order.ID] {
			found = append(found, order.ID)
		}
	}

	assert.Equal(t, []uint{partial.ID, sameTimeFirst.ID, sameTimeSecond.ID, pending.ID}, found)
}

func TestIntegrationListOrdersByUserIDScopesFiltersAndOrders(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(31)
	otherUserID := repositoryTestUserID(32)
	coinSymbol := "ORDER-LIST"
	defer cleanupRepositoryUsers(t, db, userID, otherUserID)

	base := time.Now().UTC().Add(-time.Hour)
	older := bootstrapRepositoryOrder(userID, coinSymbol, model.OrderStatusPending, base, decimal.NewFromInt(5), decimal.Zero)
	newer := bootstrapRepositoryOrder(userID, coinSymbol, model.OrderStatusFilled, base.Add(time.Minute), decimal.NewFromInt(5), decimal.NewFromInt(5))
	otherCoin := bootstrapRepositoryOrder(userID, "OTHER-LIST", model.OrderStatusPending, base.Add(2*time.Minute), decimal.NewFromInt(5), decimal.Zero)
	otherUser := bootstrapRepositoryOrder(otherUserID, coinSymbol, model.OrderStatusPending, base.Add(3*time.Minute), decimal.NewFromInt(5), decimal.Zero)
	for _, order := range []*model.Order{&older, &newer, &otherCoin, &otherUser} {
		require.NoError(t, db.Create(order).Error)
	}

	status := model.OrderStatusPending
	got, err := NewOrderRepository(db).ListByUserID(userID, OrderListFilter{
		Status:     &status,
		CoinSymbol: coinSymbol,
		Limit:      10,
	})
	require.NoError(t, err)

	require.Len(t, got, 1)
	assert.Equal(t, older.ID, got[0].ID)
}

func TestIntegrationFindOrderByUserIDAndIDDoesNotReturnOtherUsersOrder(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(33)
	otherUserID := repositoryTestUserID(34)
	defer cleanupRepositoryUsers(t, db, userID, otherUserID)

	order := bootstrapRepositoryOrder(otherUserID, "ORDER-SCOPE", model.OrderStatusPending, time.Now().UTC(), decimal.NewFromInt(5), decimal.Zero)
	require.NoError(t, db.Create(&order).Error)

	_, err := NewOrderRepository(db).FindByUserIDAndID(userID, order.ID)

	require.Error(t, err)
}

func bootstrapRepositoryOrder(userID uint, coinSymbol string, status model.OrderStatus, createdAt time.Time, amount decimal.Decimal, filled decimal.Decimal) model.Order {
	return model.Order{
		UserID:       userID,
		CoinSymbol:   coinSymbol,
		Side:         model.OrderSideBuy,
		OrderType:    model.OrderTypeLimit,
		Price:        decimal.NewFromInt(100),
		Amount:       amount,
		Status:       status,
		FilledAmount: filled,
		CreatedAt:    createdAt,
	}
}

func TestIntegrationOrderLockByIDsReturnsAllRequestedRowsInIDOrder(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(40)
	defer cleanupBootstrapRepositoryOrders(t, db, userID)

	repo := NewOrderRepository(db)
	order1 := bootstrapRepositoryOrder(userID, "LOCK-TEST", model.OrderStatusPending, time.Now().UTC().Add(-time.Minute), decimal.NewFromInt(10), decimal.Zero)
	order2 := bootstrapRepositoryOrder(userID, "LOCK-TEST", model.OrderStatusPending, time.Now().UTC(), decimal.NewFromInt(20), decimal.Zero)

	require.NoError(t, db.Create(&order1).Error)
	require.NoError(t, db.Create(&order2).Error)

	// Request in non-sorted order: [id2, id1]
	ids := []uint{order2.ID, order1.ID}
	got, err := repo.LockByIDs(ids)
	require.NoError(t, err)

	// Should return 2 rows in ID ascending order
	require.Len(t, got, 2)
	assert.Equal(t, order1.ID, got[0].ID)
	assert.Equal(t, order2.ID, got[1].ID)
}

func TestIntegrationOrderLockByIDsFailsWhenARowIsMissing(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(41)
	defer cleanupBootstrapRepositoryOrders(t, db, userID)

	repo := NewOrderRepository(db)
	order1 := bootstrapRepositoryOrder(userID, "LOCK-MISSING", model.OrderStatusPending, time.Now().UTC(), decimal.NewFromInt(10), decimal.Zero)
	require.NoError(t, db.Create(&order1).Error)

	// Request existing ID + non-existent ID
	ids := []uint{order1.ID, 999999999}
	_, err := repo.LockByIDs(ids)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected 2 rows, locked 1")
}

func TestIntegrationBatchUpdateExecutionsUpdatesAllColumns(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(42)
	defer cleanupBootstrapRepositoryOrders(t, db, userID)

	repo := NewOrderRepository(db)
	order1 := bootstrapRepositoryOrder(userID, "BATCH-UPDATE", model.OrderStatusPending, time.Now().UTC().Add(-time.Minute), decimal.NewFromInt(10), decimal.Zero)
	order2 := bootstrapRepositoryOrder(userID, "BATCH-UPDATE", model.OrderStatusPending, time.Now().UTC(), decimal.NewFromInt(20), decimal.Zero)

	require.NoError(t, db.Create(&order1).Error)
	require.NoError(t, db.Create(&order2).Error)

	// Update with different values
	err := repo.BatchUpdateExecutions([]OrderExecutionBatchUpdate{
		{
			OrderID:           order1.ID,
			FilledAmount:      decimal.NewFromInt(5),
			FilledQuoteAmount: decimal.NewFromInt(500),
			Status:            model.OrderStatusPartial,
		},
		{
			OrderID:           order2.ID,
			FilledAmount:      decimal.NewFromInt(20),
			FilledQuoteAmount: decimal.NewFromInt(2000),
			Status:            model.OrderStatusFilled,
		},
	})
	require.NoError(t, err)

	// Verify updates
	updated1, err := repo.FindByID(order1.ID)
	require.NoError(t, err)
	assert.Equal(t, model.OrderStatusPartial, updated1.Status)
	assert.True(t, updated1.FilledAmount.Equal(decimal.NewFromInt(5)))
	assert.True(t, updated1.FilledQuoteAmount.Equal(decimal.NewFromInt(500)))

	updated2, err := repo.FindByID(order2.ID)
	require.NoError(t, err)
	assert.Equal(t, model.OrderStatusFilled, updated2.Status)
	assert.True(t, updated2.FilledAmount.Equal(decimal.NewFromInt(20)))
	assert.True(t, updated2.FilledQuoteAmount.Equal(decimal.NewFromInt(2000)))
}

func TestIntegrationBatchUpdateExecutionsFailsOnRowCountMismatch(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(43)
	defer cleanupBootstrapRepositoryOrders(t, db, userID)

	repo := NewOrderRepository(db)
	order1 := bootstrapRepositoryOrder(userID, "BATCH-MISSING", model.OrderStatusPending, time.Now().UTC(), decimal.NewFromInt(10), decimal.Zero)
	require.NoError(t, db.Create(&order1).Error)

	// Try to update existing order + non-existent order
	err := repo.BatchUpdateExecutions([]OrderExecutionBatchUpdate{
		{
			OrderID:           order1.ID,
			FilledAmount:      decimal.NewFromInt(5),
			FilledQuoteAmount: decimal.NewFromInt(500),
			Status:            model.OrderStatusPartial,
		},
		{
			OrderID:           999999999,
			FilledAmount:      decimal.NewFromInt(1),
			FilledQuoteAmount: decimal.NewFromInt(100),
			Status:            model.OrderStatusFilled,
		},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected 2")
}

func cleanupBootstrapRepositoryOrders(t *testing.T, db *gorm.DB, userID uint) {
	t.Helper()

	require.NoError(t, db.Where("user_id = ?", userID).Delete(&model.Order{}).Error)
}
