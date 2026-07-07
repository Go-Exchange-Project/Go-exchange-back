package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
)

type BootstrapOrderRepository interface {
	FindOpenOrdersForBootstrap() ([]model.Order, error)
}

type MatchingBootstrapService struct {
	OrderRepository BootstrapOrderRepository
	MatchingEngine  *matching.MatchingEngine
}

type MatchingBootstrapResult struct {
	Loaded       int
	Submitted    int
	Skipped      int
	StatusCounts map[model.OrderStatus]int
}

func NewMatchingBootstrapService(orderRepo BootstrapOrderRepository, me *matching.MatchingEngine) *MatchingBootstrapService {
	return &MatchingBootstrapService{
		OrderRepository: orderRepo,
		MatchingEngine:  me,
	}
}

func (s *MatchingBootstrapService) BootstrapOpenOrders(ctx context.Context) (MatchingBootstrapResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if s == nil || s.OrderRepository == nil {
		return MatchingBootstrapResult{}, fmt.Errorf("bootstrap order repository is required")
	}
	if s.MatchingEngine == nil || s.MatchingEngine.OrderCh == nil {
		return MatchingBootstrapResult{}, fmt.Errorf("matching engine is required")
	}

	orders, err := s.OrderRepository.FindOpenOrdersForBootstrap()
	if err != nil {
		return MatchingBootstrapResult{}, err
	}

	result := MatchingBootstrapResult{
		Loaded:       len(orders),
		StatusCounts: make(map[model.OrderStatus]int),
	}
	for _, order := range orders {
		result.StatusCounts[order.Status]++

		matchingOrder, err := matchingOrderFromModelOrder(order)
		if err != nil {
			return result, fmt.Errorf("bootstrap order %d: %w", order.ID, err)
		}
		if matchingOrder == nil {
			result.Skipped++
			continue
		}

		select {
		case s.MatchingEngine.OrderCh <- matchingOrder:
			result.Submitted++
		case <-ctx.Done():
			return result, fmt.Errorf("bootstrap open orders interrupted after %d/%d submissions: %w", result.Submitted, result.Loaded, ctx.Err())
		}
	}

	return result, nil
}

func matchingOrderFromModelOrder(order model.Order) (*matching.Order, error) {
	remaining := order.Amount.Sub(order.FilledAmount)
	if !remaining.GreaterThan(decimal.Zero) {
		return nil, nil
	}
	if order.ID == 0 {
		return nil, fmt.Errorf("order id is required")
	}
	if order.UserID == 0 {
		return nil, fmt.Errorf("user_id is required")
	}

	coinSymbol := strings.ToUpper(strings.TrimSpace(order.CoinSymbol))
	if coinSymbol == "" {
		return nil, fmt.Errorf("coin_symbol is required")
	}

	switch order.Status {
	case model.OrderStatusPending, model.OrderStatusPartial:
	default:
		return nil, nil
	}

	switch order.Side {
	case model.OrderSideBuy, model.OrderSideSell:
	default:
		return nil, fmt.Errorf("invalid order side %q", order.Side)
	}

	if order.OrderType != model.OrderTypeLimit {
		return nil, fmt.Errorf("unsupported order type %q", order.OrderType)
	}
	if !order.Price.GreaterThanOrEqual(decimal.Zero) {
		return nil, fmt.Errorf("price must be non-negative")
	}

	return &matching.Order{
		ID:           order.ID,
		UserID:       order.UserID,
		CoinSymbol:   coinSymbol,
		Side:         order.Side,
		Price:        order.Price,
		Amount:       remaining,
		CreatedAt:    order.CreatedAt,
		EnqueuedAt:   time.Now(),
		OrderType:    order.OrderType,
		FilledAmount: order.FilledAmount,
	}, nil
}
