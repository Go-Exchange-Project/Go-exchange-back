package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/auth"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/httpapi"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
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

// acceptingEngine은 주문을 항상 접수하는 최소 더블이다. 핸들러 계약만 보므로
// 매칭 동작은 필요 없다.
type acceptingEngine struct{}

func (acceptingEngine) SubmitOrder(*matching.Order) {}

func (acceptingEngine) TrySubmitOrder(*matching.Order, time.Duration) bool { return true }

func (acceptingEngine) IsIntakeAdmissible(string) bool { return true }

func (acceptingEngine) CancelOrder(matching.CancelOrderCommand) matching.CancelOrderResult {
	return matching.CancelOrderResult{}
}

func (acceptingEngine) RequestOrderBookSnapshot(string, int) (matching.OrderBookSnapshot, error) {
	return matching.OrderBookSnapshot{}, nil
}

func newCreateOrderHandler(db *gorm.DB) *OrderHandler {
	return NewOrderHandler(service.NewOrderService(
		repository.NewOrderRepository(db), repository.NewWalletRepository(db), acceptingEngine{}))
}

func seedFundedUser(t *testing.T, db *gorm.DB, userID uint) uint {
	t.Helper()

	require.NoError(t, db.Create(&model.User{ID: userID, Name: fmt.Sprintf("idem-%d", userID)}).Error)
	require.NoError(t, db.Create(&model.Wallet{
		UserID: userID, CoinSymbol: model.KRWAssetSymbol,
		KRW:              decimal.NewFromInt(100000),
		AvailableBalance: decimal.NewFromInt(100000),
		LockedBalance:    decimal.Zero,
	}).Error)

	t.Cleanup(func() {
		require.NoError(t, db.Where("user_id = ?", userID).Delete(&model.OrderIdempotencyKey{}).Error)
		require.NoError(t, db.Where("user_id = ?", userID).Delete(&model.LedgerEntry{}).Error)
		require.NoError(t, db.Where("user_id = ?", userID).Delete(&model.Order{}).Error)
		require.NoError(t, db.Where("user_id = ?", userID).Delete(&model.Wallet{}).Error)
		require.NoError(t, db.Delete(&model.User{}, userID).Error)
	})
	return userID
}

func validOrderBody() CreateOrderRequest {
	return CreateOrderRequest{
		CoinSymbol: "BTC", Side: "BUY", OrderType: "LIMIT",
		Price: "100", Amount: "1",
	}
}

