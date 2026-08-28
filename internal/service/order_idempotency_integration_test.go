package service

import (
	"fmt"
	"sync"
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/testdb"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func newIdemOrderService(db *gorm.DB, engine *countingAcceptanceEngine) *OrderService {
	return NewOrderService(repository.NewOrderRepository(db), repository.NewWalletRepository(db), engine)
}

func rejectingIdemEngine() *countingAcceptanceEngine {
	return &countingAcceptanceEngine{
		fakeAcceptanceEngine: fakeAcceptanceEngine{admissible: true, submitSucceeds: false},
	}
}

func idemRecordOf(t *testing.T, db *gorm.DB, userID uint) model.OrderIdempotencyKey {
	t.Helper()
	var record model.OrderIdempotencyKey
	require.NoError(t, db.Where("user_id = ?", userID).First(&record).Error)
	return record
}

func lockedKRWOf(t *testing.T, db *gorm.DB, userID uint) string {
	t.Helper()
	var wallet model.Wallet
	require.NoError(t, db.Where("user_id = ? AND coin_symbol = ?", userID, model.KRWAssetSymbol).
		First(&wallet).Error)
	return wallet.LockedBalance.String()
}

// 검증 4. 애플리케이션 lock 없이 DB UNIQUE가 직렬화한다. 판정은 주문 상태가 아니라
// 원장 hold 건수와 엔진 제출 횟수다 — 상태만 보면 "hold 1회·엔진 제출 2회"를 놓친다.
func TestIntegrationCreateOrderConcurrentSameKeyCreatesOneOrder(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(790)
	defer cleanupServiceUsers(t, db, userID)
	defer cleanupIdemKeys(t, db, userID)
	seedIdemBuyerWallet(t, db, userID, 100_000_000)

	engine := &countingAcceptanceEngine{
		fakeAcceptanceEngine: fakeAcceptanceEngine{admissible: true, submitSucceeds: true},
	}
	orderService := newIdemOrderService(db, engine)

	const concurrency = 100
	results := make([]*CreateOrderResult, concurrency)
	errs := make([]error, concurrency)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 전부 같은 순간에 출발시켜 UNIQUE 경합을 실제로 만든다
			results[i], errs[i] = orderService.CreateOrder(idemOrderInput(userID, "concurrent-key", "1"))
		}(i)
	}
	close(start)
	wg.Wait()

	owners := 0
	var orderID uint
	for i := range results {
		require.NoError(t, errs[i], "goroutine %d", i)
		require.NotNil(t, results[i])
		require.NotZero(t, results[i].Order.ID, "goroutine %d가 order_id 없는 결과를 받았다", i)
		if orderID == 0 {
			orderID = results[i].Order.ID
		}
		assert.Equal(t, orderID, results[i].Order.ID, "goroutine %d가 다른 주문을 받았다", i)
		if !results[i].Replay {
			owners++
		}
	}
	assert.Equal(t, 1, owners, "owner가 하나가 아니다")

	assert.EqualValues(t, 1, countOrders(t, db, userID))
	assert.EqualValues(t, 1, countIdemKeys(t, db, userID))
	assert.EqualValues(t, 1, countHoldEntries(t, db, orderID), "hold가 두 번 잡혔다")
	assert.EqualValues(t, 1, engine.submits.Load(), "엔진에 두 번 제출됐다")
}

// 검증 7c. 배치의 모든 요청이 검증에 실패하면 커밋할 성공분이 없다. 이때도 선점된 키가
// 남으면 안 된다 — 남으면 사용자가 그 키를 영원히 다시 쓸 수 없다.
func TestIntegrationHoldBatchAllFailingCleansKeys(t *testing.T) {
	db := openServiceIntegrationDB(t)
	first := serviceTestUserID(791)
	second := serviceTestUserID(792)
	defer cleanupServiceUsers(t, db, first, second)
	defer cleanupIdemKeys(t, db, first)
	defer cleanupIdemKeys(t, db, second)
	// seedHoldWallets의 두 번째 인자가 KRW 0이다. 둘 다 0으로 만들려고 두 번 부른다.
	seedHoldWallets(t, db, serviceTestUserID(793), first)
	seedHoldWallets(t, db, serviceTestUserID(794), second)

	coordinator := newIdemHoldCoordinator(db)
	results, err := coordinator.HoldBatch([]holdRequest{
		idemHoldRequest(idemBuyOrder(first, 1), "all-fail-1", "fp-1"),
		idemHoldRequest(idemBuyOrder(second, 1), "all-fail-2", "fp-2"),
	})
	require.NoError(t, err, "전건 실패가 배치 자체를 실패시켰다")
	require.Len(t, results, 2)
	require.Error(t, results[0].Err, "잔고가 없는데 hold가 성공했다")
	require.Error(t, results[1].Err, "잔고가 없는데 hold가 성공했다")

	assert.EqualValues(t, 0, countIdemKeys(t, db, first), "전건 실패인데 키가 남았다")
	assert.EqualValues(t, 0, countIdemKeys(t, db, second), "전건 실패인데 키가 남았다")
	assert.EqualValues(t, 0, countOrders(t, db, first))
	assert.EqualValues(t, 0, countOrders(t, db, second))
}

