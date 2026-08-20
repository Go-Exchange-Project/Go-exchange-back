package repository

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// orderID는 UNIQUE라 테스트마다 겹치지 않아야 한다. 공유 test DB에 다른 태스크의
// 데이터가 남아 있을 수 있으므로 시간 기반으로 뽑는다.
func uniqueCancelOrderID() uint {
	return uint(time.Now().UnixNano() % 1_000_000_000)
}

func seedCancelCommand(orderID uint) *model.CancelCommand {
	return &model.CancelCommand{
		OrderID:    orderID,
		UserID:     7,
		CoinSymbol: "BTC",
		Side:       model.OrderSideBuy,
		Price:      decimal.RequireFromString("31234.56"),
		Status:     model.CancelCommandStatusPending,
	}
}

func cleanupCancelCommands(t *testing.T, db *gorm.DB, ids []uint64) {
	t.Helper()
	if len(ids) == 0 {
		return
	}
	require.NoError(t, db.Where("id IN ?", ids).Delete(&model.CancelCommand{}).Error)
}

func TestIntegrationCancelCommandCreateOrGetIsIdempotent(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewCancelCommandRepository(db)
	orderID := uniqueCancelOrderID()

	first, created, err := repo.CreateOrGet(seedCancelCommand(orderID))
	require.NoError(t, err)
	defer cleanupCancelCommands(t, db, []uint64{first.ID})

	require.True(t, created)
	require.NotZero(t, first.ID)

	second, created, err := repo.CreateOrGet(seedCancelCommand(orderID))
	require.NoError(t, err)

	assert.False(t, created, "같은 주문의 두 번째 요청은 새 command를 만들면 안 된다")
	assert.Equal(t, first.ID, second.ID)
	// 재구성에 필요한 필드가 기존 행에서 그대로 돌아와야 한다.
	assert.Equal(t, "BTC", second.CoinSymbol)
	assert.Equal(t, model.OrderSideBuy, second.Side)
	assert.True(t, second.Price.Equal(decimal.RequireFromString("31234.56")), "price=%s", second.Price)

	var count int64
	require.NoError(t, db.Model(&model.CancelCommand{}).Where("order_id = ?", orderID).Count(&count).Error)
	assert.EqualValues(t, 1, count)
}

// command가 PROCESSED이고 정산이 아직 안 끝난 창에서도 두 번째 command가 생기면 안 된다.
// 부분 UNIQUE였다면 여기서 두 번째 행이 만들어진다.
func TestIntegrationCancelCommandCreateOrGetReturnsProcessedCommand(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewCancelCommandRepository(db)
	orderID := uniqueCancelOrderID()

	first, _, err := repo.CreateOrGet(seedCancelCommand(orderID))
	require.NoError(t, err)
	defer cleanupCancelCommands(t, db, []uint64{first.ID})

	require.NoError(t, db.Model(&model.CancelCommand{}).Where("id = ?", first.ID).
		Update("status", model.CancelCommandStatusProcessed).Error)

	second, created, err := repo.CreateOrGet(seedCancelCommand(orderID))
	require.NoError(t, err)

	assert.False(t, created)
	assert.Equal(t, first.ID, second.ID)
	assert.Equal(t, model.CancelCommandStatusProcessed, second.Status)
}

func TestIntegrationCancelCommandCreateOrGetSurvivesConcurrentRequests(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewCancelCommandRepository(db)
	orderID := uniqueCancelOrderID()

	const concurrency = 24
	ids := make([]uint64, concurrency)
	errs := make([]error, concurrency)
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			command, _, err := repo.CreateOrGet(seedCancelCommand(orderID))
			if err != nil {
				errs[i] = err
				return
			}
			ids[i] = command.ID
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "goroutine %d", i)
	}
	defer cleanupCancelCommands(t, db, ids[:1])

	for i, id := range ids {
		assert.Equal(t, ids[0], id, "goroutine %d가 다른 command ID를 받았다", i)
	}

	var count int64
	require.NoError(t, db.Model(&model.CancelCommand{}).Where("order_id = ?", orderID).Count(&count).Error)
	assert.EqualValues(t, 1, count)
}

func TestIntegrationCancelCommandFindPendingOrdersByIDAndExcludesTerminal(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewCancelCommandRepository(db)

	var ids []uint64
	for i := 0; i < 3; i++ {
		command, _, err := repo.CreateOrGet(seedCancelCommand(uniqueCancelOrderID() + uint(i)))
		require.NoError(t, err)
		ids = append(ids, command.ID)
	}
	defer cleanupCancelCommands(t, db, ids)

	require.NoError(t, db.Model(&model.CancelCommand{}).Where("id = ?", ids[1]).
		Update("status", model.CancelCommandStatusProcessed).Error)

	pending, err := repo.FindPending(nil, 128)
	require.NoError(t, err)

	found := map[uint64]bool{}
	var previous uint64
	for _, command := range pending {
		assert.Equal(t, model.CancelCommandStatusPending, command.Status)
		assert.Greater(t, command.ID, previous, "ID 오름차순이 아니다")
		previous = command.ID
		found[command.ID] = true
	}

	assert.True(t, found[ids[0]])
	assert.False(t, found[ids[1]], "PROCESSED가 PENDING 스캔에 포함됐다")
	assert.True(t, found[ids[2]])

	limited, err := repo.FindPending(nil, 1)
	require.NoError(t, err)
	assert.Len(t, limited, 1)
}

