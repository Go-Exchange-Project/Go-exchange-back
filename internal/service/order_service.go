package service

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

type OrderService struct {
	OrderRepository  *repository.OrderRepository
	WalletRepository *repository.WalletRepository
	MatchingEngine   *matching.MatchingEngine
	TradeRepository  *repository.TradeRepository
	LedgerRepository *repository.LedgerRepository
	MarketRules      *MarketRulesRegistry
}

const (
	DefaultQueryLimit = 50
	MaxQueryLimit     = 200
)

type CreateOrderInput struct {
	UserID      uint
	CoinSymbol  string
	Side        string
	OrderType   string
	Price       string
	Amount      string
	QuoteAmount string
}

type CancelOrderInput struct {
	UserID  uint
	OrderID uint
}

type ListOrdersInput struct {
	UserID     uint
	Status     string
	CoinSymbol string
	Limit      int
}

type ListTradesInput struct {
	UserID     uint
	CoinSymbol string
	Limit      int
}

type CancelOrderResult struct {
	OrderID        uint
	Status         model.OrderStatus
	ReleasedAsset  string
	ReleasedAmount decimal.Decimal
	EngineRemoved  bool
}

type CompleteMarketOrderInput struct {
	OrderID              uint
	FilledAmount         decimal.Decimal
	FilledQuoteAmount    decimal.Decimal
	RemainingQuoteAmount decimal.Decimal
}

func NewOrderService(repo *repository.OrderRepository, walletRepo *repository.WalletRepository, me *matching.MatchingEngine) *OrderService {
	service := &OrderService{
		OrderRepository:  repo,
		WalletRepository: walletRepo,
		MatchingEngine:   me,
		MarketRules:      defaultMarketRulesRegistry,
	}
	if repo != nil && repo.DB != nil {
		service.TradeRepository = repository.NewTradeRepository(repo.DB)
		service.LedgerRepository = repository.NewLedgerRepository(repo.DB)
	}
	return service
}

func (s *OrderService) CreateOrder(input CreateOrderInput) (*model.Order, error) {
	order, err := s.BuildOrder(input)
	if err != nil {
		return nil, err
	}

	if err := s.OrderRepository.DB.Transaction(func(tx *gorm.DB) error {
		orderRepo := s.OrderRepository.WithTx(tx)
		walletRepo := s.WalletRepository.WithTx(tx)
		ledgerRepo := s.LedgerRepository.WithTx(tx)
		if err := orderRepo.CreateOrder(order); err != nil {
			return err
		}
		return holdOrderAssets(walletRepo, ledgerRepo, order)
	}); err != nil {
		return nil, err
	}

	if s.MatchingEngine != nil {
		s.MatchingEngine.OrderCh <- &matching.Order{
			ID:                order.ID,
			UserID:            order.UserID,
			CoinSymbol:        order.CoinSymbol,
			Side:              order.Side,
			Price:             order.Price,
			Amount:            order.Amount,
			QuoteAmount:       matchingQuoteAmountForOrder(order),
			CreatedAt:         order.CreatedAt,
			EnqueuedAt:        time.Now(),
			OrderType:         order.OrderType,
			FilledAmount:      order.FilledAmount,
			FilledQuoteAmount: order.FilledQuoteAmount,
		}
	}

	return order, nil
}

func (s *OrderService) BuildOrder(input CreateOrderInput) (*model.Order, error) {
	return BuildOrderWithRegistry(input, s.marketRulesRegistry())
}

