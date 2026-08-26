package service

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
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

	// OrderIdempotencyRepository는 주문 생성 재시도를 식별한다. nil이면 CreateOrder가
	// 키를 선점할 수 없다 — NewOrderService가 DB와 함께 채운다.
	OrderIdempotencyRepository *repository.OrderIdempotencyRepository

	CancelCommandRepository *repository.CancelCommandRepository
	// CancelCommandWake는 command 커밋 직후 worker를 깨운다. nil이어도 command는
	// 이미 내구 기록됐으므로 worker의 polling이 복구한다.
	CancelCommandWake func()
}

const defaultAcceptanceTimeout = 100 * time.Millisecond

const (
	DefaultQueryLimit = 50
	MaxQueryLimit     = 200
)

type CreateOrderInput struct {
	UserID         uint
	CoinSymbol     string
	Side           string
	OrderType      string
	Price          string
	Amount         string
	QuoteAmount    string
	IdempotencyKey string
}

// CreateOrderResult는 "무엇이 일어났는지"까지 담는다. Replay는 이 요청이 새 주문을
// 만들지 않았다는 뜻이고, Outcome은 서버가 durable하게 아는 마지막 상태다.
type CreateOrderResult struct {
	Order   *model.Order
	Replay  bool
	Outcome model.OrderIdempotencyOutcome
}

const maxIdempotencyKeyLength = 128

// 계약은 "공백 제외 1~128자"다. len()은 바이트를 세므로 멀티바이트 키에서 DB CHECK의
// length()(문자 수)와 단위가 어긋난다. 두 곳이 같은 단위를 쓰도록 rune으로 센다.
func normalizeIdempotencyKey(raw string) (string, error) {
	key := strings.TrimSpace(raw)
	if key == "" || utf8.RuneCountInString(key) > maxIdempotencyKeyLength {
		return "", NewValidationErrorf(
			"idempotency_key is required and must be 1..%d characters", maxIdempotencyKeyLength)
	}
	return key, nil
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

// CancelOrderAcceptedStatus는 "취소 의도가 내구적으로 저장됐다"는 뜻이지
// "오더북에서 이미 제거됐다"가 아니다. 그 사이에는 추가 체결 가능 창이 있다.
const CancelOrderAcceptedStatus = "ACCEPTED"

type CancelOrderResult struct {
	OrderID   uint
	CommandID uint64
	Status    string
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
		service.CancelCommandRepository = repository.NewCancelCommandRepository(repo.DB)
		service.OrderIdempotencyRepository = repository.NewOrderIdempotencyRepository(repo.DB)
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

func (s *OrderService) CreateOrder(input CreateOrderInput) (*CreateOrderResult, error) {
	key, err := normalizeIdempotencyKey(input.IdempotencyKey)
	if err != nil {
		return nil, err
	}

	order, err := s.BuildOrder(input)
	if err != nil {
		return nil, err
	}

	fingerprint, err := ComputeOrderFingerprint(OrderFingerprintInput{
		UserID:      order.UserID,
		CoinSymbol:  order.CoinSymbol,
		Side:        string(order.Side),
		OrderType:   string(order.OrderType),
		Price:       order.Price,
		Amount:      order.Amount,
		QuoteAmount: order.QuoteAmount,
	}, CurrentOrderFingerprintVersion)
	if err != nil {
		return nil, err
	}
	idem := &idempotencyContext{
		Key: key, Fingerprint: fingerprint, Version: CurrentOrderFingerprintVersion,
	}

	// 입장 게이트: 엔진 유입이 포화면 DB 작업 전에 빠른 거절(503).
	if s.MatchingEngine != nil && !s.MatchingEngine.IsIntakeAdmissible(order.CoinSymbol) {
		metrics.OrdersAdmissionRejectedTotal.WithLabelValues("engine_gate").Inc()
		return nil, NewUnavailableErrorf("order intake is saturated, please retry shortly")
	}

	res, err := s.holdWithIdempotency(order, idem)
	if err != nil {
		return nil, err
	}

	// follower는 엔진에 제출하지 않는다. 제출하면 hold는 한 번인데 주문이 두 번 들어간다.
	if res.Role == holdRoleFollower {
		return s.followerResult(res, fingerprint)
	}
	order = res.Order

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
			metrics.OrdersAdmissionRejectedTotal.WithLabelValues("engine_handoff").Inc()
			return nil, s.rejectAcceptedOrderWithIdempotency(order, idem.RecordID)
		}
		// 엔진 접수 성공 — ACCEPTED로 전이한다. 실패하면 PENDING에 머문다(재요청은 202).
		if err := s.OrderIdempotencyRepository.UpdateOutcome(
			idem.RecordID, model.OrderIdempotencyOutcomeAccepted,
		); err != nil {
			metrics.OrderIdempotencyOutcomeUpdateFailuresTotal.Inc()
			log.Printf("order idempotency: ACCEPTED update failed for record %d: %v", idem.RecordID, err)
		}
	}

	return &CreateOrderResult{Order: order, Outcome: model.OrderIdempotencyOutcomeAccepted}, nil
}

