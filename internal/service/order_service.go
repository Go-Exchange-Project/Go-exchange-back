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
	OrderRepository   *repository.OrderRepository
	WalletRepository  *repository.WalletRepository
	MatchingEngine    matching.Engine
	TradeRepository   *repository.TradeRepository
	LedgerRepository  *repository.LedgerRepository
	MarketRules       *MarketRulesRegistry
	AcceptanceTimeout time.Duration    // 0이면 defaultAcceptanceTimeout
	HoldCoordinator   *HoldCoordinator // nil이면 persistAndHold 직접 호출(기존 테스트 경로)
}

const defaultAcceptanceTimeout = 100 * time.Millisecond

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

func NewOrderService(repo *repository.OrderRepository, walletRepo *repository.WalletRepository, me matching.Engine) *OrderService {
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

// persistAndHold는 주문 1건을 한 트랜잭션에 영속화하고 자금을 홀드한다.
// no-coordinator 경로와 배치 실패 폴백이 공유하는 단건 경로 — 정합성의 진실.
func persistAndHold(db *gorm.DB, orderRepo *repository.OrderRepository, walletRepo *repository.WalletRepository, ledgerRepo *repository.LedgerRepository, order *model.Order) error {
	return db.Transaction(func(tx *gorm.DB) error {
		or := orderRepo.WithTx(tx)
		wr := walletRepo.WithTx(tx)
		lr := ledgerRepo.WithTx(tx)
		if err := or.CreateOrder(order); err != nil {
			return err
		}
		return holdOrderAssets(wr, lr, order)
	})
}

func (s *OrderService) CreateOrder(input CreateOrderInput) (*model.Order, error) {
	order, err := s.BuildOrder(input)
	if err != nil {
		return nil, err
	}

	// 입장 게이트: 엔진 유입이 포화면 DB 작업 전에 빠른 거절(503).
	if s.MatchingEngine != nil && !s.MatchingEngine.IsIntakeAdmissible(order.CoinSymbol) {
		return nil, NewUnavailableErrorf("order intake is saturated, please retry shortly")
	}

	// [②] 코디네이터 있으면 배치 경유, 없으면(테스트·미배선) 단건 직접.
	if s.HoldCoordinator != nil {
		held, err := s.HoldCoordinator.Submit(order)
		if err != nil {
			return nil, err
		}
		order = held
	} else {
		if err := persistAndHold(s.OrderRepository.DB, s.OrderRepository, s.WalletRepository, s.LedgerRepository, order); err != nil {
			return nil, err
		}
	}

	// 바운디드 핸드오프: 매칭 처리량에 응답이 매달리지 않게. 주문은 이미
	// 영속화+홀드로 내구·정합 확정 상태다. 바운드 내 접수 못 하면(레이스로 포화)
	// 보상으로 홀드를 풀고 REJECTED로 종결한 뒤 503.
	if s.MatchingEngine != nil {
		submitted := s.MatchingEngine.TrySubmitOrder(&matching.Order{
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
		}, s.acceptanceTimeout())
		if !submitted {
			if rerr := s.rejectAcceptedOrder(order); rerr != nil {
				return nil, fmt.Errorf("order intake saturated and hold release failed for order %d: %w", order.ID, rerr)
			}
			return nil, NewUnavailableErrorf("order intake is saturated, please retry shortly")
		}
	}

	return order, nil
}

func (s *OrderService) acceptanceTimeout() time.Duration {
	if s.AcceptanceTimeout > 0 {
		return s.AcceptanceTimeout
	}
	return defaultAcceptanceTimeout
}

// rejectAcceptedOrder는 영속화·홀드됐으나 엔진 접수에 실패한 주문을 원상복구한다:
// 초기 홀드를 전액 해제하고 상태를 REJECTED로 종결한다(한 트랜잭션, 원장 기록 포함).
func (s *OrderService) rejectAcceptedOrder(order *model.Order) error {
	return s.OrderRepository.DB.Transaction(func(tx *gorm.DB) error {
		orderRepo := s.OrderRepository.WithTx(tx)
		walletRepo := s.WalletRepository.WithTx(tx)
		ledgerRepo := s.LedgerRepository.WithTx(tx)
		if err := releaseInitialHold(walletRepo, ledgerRepo, order); err != nil {
			return err
		}
		return orderRepo.UpdateOrderExecution(order.ID, order.FilledAmount, order.FilledQuoteAmount, model.OrderStatusRejected)
	})
}

// releaseInitialHold는 holdOrderAssets가 건 초기 홀드의 정확한 역이다(미체결 주문
// 이므로 홀드 전액). 매수=예약 KRW, 매도=예약 코인 수량.
func releaseInitialHold(walletRepo *repository.WalletRepository, ledgerRepo *repository.LedgerRepository, order *model.Order) error {
	switch order.Side {
	case model.OrderSideBuy:
		wallet, err := walletRepo.FindKRWWalletByUserIDForUpdate(order.UserID)
		if err != nil {
			return err
		}
		releaseAmount := quoteAmountWithTradingFee(order.Price.Mul(order.Amount))
		if order.OrderType == model.OrderTypeMarket {
			releaseAmount = order.QuoteAmount
		}
		update, err := releaseBuyOrderHold(wallet, releaseAmount)
		if err != nil {
			return err
		}
		if err := walletRepo.UpdateBalances(order.UserID, model.KRWAssetSymbol, update.AvailableBalance, update.LockedBalance); err != nil {
			return err
		}
		entry := ledgerEntryFromWalletUpdate(wallet, update, model.LedgerEntryTypeOrderRelease, model.LedgerReferenceTypeOrder, order.ID, "")
		return ledgerRepo.Create(&entry)
	case model.OrderSideSell:
		wallet, err := walletRepo.FindByUserIDAndCoinSymbolForUpdate(order.UserID, order.CoinSymbol)
		if err != nil {
			return err
		}
		update, err := releaseSellOrderHold(wallet, order.Amount)
		if err != nil {
			return err
		}
		if err := walletRepo.UpdateBalances(order.UserID, order.CoinSymbol, update.AvailableBalance, update.LockedBalance); err != nil {
			return err
		}
		entry := ledgerEntryFromWalletUpdate(wallet, update, model.LedgerEntryTypeOrderRelease, model.LedgerReferenceTypeOrder, order.ID, "")
		return ledgerRepo.Create(&entry)
	default:
		return NewValidationErrorf("invalid order side")
	}
}

func (s *OrderService) BuildOrder(input CreateOrderInput) (*model.Order, error) {
	return BuildOrderWithRegistry(input, s.marketRulesRegistry())
}

// CancelOrder는 DB 상태를 직접 확정하지 않는다(A-4 취소-체결 레이스 수정). 이
// 함수는 소유권·취소가능 상태·시장가 여부만 검증하고, 실제 취소는 매칭 엔진에
// 커맨드로 접수한다. 엔진이 Removed=true를 반환하면(=오더북에서 실제 제거)
// ExecutionCh에 OrderCancelled 이벤트가 방출되고, 같은 주문의 선행 체결들
// 뒤에 FIFO로 정렬된 그 이벤트를 정산 파이프라인이 ProcessOrderCancellation으로
// 소비할 때 비로소 hold 해제·CANCELLED 커밋이 일어난다. 즉 이 함수가 반환하는
// 시점에는 DB가 아직 PENDING/PARTIAL일 수 있다 — 응답은 "확정"이 아니라 "접수"다.
func (s *OrderService) CancelOrder(input CancelOrderInput) (*CancelOrderResult, error) {
	if input.UserID == 0 {
		return nil, NewValidationErrorf("user_id is required")
	}
	if input.OrderID == 0 {
		return nil, NewValidationErrorf("order_id is required")
	}

	var cancelCommand matching.CancelOrderCommand
	var estimatedAsset string
	var estimatedAmount decimal.Decimal
	if err := s.OrderRepository.DB.Transaction(func(tx *gorm.DB) error {
		orderRepo := s.OrderRepository.WithTx(tx)

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

		// 검증 시점 스냅샷 기준 추정치다(판단 근거는 estimateCancelRelease 참고) —
		// 실제 해제량은 ProcessOrderCancellation이 비동기로 확정한다.
		estimatedAsset, estimatedAmount = estimateCancelRelease(order, remaining)
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

	if s.MatchingEngine == nil {
		return nil, matching.ErrCancelOrderEngineUnavailable
	}

	cancelResult := s.MatchingEngine.CancelOrder(cancelCommand)
	if cancelResult.Err != nil {
		// 오더북에 없음(이미 체결/소진)은 정상적인 "이미 늦음" 케이스다 — 409로
		// 응답한다. 그 외(커맨드 오류·엔진 다운·타임아웃)는 드물어야 하는
		// 인프라/버그성 실패라 그대로 감싸 상위(핸들러)가 500으로 매핑한다.
		if errors.Is(cancelResult.Err, matching.ErrCancelOrderNotFound) {
			return nil, NewConflictErrorf("order %d already filled or not found in matching engine", cancelCommand.OrderID)
		}
		return nil, fmt.Errorf("matching engine cancel failed: %w", cancelResult.Err)
	}

	return &CancelOrderResult{
		OrderID: cancelCommand.OrderID,
		// Status는 더 이상 "이 호출이 DB에 커밋한 최종 상태"가 아니라 "취소가
		// 엔진에 접수됐다"는 의미로 재정의된다(설계 결정 4). 실제 CANCELLED 커밋은
		// ProcessOrderCancellation이 비동기로 수행한다.
		Status:         model.OrderStatusCancelled,
		ReleasedAsset:  estimatedAsset,
		ReleasedAmount: estimatedAmount,
		EngineRemoved:  cancelResult.Removed,
	}, nil
}

// estimateCancelRelease는 CancelOrderResult.ReleasedAsset/ReleasedAmount를 검증
// 시점(트랜잭션 내 FOR UPDATE 스냅샷) 기준으로 추정한다. releaseOrderHold와 같은
// 계산식을 쓰지만 지갑을 실제로 건드리지 않는다 — 실제 hold 해제는 이제
// ProcessOrderCancellation이 엔진의 OrderCancelled 이벤트를 소비할 때 비동기로
// 수행하기 때문이다.
//
// 판단: 빈 값(zero-value)으로 남기는 대신 추정치를 채워 넣기로 했다 — 응답 시점과
// 실제 해제 시점 사이에 선행 체결이 끼어들면(레이스가 닫혔으므로 파이프라인은
// 정확하지만) 이 추정치가 실제보다 클 수 있다는 점을 명확히 문서화하는 편이,
// API 소비자에게 아무 정보도 안 주는 것보다 유용하다고 판단했다.
func estimateCancelRelease(order *model.Order, remaining decimal.Decimal) (string, decimal.Decimal) {
	switch order.Side {
	case model.OrderSideBuy:
		return model.KRWAssetSymbol, quoteAmountWithTradingFee(order.Price.Mul(remaining))
	case model.OrderSideSell:
		return order.CoinSymbol, remaining
	default:
		return "", decimal.Zero
	}
}

// ProcessOrderCancellation은 엔진이 방출한 OrderCancelled 실행 이벤트를 정산
// 파이프라인에서 확정한다. 심볼 FIFO 순서상 이 이벤트는 같은 주문의 선행 체결들
// 뒤에 처리되므로, 이 시점의 order.FilledAmount는 이미 모든 선행 체결이 정산된
// 최신값이다 — "잔여 = Amount - FilledAmount"가 항상 정확하다(A-4 레이스 수정의 핵심).
// 멱등: 이미 CANCELLED/FILLED면 no-op. releaseOrderHold·CANCELLED 커밋은 CancelOrder가
// 하던 것과 동일한 로직이지만, 여기서는 엔진이 실제로 오더북에서 제거한 뒤에만
// 호출되므로 CancelOrder 자신은(1E에서) 더 이상 hold 해제를 직접 하지 않게 된다.
func (s *OrderService) ProcessOrderCancellation(event matching.OrderCancelled) error {
	if event.OrderID == 0 {
		return NewValidationErrorf("order_id is required")
	}

	return s.OrderRepository.DB.Transaction(func(tx *gorm.DB) error {
		orderRepo := s.OrderRepository.WithTx(tx)
		walletRepo := s.WalletRepository.WithTx(tx)
		ledgerRepo := s.LedgerRepository.WithTx(tx)

		order, err := orderRepo.FindByIDForUpdate(event.OrderID)
		if err != nil {
			return err
		}
		if !isCancellableOrderStatus(order.Status) {
			return nil
		}

		// remainingOrderQuantity를 재사용하지 않는다: 그 헬퍼는 잔여 <= 0을 에러로
		// 취급하지만(CancelOrder API의 즉시 검증용), 여기서는 Removed=true를 방출한
		// 시점엔 오더북에 잔여분이 있었으므로 정상 경로에서 이 분기는 발생하지 않는다
		// (설계 문서 결정 5). 그래도 도달하면 이미 사실상 체결 완료된 상태이므로
		// 에러 없이 스킵한다 — 상태는 뒤따르는(또는 이미 끝난) 체결 정산이 FILLED로
		// 정리한다.
		remaining := order.Amount.Sub(order.FilledAmount)
		if !remaining.GreaterThan(decimal.Zero) {
			return nil
		}

		if _, _, err := releaseOrderHold(walletRepo, ledgerRepo, order, remaining); err != nil {
			return err
		}

		return orderRepo.UpdateOrderExecution(order.ID, order.FilledAmount, order.FilledQuoteAmount, model.OrderStatusCancelled)
	})
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
