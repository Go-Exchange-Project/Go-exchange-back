package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/auth"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/httpapi"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

type OrderHandler struct {
	OrderService *service.OrderService
}

type CreateOrderRequest struct {
	CoinSymbol  string `json:"coin_symbol" binding:"required"`
	Side        string `json:"side" binding:"required"`
	OrderType   string `json:"order_type"`
	Price       string `json:"price"`
	Amount      string `json:"amount"`
	QuoteAmount string `json:"quote_amount"`
}

func NewOrderHandler(service *service.OrderService) *OrderHandler {
	return &OrderHandler{OrderService: service}
}

func (h *OrderHandler) CreateOrder(c *gin.Context) {
	userID, ok := authenticatedUserID(c)
	if !ok {
		httpapi.WriteError(c, http.StatusUnauthorized, httpapi.CodeAuthRequired, "authenticated user is required")
		return
	}

	var req CreateOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeBindingError(c, err)
		return
	}

	// 키 누락은 400이다. 서비스는 키 형식 오류를 ErrorKindValidation으로 내는데
	// serviceErrorStatus가 그것을 422로 매핑하므로, 여기서 먼저 본다.
	if strings.TrimSpace(c.GetHeader("Idempotency-Key")) == "" {
		httpapi.WriteError(c, http.StatusBadRequest, httpapi.CodeBadRequest,
			"Idempotency-Key header is required")
		return
	}

	result, err := h.OrderService.CreateOrder(service.CreateOrderInput{
		UserID:         userID,
		CoinSymbol:     req.CoinSymbol,
		Side:           req.Side,
		OrderType:      req.OrderType,
		Price:          req.Price,
		Amount:         req.Amount,
		QuoteAmount:    req.QuoteAmount,
		IdempotencyKey: c.GetHeader("Idempotency-Key"),
	})
	if err != nil {
		// 실패해도 주문 행이 이미 있으면 order_id와 durable outcome을 함께 준다.
		// 저장된 결과의 재요청만 order_id를 받고 최초 실패는 못 받으면, 같은 상태가
		// 두 가지 응답으로 보인다.
		if result != nil && result.Order != nil {
			status := serviceErrorStatus(err)
			httpapi.WriteErrorWithData(c, status, errorCodeForStatus(status),
				idempotencyFailureMessage(result.Outcome),
				gin.H{"order_id": result.Order.ID, "status": string(result.Outcome)})
			return
		}
		writeServiceError(c, err)
		return
	}

	// 네 outcome을 모두 분기한다. PENDING만 특별 처리하고 나머지를 200으로 흘리면
	// REJECTED·UNKNOWN 재요청이 "order accepted"로 표시된다 — 접수되지 않은 주문을
	// 접수됐다고 말하는 것이다.
	switch result.Outcome {
	case model.OrderIdempotencyOutcomeAccepted:
		body := gin.H{"message": "order accepted", "order_id": result.Order.ID}
		if result.Replay {
			body["idempotent_replay"] = true
		}
		httpapi.WriteData(c, http.StatusOK, body)

	// PENDING은 "주문은 있는데 그 뒤를 durable하게 알지 못한다"이다. 200은 "접수됐다"는
	// 거짓이 되고 503은 "없다"는 거짓이 되므로 202를 쓴다.
	case model.OrderIdempotencyOutcomePending:
		httpapi.WriteData(c, http.StatusAccepted, gin.H{
			"order_id":          result.Order.ID,
			"status":            string(result.Outcome),
			"idempotent_replay": result.Replay,
		})

	// REJECTED는 "접수되지 않았고 되돌렸다", UNKNOWN은 "되돌리다 실패했다"이다. 둘 다
	// 성공이 아니므로 503이되, order_id는 준다 — 클라이언트가 상태를 조회할 수 있어야 한다.
	case model.OrderIdempotencyOutcomeRejected, model.OrderIdempotencyOutcomeUnknown:
		httpapi.WriteErrorWithData(c, http.StatusServiceUnavailable, httpapi.CodeUnavailable,
			idempotencyFailureMessage(result.Outcome),
			gin.H{"order_id": result.Order.ID, "status": string(result.Outcome)})

	default:
		httpapi.WriteError(c, http.StatusInternalServerError, httpapi.CodeInternal,
			"unknown idempotency outcome")
	}
}