// holdWithIdempotency는 코디네이터가 있으면 배치로, 없으면(테스트·미배선) 같은 순서를
// 지키는 단건 경로로 hold한다. 어느 쪽이든 키를 먼저 선점한다.
func (s *OrderService) holdWithIdempotency(order *model.Order, idem *idempotencyContext) (holdResult, error) {
	if s.HoldCoordinator != nil {
		return s.HoldCoordinator.SubmitWithIdempotency(order, idem)
	}
	res := persistAndHoldIdempotent(
		s.OrderRepository.DB, s.OrderRepository, s.WalletRepository, s.LedgerRepository,
		s.OrderIdempotencyRepository, holdRequest{order: order, idem: idem},
	)
	return res, res.Err
}

// followerResult는 follower 두 종류를 가른다.
//
//   - Existing != nil: 이전 요청이 이미 커밋한 키다. 저장된 레코드로 replay한다.
//   - Existing == nil && Order != nil: 같은 배치의 중복이다. leader가 이번에 주문을
//     만들었으므로 별도로 조회해 온 레코드가 없다. 엔진 제출 없이 202 PENDING이다.
//     여기서 replayResult를 부르면 정상적인 중복 요청이 503이 된다.
//   - 그 외: 결과를 만들지 못한 것이므로 503이다.
func (s *OrderService) followerResult(res holdResult, fingerprint string) (*CreateOrderResult, error) {
	if res.Existing != nil {
		return s.replayResult(res.Existing, fingerprint)
	}
	if res.Order != nil {
		return &CreateOrderResult{
			Order:   res.Order,
			Replay:  true,
			Outcome: model.OrderIdempotencyOutcomePending,
		}, nil
	}
	return nil, NewUnavailableErrorf("idempotency record is unavailable, please retry")
}

// replayResult는 저장된 결과로 응답을 재구성한다. 지문이 다르면 409다.
func (s *OrderService) replayResult(record *model.OrderIdempotencyKey, fingerprint string) (*CreateOrderResult, error) {
	if record == nil {
		return nil, NewUnavailableErrorf("idempotency record is unavailable, please retry")
	}
	if record.Fingerprint != fingerprint {
		return nil, NewConflictErrorf("idempotency key was used with a different request")
	}

	order := &model.Order{}
	if record.OrderID != nil {
		order.ID = *record.OrderID
	}
	return &CreateOrderResult{Order: order, Replay: true, Outcome: record.Outcome}, nil
}

