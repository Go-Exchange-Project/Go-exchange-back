// db와 직접 연결되는 레포지토리 파일

package repository

import (
	"gorm.io/gorm"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
)

type OrderRepository struct{
	DB *gorm.DB
}

func NewOrderRepository(db *gorm.DB) *OrderRepository {
	return &OrderRepository{DB: db}
}

func (r *OrderRepository) CreateOrder(order *model.Order) error { 
	return r.DB.Create(order).Error
}

func (r *OrderRepository) UpdateOrderStatus(orderID uint, status model.OrderStatus, filledAmount decimal.Decimal) error {
    return r.DB.Model(&model.Order{}).Where("id = ?", orderID).Updates(map[string]interface{}{
        "status":        status,
        "filled_amount": filledAmount,
    }).Error
}

func (r *OrderRepository) FindByID(orderID uint) (*model.Order, error) {
    var order model.Order
    err := r.DB.First(&order, orderID).Error
    return &order, err
}