// idempotencyFailureMessage는 실패 응답의 안내를 outcome별로 나눈다. 최초 실패와
// 저장된 결과의 재요청이 같은 문장을 쓰도록 한 곳에서 정한다.
//
// "같은 키로 재시도하면 된다"고 뭉뚱그리면 안 된다. 키는 이미 이 결과에 묶여 있어
// 재시도해도 같은 응답이 돌아온다 — 재시도가 상태를 바꿔 줄 것처럼 안내하면
// 클라이언트는 해결되지 않는 루프를 돈다.
func idempotencyFailureMessage(outcome model.OrderIdempotencyOutcome) string {
	switch outcome {
	case model.OrderIdempotencyOutcomeRejected:
		// 보상이 끝나 hold도 풀렸다. 새 주문을 내려면 새 키가 필요하다.
		return "order was not accepted and the hold was released; use a new idempotency key to place a new order"
	default:
		// UNKNOWN·PENDING: 되돌리기가 끝나지 않아 자금이 묶여 있을 수 있다.
		// 자동으로 해결되지 않으므로 주문 상태를 먼저 확인해야 한다.
		return "order state could not be finalized; check the order status before acting — retrying with the same key returns this same state"
	}
}

func (h *OrderHandler) ListOrders(c *gin.Context) {
	userID, ok := authenticatedUserID(c)
	if !ok {
		httpapi.WriteError(c, http.StatusUnauthorized, httpapi.CodeAuthRequired, "authenticated user is required")
		return
	}

	orders, err := h.OrderService.ListOrders(service.ListOrdersInput{
		UserID:     userID,
		Status:     c.Query("status"),
		CoinSymbol: c.Query("coin_symbol"),
		Limit:      parseLimitQuery(c),
	})
	if err != nil {
		writeServiceError(c, err)
		return
	}

	response := make([]OrderResponse, 0, len(orders))
	for _, order := range orders {
		response = append(response, orderResponse(order))
	}
	httpapi.WriteData(c, http.StatusOK, gin.H{"orders": response})
}

