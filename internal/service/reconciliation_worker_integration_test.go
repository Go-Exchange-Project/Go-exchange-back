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

	// 정상 지갑: 원장과 지갑이 정확히 일치 → 위반이 없어야 한다.
	cleanWallet := seedReconciliationWallet(t, db, userID, model.KRWAssetSymbol,
		decimal.NewFromInt(1000), decimal.Zero)
	seedReconciliationLedgerEntry(t, db, userID, model.KRWAssetSymbol,
		decimal.NewFromInt(1000), decimal.Zero, decimal.NewFromInt(1000), decimal.Zero)

	// 진짜 버그: 원장 델타 합(500)과 지갑(700)이 어긋난 지갑.
	bugUserID := serviceTestUserID(91)
	defer cleanupServiceUsers(t, db, bugUserID)
	bugWallet := seedReconciliationWallet(t, db, bugUserID, model.KRWAssetSymbol,
		decimal.NewFromInt(700), decimal.Zero)
	seedReconciliationLedgerEntry(t, db, bugUserID, model.KRWAssetSymbol,
		decimal.NewFromInt(500), decimal.Zero, decimal.NewFromInt(500), decimal.Zero)

	// 레거시 패턴: 첫 원장 항목의 after-delta가 100 → 추적 안 된 초기 잔액 100이
	// available gap(70-(-30)=100)을 정확히 설명하고 locked gap은 0 → legacy_mismatch.
	legacyWallet := seedReconciliationWallet(t, db, bugUserID, "BTC",
		decimal.NewFromInt(70), decimal.NewFromInt(30))
	seedReconciliationLedgerEntry(t, db, bugUserID, "BTC",
		decimal.NewFromInt(-30), decimal.NewFromInt(30), decimal.NewFromInt(70), decimal.NewFromInt(30))

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

	cleanSubject := fmt.Sprintf("wallet:%d", cleanWallet.ID)
	bugSubject := fmt.Sprintf("wallet:%d", bugWallet.ID)
	legacySubject := fmt.Sprintf("wallet:%d", legacyWallet.ID)
	orderSubject := fmt.Sprintf("order:%d", staleOrder.ID)
	subjects := []string{cleanSubject, bugSubject, legacySubject, orderSubject}
	defer func() {
		require.NoError(t, db.Where("subject_key IN ?", subjects).Delete(&model.ReconciliationViolation{}).Error)
	}()

	worker := &ReconciliationWorker{
		Repository: repository.NewReconciliationRepository(db),
		Logger:     discardServiceLogger(),
	}
	worker.RunOnce()

	violations := findViolationsBySubject(t, db, subjects)
	assert.Empty(t, violations[cleanSubject], "정상 지갑은 위반이 없어야 한다")
	require.Len(t, violations[bugSubject], 1)
	assert.Equal(t, "ledger_wallet", violations[bugSubject][0].CheckName)
	assert.Contains(t, violations[bugSubject][0].Detail, "available_balance=700")
	require.Len(t, violations[legacySubject], 1)
	assert.Equal(t, "legacy_mismatch", violations[legacySubject][0].CheckName)
	require.Len(t, violations[orderSubject], 1)
	assert.Equal(t, "stale_market_order", violations[orderSubject][0].CheckName)
}

// 위반이 해소되면 다음 실행에서는 새 행이 쌓이지 않아야 한다.
func TestIntegrationReconciliationWorkerStopsRecordingAfterResolution(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(92)
	defer cleanupServiceUsers(t, db, userID)

	wallet := seedReconciliationWallet(t, db, userID, model.KRWAssetSymbol,
		decimal.NewFromInt(700), decimal.Zero)
	seedReconciliationLedgerEntry(t, db, userID, model.KRWAssetSymbol,
		decimal.NewFromInt(500), decimal.Zero, decimal.NewFromInt(500), decimal.Zero)

	subject := fmt.Sprintf("wallet:%d", wallet.ID)
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

	// 지갑을 원장과 일치하도록 "복구"하면 더 이상 기록되지 않는다.
	require.NoError(t, db.Model(&model.Wallet{}).Where("id = ?", wallet.ID).
		Updates(map[string]interface{}{"available_balance": decimal.NewFromInt(500), "krw": decimal.NewFromInt(500)}).Error)

	worker.RunOnce()
	var afterSecond int64
	require.NoError(t, db.Model(&model.ReconciliationViolation{}).Where("subject_key = ?", subject).Count(&afterSecond).Error)
	assert.Equal(t, int64(1), afterSecond, "해소된 위반은 새 행을 만들지 않아야 한다")
}

func seedReconciliationWallet(t *testing.T, db *gorm.DB, userID uint, coinSymbol string, available decimal.Decimal, locked decimal.Decimal) model.Wallet {
	t.Helper()

	total := available.Add(locked)
	wallet := model.Wallet{
		UserID:           userID,
		CoinSymbol:       coinSymbol,
		AvailableBalance: available,
		LockedBalance:    locked,
	}
	if coinSymbol == model.KRWAssetSymbol {
		wallet.KRW = total
	} else {
		wallet.Quantity = total
	}
	require.NoError(t, db.Create(&wallet).Error)
	return wallet
}

func seedReconciliationLedgerEntry(t *testing.T, db *gorm.DB, userID uint, coinSymbol string, availableDelta, lockedDelta, availableAfter, lockedAfter decimal.Decimal) {
	t.Helper()

	require.NoError(t, db.Create(&model.LedgerEntry{
		UserID:                userID,
		CoinSymbol:            coinSymbol,
		EntryType:             model.LedgerEntryTypeDevFund,
		AvailableDelta:        availableDelta,
		LockedDelta:           lockedDelta,
		AvailableBalanceAfter: availableAfter,
		LockedBalanceAfter:    lockedAfter,
		ReferenceType:         model.LedgerReferenceTypeDevFund,
		ReferenceID:           0,
	}).Error)
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
