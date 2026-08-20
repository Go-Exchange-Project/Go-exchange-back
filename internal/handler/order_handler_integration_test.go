package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/auth"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/testdb"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// seedCancellableOrder는 취소 가능한 지정가 매수 주문 한 건과 그 사용자를 만든다.
func seedCancellableOrder(t *testing.T, db *gorm.DB, userID uint) model.Order {
	t.Helper()

	user := model.User{ID: userID, Name: fmt.Sprintf("handler-%d", userID)}
	require.NoError(t, db.Create(&user).Error)

	order := model.Order{
		UserID:       userID,
		CoinSymbol:   "BTC",
		Side:         model.OrderSideBuy,
		OrderType:    model.OrderTypeLimit,
		Status:       model.OrderStatusPending,
		Price:        decimal.NewFromInt(100),
		Amount:       decimal.NewFromInt(5),
		FilledAmount: decimal.Zero,
	}
	require.NoError(t, db.Create(&order).Error)

	t.Cleanup(func() {
		require.NoError(t, db.Where("order_id = ?", order.ID).Delete(&model.CancelCommand{}).Error)
		require.NoError(t, db.Delete(&model.Order{}, order.ID).Error)
		require.NoError(t, db.Delete(&model.User{}, userID).Error)
	})
	return order
}

func newCancelHandlerRequest(t *testing.T, handler *OrderHandler, userID uint, orderID uint) *httptest.ResponseRecorder {
	t.Helper()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set(auth.UserIDContextKey, userID)
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", orderID)}}
	c.Request = httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/orders/%d", orderID), nil)

	handler.CancelOrder(c)
	return recorder
}

func newIntegrationOrderHandler(db *gorm.DB) *OrderHandler {
	orderService := service.NewOrderService(
		repository.NewOrderRepository(db),
		repository.NewWalletRepository(db),
		nil,
	)
	return NewOrderHandler(orderService)
}

// 취소는 더 이상 "완료"가 아니라 "접수"다. 상태 코드와 본문이 그 의미와 같은 말을
// 해야 한다 — 200 + "order cancelled"는 아직 오더북에 남아 있는 주문을 두고
// 취소됐다고 말하는 것이었다.
func TestIntegrationCancelOrderHandlerReturns202Accepted(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)
	order := seedCancellableOrder(t, db, 909001)
	handler := newIntegrationOrderHandler(db)

	recorder := newCancelHandlerRequest(t, handler, order.UserID, order.ID)

	require.Equal(t, http.StatusAccepted, recorder.Code, "body=%s", recorder.Body.String())

	var response struct {
		Data struct {
			Message   string `json:"message"`
			OrderID   uint   `json:"order_id"`
			CommandID uint64 `json:"command_id"`
			Status    string `json:"status"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))

	assert.Equal(t, "cancellation accepted", response.Data.Message)
	assert.Equal(t, order.ID, response.Data.OrderID)
	assert.Equal(t, service.CancelOrderAcceptedStatus, response.Data.Status)
	assert.NotZero(t, response.Data.CommandID)

	// 해제 금액·엔진 제거 여부는 이 응답이 알 수 없는 값이므로 사라져야 한다.
	assert.NotContains(t, recorder.Body.String(), "released_amount")
	assert.NotContains(t, recorder.Body.String(), "engine_removed")
}

// 중복 요청은 새 command를 만들지 않고 같은 ID로 다시 202다.
func TestIntegrationCancelOrderHandlerRepeatReturnsSameCommandID(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)
	order := seedCancellableOrder(t, db, 909002)
	handler := newIntegrationOrderHandler(db)

	first := newCancelHandlerRequest(t, handler, order.UserID, order.ID)
	second := newCancelHandlerRequest(t, handler, order.UserID, order.ID)

	require.Equal(t, http.StatusAccepted, first.Code)
	require.Equal(t, http.StatusAccepted, second.Code)

	commandIDOf := func(recorder *httptest.ResponseRecorder) uint64 {
		var response struct {
			Data struct {
				CommandID uint64 `json:"command_id"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
		return response.Data.CommandID
	}
	assert.Equal(t, commandIDOf(first), commandIDOf(second))

	var count int64
	require.NoError(t, db.Model(&model.CancelCommand{}).
		Where("order_id = ?", order.ID).Count(&count).Error)
	assert.EqualValues(t, 1, count)
}

// 권한·상태 오류 매핑은 그대로다. 202 전환이 4xx 계약까지 바꾸면 안 된다.
func TestIntegrationCancelOrderHandlerKeepsErrorMapping(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)
	order := seedCancellableOrder(t, db, 909003)
	handler := newIntegrationOrderHandler(db)

	t.Run("다른 사용자는 403", func(t *testing.T) {
		other := seedCancellableOrder(t, db, 909004)
		recorder := newCancelHandlerRequest(t, handler, other.UserID, order.ID)
		assert.Equal(t, http.StatusForbidden, recorder.Code, "body=%s", recorder.Body.String())
	})

	t.Run("종결된 주문은 409", func(t *testing.T) {
		require.NoError(t, db.Model(&model.Order{}).Where("id = ?", order.ID).
			Update("status", model.OrderStatusFilled).Error)
		recorder := newCancelHandlerRequest(t, handler, order.UserID, order.ID)
		assert.Equal(t, http.StatusConflict, recorder.Code, "body=%s", recorder.Body.String())
	})

	t.Run("잘못된 주문 ID는 400", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Set(auth.UserIDContextKey, order.UserID)
		c.Params = gin.Params{{Key: "id", Value: "abc"}}
		c.Request = httptest.NewRequest(http.MethodDelete, "/orders/abc", nil)

		handler.CancelOrder(c)
		assert.Equal(t, http.StatusBadRequest, recorder.Code)
	})
}
