package repository

import (
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func uniqueIdemUserID() uint {
	return uint(time.Now().UnixNano() % 1_000_000_000)
}

func seedIdemRecord(userID uint, key string) *model.OrderIdempotencyKey {
	return &model.OrderIdempotencyKey{
		UserID:             userID,
		IdempotencyKey:     key,
		Fingerprint:        "fp-" + key,
		FingerprintVersion: 1,
		// outcome은 NOT NULL이다. 비워 두면 GORM이 빈 문자열을 넣어 CHECK에 걸린다.
		Outcome: model.OrderIdempotencyOutcomePending,
	}
}

func cleanupIdemRecords(t *testing.T, db *gorm.DB, userID uint) {
	t.Helper()
	require.NoError(t, db.Where("user_id = ?", userID).Delete(&model.OrderIdempotencyKey{}).Error)
}

// 배치 INSERT는 "어느 것이 실제로 들어갔는지"를 한 왕복에 알려줘야 한다.
// 요청마다 INSERT하면 배치의 존재 이유(왕복 절감)가 사라진다.
func TestIntegrationOrderIdempotencyInsertNewReturnsOnlyInserted(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewOrderIdempotencyRepository(db)
	userID := uniqueIdemUserID()
	defer cleanupIdemRecords(t, db, userID)

	first := seedIdemRecord(userID, "k1")
	inserted, err := repo.InsertNew([]*model.OrderIdempotencyKey{first})
	require.NoError(t, err)
	require.Len(t, inserted, 1)
	assert.Equal(t, first.ID, inserted[0])

	// 같은 키 재시도 + 새 키 → 새 키만 들어간다.
	dup := seedIdemRecord(userID, "k1")
	fresh := seedIdemRecord(userID, "k2")
	inserted, err = repo.InsertNew([]*model.OrderIdempotencyKey{dup, fresh})
	require.NoError(t, err)
	require.Len(t, inserted, 1)

	// ID는 실제로 들어간 레코드에 붙어야 한다. 반환 행을 슬라이스 순서대로 채우면
	// 충돌한 dup이 fresh의 ID를 갖고, follower가 owner의 행을 가리키게 된다.
	assert.Zero(t, dup.ID, "삽입되지 않은 레코드에 ID가 채워졌다")
	assert.NotZero(t, fresh.ID, "삽입된 레코드에 ID가 채워지지 않았다")
	assert.Equal(t, fresh.ID, inserted[0])

	var count int64
	require.NoError(t, db.Model(&model.OrderIdempotencyKey{}).
		Where("user_id = ?", userID).Count(&count).Error)
	assert.EqualValues(t, 2, count)
}

// 같은 배치에 같은 키가 두 번 오면 한 건만 들어가고, ID는 그중 하나에만 붙어야 한다.
// 둘 다 ID를 받으면 뒤쪽 요청이 owner처럼 행동해 엔진에 두 번 제출된다.
func TestIntegrationOrderIdempotencyInsertNewDeduplicatesWithinBatch(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewOrderIdempotencyRepository(db)
	userID := uniqueIdemUserID()
	defer cleanupIdemRecords(t, db, userID)

	first := seedIdemRecord(userID, "same")
	second := seedIdemRecord(userID, "same")
	inserted, err := repo.InsertNew([]*model.OrderIdempotencyKey{first, second})
	require.NoError(t, err)

	require.Len(t, inserted, 1)
	assert.NotZero(t, first.ID)
	assert.Zero(t, second.ID, "같은 배치의 중복 요청이 owner가 됐다")
	assert.Equal(t, first.ID, inserted[0])

	var count int64
	require.NoError(t, db.Model(&model.OrderIdempotencyKey{}).
		Where("user_id = ?", userID).Count(&count).Error)
	assert.EqualValues(t, 1, count)
}

// 다른 사용자의 같은 키는 충돌하지 않는다. 전역 UNIQUE였다면 여기서 막힌다.
func TestIntegrationOrderIdempotencyKeyScopeIsPerUser(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewOrderIdempotencyRepository(db)
	userA := uniqueIdemUserID()
	userB := userA + 1
	defer cleanupIdemRecords(t, db, userA)
	defer cleanupIdemRecords(t, db, userB)

	inserted, err := repo.InsertNew([]*model.OrderIdempotencyKey{
		seedIdemRecord(userA, "shared"),
		seedIdemRecord(userB, "shared"),
	})
	require.NoError(t, err)
	assert.Len(t, inserted, 2)
}

func TestIntegrationOrderIdempotencyFindByUserKeys(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewOrderIdempotencyRepository(db)
	userID := uniqueIdemUserID()
	defer cleanupIdemRecords(t, db, userID)

	_, err := repo.InsertNew([]*model.OrderIdempotencyKey{
		seedIdemRecord(userID, "k1"), seedIdemRecord(userID, "k2"),
	})
	require.NoError(t, err)

	found, err := repo.FindByUserKeys([]UserKeyPair{{UserID: userID, Key: "k1"}})
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, "k1", found[0].IdempotencyKey)
	assert.Equal(t, 1, found[0].FingerprintVersion)

	empty, err := repo.FindByUserKeys(nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// outcome과 updated_at은 한 UPDATE 문에서 함께 바뀌어야 한다.
func TestIntegrationOrderIdempotencyOutcomeUpdatesTouchUpdatedAt(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewOrderIdempotencyRepository(db)
	userID := uniqueIdemUserID()
	defer cleanupIdemRecords(t, db, userID)

	record := seedIdemRecord(userID, "k1")
	_, err := repo.InsertNew([]*model.OrderIdempotencyKey{record})
	require.NoError(t, err)

	var before model.OrderIdempotencyKey
	require.NoError(t, db.First(&before, record.ID).Error)

	time.Sleep(10 * time.Millisecond)
	require.NoError(t, repo.SetOrderAndOutcome(record.ID, 4242, model.OrderIdempotencyOutcomePending))

	var afterSet model.OrderIdempotencyKey
	require.NoError(t, db.First(&afterSet, record.ID).Error)
	require.NotNil(t, afterSet.OrderID)
	assert.EqualValues(t, 4242, *afterSet.OrderID)
	assert.Equal(t, model.OrderIdempotencyOutcomePending, afterSet.Outcome)
	assert.True(t, afterSet.UpdatedAt.After(before.UpdatedAt), "updated_at이 함께 갱신되지 않았다")

	time.Sleep(10 * time.Millisecond)
	require.NoError(t, repo.UpdateOutcome(record.ID, model.OrderIdempotencyOutcomeAccepted))

	var afterOutcome model.OrderIdempotencyKey
	require.NoError(t, db.First(&afterOutcome, record.ID).Error)
	assert.Equal(t, model.OrderIdempotencyOutcomeAccepted, afterOutcome.Outcome)
	assert.True(t, afterOutcome.UpdatedAt.After(afterSet.UpdatedAt))
}

// hold 검증에 실패한 미커밋 키는 지워야 재사용할 수 있다.
func TestIntegrationOrderIdempotencyDeleteByIDs(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewOrderIdempotencyRepository(db)
	userID := uniqueIdemUserID()
	defer cleanupIdemRecords(t, db, userID)

	keep := seedIdemRecord(userID, "keep")
	drop := seedIdemRecord(userID, "drop")
	_, err := repo.InsertNew([]*model.OrderIdempotencyKey{keep, drop})
	require.NoError(t, err)

	require.NoError(t, repo.DeleteByIDs([]uint64{drop.ID}))
	require.NoError(t, repo.DeleteByIDs(nil))

	found, err := repo.FindByUserKeys([]UserKeyPair{
		{UserID: userID, Key: "keep"}, {UserID: userID, Key: "drop"},
	})
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, "keep", found[0].IdempotencyKey)
}

// 0행 변경을 성공으로 돌려주면 호출자는 계약이 깨진 것을 알 수 없다.
// DB 오류가 나지 않는 경로라 오직 RowsAffected 검사만이 이를 잡는다.
func TestIntegrationOrderIdempotencyStateChangesRejectZeroRows(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewOrderIdempotencyRepository(db)
	userID := uniqueIdemUserID()
	defer cleanupIdemRecords(t, db, userID)

	const missingID = uint64(1 << 62)

	t.Run("SetOrderAndOutcome은 없는 ID에서 실패한다", func(t *testing.T) {
		err := repo.SetOrderAndOutcome(missingID, 1, model.OrderIdempotencyOutcomeAccepted)
		require.Error(t, err)
	})

	t.Run("UpdateOutcome은 없는 ID에서 실패한다", func(t *testing.T) {
		err := repo.UpdateOutcome(missingID, model.OrderIdempotencyOutcomeAccepted)
		require.Error(t, err)
	})

	t.Run("DeleteByIDs는 일부만 존재하면 실패한다", func(t *testing.T) {
		record := seedIdemRecord(userID, "partial")
		_, err := repo.InsertNew([]*model.OrderIdempotencyKey{record})
		require.NoError(t, err)

		err = repo.DeleteByIDs([]uint64{record.ID, missingID})
		require.Error(t, err, "요청한 키 중 하나가 없는데 성공으로 처리됐다")
	})

	t.Run("DeleteByIDs는 전부 없으면 실패한다", func(t *testing.T) {
		err := repo.DeleteByIDs([]uint64{missingID})
		require.Error(t, err)
	})

	// 0은 "지울 것이 없다"가 아니라 ID 전달이 깨졌다는 신호다. 조용히 버리면 그 키가
	// PENDING으로 남는다 — 이 메서드가 막으려던 바로 그 상태다.
	t.Run("DeleteByIDs는 0 ID를 거부한다", func(t *testing.T) {
		require.Error(t, repo.DeleteByIDs([]uint64{0}))

		record := seedIdemRecord(userID, "with-zero")
		_, err := repo.InsertNew([]*model.OrderIdempotencyKey{record})
		require.NoError(t, err)

		require.Error(t, repo.DeleteByIDs([]uint64{record.ID, 0}),
			"0이 섞였는데 실제 ID만 지우고 성공했다")

		var count int64
		require.NoError(t, db.Model(&model.OrderIdempotencyKey{}).
			Where("id = ?", record.ID).Count(&count).Error)
		assert.EqualValues(t, 1, count, "0을 버리고 실제 행을 지웠다")
	})

	// 부분 누락에서 error를 돌려주는 것만으로는 부족하다. 호출자 트랜잭션 안에서
	// 실제로 지워진 행까지 함께 롤백되어야 "키를 소비하지 않는다"가 성립한다.
	t.Run("호출자 트랜잭션에서 부분 삭제가 롤백된다", func(t *testing.T) {
		record := seedIdemRecord(userID, "rollback")
		_, err := repo.InsertNew([]*model.OrderIdempotencyKey{record})
		require.NoError(t, err)

		txErr := db.Transaction(func(tx *gorm.DB) error {
			return repo.WithTx(tx).DeleteByIDs([]uint64{record.ID, missingID})
		})
		require.Error(t, txErr)

		var count int64
		require.NoError(t, db.Model(&model.OrderIdempotencyKey{}).
			Where("id = ?", record.ID).Count(&count).Error)
		assert.EqualValues(t, 1, count, "부분 삭제가 롤백되지 않았다")
	})
}

func TestIntegrationOrderIdempotencyCountStalePending(t *testing.T) {
	// CountStalePending은 부분 인덱스를 쓰려고 outcome을 SQL 리터럴로 고정한다.
	// 상수가 바뀌면 조회는 조용히 아무것도 세지 않게 되므로 여기서 붙잡는다.
	require.Equal(t, "PENDING", string(model.OrderIdempotencyOutcomePending),
		"CountStalePending의 SQL 리터럴 'PENDING'과 상수가 어긋났다")

	db := openRepositoryIntegrationDB(t)
	repo := NewOrderIdempotencyRepository(db)
	userID := uniqueIdemUserID()
	defer cleanupIdemRecords(t, db, userID)

	stale := seedIdemRecord(userID, "stale")
	fresh := seedIdemRecord(userID, "fresh")
	done := seedIdemRecord(userID, "done")
	_, err := repo.InsertNew([]*model.OrderIdempotencyKey{stale, fresh, done})
	require.NoError(t, err)

	require.NoError(t, repo.SetOrderAndOutcome(stale.ID, 1, model.OrderIdempotencyOutcomePending))
	require.NoError(t, repo.SetOrderAndOutcome(fresh.ID, 2, model.OrderIdempotencyOutcomePending))
	require.NoError(t, repo.SetOrderAndOutcome(done.ID, 3, model.OrderIdempotencyOutcomeAccepted))

	require.NoError(t, db.Model(&model.OrderIdempotencyKey{}).Where("id = ?", stale.ID).
		Update("updated_at", time.Now().Add(-time.Hour)).Error)

	before, err := repo.CountStalePending(time.Now().Add(-30 * time.Minute))
	require.NoError(t, err)

	// 다른 테스트 데이터가 섞일 수 있어 절대값이 아니라 증분으로 본다.
	require.NoError(t, db.Model(&model.OrderIdempotencyKey{}).Where("id = ?", fresh.ID).
		Update("updated_at", time.Now().Add(-time.Hour)).Error)
	after, err := repo.CountStalePending(time.Now().Add(-30 * time.Minute))
	require.NoError(t, err)

	assert.Equal(t, before+1, after, "PENDING이면서 임계보다 오래된 것만 세야 한다")

	// ACCEPTED는 아무리 오래돼도 잡히지 않는다.
	require.NoError(t, db.Model(&model.OrderIdempotencyKey{}).Where("id = ?", done.ID).
		Update("updated_at", time.Now().Add(-time.Hour)).Error)
	withDone, err := repo.CountStalePending(time.Now().Add(-30 * time.Minute))
	require.NoError(t, err)
	assert.Equal(t, after, withDone, "PENDING이 아닌 레코드가 gauge에 잡혔다")
}