// 검증 8. 접수 실패로 REJECTED가 된 주문은 되돌리기가 끝난 상태다. 같은 키의 재요청은
// 새 주문을 만들지 않고 그 주문을 그대로 가리켜야 한다.
func TestIntegrationCreateOrderRejectedReplaysSameOrder(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(795)
	defer cleanupServiceUsers(t, db, userID)
	defer cleanupIdemKeys(t, db, userID)
	seedIdemBuyerWallet(t, db, userID, 10000)

	engine := rejectingIdemEngine()
	orderService := newIdemOrderService(db, engine)

	first, err := orderService.CreateOrder(idemOrderInput(userID, "rejected-key", "1"))
	require.Error(t, err, "엔진이 거절했는데 주문이 접수됐다")
	require.NotNil(t, first, "최초 실패가 order_id 없는 결과를 돌려줬다")
	require.NotZero(t, first.Order.ID)
	assert.Equal(t, model.OrderIdempotencyOutcomeRejected, first.Outcome)
	assert.Equal(t, model.OrderIdempotencyOutcomeRejected, idemRecordOf(t, db, userID).Outcome)

	replay, err := orderService.CreateOrder(idemOrderInput(userID, "rejected-key", "1"))
	require.NoError(t, err, "저장된 REJECTED 결과의 재요청이 오류가 됐다")
	require.NotNil(t, replay)
	assert.True(t, replay.Replay)
	assert.Equal(t, first.Order.ID, replay.Order.ID, "재요청이 다른 주문을 가리켰다")
	assert.Equal(t, model.OrderIdempotencyOutcomeRejected, replay.Outcome)

	assert.EqualValues(t, 1, countOrders(t, db, userID), "재요청이 새 주문을 만들었다")
	assert.EqualValues(t, 1, countHoldEntries(t, db, first.Order.ID))
	assert.EqualValues(t, 1, engine.submits.Load(), "재요청이 엔진에 다시 제출됐다")
}

// 검증 8b. 보상은 hold 해제·주문 REJECTED·outcome REJECTED가 한 트랜잭션이다.
// 실패하면 부분 반영이 0이어야 한다 — "hold는 풀렸는데 주문은 그대로"가 남으면 안 된다.
func TestIntegrationCreateOrderCompensationIsAtomic(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(796)
	defer cleanupServiceUsers(t, db, userID)
	defer cleanupIdemKeys(t, db, userID)
	seedIdemBuyerWallet(t, db, userID, 10000)

	orderService := newIdemOrderService(db, rejectingIdemEngine())

	// 주문의 REJECTED 전이만 막는다. 보상 트랜잭션이 통째로 롤백되고, 트랜잭션 밖의
	// UNKNOWN 기록은 성공한다.
	testdb.BlockUpdate(t, db, "orders",
		fmt.Sprintf("NEW.status = 'REJECTED' AND NEW.user_id = %d", userID))

	result, err := orderService.CreateOrder(idemOrderInput(userID, "atomic-key", "1"))
	require.Error(t, err)
	require.NotNil(t, result)
	require.NotZero(t, result.Order.ID)
	assert.Equal(t, model.OrderIdempotencyOutcomeUnknown, result.Outcome)

	// 부분 반영 0: 둘 다 보상 전 상태 그대로여야 한다.
	var order model.Order
	require.NoError(t, db.First(&order, result.Order.ID).Error)
	assert.Equal(t, model.OrderStatusPending, order.Status,
		"주문만 REJECTED로 넘어갔다 — 보상이 부분 반영됐다")
	assert.NotEqual(t, "0", lockedKRWOf(t, db, userID),
		"hold만 풀렸다 — 보상이 부분 반영됐다")

	assert.Equal(t, model.OrderIdempotencyOutcomeUnknown, idemRecordOf(t, db, userID).Outcome,
		"보상 실패를 UNKNOWN으로 남기지 않았다")
}