func postOrder(t *testing.T, handler *OrderHandler, userID uint, key string, body CreateOrderRequest) *httptest.ResponseRecorder {
	t.Helper()

	payload, err := json.Marshal(body)
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set(auth.UserIDContextKey, userID)
	c.Request = httptest.NewRequest(http.MethodPost, "/orders", bytes.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")
	if key != "" {
		c.Request.Header.Set("Idempotency-Key", key)
	}

	handler.CreateOrder(c)
	return recorder
}

// orderResponse는 성공 응답(data)과 오류 응답(error+data)을 같은 모양으로 읽는다.
type idemOrderResponse struct {
	Data  map[string]any `json:"data"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeOrderResponse(t *testing.T, recorder *httptest.ResponseRecorder) idemOrderResponse {
	t.Helper()

	var body idemOrderResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body), "body=%s", recorder.Body.String())
	return body
}

func orderIDOf(t *testing.T, recorder *httptest.ResponseRecorder) uint64 {
	t.Helper()

	body := decodeOrderResponse(t, recorder)
	require.Contains(t, body.Data, "order_id", "body=%s", recorder.Body.String())

	id, ok := body.Data["order_id"].(float64)
	require.True(t, ok, "body=%s", recorder.Body.String())
	return uint64(id)
}

// 키 누락은 400이다. 서비스의 ErrorKindValidation은 422로 매핑되므로 핸들러가 먼저 본다.
func TestIntegrationCreateOrderHandlerRequiresIdempotencyKey(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)
	userID := seedFundedUser(t, db, 919001)
	handler := newCreateOrderHandler(db)

	for name, key := range map[string]string{"헤더 없음": "", "공백만": "   "} {
		t.Run(name, func(t *testing.T) {
			recorder := postOrder(t, handler, userID, key, validOrderBody())
			assert.Equal(t, http.StatusBadRequest, recorder.Code, "body=%s", recorder.Body.String())
		})
	}

	var count int64
	require.NoError(t, db.Model(&model.Order{}).Where("user_id = ?", userID).Count(&count).Error)
	assert.Zero(t, count, "키 없는 요청이 주문을 만들었다")
}

func TestIntegrationCreateOrderHandlerReplaysSameKey(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)
	userID := seedFundedUser(t, db, 919002)
	handler := newCreateOrderHandler(db)

	first := postOrder(t, handler, userID, "key-1", validOrderBody())
	require.Equal(t, http.StatusOK, first.Code, "body=%s", first.Body.String())

	second := postOrder(t, handler, userID, "key-1", validOrderBody())
	require.Equal(t, http.StatusOK, second.Code, "body=%s", second.Body.String())

	assert.Equal(t, orderIDOf(t, first), orderIDOf(t, second))

	// 필드 존재만 보면 false여도 통과한다. 값까지 확인한다.
	assert.Equal(t, true, decodeOrderResponse(t, second).Data["idempotent_replay"],
		"body=%s", second.Body.String())
	assert.NotContains(t, decodeOrderResponse(t, first).Data, "idempotent_replay",
		"최초 요청에 replay 표시가 붙었다")
}

func TestIntegrationCreateOrderHandlerRejectsReusedKeyWithDifferentBody(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)
	userID := seedFundedUser(t, db, 919003)
	handler := newCreateOrderHandler(db)

	first := postOrder(t, handler, userID, "key-2", validOrderBody())
	require.Equal(t, http.StatusOK, first.Code, "body=%s", first.Body.String())

	changed := validOrderBody()
	changed.Amount = "2"
	second := postOrder(t, handler, userID, "key-2", changed)

	assert.Equal(t, http.StatusConflict, second.Code, "body=%s", second.Body.String())
}

// 네 outcome이 서로 다른 상태 코드로 나가야 한다. PENDING만 분기하고 나머지를 200으로
// 흘리면 접수되지 않은 주문이 "order accepted"로 표시된다.
func TestIntegrationCreateOrderHandlerMapsOutcomeToStatus(t *testing.T) {
	cases := map[string]struct {
		outcome    model.OrderIdempotencyOutcome
		wantStatus int
	}{
		"ACCEPTED는 200": {model.OrderIdempotencyOutcomeAccepted, http.StatusOK},
		"PENDING은 202":  {model.OrderIdempotencyOutcomePending, http.StatusAccepted},
		"REJECTED는 503": {model.OrderIdempotencyOutcomeRejected, http.StatusServiceUnavailable},
		"UNKNOWN은 503":  {model.OrderIdempotencyOutcomeUnknown, http.StatusServiceUnavailable},
	}

	userID := uint(919010)
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			userID++
			db := testdb.OpenIntegrationDB(t)
			seedFundedUser(t, db, userID)
			handler := newCreateOrderHandler(db)

			first := postOrder(t, handler, userID, "outcome-key", validOrderBody())
			require.Equal(t, http.StatusOK, first.Code, "body=%s", first.Body.String())

			// 저장된 outcome을 바꿔 재요청이 그 상태를 그대로 반영하는지 본다.
			require.NoError(t, db.Model(&model.OrderIdempotencyKey{}).
				Where("user_id = ? AND idempotency_key = ?", userID, "outcome-key").
				Update("outcome", tc.outcome).Error)

			replay := postOrder(t, handler, userID, "outcome-key", validOrderBody())
			require.Equal(t, tc.wantStatus, replay.Code, "body=%s", replay.Body.String())

			// 어떤 상태든 order_id는 준다 — 클라이언트가 주문 상태를 조회할 수 있어야 한다.
			assert.Equal(t, orderIDOf(t, first), orderIDOf(t, replay))

			body := decodeOrderResponse(t, replay)
			if tc.wantStatus == http.StatusServiceUnavailable {
				assert.Equal(t, string(tc.outcome), body.Data["status"])
				assert.Equal(t, httpapi.CodeUnavailable, body.Error.Code)
				assert.Equal(t, "1", replay.Header().Get("Retry-After"),
					"503에 Retry-After가 없다")
				return
			}
			assert.Empty(t, replay.Header().Get("Retry-After"))
			assert.Equal(t, true, body.Data["idempotent_replay"],
				"replay 표시가 없거나 false다: body=%s", replay.Body.String())
			if tc.wantStatus == http.StatusAccepted {
				assert.Equal(t, string(tc.outcome), body.Data["status"])
			}
		})
	}
}

// 접수에 실패하는 엔진. 보상 경로를 태우기 위한 최소 더블이다.
type rejectingEngine struct{ acceptingEngine }

func (rejectingEngine) TrySubmitOrder(*matching.Order, time.Duration) bool { return false }

func newRejectingOrderHandler(db *gorm.DB) *OrderHandler {
	return NewOrderHandler(service.NewOrderService(
		repository.NewOrderRepository(db), repository.NewWalletRepository(db), rejectingEngine{}))
}

// 최초 접수 실패도 order_id를 줘야 한다. 저장된 결과의 재요청만 order_id를 받고 최초
// 실패는 못 받으면, 같은 상태가 두 가지 응답으로 보인다.
func TestIntegrationCreateOrderHandlerFirstRejectionReturnsOrderID(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)
	userID := seedFundedUser(t, db, 919021)
	handler := newRejectingOrderHandler(db)

	recorder := postOrder(t, handler, userID, "reject-key", validOrderBody())

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code, "body=%s", recorder.Body.String())
	body := decodeOrderResponse(t, recorder)
	assert.Equal(t, httpapi.CodeUnavailable, body.Error.Code)
	assert.Equal(t, string(model.OrderIdempotencyOutcomeRejected), body.Data["status"])
	assert.Equal(t, "1", recorder.Header().Get("Retry-After"))

	orderID := orderIDOf(t, recorder)
	require.NotZero(t, orderID)

	// 보상이 끝났으므로 주문은 REJECTED이고 hold는 풀려 있어야 한다.
	var order model.Order
	require.NoError(t, db.First(&order, orderID).Error)
	assert.Equal(t, model.OrderStatusRejected, order.Status)

	var wallet model.Wallet
	require.NoError(t, db.Where("user_id = ? AND coin_symbol = ?", userID, model.KRWAssetSymbol).
		First(&wallet).Error)
	assert.True(t, wallet.LockedBalance.IsZero(), "hold가 남았다: %s", wallet.LockedBalance)

	var record model.OrderIdempotencyKey
	require.NoError(t, db.Where("user_id = ?", userID).First(&record).Error)
	assert.Equal(t, model.OrderIdempotencyOutcomeRejected, record.Outcome)
}

// 보상 자체가 실패하면 hold가 남는다. 그래도 응답은 order_id와 durable outcome을 줘야
// 한다 — 사람이 그 주문을 찾아갈 수 있어야 한다.
func TestIntegrationCreateOrderHandlerFailedCompensationReportsUnknown(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)
	userID := seedFundedUser(t, db, 919022)
	handler := newRejectingOrderHandler(db)

	// 주문의 REJECTED 전이만 막는다. 보상 트랜잭션이 통째로 롤백되고, 트랜잭션 밖의
	// UNKNOWN 기록은 성공한다.
	blockUpdate(t, db, "orders", fmt.Sprintf("NEW.status = 'REJECTED' AND NEW.user_id = %d", userID))

	recorder := postOrder(t, handler, userID, "unknown-key", validOrderBody())

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code, "body=%s", recorder.Body.String())
	body := decodeOrderResponse(t, recorder)
	assert.Equal(t, string(model.OrderIdempotencyOutcomeUnknown), body.Data["status"])
	require.NotZero(t, orderIDOf(t, recorder))

	var record model.OrderIdempotencyKey
	require.NoError(t, db.Where("user_id = ?", userID).First(&record).Error)
	assert.Equal(t, model.OrderIdempotencyOutcomeUnknown, record.Outcome,
		"보상 실패를 UNKNOWN으로 남기지 않았다")
}

// blockUpdate는 특정 UPDATE만 실패시키는 트리거를 건다. OrderService의 저장소 필드는
// 인터페이스가 아니라 concrete pointer라, 실패를 주입하려면 운영 코드를 추상화해야 한다.
// 테스트 하나 때문에 운영 경로를 바꾸는 대신 DB 쪽에서 막는다.
func blockUpdate(t *testing.T, db *gorm.DB, table string, when string) {
	t.Helper()

	// 함수도 영구 객체다. 이름을 고정하면 다음 실행의 CREATE FUNCTION이 충돌한다.
	suffix := fmt.Sprintf("%d_%d", time.Now().UnixNano(), blockedUpdateSeq.Add(1))
	fn := "fail_" + table + "_" + suffix
	trg := fn + "_trg"

	require.NoError(t, db.Exec(fmt.Sprintf(`
CREATE FUNCTION %s() RETURNS trigger AS $$
BEGIN RAISE EXCEPTION 'injected failure'; END $$ LANGUAGE plpgsql`, fn)).Error)

	// 함수 생성 직후에 등록한다. 트리거 생성이 실패하면 require.NoError가 테스트를
	// 중단하므로, 뒤에 등록하면 함수가 공유 DB에 그대로 남는다.
	t.Cleanup(func() {
		require.NoError(t, db.Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON %s`, trg, table)).Error)
		require.NoError(t, db.Exec(fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn)).Error)
	})

	require.NoError(t, db.Exec(fmt.Sprintf(`
CREATE TRIGGER %s BEFORE UPDATE ON %s
FOR EACH ROW WHEN (%s)
EXECUTE FUNCTION %s()`, trg, table, when, fn)).Error)
}

