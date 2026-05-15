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

func cleanupBootstrapRepositoryOrders(t *testing.T, db *gorm.DB, userID uint) {
	t.Helper()

	require.NoError(t, db.Where("user_id = ?", userID).Delete(&model.Order{}).Error)
}
