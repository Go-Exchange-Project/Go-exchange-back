package handler

import (
	"net/http"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/httpapi"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/gin-gonic/gin"
)

type TransferHandler struct {
	TransferService *service.TransferService
}

func NewTransferHandler(transferService *service.TransferService) *TransferHandler {
	return &TransferHandler{TransferService: transferService}
}

type TransferRequestBody struct {
	Rail             string `json:"rail" binding:"required"`
	Asset            string `json:"asset" binding:"required"`
	Amount           string `json:"amount" binding:"required"`
	ClientRequestKey string `json:"client_request_key" binding:"required"`
}

func (h *TransferHandler) RequestDeposit(c *gin.Context) {
	userID, ok := authenticatedUserID(c)
	if !ok {
		httpapi.WriteError(c, http.StatusUnauthorized, httpapi.CodeAuthRequired, "authenticated user is required")
		return
	}

	var req TransferRequestBody
	if err := c.ShouldBindJSON(&req); err != nil {
		writeBindingError(c, err)
		return
	}

	request, err := h.TransferService.RequestDeposit(service.DepositInput{
		UserID: userID, Rail: model.TransferRail(req.Rail), Asset: req.Asset,
		Amount: req.Amount, ClientRequestKey: req.ClientRequestKey,
	})
	if err != nil {
		writeServiceError(c, err)
		return
	}
	httpapi.WriteData(c, http.StatusOK, gin.H{"transfer": transferResponse(*request)})
}

func (h *TransferHandler) RequestWithdrawal(c *gin.Context) {
	userID, ok := authenticatedUserID(c)
	if !ok {
		httpapi.WriteError(c, http.StatusUnauthorized, httpapi.CodeAuthRequired, "authenticated user is required")
		return
	}

	var req TransferRequestBody
	if err := c.ShouldBindJSON(&req); err != nil {
		writeBindingError(c, err)
		return
	}

	request, err := h.TransferService.RequestWithdrawal(service.WithdrawalInput{
		UserID: userID, Rail: model.TransferRail(req.Rail), Asset: req.Asset,
		Amount: req.Amount, ClientRequestKey: req.ClientRequestKey,
	})
	if err != nil {
		writeServiceError(c, err)
		return
	}
	httpapi.WriteData(c, http.StatusOK, gin.H{"transfer": transferResponse(*request)})
}

func (h *TransferHandler) ListTransfers(c *gin.Context) {
	userID, ok := authenticatedUserID(c)
	if !ok {
		httpapi.WriteError(c, http.StatusUnauthorized, httpapi.CodeAuthRequired, "authenticated user is required")
		return
	}

	requests, err := h.TransferService.Transfers.ListByUser(userID, normalizeTransferQueryLimit(parseLimitQuery(c)))
	if err != nil {
		writeServiceError(c, err)
		return
	}

	response := make([]TransferResponse, 0, len(requests))
	for _, request := range requests {
		response = append(response, transferResponse(request))
	}
	httpapi.WriteData(c, http.StatusOK, gin.H{"transfers": response})
}

// CallbackRequest는 가짜 은행·가짜 체인이 우리에게 보내는 알림을 흉내 낸다.
// 운영 라우트가 아니다 — dev 그룹에만 배선된다.
type CallbackRequest struct {
	ExternalRef string         `json:"external_ref" binding:"required"`
	EventID     string         `json:"event_id" binding:"required"`
	Outcome     string         `json:"outcome" binding:"required"`
	Payload     map[string]any `json:"payload"`
}

func (h *TransferHandler) ReceiveCallback(c *gin.Context) {
	var req CallbackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeBindingError(c, err)
		return
	}

	outcome := model.TransferOutcome(req.Outcome)
	if !outcome.IsTerminal() {
		httpapi.WriteError(c, http.StatusUnprocessableEntity, httpapi.CodeValidation,
			"callback outcome must be SUCCESS or FAILURE")
		return
	}

	request, err := h.TransferService.Transfers.FindByExternalRef(req.ExternalRef)
	if err != nil {
		writeServiceError(c, err)
		return
	}

	err = h.TransferService.ResolveTransfer(service.ResolveInput{
		TransferRequestID: request.ID,
		Source:            model.TransferEventSourceCallback,
		EventKey:          "callback:" + string(request.Rail) + ":" + req.EventID,
		Outcome:           outcome,
		Payload:           req.Payload,
	})
	if err != nil {
		writeServiceError(c, err)
		return
	}
	httpapi.WriteData(c, http.StatusOK, gin.H{"message": "callback accepted"})
}

// TransferResponse는 사용자용 응답이다. last_checked_at·next_check_at·
// review_reason·check_attempts는 운영자용 정보라 절대 넣지 않는다(설계 §8.7).
// Delayed는 그중 review_required_at의 존재 여부만 불리언으로 알려준다 —
// PROCESSING 상태에서 "처리 지연" 문구를 보여줄지 판단하는 데만 쓰고, 시각이나
// 사유는 노출하지 않는다.
type TransferResponse struct {
	ID          uint      `json:"id"`
	Direction   string    `json:"direction"`
	Rail        string    `json:"rail"`
	Asset       string    `json:"asset"`
	Amount      string    `json:"amount"`
	FeeAmount   string    `json:"fee_amount"`
	Status      string    `json:"status"`
	ExternalRef *string   `json:"external_ref"`
	Delayed     bool      `json:"delayed"`
	CreatedAt   time.Time `json:"created_at"`
}

func transferResponse(request model.TransferRequest) TransferResponse {
	return TransferResponse{
		ID:          request.ID,
		Direction:   string(request.Direction),
		Rail:        string(request.Rail),
		Asset:       request.Asset,
		Amount:      request.Amount.String(),
		FeeAmount:   request.FeeAmount.String(),
		Status:      string(request.Status),
		ExternalRef: request.ExternalRef,
		Delayed:     request.Status == model.TransferStatusProcessing && request.ReviewRequiredAt != nil,
		CreatedAt:   request.CreatedAt,
	}
}

// normalizeTransferQueryLimit은 order_handler.go 계열과 같은 상한(service.MaxQueryLimit)을
// 쓴다. normalizeQueryLimit 자체는 패키지 밖에 노출돼 있지 않아 상수만 재사용한다.
func normalizeTransferQueryLimit(limit int) int {
	if limit <= 0 {
		return service.DefaultQueryLimit
	}
	if limit > service.MaxQueryLimit {
		return service.MaxQueryLimit
	}
	return limit
}