var blockedUpdateSeq atomic.Uint64

// 보상도 실패하고 UNKNOWN 기록마저 실패하면 durable outcome은 PENDING에 머문다.
// 응답이 UNKNOWN이라고 말하면 저장된 상태보다 앞서 말하는 것이다.
func TestIntegrationCreateOrderHandlerUnknownUpdateFailureReportsPending(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)
	userID := seedFundedUser(t, db, 919023)
	handler := newRejectingOrderHandler(db)

	blockUpdate(t, db, "orders", fmt.Sprintf("NEW.status = 'REJECTED' AND NEW.user_id = %d", userID))
	blockUpdate(t, db, "order_idempotency_keys",
		fmt.Sprintf("NEW.outcome = 'UNKNOWN' AND NEW.user_id = %d", userID))

	recorder := postOrder(t, handler, userID, "pending-key", validOrderBody())

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code, "body=%s", recorder.Body.String())
	body := decodeOrderResponse(t, recorder)
	assert.Equal(t, httpapi.CodeUnavailable, body.Error.Code)
	assert.Equal(t, string(model.OrderIdempotencyOutcomePending), body.Data["status"],
		"UNKNOWN 기록이 실패했는데 응답이 UNKNOWN이라고 말했다")
	assert.Equal(t, "1", recorder.Header().Get("Retry-After"))
	require.NotZero(t, orderIDOf(t, recorder))

	var record model.OrderIdempotencyKey
	require.NoError(t, db.Where("user_id = ?", userID).First(&record).Error)
	assert.Equal(t, model.OrderIdempotencyOutcomePending, record.Outcome,
		"저장된 outcome이 PENDING이 아니다")
}

