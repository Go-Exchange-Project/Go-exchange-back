package repository

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestIntegrationRecordFailedSettlementCreatesAndDedupesByIdempotencyKey(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	key := fmt.Sprintf("repo-failed-settlement-%d", time.Now().UnixNano())
	defer cleanupFailedSettlementsByPrefix(t, db, key)

	repo := NewFailedSettlementRepository(db)
	first := failedSettlementFixture(key, "first error")

	persisted, err := repo.RecordFailure(&first)
	require.NoError(t, err)
	assert.Equal(t, key, persisted.TradeIdempotencyKey)
	assert.Equal(t, uint(1), persisted.RetryCount)
	assert.Equal(t, "first error", persisted.ErrorMessage)

	second := failedSettlementFixture(key, "second error")
	second.Price = decimal.NewFromInt(91)
	persisted, err = repo.RecordFailure(&second)
	require.NoError(t, err)
	assert.Equal(t, uint(2), persisted.RetryCount)
	assert.Equal(t, "second error", persisted.ErrorMessage)

	var count int64
	require.NoError(t, db.Model(&model.FailedSettlement{}).Where("trade_idempotency_key = ?", key).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestIntegrationRecordFailedSettlementAllowsDifferentIdempotencyKeys(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	keyPrefix := fmt.Sprintf("repo-failed-settlement-distinct-%d", time.Now().UnixNano())
	defer cleanupFailedSettlementsByPrefix(t, db, keyPrefix)

	repo := NewFailedSettlementRepository(db)
	first := failedSettlementFixture(keyPrefix+"-1", "first error")
	second := failedSettlementFixture(keyPrefix+"-2", "second error")

	_, err := repo.RecordFailure(&first)
	require.NoError(t, err)
	_, err = repo.RecordFailure(&second)
	require.NoError(t, err)

	var count int64
	require.NoError(t, db.Model(&model.FailedSettlement{}).Where("trade_idempotency_key LIKE ?", keyPrefix+"%").Count(&count).Error)
	assert.Equal(t, int64(2), count)
}

func TestIntegrationFailedSettlementConstraints(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	keyPrefix := fmt.Sprintf("repo-failed-settlement-constraint-%d", time.Now().UnixNano())
	defer cleanupFailedSettlementsByPrefix(t, db, keyPrefix)

	valid := failedSettlementFixture(keyPrefix+"-valid", "valid error")
	require.NoError(t, db.Create(&valid).Error)

	blankKey := failedSettlementFixture("", "blank key")
	require.Error(t, db.Create(&blankKey).Error)

	zeroPrice := failedSettlementFixture(keyPrefix+"-zero-price", "zero price")
	zeroPrice.Price = decimal.Zero
	require.Error(t, db.Create(&zeroPrice).Error)

	zeroQuantity := failedSettlementFixture(keyPrefix+"-zero-quantity", "zero quantity")
	zeroQuantity.Quantity = decimal.Zero
	require.Error(t, db.Create(&zeroQuantity).Error)

	require.Error(t, db.Exec(
		`INSERT INTO failed_settlements
			(trade_idempotency_key, coin_symbol, buy_order_id, sell_order_id, price, quantity, error_message, status, retry_count, occurred_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		keyPrefix+"-blank-status",
		"BTC",
		10,
		20,
		90,
		5,
		"blank status",
		"",
		1,
		time.Now().UTC(),
		time.Now().UTC(),
		time.Now().UTC(),
	).Error)

	blankError := failedSettlementFixture(keyPrefix+"-blank-error", "")
	require.Error(t, db.Create(&blankError).Error)

	invalidStatus := failedSettlementFixture(keyPrefix+"-invalid-status", "invalid status")
	invalidStatus.Status = model.FailedSettlementStatus("BROKEN")
	require.Error(t, db.Create(&invalidStatus).Error)

	resolvedWithoutAudit := failedSettlementFixture(keyPrefix+"-resolved-without-audit", "missing audit")
	resolvedWithoutAudit.Status = model.FailedSettlementStatusResolved
	require.Error(t, db.Create(&resolvedWithoutAudit).Error)
}

func TestIntegrationFindOpenFailedSettlementsReturnsOnlyOpenSorted(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	keyPrefix := fmt.Sprintf("repo-failed-settlement-open-%d", time.Now().UnixNano())
	defer cleanupFailedSettlementsByPrefix(t, db, keyPrefix)

	base := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	later := failedSettlementFixture(keyPrefix+"-later", "later")
	later.OccurredAt = base.Add(3 * time.Minute)
	early := failedSettlementFixture(keyPrefix+"-early", "early")
	early.OccurredAt = base.Add(time.Minute)
	sameTimeFirst := failedSettlementFixture(keyPrefix+"-same-1", "same 1")
	sameTimeFirst.OccurredAt = base.Add(2 * time.Minute)
	sameTimeSecond := failedSettlementFixture(keyPrefix+"-same-2", "same 2")
	sameTimeSecond.OccurredAt = base.Add(2 * time.Minute)
	resolved := failedSettlementFixture(keyPrefix+"-resolved", "resolved")
	resolved.Status = model.FailedSettlementStatusResolved
	resolved.Resolution = "already handled"
	resolved.ResolvedAt = ptrTime(base)

	for _, failure := range []*model.FailedSettlement{&later, &early, &sameTimeFirst, &sameTimeSecond, &resolved} {
		require.NoError(t, db.Create(failure).Error)
	}

	failures, err := NewFailedSettlementRepository(db).FindOpen(10)
	require.NoError(t, err)

	targetIDs := map[uint]bool{
		later.ID:          true,
		early.ID:          true,
		sameTimeFirst.ID:  true,
		sameTimeSecond.ID: true,
		resolved.ID:       true,
	}
	var found []uint
	for _, failure := range failures {
		if targetIDs[failure.ID] {
			found = append(found, failure.ID)
		}
	}

	assert.Equal(t, []uint{early.ID, sameTimeFirst.ID, sameTimeSecond.ID, later.ID}, found)
}

func TestIntegrationFindOpenFailedSettlementsAppliesLimit(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	keyPrefix := fmt.Sprintf("repo-failed-settlement-limit-%d", time.Now().UnixNano())
	defer cleanupFailedSettlementsByPrefix(t, db, keyPrefix)

	for i := 0; i < 3; i++ {
		failure := failedSettlementFixture(fmt.Sprintf("%s-%d", keyPrefix, i), "limited")
		failure.OccurredAt = time.Date(1970, 1, 2, 0, i, 0, 0, time.UTC)
		require.NoError(t, db.Create(&failure).Error)
	}

	failures, err := NewFailedSettlementRepository(db).FindOpen(2)
	require.NoError(t, err)

	matched := 0
	for _, failure := range failures {
		if strings.HasPrefix(failure.TradeIdempotencyKey, keyPrefix) {
			matched++
		}
	}
	assert.LessOrEqual(t, matched, 2)
}

func TestIntegrationMarkFailedSettlementResolved(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	keyPrefix := fmt.Sprintf("repo-failed-settlement-resolve-%d", time.Now().UnixNano())
	defer cleanupFailedSettlementsByPrefix(t, db, keyPrefix)

	failure := failedSettlementFixture(keyPrefix, "resolve me")
	require.NoError(t, db.Create(&failure).Error)

	repo := NewFailedSettlementRepository(db)
	require.NoError(t, repo.MarkResolved(failure.ID, "stale engine event reviewed", "ops", "no retry required"))

	var persisted model.FailedSettlement
	require.NoError(t, db.First(&persisted, failure.ID).Error)
	assert.Equal(t, model.FailedSettlementStatusResolved, persisted.Status)
	assert.Equal(t, "stale engine event reviewed", persisted.Resolution)
	assert.Equal(t, "ops", persisted.ResolvedBy)
	assert.Equal(t, "no retry required", persisted.Notes)
	require.NotNil(t, persisted.ResolvedAt)
}

func TestIntegrationMarkResolvedIsNoOpForAlreadyResolved(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	keyPrefix := fmt.Sprintf("repo-failed-settlement-resolved-noop-%d", time.Now().UnixNano())
	defer cleanupFailedSettlementsByPrefix(t, db, keyPrefix)

	resolvedAt := time.Now().UTC().Add(-time.Minute)
	failure := failedSettlementFixture(keyPrefix, "already resolved")
	failure.Status = model.FailedSettlementStatusResolved
	failure.Resolution = "first resolution"
	failure.ResolvedBy = "ops-a"
	failure.ResolvedAt = &resolvedAt
	require.NoError(t, db.Create(&failure).Error)

	repo := NewFailedSettlementRepository(db)
	require.NoError(t, repo.MarkResolved(failure.ID, "second resolution", "ops-b", "ignored"))

	var persisted model.FailedSettlement
	require.NoError(t, db.First(&persisted, failure.ID).Error)
	assert.Equal(t, "first resolution", persisted.Resolution)
	assert.Equal(t, "ops-a", persisted.ResolvedBy)
	require.NotNil(t, persisted.ResolvedAt)
	assert.True(t, persisted.ResolvedAt.Equal(resolvedAt))
}

func TestIntegrationMarkResolvedMissingIDReturnsError(t *testing.T) {
	db := openRepositoryIntegrationDB(t)

	err := NewFailedSettlementRepository(db).MarkResolved(999_999_999, "not found", "ops", "")

	require.Error(t, err)
}

func failedSettlementFixture(key string, message string) model.FailedSettlement {
	return model.FailedSettlement{
		TradeIdempotencyKey: key,
		CoinSymbol:          "BTC",
		BuyOrderID:          10,
		SellOrderID:         20,
		Price:               decimal.NewFromInt(90),
		Quantity:            decimal.NewFromInt(5),
		ErrorMessage:        message,
		Status:              model.FailedSettlementStatusOpen,
		RetryCount:          1,
		OccurredAt:          time.Now().UTC(),
	}
}

func cleanupFailedSettlementsByPrefix(t *testing.T, db *gorm.DB, keyPrefix string) {
	t.Helper()

	require.NoError(t, db.Where("trade_idempotency_key LIKE ?", keyPrefix+"%").Delete(&model.FailedSettlement{}).Error)
}

func ptrTime(value time.Time) *time.Time {
	return &value
}
