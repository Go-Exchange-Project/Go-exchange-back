package service

import (
	"sync"
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func requireCancelCommand(t *testing.T, db *gorm.DB, commandID uint64) model.CancelCommand {
	t.Helper()
	var command model.CancelCommand
	require.NoError(t, db.Where("id = ?", commandID).First(&command).Error)
	return command
}

func cleanupServiceCancelCommands(t *testing.T, db *gorm.DB, ids ...uint64) {
	t.Helper()
	if len(ids) == 0 {
		return
	}
	require.NoError(t, db.Where("id IN ?", ids).Delete(&model.CancelCommand{}).Error)
}

func assertNoCancelCommandForOrder(t *testing.T, db *gorm.DB, orderID uint) {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(&model.CancelCommand{}).
		Where("order_id = ?", orderID).Count(&count).Error)
	assert.Zero(t, count, "거부된 요청이 command를 남겼다")
}

func seedCancellableBuyOrder(t *testing.T, db *gorm.DB, userID uint) model.Order {
	t.Helper()
	return seedCancelOrderRows(t, db, cancelOrderSeed{
		UserID:        userID,
		CoinSymbol:    "BTC",
		Side:          model.OrderSideBuy,
		Status:        model.OrderStatusPending,
		Price:         decimal.NewFromInt(100),
		Amount:        decimal.NewFromInt(5),
		FilledAmount:  decimal.Zero,
		LockedBalance: decimal.RequireFromString("500.25"),
	})
}

// wake는 command가 커밋된 뒤에 호출돼야 한다. 먼저 부르면 worker가 아직 보이지
// 않는 행을 찾으러 가고, 그 사이 프로세스가 죽으면 접수만 알리고 기록은 없다.
func TestIntegrationCancelCommitsCommandBeforeWake(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(20)
	defer cleanupServiceUsers(t, db, userID)

	order := seedCancellableBuyOrder(t, db, userID)
	orderService := newIntegrationOrderService(db, nil)

	var wakeCount int
	var visibleAtWake int64
	orderService.CancelCommandWake = func() {
		wakeCount++
		require.NoError(t, db.Model(&model.CancelCommand{}).
			Where("order_id = ?", order.ID).Count(&visibleAtWake).Error)
	}

	result, err := orderService.CancelOrder(CancelOrderInput{UserID: userID, OrderID: order.ID})
	require.NoError(t, err)
	defer cleanupServiceCancelCommands(t, db, result.CommandID)

	assert.Equal(t, 1, wakeCount)
	assert.EqualValues(t, 1, visibleAtWake, "wake 시점에 command가 아직 커밋되지 않았다")
}

// wake가 없어도 command는 이미 내구 기록됐다. worker의 polling이 복구한다.
func TestIntegrationCancelSucceedsWithoutWakeCallback(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(21)
	defer cleanupServiceUsers(t, db, userID)

	order := seedCancellableBuyOrder(t, db, userID)
	orderService := newIntegrationOrderService(db, nil)
	orderService.CancelCommandWake = nil

	result, err := orderService.CancelOrder(CancelOrderInput{UserID: userID, OrderID: order.ID})
	require.NoError(t, err)
	defer cleanupServiceCancelCommands(t, db, result.CommandID)

	command := requireCancelCommand(t, db, result.CommandID)
	assert.Equal(t, model.CancelCommandStatusPending, command.Status)
}

// 같은 주문에 취소가 몰려도 command는 하나다. 두 개가 생기면 정산이 hold를 두 번
// 풀어 ORDER_RELEASE가 두 건 남는다.
func TestIntegrationCancelConcurrentRequestsShareOneCommand(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(22)
	defer cleanupServiceUsers(t, db, userID)

	order := seedCancellableBuyOrder(t, db, userID)
	orderService := newIntegrationOrderService(db, nil)

	const concurrency = 100
	commandIDs := make([]uint64, concurrency)
	errs := make([]error, concurrency)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			result, err := orderService.CancelOrder(CancelOrderInput{UserID: userID, OrderID: order.ID})
			if err != nil {
				errs[i] = err
				return
			}
			commandIDs[i] = result.CommandID
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "goroutine %d", i)
	}
	defer cleanupServiceCancelCommands(t, db, commandIDs[0])

	for i, id := range commandIDs {
		assert.Equal(t, commandIDs[0], id, "goroutine %d가 다른 command를 받았다", i)
	}

	var count int64
	require.NoError(t, db.Model(&model.CancelCommand{}).
		Where("order_id = ?", order.ID).Count(&count).Error)
	assert.EqualValues(t, 1, count)
}

