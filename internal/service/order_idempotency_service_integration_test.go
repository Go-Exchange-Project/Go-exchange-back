package service

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// countingAcceptanceEngine은 엔진 제출 횟수를 센다. 상태만 보면 "두 번 제출됐지만
// 결과가 같아 보이는" 경우를 구분할 수 없다(B-1에서 확인).
type countingAcceptanceEngine struct {
	fakeAcceptanceEngine
	submits atomic.Int64
}

func (e *countingAcceptanceEngine) TrySubmitOrder(order *matching.Order, within time.Duration) bool {
	e.submits.Add(1)
	return e.submitSucceeds
}

func seedIdemBuyerWallet(t *testing.T, db *gorm.DB, userID uint, krw int64) {
	t.Helper()
	require.NoError(t, db.Create(&model.Wallet{
		UserID: userID, CoinSymbol: model.KRWAssetSymbol,
		KRW:              decimal.NewFromInt(krw),
		AvailableBalance: decimal.NewFromInt(krw),
		LockedBalance:    decimal.Zero,
	}).Error)
}

func idemOrderInput(userID uint, key, amount string) CreateOrderInput {
	return CreateOrderInput{
		UserID: userID, CoinSymbol: "BTC", Side: "BUY", OrderType: "LIMIT",
		Price: "100", Amount: amount, IdempotencyKey: key,
	}
}

func countHoldEntries(t *testing.T, db *gorm.DB, orderID uint) int64 {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(&model.LedgerEntry{}).
		Where("reference_id = ? AND entry_type = ?", orderID, model.LedgerEntryTypeOrderHold).
		Count(&count).Error)
	return count
}

// 같은 키의 순차 재시도는 새 주문을 만들지 않고 저장된 결과를 돌려준다.
func TestIntegrationCreateOrderRetryWithSameKeyReplays(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(780)
	defer cleanupServiceUsers(t, db, userID)
	defer cleanupIdemKeys(t, db, userID)
	seedIdemBuyerWallet(t, db, userID, 10000)

	engine := &countingAcceptanceEngine{fakeAcceptanceEngine: fakeAcceptanceEngine{admissible: true, submitSucceeds: true}}
	orderService := NewOrderService(repository.NewOrderRepository(db), repository.NewWalletRepository(db), engine)

	first, err := orderService.CreateOrder(idemOrderInput(userID, "retry-key", "1"))
	require.NoError(t, err)
	require.NotZero(t, first.Order.ID)
	assert.False(t, first.Replay)
	assert.Equal(t, model.OrderIdempotencyOutcomeAccepted, first.Outcome)

	second, err := orderService.CreateOrder(idemOrderInput(userID, "retry-key", "1"))
	require.NoError(t, err)
	assert.True(t, second.Replay, "재시도가 새 주문을 만들었다")
	assert.Equal(t, first.Order.ID, second.Order.ID)
	assert.Equal(t, model.OrderIdempotencyOutcomeAccepted, second.Outcome)

	// 판정은 상태가 아니라 횟수로 한다.
	assert.EqualValues(t, 1, engine.submits.Load(), "재시도가 엔진에 다시 제출됐다")
	assert.EqualValues(t, 1, countHoldEntries(t, db, first.Order.ID), "hold가 두 번 잡혔다")
	assert.EqualValues(t, 1, countOrders(t, db, userID))
}

// 같은 키를 다른 요청에 쓰면 409다. 원래 주문은 그대로여야 한다.
func TestIntegrationCreateOrderSameKeyDifferentRequestConflicts(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(781)
	defer cleanupServiceUsers(t, db, userID)
	defer cleanupIdemKeys(t, db, userID)
	seedIdemBuyerWallet(t, db, userID, 10000)

	engine := &countingAcceptanceEngine{fakeAcceptanceEngine: fakeAcceptanceEngine{admissible: true, submitSucceeds: true}}
	orderService := NewOrderService(repository.NewOrderRepository(db), repository.NewWalletRepository(db), engine)

	first, err := orderService.CreateOrder(idemOrderInput(userID, "conflict-key", "1"))
	require.NoError(t, err)

	result, err := orderService.CreateOrder(idemOrderInput(userID, "conflict-key", "2"))
	require.Error(t, err)
	assert.Nil(t, result)
	kind, ok := DomainErrorKind(err)
	require.True(t, ok)
	assert.Equal(t, ErrorKindConflict, kind)

	assert.EqualValues(t, 1, countOrders(t, db, userID))
	var persisted model.Order
	require.NoError(t, db.First(&persisted, first.Order.ID).Error)
	assert.True(t, persisted.Amount.Equal(decimal.NewFromInt(1)), "원래 주문이 바뀌었다")
}