// 검증 8d. UNKNOWN 기록마저 실패하면 durable outcome은 PENDING에 머문다. 이 경로는
// 로그 말고는 흔적이 없으므로 counter가 올라야 한다.
func TestIntegrationCreateOrderUnknownUpdateFailureKeepsPending(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(797)
	defer cleanupServiceUsers(t, db, userID)
	defer cleanupIdemKeys(t, db, userID)
	seedIdemBuyerWallet(t, db, userID, 10000)

	orderService := newIdemOrderService(db, rejectingIdemEngine())

	testdb.BlockUpdate(t, db, "orders",
		fmt.Sprintf("NEW.status = 'REJECTED' AND NEW.user_id = %d", userID))
	testdb.BlockUpdate(t, db, "order_idempotency_keys",
		fmt.Sprintf("NEW.outcome = 'UNKNOWN' AND NEW.user_id = %d", userID))

	before := testutil.ToFloat64(metrics.OrderIdempotencyOutcomeUpdateFailuresTotal)

	result, err := orderService.CreateOrder(idemOrderInput(userID, "still-pending-key", "1"))
	require.Error(t, err)
	require.NotNil(t, result)
	assert.Equal(t, model.OrderIdempotencyOutcomePending, result.Outcome,
		"UNKNOWN 기록이 실패했는데 결과가 UNKNOWN이라고 말했다")

	assert.Equal(t, model.OrderIdempotencyOutcomePending, idemRecordOf(t, db, userID).Outcome,
		"저장된 outcome이 PENDING이 아니다")
	assert.EqualValues(t, 1, countOrders(t, db, userID))
	assert.Equal(t, before+1, testutil.ToFloat64(metrics.OrderIdempotencyOutcomeUpdateFailuresTotal),
		"UNKNOWN 기록 실패가 counter에 잡히지 않았다")
}

// 검증 8e. 엔진 접수는 성공했는데 ACCEPTED 기록만 실패하면 저장된 outcome은 PENDING이다.
// 재요청은 그 PENDING을 그대로 돌려줘야 한다 — 다시 hold하거나 다시 제출하면 안 된다.
func TestIntegrationCreateOrderAcceptedUpdateFailureKeepsPending(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(798)
	defer cleanupServiceUsers(t, db, userID)
	defer cleanupIdemKeys(t, db, userID)
	seedIdemBuyerWallet(t, db, userID, 10000)

	engine := &countingAcceptanceEngine{
		fakeAcceptanceEngine: fakeAcceptanceEngine{admissible: true, submitSucceeds: true},
	}
	orderService := newIdemOrderService(db, engine)

	testdb.BlockUpdate(t, db, "order_idempotency_keys",
		fmt.Sprintf("NEW.outcome = 'ACCEPTED' AND NEW.user_id = %d", userID))

	before := testutil.ToFloat64(metrics.OrderIdempotencyOutcomeUpdateFailuresTotal)

	first, err := orderService.CreateOrder(idemOrderInput(userID, "accepted-fail-key", "1"))
	require.NoError(t, err, "ACCEPTED 기록 실패가 접수 자체를 실패시켰다")
	require.NotZero(t, first.Order.ID)
	assert.Equal(t, before+1, testutil.ToFloat64(metrics.OrderIdempotencyOutcomeUpdateFailuresTotal),
		"ACCEPTED 기록 실패가 counter에 잡히지 않았다")

	assert.Equal(t, model.OrderIdempotencyOutcomePending, idemRecordOf(t, db, userID).Outcome,
		"기록이 실패했는데 저장된 outcome이 PENDING이 아니다")

	replay, err := orderService.CreateOrder(idemOrderInput(userID, "accepted-fail-key", "1"))
	require.NoError(t, err)
	assert.True(t, replay.Replay)
	assert.Equal(t, first.Order.ID, replay.Order.ID)
	assert.Equal(t, model.OrderIdempotencyOutcomePending, replay.Outcome,
		"저장된 PENDING을 그대로 돌려주지 않았다")

	assert.EqualValues(t, 1, countOrders(t, db, userID))
	assert.EqualValues(t, 1, countHoldEntries(t, db, first.Order.ID), "재요청이 다시 hold했다")
	assert.EqualValues(t, 1, engine.submits.Load(), "재요청이 엔진에 다시 제출됐다")
}
