// db와 직접 연결되는 레포지토리 파일

package repository

import "gorm.io/gorm"

type OrderRepository struct{
	DB *gorm.DB
}

func NewOrderRepository(db *gorm.DB) *OrderRepository {
	return &OrderRepository{DB: db}
}