func (s *OrderService) CancelOrder(input CancelOrderInput) (*CancelOrderResult, error) {
	if input.UserID == 0 {
		return nil, NewValidationErrorf("user_id is required")
	}
	if input.OrderID == 0 {
		return nil, NewValidationErrorf("order_id is required")
	}

	var result *CancelOrderResult
	var cancelCommand matching.CancelOrderCommand
	if err := s.OrderRepository.DB.Transaction(func(tx *gorm.DB) error {
		orderRepo := s.OrderRepository.WithTx(tx)
		walletRepo := s.WalletRepository.WithTx(tx)
		ledgerRepo := s.LedgerRepository.WithTx(tx)

		order, err := orderRepo.FindByIDForUpdate(input.OrderID)
		if err != nil {
			return err
		}
		if order.UserID != input.UserID {
			return NewForbiddenErrorf("order does not belong to user")
		}
		if !isCancellableOrderStatus(order.Status) {
			return NewConflictErrorf("order status %s cannot be cancelled", order.Status)
		}
		if order.OrderType == model.OrderTypeMarket {
			return NewConflictErrorf("market orders cannot be cancelled")
		}

		remaining, err := remainingOrderQuantity(order)
		if err != nil {
			return err
		}

		releasedAsset, releasedAmount, err := releaseOrderHold(walletRepo, ledgerRepo, order, remaining)
		if err != nil {
			return err
		}

		if err := orderRepo.UpdateOrderExecution(order.ID, order.FilledAmount, order.FilledQuoteAmount, model.OrderStatusCancelled); err != nil {
			return err
		}

		result = &CancelOrderResult{
			OrderID:        order.ID,
			Status:         model.OrderStatusCancelled,
			ReleasedAsset:  releasedAsset,
			ReleasedAmount: releasedAmount,
		}
		cancelCommand = matching.CancelOrderCommand{
			CoinSymbol: order.CoinSymbol,
			OrderID:    order.ID,
			Side:       order.Side,
			Price:      order.Price,
		}
		return nil
	}); err != nil {
		return nil, err
	}

	if s.MatchingEngine != nil {
		cancelResult := s.MatchingEngine.CancelOrder(cancelCommand)
		if cancelResult.Err != nil {
			return result, fmt.Errorf("order cancelled in DB but matching engine cancel failed: %w", cancelResult.Err)
		}
		result.EngineRemoved = cancelResult.Removed
	}

	return result, nil
}

func (s *OrderService) CompleteMarketOrder(input CompleteMarketOrderInput) error {
	if input.OrderID == 0 {
		return NewValidationErrorf("order_id is required")
	}

	return s.OrderRepository.DB.Transaction(func(tx *gorm.DB) error {
		orderRepo := s.OrderRepository.WithTx(tx)
		walletRepo := s.WalletRepository.WithTx(tx)
		ledgerRepo := s.LedgerRepository.WithTx(tx)
		tradeRepo := s.TradeRepository.WithTx(tx)

		order, err := orderRepo.FindByIDForUpdate(input.OrderID)
		if err != nil {
			return err
		}
		if order.OrderType != model.OrderTypeMarket {
			return NewValidationErrorf("order %d is not a market order", order.ID)
		}
		if order.Status == model.OrderStatusFilled || order.Status == model.OrderStatusCancelled {
			return nil
		}
		if order.FilledAmount.LessThan(input.FilledAmount) ||
			order.FilledQuoteAmount.LessThan(input.FilledQuoteAmount) {
			return NewConflictErrorf("market order %d settlement is not complete", order.ID)
		}

		switch order.Side {
		case model.OrderSideBuy:
			return completeMarketBuyOrder(orderRepo, walletRepo, ledgerRepo, tradeRepo, order, input)
		case model.OrderSideSell:
			return completeMarketSellOrder(orderRepo, walletRepo, ledgerRepo, order)
		default:
			return NewValidationErrorf("invalid order side")
		}
	})
}

func completeMarketBuyOrder(orderRepo *repository.OrderRepository, walletRepo *repository.WalletRepository, ledgerRepo *repository.LedgerRepository, tradeRepo *repository.TradeRepository, order *model.Order, input CompleteMarketOrderInput) error {
	buyerFeeTotal, err := tradeRepo.SumBuyerFeesByBuyOrderID(order.ID)
	if err != nil {
		return err
	}
	spentQuoteWithFees := order.FilledQuoteAmount.Add(buyerFeeTotal)
	if spentQuoteWithFees.GreaterThan(order.QuoteAmount) {
		return NewConflictErrorf("market buy order %d spent quote amount %s exceeds quote budget %s", order.ID, spentQuoteWithFees.String(), order.QuoteAmount.String())
	}

	remainingQuote := order.QuoteAmount.Sub(spentQuoteWithFees)
	if remainingQuote.GreaterThan(decimal.Zero) {
		wallet, err := walletRepo.FindKRWWalletByUserIDForUpdate(order.UserID)
		if err != nil {
			return err
		}
		update, err := releaseBuyOrderHold(wallet, remainingQuote)
		if err != nil {
			return err
		}
		if err := walletRepo.UpdateBalances(order.UserID, model.KRWAssetSymbol, update.AvailableBalance, update.LockedBalance); err != nil {
			return err
		}
		entry := ledgerEntryFromWalletUpdate(wallet, update, model.LedgerEntryTypeOrderRelease, model.LedgerReferenceTypeOrder, order.ID, "")
		if err := ledgerRepo.Create(&entry); err != nil {
			return err
		}
	}
	if !input.RemainingQuoteAmount.GreaterThan(decimal.Zero) {
		return orderRepo.UpdateOrderExecution(order.ID, order.FilledAmount, order.FilledQuoteAmount, model.OrderStatusFilled)
	}
	return orderRepo.UpdateOrderExecution(order.ID, order.FilledAmount, order.FilledQuoteAmount, model.OrderStatusCancelled)
}

