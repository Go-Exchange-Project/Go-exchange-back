// 비즈니스 로직

package service

import (
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
)

type OrderService struct {
	OrderRepository *repository.OrderRepository
}

func NewOrderService(repo *repository.OrderRepository) *OrderService {
	return &OrderService{OrderRepository: repo}
}

func (s *OrderService) CreateOrder(coinSymbol string, side string, price int64, amount int64) error {
	return nil
}