package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/auth"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/testdb"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func transferHandlerTestUserID(offset uint) uint {
	return uint(time.Now().UnixNano()%1_000_000_000) + 400_000 + offset
}

// TestIntegrationListTransfersExposesFailureReasonNotReviewReason은 item 4다.
// FAILURE로 확정된 출금의 사용자용 응답에 failure_reason이 있고, 운영자 전용인
// review_reason은 어떤 이름으로도 노출되면 안 된다(설계 §8.7).
func TestIntegrationListTransfersExposesFailureReasonNotReviewReason(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)
	userID := transferHandlerTestUserID(1)
	t.Cleanup(func() {
		require.NoError(t, db.Exec(
			`DELETE FROM transfer_status_events WHERE transfer_request_id IN (SELECT id FROM transfer_requests WHERE user_id = ?)`,
			userID).Error)
		require.NoError(t, db.Where("user_id = ?", userID).Delete(&model.TransferRequest{}).Error)
	})

	_, err := service.NewDevWalletService(db).FundWallet(service.FundWalletInput{
		UserID: userID, CoinSymbol: model.KRWAssetSymbol, Amount: "100000",
		RequestKey: fmt.Sprintf("handler-t4-fund-%d", userID),
	})
	require.NoError(t, err)

	transferSvc := service.NewTransferService(db, service.NewFakeTransferProcessor())
	request, err := transferSvc.RequestWithdrawal(service.WithdrawalInput{
		UserID: userID, Rail: model.TransferRailBank, Asset: model.KRWAssetSymbol,
		Amount: "100000", ClientRequestKey: fmt.Sprintf("handler-t4-%d", userID),
	})
	require.NoError(t, err)
	require.NotNil(t, request.ExternalRef)

	require.NoError(t, transferSvc.ResolveTransfer(service.ResolveInput{
		TransferRequestID: request.ID, Source: model.TransferEventSourceCallback,
		EventKey: fmt.Sprintf("callback:%s:handler-t4-evt-%d", request.Rail, request.ID),
		Outcome:  model.TransferOutcomeFailure,
		Payload:  map[string]any{"reason": "ACCOUNT_FROZEN"},
	}))

	handler := NewTransferHandler(transferSvc)

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set(auth.UserIDContextKey, userID)
	c.Request = httptest.NewRequest(http.MethodGet, "/transfers", nil)

	handler.ListTransfers(c)

	require.Equal(t, http.StatusOK, recorder.Code, "body=%s", recorder.Body.String())

	// map[string]any로 그대로 풀어야 "review_reason 키가 아예 없다"를 확인할 수
	// 있다 — TransferResponse에는 그 필드가 없으니 구조체로 풀면 "없다"는 걸
	// 검증하는 게 아니라 그냥 무시하게 된다.
	var raw struct {
		Data struct {
			Transfers []map[string]any `json:"transfers"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &raw))
	require.Len(t, raw.Data.Transfers, 1)
	entry := raw.Data.Transfers[0]

	assert.Equal(t, "ACCOUNT_FROZEN", entry["failure_reason"])
	_, hasReviewReason := entry["review_reason"]
	assert.False(t, hasReviewReason, "review_reason은 운영자 전용이라 사용자 응답에 노출되면 안 된다")
}