func completeMarketSellOrder(orderRepo *repository.OrderRepository, walletRepo *repository.WalletRepository, ledgerRepo *repository.LedgerRepository, order *model.Order) error {
	remaining, err := remainingMarketSellQuantity(order)
	if err != nil {
		return err
	}
	if remaining.GreaterThan(decimal.Zero) {
		wallet, err := walletRepo.FindByUserIDAndCoinSymbolForUpdate(order.UserID, order.CoinSymbol)
		if err != nil {
			return err
		}
		update, err := releaseSellOrderHold(wallet, remaining)
		if err != nil {
			return err
		}
		if err := walletRepo.UpdateBalances(order.UserID, order.CoinSymbol, update.AvailableBalance, update.LockedBalance); err != nil {
			return err
		}
		entry := ledgerEntryFromWalletUpdate(wallet, update, model.LedgerEntryTypeOrderRelease, model.LedgerReferenceTypeOrder, order.ID, "")
		if err := ledgerRepo.Create(&entry); err != nil {
			return err
		}
	}
	if order.FilledAmount.Equal(order.Amount) {
		return orderRepo.UpdateOrderExecution(order.ID, order.FilledAmount, order.FilledQuoteAmount, model.OrderStatusFilled)
	}
	return orderRepo.UpdateOrderExecution(order.ID, order.FilledAmount, order.FilledQuoteAmount, model.OrderStatusCancelled)
}

func (s *OrderService) ListOrders(input ListOrdersInput) ([]model.Order, error) {
	if input.UserID == 0 {
		return nil, NewValidationErrorf("user_id is required")
	}
	if s == nil || s.OrderRepository == nil {
		return nil, fmt.Errorf("order repository is required")
	}

	var status *model.OrderStatus
	if strings.TrimSpace(input.Status) != "" {
		parsedStatus, err := parseOrderStatus(input.Status)
		if err != nil {
			return nil, err
		}
		status = &parsedStatus
	}

	return s.OrderRepository.ListByUserID(input.UserID, repository.OrderListFilter{
		Status:     status,
		CoinSymbol: normalizeCoinSymbol(input.CoinSymbol),
		Limit:      normalizeQueryLimit(input.Limit),
	})
}

func (s *OrderService) GetOrder(userID uint, orderID uint) (*model.Order, error) {
	if userID == 0 {
		return nil, NewValidationErrorf("user_id is required")
	}
	if orderID == 0 {
		return nil, NewValidationErrorf("order_id is required")
	}
	if s == nil || s.OrderRepository == nil {
		return nil, fmt.Errorf("order repository is required")
	}
	return s.OrderRepository.FindByUserIDAndID(userID, orderID)
}

func (s *OrderService) ListWallets(userID uint) ([]model.Wallet, error) {
	if userID == 0 {
		return nil, NewValidationErrorf("user_id is required")
	}
	if s == nil || s.WalletRepository == nil {
		return nil, fmt.Errorf("wallet repository is required")
	}
	return s.WalletRepository.ListByUserID(userID)
}

func (s *OrderService) ListTrades(input ListTradesInput) ([]repository.UserTrade, error) {
	if input.UserID == 0 {
		return nil, NewValidationErrorf("user_id is required")
	}
	if s == nil || s.TradeRepository == nil {
		return nil, fmt.Errorf("trade repository is required")
	}
	return s.TradeRepository.ListByUserID(input.UserID, repository.TradeListFilter{
		CoinSymbol: normalizeCoinSymbol(input.CoinSymbol),
		Limit:      normalizeQueryLimit(input.Limit),
	})
}

func BuildOrder(input CreateOrderInput) (*model.Order, error) {
	return BuildOrderWithRegistry(input, defaultMarketRulesRegistry)
}

