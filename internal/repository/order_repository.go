package repository

import (
	"fmt"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type OrderRepository struct {
	DB *gorm.DB
}

func NewOrderRepository(db *gorm.DB) *OrderRepository {
	return &OrderRepository{DB: db}
}

func (r *OrderRepository) WithTx(tx *gorm.DB) *OrderRepository {
	return &OrderRepository{DB: tx}
}

func (r *OrderRepository) CreateOrder(order *model.Order) error {
	return r.DB.Create(order).Error
}

func (r *OrderRepository) UpdateOrderStatus(orderID uint, status model.OrderStatus, filledAmount decimal.Decimal) error {
	return r.UpdateOrderFill(orderID, filledAmount, status)
}

func (r *OrderRepository) UpdateOrderFill(orderID uint, filledAmount decimal.Decimal, status model.OrderStatus) error {
	result := r.DB.Model(&model.Order{}).Where("id = ?", orderID).Updates(map[string]interface{}{
		"status":        status,
		"filled_amount": filledAmount,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("order fill update affected no rows")
	}
	return nil
}

func (r *OrderRepository) FindByID(orderID uint) (*model.Order, error) {
	var order model.Order
	err := r.DB.First(&order, orderID).Error
	return &order, err
}

func (r *OrderRepository) FindByIDForUpdate(orderID uint) (*model.Order, error) {
	var order model.Order
	err := r.DB.Clauses(clause.Locking{Strength: "UPDATE"}).First(&order, orderID).Error
	return &order, err
}

func (r *OrderRepository) FindOpenOrdersForBootstrap() ([]model.Order, error) {
	var orders []model.Order
	err := r.DB.
		Where("status IN ?", []model.OrderStatus{model.OrderStatusPending, model.OrderStatusPartial}).
		Where("amount > filled_amount").
		Order("created_at ASC").
		Order("id ASC").
		Find(&orders).Error
	return orders, err
}