func (h *OrderHandler) GetOrder(c *gin.Context) {
	userID, ok := authenticatedUserID(c)
	if !ok {
		httpapi.WriteError(c, http.StatusUnauthorized, httpapi.CodeAuthRequired, "authenticated user is required")
		return
	}

	orderID, err := parseIDParam(c.Param("id"))
	if err != nil {
		httpapi.WriteError(c, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid order id")
		return
	}

	order, err := h.OrderService.GetOrder(userID, orderID)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	httpapi.WriteData(c, http.StatusOK, gin.H{"order": orderResponse(*order)})
}

func (h *OrderHandler) ListWallets(c *gin.Context) {
	userID, ok := authenticatedUserID(c)
	if !ok {
		httpapi.WriteError(c, http.StatusUnauthorized, httpapi.CodeAuthRequired, "authenticated user is required")
		return
	}

	wallets, err := h.OrderService.ListWallets(userID)
	if err != nil {
		writeServiceError(c, err)
		return
	}

	response := make([]WalletResponse, 0, len(wallets))
	for _, wallet := range wallets {
		response = append(response, walletResponse(wallet))
	}
	httpapi.WriteData(c, http.StatusOK, gin.H{"wallets": response})
}

func (h *OrderHandler) ListTrades(c *gin.Context) {
	userID, ok := authenticatedUserID(c)
	if !ok {
		httpapi.WriteError(c, http.StatusUnauthorized, httpapi.CodeAuthRequired, "authenticated user is required")
		return
	}

	trades, err := h.OrderService.ListTrades(service.ListTradesInput{
		UserID:     userID,
		CoinSymbol: c.Query("coin_symbol"),
		Limit:      parseLimitQuery(c),
	})
	if err != nil {
		writeServiceError(c, err)
		return
	}

	response := make([]TradeResponse, 0, len(trades))
	for _, trade := range trades {
		response = append(response, tradeResponse(trade))
	}
	httpapi.WriteData(c, http.StatusOK, gin.H{"trades": response})
}

func (h *OrderHandler) CancelOrder(c *gin.Context) {
	userID, ok := authenticatedUserID(c)
	if !ok {
		httpapi.WriteError(c, http.StatusUnauthorized, httpapi.CodeAuthRequired, "authenticated user is required")
		return
	}

	orderID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || orderID == 0 {
		httpapi.WriteError(c, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid order id")
		return
	}

	result, err := h.OrderService.CancelOrder(service.CancelOrderInput{
		UserID:  userID,
		OrderID: uint(orderID),
	})
	if err != nil {
		status := serviceErrorStatus(err)
		httpapi.WriteError(c, status, errorCodeForStatus(status), err.Error())
		return
	}

	// 202는 "취소 의도가 내구적으로 저장됐다"이지 "오더북에서 이미 제거됐다"가
	// 아니다. 최종 상태는 주문 조회로 확인해야 한다.
	httpapi.WriteData(c, http.StatusAccepted, gin.H{
		"message":    "cancellation accepted",
		"order_id":   result.OrderID,
		"command_id": result.CommandID,
		"status":     result.Status,
	})
}

type OrderResponse struct {
	ID                uint              `json:"id"`
	CoinSymbol        string            `json:"coin_symbol"`
	Side              model.OrderSide   `json:"side"`
	OrderType         model.OrderType   `json:"order_type"`
	Status            model.OrderStatus `json:"status"`
	Price             string            `json:"price"`
	Amount            string            `json:"amount"`
	QuoteAmount       string            `json:"quote_amount"`
	FilledAmount      string            `json:"filled_amount"`
	FilledQuoteAmount string            `json:"filled_quote_amount"`
	Remaining         string            `json:"remaining"`
	CreatedAt         time.Time         `json:"created_at"`
}

type WalletResponse struct {
	ID               uint   `json:"id"`
	CoinSymbol       string `json:"coin_symbol"`
	AvailableBalance string `json:"available_balance"`
	LockedBalance    string `json:"locked_balance"`
	TotalBalance     string `json:"total_balance"`
	AvgBuyPrice      string `json:"avg_buy_price"`
}

type TradeResponse struct {
	ID             uint            `json:"id"`
	IdempotencyKey string          `json:"idempotency_key"`
	EngineSequence int64           `json:"engine_sequence"`
	EngineEventID  string          `json:"engine_event_id"`
	CoinSymbol     string          `json:"coin_symbol"`
	Side           model.OrderSide `json:"side"`
	Price          string          `json:"price"`
	Quantity       string          `json:"quantity"`
	FeeRate        string          `json:"fee_rate"`
	BuyerFee       string          `json:"buyer_fee"`
	BuyerFeeAsset  string          `json:"buyer_fee_asset"`
	SellerFee      string          `json:"seller_fee"`
	SellerFeeAsset string          `json:"seller_fee_asset"`
	TradedAt       time.Time       `json:"traded_at"`
	BuyOrderID     uint            `json:"buy_order_id"`
	SellOrderID    uint            `json:"sell_order_id"`
}

func orderResponse(order model.Order) OrderResponse {
	remaining := order.Amount.Sub(order.FilledAmount)
	if remaining.IsNegative() {
		remaining = decimal.Zero
	}
	return OrderResponse{
		ID:                order.ID,
		CoinSymbol:        order.CoinSymbol,
		Side:              order.Side,
		OrderType:         order.OrderType,
		Status:            order.Status,
		Price:             order.Price.String(),
		Amount:            order.Amount.String(),
		QuoteAmount:       order.QuoteAmount.String(),
		FilledAmount:      order.FilledAmount.String(),
		FilledQuoteAmount: order.FilledQuoteAmount.String(),
		Remaining:         remaining.String(),
		CreatedAt:         order.CreatedAt,
	}
}

func walletResponse(wallet model.Wallet) WalletResponse {
	total := wallet.AvailableBalance.Add(wallet.LockedBalance)
	return WalletResponse{
		ID:               wallet.ID,
		CoinSymbol:       wallet.CoinSymbol,
		AvailableBalance: wallet.AvailableBalance.String(),
		LockedBalance:    wallet.LockedBalance.String(),
		TotalBalance:     total.String(),
		AvgBuyPrice:      wallet.AvgBuyPrice.String(),
	}
}

func tradeResponse(trade repository.UserTrade) TradeResponse {
	return TradeResponse{
		ID:             trade.ID,
		IdempotencyKey: trade.IdempotencyKey,
		EngineSequence: trade.EngineSequence,
		EngineEventID:  trade.EngineEventID,
		CoinSymbol:     trade.CoinSymbol,
		Side:           trade.Side,
		Price:          trade.Price.String(),
		Quantity:       trade.Quantity.String(),
		FeeRate:        trade.FeeRate.String(),
		BuyerFee:       trade.BuyerFee.String(),
		BuyerFeeAsset:  trade.BuyerFeeAsset,
		SellerFee:      trade.SellerFee.String(),
		SellerFeeAsset: trade.SellerFeeAsset,
		TradedAt:       trade.TradedAt,
		BuyOrderID:     trade.BuyOrderID,
		SellOrderID:    trade.SellOrderID,
	}
}

func parseIDParam(value string) (uint, error) {
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || parsed == 0 {
		return 0, errors.New("invalid id")
	}
	return uint(parsed), nil
}

func parseLimitQuery(c *gin.Context) int {
	value := c.Query("limit")
	if value == "" {
		return 0
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return parsed
}

func writeServiceError(c *gin.Context, err error) {
	status := serviceErrorStatus(err)
	message := err.Error()
	if errors.Is(err, gorm.ErrRecordNotFound) {
		message = "not found"
	}
	httpapi.WriteError(c, status, errorCodeForStatus(status), message)
}

func writeBindingError(c *gin.Context, err error) {
	httpapi.WriteError(c, http.StatusUnprocessableEntity, httpapi.CodeValidation, err.Error())
}

func serviceErrorStatus(err error) int {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return http.StatusNotFound
	}

	if kind, ok := service.DomainErrorKind(err); ok {
		switch kind {
		case service.ErrorKindValidation:
			return http.StatusUnprocessableEntity
		case service.ErrorKindConflict:
			return http.StatusConflict
		case service.ErrorKindForbidden:
			return http.StatusForbidden
		case service.ErrorKindUnavailable:
			return http.StatusServiceUnavailable
		}
	}

	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "does not belong"):
		return http.StatusForbidden
	case strings.Contains(message, "insufficient"),
		strings.Contains(message, "cannot be cancelled"),
		strings.Contains(message, "no remaining quantity"),
		strings.Contains(message, "already registered"):
		return http.StatusConflict
	case strings.Contains(message, "invalid"),
		strings.Contains(message, "required"),
		strings.Contains(message, "must be"),
		strings.Contains(message, "not supported"):
		return http.StatusUnprocessableEntity
	default:
		return http.StatusBadRequest
	}
}

func errorCodeForStatus(status int) string {
	return httpapi.CodeForStatus(status)
}

func authenticatedUserID(c *gin.Context) (uint, bool) {
	value, exists := c.Get(auth.UserIDContextKey)
	if !exists {
		return 0, false
	}

	switch userID := value.(type) {
	case uint:
		return userID, userID != 0
	case uint64:
		return uint(userID), userID != 0
	case int:
		if userID <= 0 {
			return 0, false
		}
		return uint(userID), true
	default:
		return 0, false
	}
}