func BuildOrderWithRegistry(input CreateOrderInput, marketRules *MarketRulesRegistry) (*model.Order, error) {
	if marketRules == nil {
		marketRules = defaultMarketRulesRegistry
	}

	if input.UserID == 0 {
		return nil, NewValidationErrorf("user_id is required")
	}

	coinSymbol := normalizeCoinSymbol(input.CoinSymbol)
	if coinSymbol == "" {
		return nil, NewValidationErrorf("coin_symbol is required")
	}

	side, err := parseOrderSide(input.Side)
	if err != nil {
		return nil, err
	}

	orderType, err := parseOrderType(input.OrderType)
	if err != nil {
		return nil, err
	}

	price := decimal.Zero
	amount := decimal.Zero
	quoteAmount := decimal.Zero

	switch orderType {
	case model.OrderTypeLimit:
		var err error
		price, err = parsePositiveDecimal(input.Price, "price")
		if err != nil {
			return nil, err
		}
		amount, err = parsePositiveDecimal(input.Amount, "amount")
		if err != nil {
			return nil, err
		}
		if err := marketRules.ValidateLimitOrder(coinSymbol, price, amount); err != nil {
			return nil, err
		}
	case model.OrderTypeMarket:
		var err error
		switch side {
		case model.OrderSideBuy:
			quoteAmount, err = parsePositiveDecimal(input.QuoteAmount, "quote_amount")
			if err != nil {
				return nil, err
			}
			if err := marketRules.ValidateMarketBuyOrder(coinSymbol, quoteAmount); err != nil {
				return nil, err
			}
		case model.OrderSideSell:
			amount, err = parsePositiveDecimal(input.Amount, "amount")
			if err != nil {
				return nil, err
			}
			if err := marketRules.ValidateMarketSellOrder(coinSymbol, amount); err != nil {
				return nil, err
			}
		}
	default:
		return nil, NewValidationErrorf("invalid order type")
	}

	return &model.Order{
		UserID:            input.UserID,
		CoinSymbol:        coinSymbol,
		Side:              side,
		OrderType:         orderType,
		Price:             price,
		Amount:            amount,
		QuoteAmount:       quoteAmount,
		Status:            model.OrderStatusPending,
		FilledAmount:      decimal.Zero,
		FilledQuoteAmount: decimal.Zero,
	}, nil
}

func (s *OrderService) marketRulesRegistry() *MarketRulesRegistry {
	if s != nil && s.MarketRules != nil {
		return s.MarketRules
	}
	return defaultMarketRulesRegistry
}

func holdOrderAssets(walletRepo *repository.WalletRepository, ledgerRepo *repository.LedgerRepository, order *model.Order) error {
	switch order.Side {
	case model.OrderSideBuy:
		wallet, err := walletRepo.FindKRWWalletByUserIDForUpdate(order.UserID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return NewConflictErrorf("insufficient available KRW balance")
			}
			return err
		}
		required := quoteAmountWithTradingFee(order.Price.Mul(order.Amount))
		if order.OrderType == model.OrderTypeMarket {
			required = order.QuoteAmount
		}
		update, err := applyBuyOrderHold(wallet, required)
		if err != nil {
			return err
		}
		if err := walletRepo.UpdateBalances(order.UserID, model.KRWAssetSymbol, update.AvailableBalance, update.LockedBalance); err != nil {
			return err
		}
		entry := ledgerEntryFromWalletUpdate(wallet, update, model.LedgerEntryTypeOrderHold, model.LedgerReferenceTypeOrder, order.ID, "")
		return ledgerRepo.Create(&entry)
	case model.OrderSideSell:
		wallet, err := walletRepo.FindByUserIDAndCoinSymbolForUpdate(order.UserID, order.CoinSymbol)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return NewConflictErrorf("insufficient available coin balance")
			}
			return err
		}
		update, err := applySellOrderHold(wallet, order.Amount)
		if err != nil {
			return err
		}
		if err := walletRepo.UpdateBalances(order.UserID, order.CoinSymbol, update.AvailableBalance, update.LockedBalance); err != nil {
			return err
		}
		entry := ledgerEntryFromWalletUpdate(wallet, update, model.LedgerEntryTypeOrderHold, model.LedgerReferenceTypeOrder, order.ID, "")
		return ledgerRepo.Create(&entry)
	default:
		return NewValidationErrorf("invalid order side")
	}
}