// UNKNOWN은 재시도로 자동 해결되지 않는다. "같은 키로 재시도하면 된다"고 안내하면
// 클라이언트가 해결되지 않는 루프를 돈다 — 최초 실패와 replay 양쪽 모두.
func TestIntegrationCreateOrderHandlerUnknownGuidanceDoesNotPromiseRetry(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)
	userID := seedFundedUser(t, db, 919024)
	handler := newRejectingOrderHandler(db)

	blockUpdate(t, db, "orders", fmt.Sprintf("NEW.status = 'REJECTED' AND NEW.user_id = %d", userID))

	first := postOrder(t, handler, userID, "guidance-key", validOrderBody())
	require.Equal(t, http.StatusServiceUnavailable, first.Code, "body=%s", first.Body.String())
	firstMessage := decodeOrderResponse(t, first).Error.Message
	assert.Equal(t, string(model.OrderIdempotencyOutcomeUnknown), decodeOrderResponse(t, first).Data["status"])
	assert.Contains(t, firstMessage, "check the order status")
	assert.NotContains(t, firstMessage, "retry is safe with the same key")

	// 저장된 결과의 재요청도 같은 안내여야 한다. 두 경로가 다른 말을 하면 클라이언트는
	// 어느 쪽을 믿어야 할지 알 수 없다.
	replay := postOrder(t, handler, userID, "guidance-key", validOrderBody())
	require.Equal(t, http.StatusServiceUnavailable, replay.Code, "body=%s", replay.Body.String())
	assert.Equal(t, firstMessage, decodeOrderResponse(t, replay).Error.Message)
}

// REJECTED는 되돌리기가 끝난 상태다. 같은 키는 계속 이 결과를 돌려주므로 새 주문에는
// 새 키가 필요하다고 안내해야 한다.
func TestIntegrationCreateOrderHandlerRejectedGuidanceAsksForNewKey(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)
	userID := seedFundedUser(t, db, 919025)
	handler := newRejectingOrderHandler(db)

	recorder := postOrder(t, handler, userID, "rejected-guidance-key", validOrderBody())

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code, "body=%s", recorder.Body.String())
	message := decodeOrderResponse(t, recorder).Error.Message
	assert.Contains(t, message, "new idempotency key")
	assert.NotContains(t, message, "retry is safe with the same key")
}