func TestIntegrationCancelCommandFindStatusesReturnsOnlyRequestedIDs(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewCancelCommandRepository(db)

	first, _, err := repo.CreateOrGet(seedCancelCommand(uniqueCancelOrderID()))
	require.NoError(t, err)
	second, _, err := repo.CreateOrGet(seedCancelCommand(uniqueCancelOrderID() + 1))
	require.NoError(t, err)
	defer cleanupCancelCommands(t, db, []uint64{first.ID, second.ID})

	require.NoError(t, db.Model(&model.CancelCommand{}).Where("id = ?", second.ID).
		Update("status", model.CancelCommandStatusProcessed).Error)

	statuses, err := repo.FindStatuses([]uint64{first.ID, second.ID})
	require.NoError(t, err)
	require.Len(t, statuses, 2)

	byID := map[uint64]model.CancelCommand{}
	for _, command := range statuses {
		byID[command.ID] = command
	}
	assert.Equal(t, model.CancelCommandStatusPending, byID[first.ID].Status)
	assert.Equal(t, model.CancelCommandStatusProcessed, byID[second.ID].Status)
	assert.False(t, byID[first.ID].CreatedAt.IsZero(), "latency 관측에 created_at이 필요하다")
	assert.False(t, byID[second.ID].UpdatedAt.IsZero())

	empty, err := repo.FindStatuses(nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestIntegrationCancelCommandMarkNoopOnlyAffectsPending(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewCancelCommandRepository(db)

	pending, _, err := repo.CreateOrGet(seedCancelCommand(uniqueCancelOrderID()))
	require.NoError(t, err)
	processed, _, err := repo.CreateOrGet(seedCancelCommand(uniqueCancelOrderID() + 1))
	require.NoError(t, err)
	defer cleanupCancelCommands(t, db, []uint64{pending.ID, processed.ID})

	require.NoError(t, db.Model(&model.CancelCommand{}).Where("id = ?", processed.ID).
		Update("status", model.CancelCommandStatusProcessed).Error)

	marked, err := repo.MarkNoop(pending.ID)
	require.NoError(t, err)
	require.NotNil(t, marked)
	assert.Equal(t, model.CancelCommandStatusNoop, marked.Status)
	assert.False(t, marked.CreatedAt.IsZero(), "latency 관측에 created_at이 필요하다")

	// 이미 PROCESSED인 command를 NOOP으로 덮으면 outbox가 이미 만든 사실을 지운다.
	untouched, err := repo.MarkNoop(processed.ID)
	require.NoError(t, err)
	assert.Nil(t, untouched, "PROCESSED는 NOOP으로 덮어쓰면 안 된다")

	var status model.CancelCommandStatus
	require.NoError(t, db.Model(&model.CancelCommand{}).Where("id = ?", processed.ID).
		Select("status").Scan(&status).Error)
	assert.Equal(t, model.CancelCommandStatusProcessed, status)

	missing, err := repo.MarkNoop(pending.ID + 90_000_000)
	require.NoError(t, err)
	assert.Nil(t, missing)
}

func TestIntegrationCancelCommandRecordAttemptIncrementsAtomically(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewCancelCommandRepository(db)

	command, _, err := repo.CreateOrGet(seedCancelCommand(uniqueCancelOrderID()))
	require.NoError(t, err)
	defer cleanupCancelCommands(t, db, []uint64{command.ID})

	const attempts = 12
	var wg sync.WaitGroup
	errs := make([]error, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = repo.RecordAttempt(command.ID, fmt.Sprintf("engine timeout %d", i))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "goroutine %d", i)
	}

	var got model.CancelCommand
	require.NoError(t, db.Where("id = ?", command.ID).First(&got).Error)

	// attempt_count는 관측용이다. 재시도 예산이 아니므로 상한을 두지 않는다.
	assert.Equal(t, attempts, got.AttemptCount)
	assert.NotEmpty(t, got.LastError)
	assert.Equal(t, model.CancelCommandStatusPending, got.Status, "재시도는 상태를 바꾸지 않는다")
}

func TestIntegrationCancelCommandCountPending(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewCancelCommandRepository(db)

	before, err := repo.CountPending()
	require.NoError(t, err)

	command, _, err := repo.CreateOrGet(seedCancelCommand(uniqueCancelOrderID()))
	require.NoError(t, err)
	defer cleanupCancelCommands(t, db, []uint64{command.ID})

	after, err := repo.CountPending()
	require.NoError(t, err)
	assert.Equal(t, before+1, after)

	require.NoError(t, db.Model(&model.CancelCommand{}).Where("id = ?", command.ID).
		Update("status", model.CancelCommandStatusProcessed).Error)

	drained, err := repo.CountPending()
	require.NoError(t, err)
	assert.Equal(t, before, drained)
}

// 제외는 LIMIT보다 먼저 적용돼야 한다. 조회 후 애플리케이션에서 빼면 앞선
// LIMIT개가 전부 in-flight일 때 그 뒤의 command가 영구히 조회되지 않는다.
func TestIntegrationCancelCommandFindPendingExcludesBeforeLimit(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewCancelCommandRepository(db)

	var ids []uint64
	for i := 0; i < 3; i++ {
		command, _, err := repo.CreateOrGet(seedCancelCommand(uniqueCancelOrderID() + uint(i)))
		require.NoError(t, err)
		ids = append(ids, command.ID)
	}
	defer cleanupCancelCommands(t, db, ids)

	// 앞의 두 개를 제외한 뒤 한 건만 요청하면 세 번째가 나와야 한다.
	// 제외가 LIMIT 뒤였다면 결과는 비어 있다.
	pending, err := repo.FindPending([]uint64{ids[0], ids[1]}, 1)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, ids[2], pending[0].ID)
}
