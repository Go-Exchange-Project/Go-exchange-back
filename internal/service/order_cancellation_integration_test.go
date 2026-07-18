package service

import (
	"fmt"
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 부분체결 주문에 ProcessOrderCancellation을 적용하면 잔여 hold만 정확히 해제되고
// CANCELLED가 커밋되며 원장 기록이 남는다(A-4 정산 파이프라인 핵심 경로). 선행 체결
// 정산은 이 테스트 범위 밖이므로 UpdateOrderExecution으로 "이미 정산된 FilledAmount"
// 상태만 시뮬레이션한다 — ProcessOrderCancellation은 정확히 이 값만 신뢰한다.
func TestIntegrationProcessOrderCancellationReleasesRemainingHoldAndCommitsCancelled(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(95)
	defer cleanupServiceUsers(t, db, userID)

	require.NoError(t, db.Create(&model.Wallet{
		UserID:           userID,
		CoinSymbol:       model.KRWAssetSymbol,
		KRW:              decimal.NewFromInt(1_000_000),
		AvailableBalance: decimal.NewFromInt(1_000_000),
		LockedBalance:    decimal.Zero,
	}).Error)

	orderService := newIntegrationOrderService(db, matching.NewMatchingEngine())
	order, err := orderService.CreateOrder(CreateOrderInput{
		UserID:     userID,
		CoinSymbol: "BTC",
		Side:       "BUY",
		Price:      "100",
		Amount:     "10",
	})
	require.NoError(t, err)

	orderRepo := repository.NewOrderRepository(db)
	require.NoError(t, orderRepo.UpdateOrderExecution(order.ID, decimal.NewFromInt(4), decimal.NewFromInt(400), model.OrderStatusPartial))

	err = orderService.ProcessOrderCancellation(matching.OrderCancelled{
		OrderID:    order.ID,
		CoinSymbol: order.CoinSymbol,
		Side:       order.Side,
	})
	require.NoError(t, err)

	var persisted model.Order
	require.NoError(t, db.First(&persisted, order.ID).Error)
	assert.Equal(t, model.OrderStatusCancelled, persisted.Status)

	walletRepo := repository.NewWalletRepository(db)
	wallet, err := walletRepo.FindKRWWalletByUserID(userID)
	require.NoError(t, err)
	// hold(10주, 1000.5) 중 이미 체결된 4주 몫은 남고, 잔여 6주 몫(600.3)만 해제된다.
	assert.True(t, wallet.AvailableBalance.Equal(decimal.RequireFromString("999599.8")), "available=%s", wallet.AvailableBalance.String())
	assert.True(t, wallet.LockedBalance.Equal(decimal.RequireFromString("400.2")), "locked=%s", wallet.LockedBalance.String())

	entries := requireLedgerEntries(t, db, userID, model.LedgerEntryTypeOrderRelease, model.LedgerReferenceTypeOrder, order.ID)
	require.Len(t, entries, 1)
	assertLedgerDelta(t, entries[0], model.KRWAssetSymbol, "600.3", "-600.3", "999599.8", "400.2")

	assertNoReconciliationViolationsForWallet(t, db, userID, model.KRWAssetSymbol)
}

// 같은 OrderCancelled 이벤트가 리플레이 등으로 두 번 처리돼도 두 번째 호출은 no-op —
// hold가 이중 해제되지 않고 원장도 중복 기록되지 않는다.
func TestIntegrationProcessOrderCancellationIsIdempotent(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(96)
	defer cleanupServiceUsers(t, db, userID)

	require.NoError(t, db.Create(&model.Wallet{
		UserID:           userID,
		CoinSymbol:       model.KRWAssetSymbol,
		KRW:              decimal.NewFromInt(1000),
		AvailableBalance: decimal.NewFromInt(1000),
		LockedBalance:    decimal.Zero,
	}).Error)

	orderService := newIntegrationOrderService(db, matching.NewMatchingEngine())
	order, err := orderService.CreateOrder(CreateOrderInput{
		UserID:     userID,
		CoinSymbol: "BTC",
		Side:       "BUY",
		Price:      "100",
		Amount:     "5",
	})
	require.NoError(t, err)

	event := matching.OrderCancelled{OrderID: order.ID, CoinSymbol: order.CoinSymbol, Side: order.Side}
	require.NoError(t, orderService.ProcessOrderCancellation(event))
	require.NoError(t, orderService.ProcessOrderCancellation(event))

	var persisted model.Order
	require.NoError(t, db.First(&persisted, order.ID).Error)
	assert.Equal(t, model.OrderStatusCancelled, persisted.Status)

	walletRepo := repository.NewWalletRepository(db)
	wallet, err := walletRepo.FindKRWWalletByUserID(userID)
	require.NoError(t, err)
	assert.True(t, wallet.AvailableBalance.Equal(decimal.NewFromInt(1000)), "hold 전액이 정확히 원복돼야 한다")
	assert.True(t, wallet.LockedBalance.IsZero())

	entries := requireLedgerEntries(t, db, userID, model.LedgerEntryTypeOrderRelease, model.LedgerReferenceTypeOrder, order.ID)
	require.Len(t, entries, 1, "두 번째 호출은 no-op이어야 하고 원장이 중복 기록되면 안 된다")

	assertNoReconciliationViolationsForWallet(t, db, userID, model.KRWAssetSymbol)
}

// 이미 FILLED인 주문에 뒤늦게 OrderCancelled가 도착해도(엔진 FIFO상 정상적으로는
// 발생하지 않지만 리플레이 중복 등 방어적으로) 상태를 덮어쓰지 않고 no-op이다.
func TestIntegrationProcessOrderCancellationOnAlreadyFilledOrderIsNoop(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(97)
	defer cleanupServiceUsers(t, db, userID)

	order := seedCancelOrderRows(t, db, cancelOrderSeed{
		UserID:        userID,
		CoinSymbol:    "BTC",
		Side:          model.OrderSideBuy,
		Status:        model.OrderStatusFilled,
		Price:         decimal.NewFromInt(100),
		Amount:        decimal.NewFromInt(5),
		FilledAmount:  decimal.NewFromInt(5),
		LockedBalance: decimal.Zero,
	})

	orderService := newIntegrationOrderService(db, nil)
	err := orderService.ProcessOrderCancellation(matching.OrderCancelled{
		OrderID:    order.ID,
		CoinSymbol: order.CoinSymbol,
		Side:       order.Side,
	})
	require.NoError(t, err)

	var persisted model.Order
	require.NoError(t, db.First(&persisted, order.ID).Error)
	assert.Equal(t, model.OrderStatusFilled, persisted.Status)
	assertLedgerCount(t, db, userID, 0)
}

// assertNoReconciliationViolationsForWallet은 기존 reconciliation_worker_integration_test.go의
// worker.RunOnce() + subject_key 필터 패턴을 재사용한다(공유 테스트 DB이므로 전역 건수는
// 단언하지 않는다). "legacy_mismatch"는 실패로 세지 않는다 — 이 테스트들은
// seedCancelOrderRows/db.Create로 지갑을 직접 시딩해(펀딩 원장 항목 없음)
// classifyLedgerWalletRow(reconciliation_worker.go)가 정의한 대로 "레거시 초기 잔고"로
// 정상 분류되는 갭이 생긴다(reconciliation_worker_integration_test.go도 동일 패턴을
// legacy_mismatch로 단언). 여기서 잡아야 할 실제 결함은 "ledger_wallet" 분류뿐이다.
func assertNoReconciliationViolationsForWallet(t *testing.T, db *gorm.DB, userID uint, coinSymbol string) {
	t.Helper()

	walletRepo := repository.NewWalletRepository(db)
	var wallet *model.Wallet
	var err error
	if coinSymbol == model.KRWAssetSymbol {
		wallet, err = walletRepo.FindKRWWalletByUserID(userID)
	} else {
		wallet, err = walletRepo.FindByUserIDAndCoinSymbol(userID, coinSymbol)
	}
	require.NoError(t, err)

	subject := fmt.Sprintf("wallet:%d", wallet.ID)
	t.Cleanup(func() {
		require.NoError(t, db.Where("subject_key = ?", subject).Delete(&model.ReconciliationViolation{}).Error)
	})

	worker := &ReconciliationWorker{
		Repository: repository.NewReconciliationRepository(db),
		Logger:     discardServiceLogger(),
	}
	worker.RunOnce()

	var realViolations []model.ReconciliationViolation
	for _, v := range findViolationsBySubject(t, db, []string{subject})[subject] {
		if v.CheckName != "legacy_mismatch" {
			realViolations = append(realViolations, v)
		}
	}
	assert.Empty(t, realViolations, "정산 후 지갑에 실제(legacy_mismatch가 아닌) 리컨실리에이션 위반이 없어야 한다")
}