// 검증 실패는 키를 소비하지 않는다. 잔고를 채운 뒤 같은 키로 다시 요청하면 성공해야 한다.
func TestIntegrationCreateOrderFailedValidationDoesNotConsumeKey(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(782)
	defer cleanupServiceUsers(t, db, userID)
	defer cleanupIdemKeys(t, db, userID)
	seedIdemBuyerWallet(t, db, userID, 10)

	engine := &countingAcceptanceEngine{fakeAcceptanceEngine: fakeAcceptanceEngine{admissible: true, submitSucceeds: true}}
	walletRepo := repository.NewWalletRepository(db)
	orderService := NewOrderService(repository.NewOrderRepository(db), walletRepo, engine)

	_, err := orderService.CreateOrder(idemOrderInput(userID, "reusable-key", "1"))
	require.Error(t, err, "잔고가 없는데 주문이 생성됐다")
	assert.EqualValues(t, 0, countIdemKeys(t, db, userID), "실패한 요청이 키를 소비했다")

	require.NoError(t, db.Model(&model.Wallet{}).
		Where("user_id = ? AND coin_symbol = ?", userID, model.KRWAssetSymbol).
		Updates(map[string]any{"krw": 10000, "available_balance": 10000}).Error)

	result, err := orderService.CreateOrder(idemOrderInput(userID, "reusable-key", "1"))
	require.NoError(t, err, "같은 키가 소비돼 재시도가 막혔다")
	assert.False(t, result.Replay)
	assert.NotZero(t, result.Order.ID)
}

// 같은 배치에 들어간 중복은 503이 아니다. owner는 ACCEPTED, follower는 PENDING이고
// hold와 엔진 제출은 각각 1회다.
func TestIntegrationCreateOrderSameBatchDuplicateIsNotUnavailable(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(783)
	defer cleanupServiceUsers(t, db, userID)
	defer cleanupIdemKeys(t, db, userID)
	seedIdemBuyerWallet(t, db, userID, 10000)

	orderRepo := repository.NewOrderRepository(db)
	walletRepo := repository.NewWalletRepository(db)
	ledgerRepo := repository.NewLedgerRepository(db)

	// 두 요청이 같은 배치에 들어가도록 배치 크기 2, flush 간격을 넉넉히 준다.
	coordinator := NewHoldCoordinator(db, orderRepo, walletRepo, ledgerRepo,
		repository.NewOrderIdempotencyRepository(db), 2)
	coordinator.FlushInterval = 2 * time.Second
	go coordinator.Run()
	defer coordinator.Shutdown()

	engine := &countingAcceptanceEngine{fakeAcceptanceEngine: fakeAcceptanceEngine{admissible: true, submitSucceeds: true}}
	orderService := NewOrderService(orderRepo, walletRepo, engine)
	orderService.HoldCoordinator = coordinator

	var wg sync.WaitGroup
	results := make([]*CreateOrderResult, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = orderService.CreateOrder(idemOrderInput(userID, "batch-key", "1"))
		}(i)
	}
	wg.Wait()

	require.NoError(t, errs[0])
	require.NoError(t, errs[1], "같은 배치의 중복 요청이 실패했다 — 503이면 Existing nil 분기다")

	// 도착 순서는 정해지지 않는다. owner 1건 + follower 1건이면 된다.
	var owner, follower *CreateOrderResult
	for _, res := range results {
		require.NotNil(t, res)
		if res.Replay {
			follower = res
		} else {
			owner = res
		}
	}
	require.NotNil(t, owner, "owner가 없다")
	require.NotNil(t, follower, "follower가 없다 — 중복이 각자 주문을 만들었다")

	assert.Equal(t, model.OrderIdempotencyOutcomeAccepted, owner.Outcome)
	assert.Equal(t, model.OrderIdempotencyOutcomePending, follower.Outcome,
		"같은 배치 follower는 방금 커밋된 PENDING을 그대로 돌려준다")
	assert.Equal(t, owner.Order.ID, follower.Order.ID)

	assert.EqualValues(t, 1, engine.submits.Load(), "follower도 엔진에 제출됐다")
	assert.EqualValues(t, 1, countHoldEntries(t, db, owner.Order.ID), "hold가 두 번 잡혔다")
	assert.EqualValues(t, 1, countOrders(t, db, userID))
}
