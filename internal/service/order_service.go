// 비즈니스 로직

package service

import (
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
)

type OrderService struct {
	OrderRepository *repository.OrderRepository
	MatchingEngine *matching.MatchingEngine
}

func NewOrderService(repo *repository.OrderRepository, me *matching.MatchingEngine) *OrderService {
	return &OrderService{
		OrderRepository: repo,
		MatchingEngine: me,
	}
}

func (s *OrderService) CreateOrder(coinSymbol string, side string, price int64, amount int64) error {
	order := &model.Order{
        CoinSymbol: coinSymbol,
        Side:       model.OrderSide(side),
        Price:      decimal.NewFromInt(price),
        Amount:     decimal.NewFromInt(amount),
        Status:     model.OrderStatusPending,
    }

	if err := s.OrderRepository.CreateOrder(order); err != nil {
        return err
    }

	s.MatchingEngine.OrderCh <- &matching.Order{
		ID: 		order.ID,
		CoinSymbol: order.CoinSymbol,
        Side:       order.Side,
        Price:      order.Price,
        Amount:     order.Amount,
        CreatedAt:  order.CreatedAt,
	}

    return nil
}