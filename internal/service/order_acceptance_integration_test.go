package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAcceptanceEngine은 matching.Engine을 만족하는 최소 테스트 더블이다.
// IsIntakeAdmissible/TrySubmitOrder만 시나리오별로 제어하고 나머지는 no-op이다
// (CreateOrder 경로에서 호출되지 않지만 인터페이스 만족을 위해 필요).
type fakeAcceptanceEngine struct {
	admissible     bool
	submitSucceeds bool
}

func (f *fakeAcceptanceEngine) SubmitOrder(*matching.Order) {}

func (f *fakeAcceptanceEngine) TrySubmitOrder(order *matching.Order, within time.Duration) bool {
	return f.submitSucceeds
}

func (f *fakeAcceptanceEngine) IsIntakeAdmissible(coinSymbol string) bool {
	return f.admissible
}

func (f *fakeAcceptanceEngine) CancelOrder(matching.CancelOrderCommand) matching.CancelOrderResult {
	return matching.CancelOrderResult{}
}

func (f *fakeAcceptanceEngine) RequestOrderBookSnapshot(coinSymbol string, depth int) (matching.OrderBookSnapshot, error) {
	return matching.OrderBookSnapshot{}, nil
}

// 정상: 여유 시 200·엔진 접수·주문 PENDING (기존 동작 보존)
func TestIntegrationCreateOrderSubmitsWhenIntakeHasRoom(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(200)
	defer cleanupServiceUsers(t, db, userID)

	seedLedgerFunds(t, db, userID, model.KRWAssetSymbol, decimal.NewFromInt(10000))

	fakeEngine := &fakeAcceptanceEngine{admissible: true, submitSucceeds: true}
	orderService := NewOrderService(repository.NewOrderRepository(db), fakeEngine)

	order, err := createTestOrder(orderService, CreateOrderInput{
		UserID:     userID,
		CoinSymbol: "BTC",
		Side:       "BUY",
		Price:      "5000",
		Amount:     "1",
	})

	require.NoError(t, err)
	require.NotNil(t, order)
	require.NotZero(t, order.ID)

	var persisted model.Order
	require.NoError(t, db.First(&persisted, order.ID).Error)
	assert.Equal(t, model.OrderStatusPending, persisted.Status)

	assertLedgerBalances(t, db, userID, model.KRWAssetSymbol, decimal.RequireFromString("4997.5"), decimal.RequireFromString("5002.5"))
}

// 게이트 거절: 유입 포화(IsIntakeAdmissible=false)면 DB 작업 없이 503(UNAVAILABLE),
// 주문 미생성·자금 미락.
func TestIntegrationCreateOrderFastRejectsWhenIntakeSaturated(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(201)
	defer cleanupServiceUsers(t, db, userID)

	seedLedgerFunds(t, db, userID, model.KRWAssetSymbol, decimal.NewFromInt(10000))

	fakeEngine := &fakeAcceptanceEngine{admissible: false, submitSucceeds: true}
	orderService := NewOrderService(repository.NewOrderRepository(db), fakeEngine)

	before := testutil.ToFloat64(metrics.OrdersAdmissionRejectedTotal.WithLabelValues("engine_gate"))

	order, err := createTestOrder(orderService, CreateOrderInput{
		UserID:     userID,
		CoinSymbol: "BTC",
		Side:       "BUY",
		Price:      "5000",
		Amount:     "1",
	})

	require.Error(t, err)
	assert.Nil(t, order)
	kind, ok := DomainErrorKind(err)
	require.True(t, ok)
	assert.Equal(t, ErrorKindUnavailable, kind)

	after := testutil.ToFloat64(metrics.OrdersAdmissionRejectedTotal.WithLabelValues("engine_gate"))
	assert.Equal(t, before+1, after)

	var orderCount int64
	require.NoError(t, db.Model(&model.Order{}).Where("user_id = ?", userID).Count(&orderCount).Error)
	assert.Equal(t, int64(0), orderCount)

	assertLedgerBalances(t, db, userID, model.KRWAssetSymbol, decimal.NewFromInt(10000), decimal.Zero)
}

// 바운디드 거절+보상: 게이트는 통과하나 TrySubmitOrder=false(레이스)면 주문이
// 영속화·홀드된 뒤 보상으로 홀드 전액 해제 + 상태 REJECTED, 503 반환. 잔고가
// 홀드 이전으로 복원되고 원장에 OrderHold+OrderRelease 쌍이 남아 리컨실리에이션 위반 0.
func TestIntegrationCreateOrderCompensatesWhenHandoffTimesOut(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(202)
	defer cleanupServiceUsers(t, db, userID)

	seedLedgerFunds(t, db, userID, model.KRWAssetSymbol, decimal.NewFromInt(10000))

	fakeEngine := &fakeAcceptanceEngine{admissible: true, submitSucceeds: false}
	orderService := NewOrderService(repository.NewOrderRepository(db), fakeEngine)

	before := testutil.ToFloat64(metrics.OrdersAdmissionRejectedTotal.WithLabelValues("engine_handoff"))

	order, err := createTestOrder(orderService, CreateOrderInput{
		UserID:     userID,
		CoinSymbol: "BTC",
		Side:       "BUY",
		Price:      "5000",
		Amount:     "1",
	})

	require.Error(t, err)
	assert.Nil(t, order)
	kind, ok := DomainErrorKind(err)
	require.True(t, ok)
	assert.Equal(t, ErrorKindUnavailable, kind)

	after := testutil.ToFloat64(metrics.OrdersAdmissionRejectedTotal.WithLabelValues("engine_handoff"))
	assert.Equal(t, before+1, after)

	var orderCount int64
	require.NoError(t, db.Model(&model.Order{}).Where("user_id = ?", userID).Count(&orderCount).Error)
	require.Equal(t, int64(1), orderCount)

	var persisted model.Order
	require.NoError(t, db.Where("user_id = ?", userID).First(&persisted).Error)
	assert.Equal(t, model.OrderStatusRejected, persisted.Status)

	assertLedgerBalances(t, db, userID, model.KRWAssetSymbol, decimal.NewFromInt(10000), decimal.Zero)

	requireJournalByKey(t, db, orderHoldKey(persisted.ID))
	requireJournalByKey(t, db, orderReleaseKey(persisted.ID, releaseReasonRejected))

	accountID := userAvailableAccountID(t, db, userID, model.KRWAssetSymbol)
	subject := fmt.Sprintf("account:%d", accountID)
	worker := &ReconciliationWorker{Repository: repository.NewReconciliationRepository(db)}
	worker.RunOnce()
	t.Cleanup(func() {
		require.NoError(t, db.Where("subject_key = ?", subject).Delete(&model.ReconciliationViolation{}).Error)
	})
	violations := findViolationsBySubject(t, db, []string{subject})
	assert.Empty(t, violations[subject], "보상 후 실제 검산 위반이 없어야 한다: %+v", violations[subject])
}