// rejectAcceptedOrderWithIdempotency는 hold 해제·주문 REJECTED·outcome REJECTED를
// 한 트랜잭션에 넣는다. outcome을 밖에 두면 "hold는 풀렸는데 outcome은 PENDING"인
// 상태가 생기고, 재요청이 202를 받아 아직 진행 중인 것처럼 보인다.
func (s *OrderService) rejectAcceptedOrderWithIdempotency(order *model.Order, recordID uint64) error {
	err := s.OrderRepository.DB.Transaction(func(tx *gorm.DB) error {
		orderRepo := s.OrderRepository.WithTx(tx)
		walletRepo := s.WalletRepository.WithTx(tx)
		ledgerRepo := s.LedgerRepository.WithTx(tx)

		if err := releaseInitialHold(walletRepo, ledgerRepo, order); err != nil {
			return err
		}
		if err := orderRepo.UpdateOrderExecution(
			order.ID, order.FilledAmount, order.FilledQuoteAmount, model.OrderStatusRejected,
		); err != nil {
			return err
		}
		return s.OrderIdempotencyRepository.WithTx(tx).UpdateOutcome(
			recordID, model.OrderIdempotencyOutcomeRejected)
	})
	if err == nil {
		return NewUnavailableErrorf("order intake is saturated, please retry shortly")
	}

	// 보상 실패 — hold가 잡힌 채 남는다. UNKNOWN은 best-effort 기록이다.
	if uerr := s.OrderIdempotencyRepository.UpdateOutcome(
		recordID, model.OrderIdempotencyOutcomeUnknown,
	); uerr != nil {
		metrics.OrderIdempotencyOutcomeUpdateFailuresTotal.Inc()
		log.Printf("order idempotency: UNKNOWN update failed for record %d: %v", recordID, uerr)
	} else {
		metrics.OrderIdempotencyUnknownTotal.Inc()
	}

	// 요청자 잘못이 아니다. raw error를 그대로 내면 serviceErrorStatus의 default가
	// 400으로 매핑한다(CancelOrder에서 e0ef22a로 고친 것과 같은 클래스).
	return NewUnavailableErrorf(
		"order intake saturated and hold release failed for order %d, retry is safe with the same key", order.ID)
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

// CancelOrder는 소유권·취소가능 상태·시장가 여부를 검증한 뒤, 취소 의도를
// cancel_commands에 내구 기록하는 것으로 끝난다. 매칭 엔진은 여기서 호출하지
// 않는다 — 엔진 호출은 내구 기록보다 앞설 수 없기 때문이다.
//
// 이후 CancelCommandWorker가 그 command를 엔진에 전달하고, 엔진이 방출한
// OrderCancelled 이벤트를 정산 파이프라인이 ProcessOrderCancellation으로 소비할 때
// 비로소 hold 해제·CANCELLED 커밋이 일어난다. 즉 이 함수가 반환하는 시점에는
// 주문이 아직 오더북에 있고 DB도 PENDING/PARTIAL이다 — 응답은 "확정"이 아니라
// "접수"이고, 그 사이에는 추가 체결 가능 창이 있다.
func (s *OrderService) CancelOrder(input CancelOrderInput) (*CancelOrderResult, error) {
	if input.UserID == 0 {
		return nil, NewValidationErrorf("user_id is required")
	}
	if input.OrderID == 0 {
		return nil, NewValidationErrorf("order_id is required")
	}

	if s.CancelCommandRepository == nil {
		return nil, NewUnavailableErrorf("cancel command repository is not configured")
	}

	var command *model.CancelCommand
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

		// 엔진에 넣을 명령을 그대로 복원할 수 있어야 한다 — 특히 Price가 없으면
		// worker가 가격 레벨을 찾지 못한다.
		candidate := &model.CancelCommand{
			OrderID:    order.ID,
			UserID:     order.UserID,
			CoinSymbol: order.CoinSymbol,
			Side:       order.Side,
			Price:      order.Price,
			Status:     model.CancelCommandStatusPending,
		}
		persisted, _, err := s.CancelCommandRepository.WithTx(tx).CreateOrGet(candidate)
		if err != nil {
			return err
		}
		command = persisted
		return nil
	}); err != nil {
		// 검증 실패(소유권·상태·시장가)와 주문 없음은 그대로 4xx다. 그 외는 DB가
		// 취소 의도를 받아주지 못한 인프라 실패이므로 503으로 알린다 — 400으로
		// 떨어지면 클라이언트가 "요청이 잘못됐다"로 읽고 재시도하지 않는다.
		// command는 order_id 단위로 멱등하므로 재시도는 안전하다.
		if _, ok := DomainErrorKind(err); ok {
			return nil, err
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
		return nil, NewUnavailableErrorf("cancel command could not be recorded, please retry: %v", err)
	}

	// 커밋된 뒤에만 깨운다. 먼저 깨우면 worker가 아직 보이지 않는 행을 찾으러 간다.
	if s.CancelCommandWake != nil {
		s.CancelCommandWake()
	}

	return &CancelOrderResult{
		OrderID:   command.OrderID,
		CommandID: command.ID,
		Status:    CancelOrderAcceptedStatus,
	}, nil
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