func releaseOrderHold(walletRepo *repository.WalletRepository, ledgerRepo *repository.LedgerRepository, order *model.Order, remaining decimal.Decimal) (string, decimal.Decimal, error) {
	switch order.Side {
	case model.OrderSideBuy:
		wallet, err := walletRepo.FindKRWWalletByUserIDForUpdate(order.UserID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return "", decimal.Zero, NewConflictErrorf("locked KRW wallet not found")
			}
			return "", decimal.Zero, err
		}
		releaseAmount := quoteAmountWithTradingFee(order.Price.Mul(remaining))
		update, err := releaseBuyOrderHold(wallet, releaseAmount)
		if err != nil {
			return "", decimal.Zero, err
		}
		if err := walletRepo.UpdateBalances(order.UserID, model.KRWAssetSymbol, update.AvailableBalance, update.LockedBalance); err != nil {
			return "", decimal.Zero, err
		}
		entry := ledgerEntryFromWalletUpdate(wallet, update, model.LedgerEntryTypeOrderRelease, model.LedgerReferenceTypeOrder, order.ID, "")
		return model.KRWAssetSymbol, releaseAmount, ledgerRepo.Create(&entry)
	case model.OrderSideSell:
		wallet, err := walletRepo.FindByUserIDAndCoinSymbolForUpdate(order.UserID, order.CoinSymbol)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return "", decimal.Zero, NewConflictErrorf("locked coin wallet not found")
			}
			return "", decimal.Zero, err
		}
		update, err := releaseSellOrderHold(wallet, remaining)
		if err != nil {
			return "", decimal.Zero, err
		}
		if err := walletRepo.UpdateBalances(order.UserID, order.CoinSymbol, update.AvailableBalance, update.LockedBalance); err != nil {
			return "", decimal.Zero, err
		}
		entry := ledgerEntryFromWalletUpdate(wallet, update, model.LedgerEntryTypeOrderRelease, model.LedgerReferenceTypeOrder, order.ID, "")
		return order.CoinSymbol, remaining, ledgerRepo.Create(&entry)
	default:
		return "", decimal.Zero, NewValidationErrorf("invalid order side")
	}
}

func matchingQuoteAmountForOrder(order *model.Order) decimal.Decimal {
	if order == nil {
		return decimal.Zero
	}
	if order.Side == model.OrderSideBuy && order.OrderType == model.OrderTypeMarket {
		return marketBuyExecutableQuoteAmount(order.QuoteAmount)
	}
	return order.QuoteAmount
}

func isCancellableOrderStatus(status model.OrderStatus) bool {
	return status == model.OrderStatusPending || status == model.OrderStatusPartial
}

func remainingOrderQuantity(order *model.Order) (decimal.Decimal, error) {
	if order == nil {
		return decimal.Zero, NewValidationErrorf("order is required")
	}
	remaining := order.Amount.Sub(order.FilledAmount)
	if !remaining.GreaterThan(decimal.Zero) {
		return decimal.Zero, NewConflictErrorf("order has no remaining quantity")
	}
	return remaining, nil
}

func remainingMarketSellQuantity(order *model.Order) (decimal.Decimal, error) {
	if order == nil {
		return decimal.Zero, NewValidationErrorf("order is required")
	}
	remaining := order.Amount.Sub(order.FilledAmount)
	if remaining.IsNegative() {
		return decimal.Zero, NewConflictErrorf("order filled amount exceeds order amount")
	}
	return remaining, nil
}

func parseOrderSide(value string) (model.OrderSide, error) {
	switch model.OrderSide(strings.ToUpper(strings.TrimSpace(value))) {
	case model.OrderSideBuy:
		return model.OrderSideBuy, nil
	case model.OrderSideSell:
		return model.OrderSideSell, nil
	default:
		return "", NewValidationErrorf("invalid order side")
	}
}

func parseOrderType(value string) (model.OrderType, error) {
	normalized := strings.ToUpper(strings.TrimSpace(value))
	if normalized == "" {
		return model.OrderTypeLimit, nil
	}

	switch model.OrderType(normalized) {
	case model.OrderTypeLimit:
		return model.OrderTypeLimit, nil
	case model.OrderTypeMarket:
		return model.OrderTypeMarket, nil
	default:
		return "", NewValidationErrorf("invalid order type")
	}
}

func parseOrderStatus(value string) (model.OrderStatus, error) {
	switch model.OrderStatus(strings.ToUpper(strings.TrimSpace(value))) {
	case model.OrderStatusPending:
		return model.OrderStatusPending, nil
	case model.OrderStatusPartial:
		return model.OrderStatusPartial, nil
	case model.OrderStatusFilled:
		return model.OrderStatusFilled, nil
	case model.OrderStatusCancelled:
		return model.OrderStatusCancelled, nil
	default:
		return "", NewValidationErrorf("invalid order status")
	}
}

func normalizeCoinSymbol(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}

func normalizeQueryLimit(limit int) int {
	if limit <= 0 {
		return DefaultQueryLimit
	}
	if limit > MaxQueryLimit {
		return MaxQueryLimit
	}
	return limit
}

func parsePositiveDecimal(value string, field string) (decimal.Decimal, error) {
	parsed, err := decimal.NewFromString(strings.TrimSpace(value))
	if err != nil {
		return decimal.Zero, NewValidationErrorf("invalid %s", field)
	}
	if !parsed.GreaterThan(decimal.Zero) {
		return decimal.Zero, NewValidationErrorf("%s must be greater than zero", field)
	}
	return parsed, nil
}
