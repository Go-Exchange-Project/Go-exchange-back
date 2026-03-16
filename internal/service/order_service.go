// 비즈니스 로직

package service

import (
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"fmt"
)

type OrderService struct {
	OrderRepository *repository.OrderRepository
	WalletRepository *repository.WalletRepository
	MatchingEngine *matching.MatchingEngine
}

func NewOrderService(repo *repository.OrderRepository, walletRepo *repository.WalletRepository, me *matching.MatchingEngine) *OrderService {
	return &OrderService{
		OrderRepository: repo,
		WalletRepository: walletRepo,
		MatchingEngine: me,
	}
}

func (s *OrderService) CreateOrder(coinSymbol string, side string, price int64, amount int64) error {
    wallet, err := s.WalletRepository.FindByUserID(1) // 임시로 user_id 1 고정
    if err != nil {
        return err
    }

    // 잔고 확인
    if side == "BUY" {
        required := decimal.NewFromInt(price * amount)
        if wallet.KRW.LessThan(required) {
            return fmt.Errorf("KRW 잔고 부족")
        }
    } else {
        if wallet.Quantity.LessThan(decimal.NewFromInt(amount)) {
            return fmt.Errorf("코인 잔고 부족")
        }
    }
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