// command가 PROCESSED인데 정산이 아직 안 끝난 창. 여기서 두 번째 command가 생기면
// ORDER_RELEASE가 두 번 난다 — order_id 전체 UNIQUE가 막는 지점이다.
func TestIntegrationCancelReturnsExistingCommandWhileSettlementPending(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(23)
	defer cleanupServiceUsers(t, db, userID)

	order := seedCancellableBuyOrder(t, db, userID)
	orderService := newIntegrationOrderService(db, nil)

	first, err := orderService.CancelOrder(CancelOrderInput{UserID: userID, OrderID: order.ID})
	require.NoError(t, err)
	defer cleanupServiceCancelCommands(t, db, first.CommandID)

	// 엔진 제거와 outbox 커밋까지 끝났지만 정산은 아직 주문을 CANCELLED로 만들지 않았다.
	require.NoError(t, db.Model(&model.CancelCommand{}).Where("id = ?", first.CommandID).
		Update("status", model.CancelCommandStatusProcessed).Error)

	second, err := orderService.CancelOrder(CancelOrderInput{UserID: userID, OrderID: order.ID})
	require.NoError(t, err)
	assert.Equal(t, first.CommandID, second.CommandID)

	var count int64
	require.NoError(t, db.Model(&model.CancelCommand{}).
		Where("order_id = ?", order.ID).Count(&count).Error)
	assert.EqualValues(t, 1, count)
}

// 시장가 주문은 취소 대상이 아니다. command를 만들기 전 검증 단계에서 걸러야 한다.
func TestIntegrationCancelMarketOrderIsRejectedWithoutCommand(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(24)
	defer cleanupServiceUsers(t, db, userID)

	order := seedCancellableBuyOrder(t, db, userID)
	// ck_orders_shape_by_type: MARKET BUY는 price=0, amount=0, quote_amount>0이다.
	require.NoError(t, db.Model(&model.Order{}).Where("id = ?", order.ID).
		Updates(map[string]any{
			"order_type":   model.OrderTypeMarket,
			"price":        decimal.Zero,
			"amount":       decimal.Zero,
			"quote_amount": decimal.NewFromInt(500),
		}).Error)

	orderService := newIntegrationOrderService(db, nil)
	result, err := orderService.CancelOrder(CancelOrderInput{UserID: userID, OrderID: order.ID})

	require.Error(t, err)
	assert.Nil(t, result)
	kind, ok := DomainErrorKind(err)
	require.True(t, ok)
	assert.Equal(t, ErrorKindConflict, kind)

	assertNoCancelCommandForOrder(t, db, order.ID)
}

// 소유자가 아닌 사용자의 요청은 403이고 command도 남기지 않는다.
func TestIntegrationCancelOtherUserOrderLeavesNoCommand(t *testing.T) {
	db := openServiceIntegrationDB(t)
	ownerID := serviceTestUserID(25)
	otherID := serviceTestUserID(26)
	defer cleanupServiceUsers(t, db, ownerID, otherID)

	order := seedCancellableBuyOrder(t, db, ownerID)
	orderService := newIntegrationOrderService(db, nil)

	result, err := orderService.CancelOrder(CancelOrderInput{UserID: otherID, OrderID: order.ID})

	require.Error(t, err)
	assert.Nil(t, result)
	kind, ok := DomainErrorKind(err)
	require.True(t, ok)
	assert.Equal(t, ErrorKindForbidden, kind)

	assertNoCancelCommandForOrder(t, db, order.ID)
}

// 주문 최종 상태가 반영된 뒤의 재요청은 409다. terminal 검증이 command 조회보다
// 앞서야 이 분기가 성립한다.
func TestIntegrationCancelAfterOrderTerminalIsRejected(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(27)
	defer cleanupServiceUsers(t, db, userID)

	order := seedCancellableBuyOrder(t, db, userID)
	orderService := newIntegrationOrderService(db, nil)

	first, err := orderService.CancelOrder(CancelOrderInput{UserID: userID, OrderID: order.ID})
	require.NoError(t, err)
	defer cleanupServiceCancelCommands(t, db, first.CommandID)

	require.NoError(t, db.Model(&model.CancelCommand{}).Where("id = ?", first.CommandID).
		Update("status", model.CancelCommandStatusProcessed).Error)
	require.NoError(t, db.Model(&model.Order{}).Where("id = ?", order.ID).
		Update("status", model.OrderStatusCancelled).Error)

	second, err := orderService.CancelOrder(CancelOrderInput{UserID: userID, OrderID: order.ID})

	require.Error(t, err)
	assert.Nil(t, second)
	kind, ok := DomainErrorKind(err)
	require.True(t, ok)
	assert.Equal(t, ErrorKindConflict, kind)
}
