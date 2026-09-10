package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 위반 주입 → RunOnce → 내구 기록까지의 전체 체인을 실제 Postgres에서 검증한다.
// 공유 테스트 DB에서 다른 테스트의 데이터도 위반으로 잡힐 수 있으므로,
// 전역 건수는 절대 단언하지 않고 이 테스트가 만든 subject_key로만 필터링한다.
func TestIntegrationReconciliationWorkerRecordsInjectedViolations(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(90)
	defer cleanupServiceUsers(t, db, userID)

	// 정상 계정: 지급만 하고 캐시를 건드리지 않음 → 위반이 없어야 한다.
	seedLedgerFunds(t, db, userID, model.KRWAssetSymbol, decimal.NewFromInt(1000))
	cleanAccountID := userAvailableAccountID(t, db, userID, model.KRWAssetSymbol)

	// 드리프트 주입: 전기 없이 잔액 캐시만 어긋나게 만든 계정 → balance_cache_drift.
	bugUserID := serviceTestUserID(91)
	defer cleanupServiceUsers(t, db, bugUserID)
	driftAccountID := seedBalanceCacheDrift(t, db, bugUserID, model.KRWAssetSymbol, decimal.NewFromInt(200))

	// 오래된 시장가 주문: 10분 전 생성된 PENDING → stale_market_order.
	staleOrder := model.Order{
		UserID:       userID,
		CoinSymbol:   "BTC",
		Side:         model.OrderSideBuy,
		OrderType:    model.OrderTypeMarket,
		Status:       model.OrderStatusPending,
		Price:        decimal.Zero,
		Amount:       decimal.Zero,
		QuoteAmount:  decimal.NewFromInt(100_000),
		FilledAmount: decimal.Zero,
		CreatedAt:    time.Now().UTC().Add(-10 * time.Minute),
	}
	require.NoError(t, db.Create(&staleOrder).Error)

	cleanSubject := fmt.Sprintf("account:%d", cleanAccountID)
	driftSubject := fmt.Sprintf("account:%d", driftAccountID)
	orderSubject := fmt.Sprintf("order:%d", staleOrder.ID)
	subjects := []string{cleanSubject, driftSubject, orderSubject}
	defer func() {
		require.NoError(t, db.Where("subject_key IN ?", subjects).Delete(&model.ReconciliationViolation{}).Error)
	}()

	worker := &ReconciliationWorker{
		Repository: repository.NewReconciliationRepository(db),
		Logger:     discardServiceLogger(),
	}
	worker.RunOnce()

	violations := findViolationsBySubject(t, db, subjects)
	assert.Empty(t, violations[cleanSubject], "정상 계정은 위반이 없어야 한다")
	require.Len(t, violations[driftSubject], 1)
	assert.Equal(t, "balance_cache_drift", violations[driftSubject][0].CheckName)
	assert.Contains(t, violations[driftSubject][0].Detail, "cached=")
	assert.Contains(t, violations[driftSubject][0].Detail, "computed=")
	require.Len(t, violations[orderSubject], 1)
	assert.Equal(t, "stale_market_order", violations[orderSubject][0].CheckName)
}

// 위반이 해소되면 다음 실행에서는 새 행이 쌓이지 않아야 한다.
func TestIntegrationReconciliationWorkerStopsRecordingAfterResolution(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(92)
	defer cleanupServiceUsers(t, db, userID)

	accountID := seedBalanceCacheDrift(t, db, userID, model.KRWAssetSymbol, decimal.NewFromInt(200))

	subject := fmt.Sprintf("account:%d", accountID)
	defer func() {
		require.NoError(t, db.Where("subject_key = ?", subject).Delete(&model.ReconciliationViolation{}).Error)
	}()

	worker := &ReconciliationWorker{
		Repository: repository.NewReconciliationRepository(db),
		Logger:     discardServiceLogger(),
	}

	worker.RunOnce()
	var afterFirst int64
	require.NoError(t, db.Model(&model.ReconciliationViolation{}).Where("subject_key = ?", subject).Count(&afterFirst).Error)
	require.Equal(t, int64(1), afterFirst)

	// 캐시를 전기 합과 다시 맞춘다("해소").
	resolveBalanceCacheDrift(t, db, accountID)

	worker.RunOnce()
	var afterSecond int64
	require.NoError(t, db.Model(&model.ReconciliationViolation{}).Where("subject_key = ?", subject).Count(&afterSecond).Error)
	assert.Equal(t, int64(1), afterSecond, "해소된 위반은 새 행을 만들지 않아야 한다")
}

// userAvailableAccountID는 사용자·자산의 USER_AVAILABLE 계정 ID를 찾는다.
func userAvailableAccountID(t *testing.T, db *gorm.DB, userID uint, asset string) uint {
	t.Helper()

	var accountID uint
	require.NoError(t, db.Raw(`
		SELECT a.id FROM accounts a
		WHERE a.owner_user_id = ? AND a.asset = ? AND a.account_type = 'USER_AVAILABLE'`,
		userID, asset).Scan(&accountID).Error)
	require.NotZero(t, accountID, "계정이 아직 없다 — 지급을 먼저 해야 한다")
	return accountID
}

// seedBalanceCacheDrift는 전기 없이 잔액 캐시만 바꿔 검사 2(balance_cache_drift)가
// 잡을 위반을 만든다.
//
// 이 주입은 전역이다 — 검사는 계정 전체를 훑는다. 복구를 t.Cleanup으로 걸어 두지
// 않으면 테스트가 중간에 실패했을 때 공유 DB에 드리프트가 영구히 남고,
// TestOrderAndSettlementPreserveAssets의 "검산 4종 위반 0건" 단언이 그때부터 계속
// 깨진다. 복구는 델타를 빼는 대신 전기 합으로 다시 계산해 SET한다(resolveBalanceCacheDrift)
// — 그래야 테스트가 중간에 이미 캐시를 되돌려도(해소 시나리오) cleanup이 다시
// 어긋나지 않는다.
func seedBalanceCacheDrift(t *testing.T, db *gorm.DB, userID uint, asset string, delta decimal.Decimal) (accountID uint) {
	t.Helper()

	seedLedgerFunds(t, db, userID, asset, decimal.NewFromInt(1000))
	accountID = userAvailableAccountID(t, db, userID, asset)

	require.NoError(t, db.Exec(`UPDATE account_balances SET balance = balance + ? WHERE account_id = ?`,
		delta, accountID).Error)
	t.Cleanup(func() { resolveBalanceCacheDrift(t, db, accountID) })
	return accountID
}

// resolveBalanceCacheDrift는 잔액 캐시를 전기 합으로 다시 맞춘다. 상대값을 빼는
// 게 아니라 절대값으로 SET하므로 몇 번을 불러도 안전하다.
func resolveBalanceCacheDrift(t *testing.T, db *gorm.DB, accountID uint) {
	t.Helper()
	require.NoError(t, db.Exec(`
		UPDATE account_balances SET balance = COALESCE(
			(SELECT SUM(amount) FROM postings WHERE account_id = ?), 0)
		WHERE account_id = ?`, accountID, accountID).Error)
}

func findViolationsBySubject(t *testing.T, db *gorm.DB, subjects []string) map[string][]model.ReconciliationViolation {
	t.Helper()

	var rows []model.ReconciliationViolation
	require.NoError(t, db.Where("subject_key IN ?", subjects).Find(&rows).Error)
	grouped := make(map[string][]model.ReconciliationViolation, len(subjects))
	for _, row := range rows {
		grouped[row.SubjectKey] = append(grouped[row.SubjectKey], row)
	}
	return grouped
}
