package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
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

	require.NoError(t, db.Create(&model.Wallet{
		UserID:           userID,
		CoinSymbol:       model.KRWAssetSymbol,
		KRW:              decimal.NewFromInt(10000),
		AvailableBalance: decimal.NewFromInt(10000),
		LockedBalance:    decimal.Zero,
	}).Error)

	fakeEngine := &fakeAcceptanceEngine{admissible: true, submitSucceeds: true}
	orderService := NewOrderService(repository.NewOrderRepository(db), repository.NewWalletRepository(db), fakeEngine)

	order, err := orderService.CreateOrder(CreateOrderInput{
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

	walletRepo := repository.NewWalletRepository(db)
	krwWallet, err := walletRepo.FindKRWWalletByUserID(userID)
	require.NoError(t, err)
	assert.True(t, krwWallet.AvailableBalance.Equal(decimal.RequireFromString("4997.5")))
	assert.True(t, krwWallet.LockedBalance.Equal(decimal.RequireFromString("5002.5")))
}

// 게이트 거절: 유입 포화(IsIntakeAdmissible=false)면 DB 작업 없이 503(UNAVAILABLE),
// 주문 미생성·자금 미락.
func TestIntegrationCreateOrderFastRejectsWhenIntakeSaturated(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(201)
	defer cleanupServiceUsers(t, db, userID)

	require.NoError(t, db.Create(&model.Wallet{
		UserID:           userID,
		CoinSymbol:       model.KRWAssetSymbol,
		KRW:              decimal.NewFromInt(10000),
		AvailableBalance: decimal.NewFromInt(10000),
		LockedBalance:    decimal.Zero,
	}).Error)

	fakeEngine := &fakeAcceptanceEngine{admissible: false, submitSucceeds: true}
	orderService := NewOrderService(repository.NewOrderRepository(db), repository.NewWalletRepository(db), fakeEngine)

	order, err := orderService.CreateOrder(CreateOrderInput{
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

	var orderCount int64
	require.NoError(t, db.Model(&model.Order{}).Where("user_id = ?", userID).Count(&orderCount).Error)
	assert.Equal(t, int64(0), orderCount)

	walletRepo := repository.NewWalletRepository(db)
	krwWallet, err := walletRepo.FindKRWWalletByUserID(userID)
	require.NoError(t, err)
	assert.True(t, krwWallet.AvailableBalance.Equal(decimal.NewFromInt(10000)))
	assert.True(t, krwWallet.LockedBalance.Equal(decimal.Zero))
	assertLedgerCount(t, db, userID, 0)
}

// 바운디드 거절+보상: 게이트는 통과하나 TrySubmitOrder=false(레이스)면 주문이
// 영속화·홀드된 뒤 보상으로 홀드 전액 해제 + 상태 REJECTED, 503 반환. 잔고가
// 홀드 이전으로 복원되고 원장에 OrderHold+OrderRelease 쌍이 남아 리컨실리에이션 위반 0.
func TestIntegrationCreateOrderCompensatesWhenHandoffTimesOut(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(202)
	defer cleanupServiceUsers(t, db, userID)

	require.NoError(t, db.Create(&model.Wallet{
		UserID:           userID,
		CoinSymbol:       model.KRWAssetSymbol,
		KRW:              decimal.NewFromInt(10000),
		AvailableBalance: decimal.NewFromInt(10000),
		LockedBalance:    decimal.Zero,
	}).Error)

	fakeEngine := &fakeAcceptanceEngine{admissible: true, submitSucceeds: false}
	orderService := NewOrderService(repository.NewOrderRepository(db), repository.NewWalletRepository(db), fakeEngine)

	order, err := orderService.CreateOrder(CreateOrderInput{
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

	var orderCount int64
	require.NoError(t, db.Model(&model.Order{}).Where("user_id = ?", userID).Count(&orderCount).Error)
	require.Equal(t, int64(1), orderCount)

	var persisted model.Order
	require.NoError(t, db.Where("user_id = ?", userID).First(&persisted).Error)
	assert.Equal(t, model.OrderStatusRejected, persisted.Status)

	walletRepo := repository.NewWalletRepository(db)
	krwWallet, err := walletRepo.FindKRWWalletByUserID(userID)
	require.NoError(t, err)
	assert.True(t, krwWallet.AvailableBalance.Equal(decimal.NewFromInt(10000)), "홀드가 전액 해제돼 원래 잔고로 복원돼야 한다")
	assert.True(t, krwWallet.LockedBalance.Equal(decimal.Zero))

	holds := requireLedgerEntries(t, db, userID, model.LedgerEntryTypeOrderHold, model.LedgerReferenceTypeOrder, persisted.ID)
	require.Len(t, holds, 1)
	releases := requireLedgerEntries(t, db, userID, model.LedgerEntryTypeOrderRelease, model.LedgerReferenceTypeOrder, persisted.ID)
	require.Len(t, releases, 1)

	subject := fmt.Sprintf("wallet:%d", krwWallet.ID)
	worker := &ReconciliationWorker{Repository: repository.NewReconciliationRepository(db)}
	worker.RunOnce()
	t.Cleanup(func() {
		require.NoError(t, db.Where("subject_key = ?", subject).Delete(&model.ReconciliationViolation{}).Error)
	})
	violations := findViolationsBySubject(t, db, []string{subject})
	for _, v := range violations[subject] {
		// legacy_mismatch는 이 테스트가 지갑을 원장 기록 없이 직접 시드해서 나오는
		// 알려진 잡음이다(classifyLedgerWalletRow 참고, 버그 아님). 여기서 증명하려는
		// 것은 hold+release 쌍이 실제 정합 위반(ledger_wallet)을 만들지 않는다는 것.
		assert.NotEqual(t, "ledger_wallet", v.CheckName, "보상 후 실제 원장-지갑 불일치가 없어야 한다: %+v", v)
	}
}