// 코디네이터 경유: OrderService.HoldCoordinator를 실제로 기동한 코디네이터로 주입하면
// persistAndHold 폴백이 아니라 Submit→HoldBatch 경로로 처리된다. 정상 홀드(ID 채워짐,
// PENDING, 잔고 홀드)와 잔고 부족(ConflictError, 주문 미생성, 잔고 무변화)을 모두
// 이 경로로 검증하고, 마지막에 리컨실리에이션에서 실제 ledger_wallet 위반이 없음을 확인한다.
func TestIntegrationCreateOrderViaHoldCoordinator(t *testing.T) {
	db := openServiceIntegrationDB(t)
	orderRepo := repository.NewOrderRepository(db)
	buyerID := serviceTestUserID(205)
	poorBuyerID := serviceTestUserID(206)
	defer cleanupServiceUsers(t, db, buyerID, poorBuyerID)

	// 원장은 출처가 하나라 지급 한 번이면 원장·잔액 캐시 일관성이 구조적으로
	// 보장된다 — 지갑 시절처럼 지갑과 원장 두 곳을 따로 심을 필요가 없다.
	seedLedgerFunds(t, db, buyerID, model.KRWAssetSymbol, decimal.NewFromInt(10000))
	seedLedgerFunds(t, db, poorBuyerID, model.KRWAssetSymbol, decimal.NewFromInt(10))

	coordinator := NewHoldCoordinator(db, orderRepo, NewLedgerService(db), repository.NewOrderIdempotencyRepository(db), 0)
	go coordinator.Run()
	defer coordinator.Shutdown()

	fakeEngine := &fakeAcceptanceEngine{admissible: true, submitSucceeds: true}
	orderService := NewOrderService(orderRepo, fakeEngine)
	orderService.HoldCoordinator = coordinator

	// 정상: 코디네이터 경유 홀드 성공.
	order, err := createTestOrder(orderService, CreateOrderInput{
		UserID: buyerID, CoinSymbol: "BTC", Side: "BUY", Price: "5000", Amount: "1",
	})
	require.NoError(t, err)
	require.NotNil(t, order)
	require.NotZero(t, order.ID)

	var persisted model.Order
	require.NoError(t, db.First(&persisted, order.ID).Error)
	assert.Equal(t, model.OrderStatusPending, persisted.Status)

	assertLedgerBalances(t, db, buyerID, model.KRWAssetSymbol, decimal.RequireFromString("4997.5"), decimal.RequireFromString("5002.5"))

	// 잔고 부족: 코디네이터 경유라도 홀드 실패는 ConflictError(409)로 전파, 주문 미생성.
	poorOrder, err := createTestOrder(orderService, CreateOrderInput{
		UserID: poorBuyerID, CoinSymbol: "BTC", Side: "BUY", Price: "5000", Amount: "1",
	})
	require.Error(t, err)
	assert.Nil(t, poorOrder)
	kind, ok := DomainErrorKind(err)
	require.True(t, ok)
	assert.Equal(t, ErrorKindConflict, kind)

	var poorOrderCount int64
	require.NoError(t, db.Model(&model.Order{}).Where("user_id = ?", poorBuyerID).Count(&poorOrderCount).Error)
	assert.Equal(t, int64(0), poorOrderCount)

	assertLedgerBalances(t, db, poorBuyerID, model.KRWAssetSymbol, decimal.NewFromInt(10), decimal.Zero)

	subjects := []string{
		fmt.Sprintf("account:%d", userAvailableAccountID(t, db, buyerID, model.KRWAssetSymbol)),
		fmt.Sprintf("account:%d", userAvailableAccountID(t, db, poorBuyerID, model.KRWAssetSymbol)),
	}
	worker := &ReconciliationWorker{Repository: repository.NewReconciliationRepository(db)}
	worker.RunOnce()
	t.Cleanup(func() {
		require.NoError(t, db.Where("subject_key IN ?", subjects).Delete(&model.ReconciliationViolation{}).Error)
	})
	violations := findViolationsBySubject(t, db, subjects)
	for _, subjectViolations := range violations {
		assert.Empty(t, subjectViolations, "코디네이터 경유 홀드가 실제 검산 위반을 만들면 안 된다: %+v", subjectViolations)
	}
}
