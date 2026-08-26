package service

import (
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func newIdemHoldCoordinator(db *gorm.DB) *HoldCoordinator {
	orderRepo, walletRepo, ledgerRepo := newHoldTestRepos(db)
	return &HoldCoordinator{
		DB: db, OrderRepo: orderRepo, WalletRepo: walletRepo, LedgerRepo: ledgerRepo,
		IdemRepo: repository.NewOrderIdempotencyRepository(db),
	}
}

func idemBuyOrder(userID uint, amount int64) *model.Order {
	return &model.Order{
		UserID: userID, CoinSymbol: "BTC",
		Side: model.OrderSideBuy, OrderType: model.OrderTypeLimit,
		Price: decimal.NewFromInt(100), Amount: decimal.NewFromInt(amount),
		Status: model.OrderStatusPending,
	}
}

func idemHoldRequest(order *model.Order, key, fingerprint string) holdRequest {
	return holdRequest{
		order: order,
		idem:  &idempotencyContext{Key: key, Fingerprint: fingerprint, Version: CurrentOrderFingerprintVersion},
	}
}

func countOrders(t *testing.T, db *gorm.DB, userID uint) int64 {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(&model.Order{}).Where("user_id = ?", userID).Count(&count).Error)
	return count
}

func countIdemKeys(t *testing.T, db *gorm.DB, userID uint) int64 {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(&model.OrderIdempotencyKey{}).
		Where("user_id = ?", userID).Count(&count).Error)
	return count
}

// 같은 배치의 중복 키가 각자 owner가 되면 hold는 한 번인데 주문이 두 건 생긴다.
func TestIntegrationHoldBatchSameKeyCreatesOneOrder(t *testing.T) {
	db := openServiceIntegrationDB(t)
	buyer := serviceTestUserID(760)
	seller := serviceTestUserID(761)
	defer cleanupServiceUsers(t, db, buyer, seller)
	seedHoldWallets(t, db, buyer, seller)
	defer cleanupIdemKeys(t, db, buyer)

	coordinator := newIdemHoldCoordinator(db)
	first := idemBuyOrder(buyer, 1)
	second := idemBuyOrder(buyer, 1)
	results, err := coordinator.HoldBatch([]holdRequest{
		idemHoldRequest(first, "same-key", "fp"),
		idemHoldRequest(second, "same-key", "fp"),
	})
	require.NoError(t, err)
	require.Len(t, results, 2)

	require.NoError(t, results[0].Err)
	require.NoError(t, results[1].Err)
	assert.Equal(t, holdRoleOwner, results[0].Role)
	assert.Equal(t, holdRoleFollower, results[1].Role, "중복 요청이 owner가 됐다 — 엔진에 두 번 제출된다")
	assert.Equal(t, results[0].Order, results[1].Order, "follower가 leader의 주문을 받지 못했다")

	assert.EqualValues(t, 1, countOrders(t, db, buyer))
	assert.EqualValues(t, 1, countIdemKeys(t, db, buyer))

	// hold도 한 번만 잡혀야 한다. 100*1*1.0005 = 100.05
	var wallet model.Wallet
	require.NoError(t, db.Where("user_id = ? AND coin_symbol = ?", buyer, model.KRWAssetSymbol).
		First(&wallet).Error)
	assert.True(t, wallet.LockedBalance.Equal(decimal.RequireFromString("100.05")),
		"locked=%s — hold가 두 번 잡혔다", wallet.LockedBalance)
}

// 같은 키·다른 지문은 하나만 진행하고 나머지는 409다.
func TestIntegrationHoldBatchSameKeyDifferentFingerprintConflicts(t *testing.T) {
	db := openServiceIntegrationDB(t)
	buyer := serviceTestUserID(762)
	seller := serviceTestUserID(763)
	defer cleanupServiceUsers(t, db, buyer, seller)
	seedHoldWallets(t, db, buyer, seller)
	defer cleanupIdemKeys(t, db, buyer)

	coordinator := newIdemHoldCoordinator(db)
	results, err := coordinator.HoldBatch([]holdRequest{
		idemHoldRequest(idemBuyOrder(buyer, 1), "k", "fp-a"),
		idemHoldRequest(idemBuyOrder(buyer, 2), "k", "fp-b"),
	})
	require.NoError(t, err)
	require.Len(t, results, 2)

	require.NoError(t, results[0].Err)
	require.Error(t, results[1].Err)
	kind, ok := DomainErrorKind(results[1].Err)
	require.True(t, ok)
	assert.Equal(t, ErrorKindConflict, kind)

	assert.EqualValues(t, 1, countOrders(t, db, buyer))
}

// 검증 실패는 키를 소비하지 않는다. 배치는 실패분을 격리하고 나머지와 함께 커밋하므로,
// 실패한 owner의 키를 같은 트랜잭션에서 지우지 않으면 그 키가 PENDING으로 남는다.
func TestIntegrationHoldBatchFailedValidationReleasesKey(t *testing.T) {
	db := openServiceIntegrationDB(t)
	rich := serviceTestUserID(764)
	poor := serviceTestUserID(765)
	defer cleanupServiceUsers(t, db, rich, poor)
	seedHoldWallets(t, db, rich, poor)
	defer cleanupIdemKeys(t, db, rich)
	defer cleanupIdemKeys(t, db, poor)

	coordinator := newIdemHoldCoordinator(db)
	// poor는 KRW 지갑이 0이다(seedHoldWallets의 seller 쪽).
	results, err := coordinator.HoldBatch([]holdRequest{
		idemHoldRequest(idemBuyOrder(rich, 1), "ok-key", "fp-ok"),
		idemHoldRequest(idemBuyOrder(poor, 1), "poor-key", "fp-poor"),
	})
	require.NoError(t, err, "부분 실패가 배치를 실패시켰다")
	require.Len(t, results, 2)

	require.NoError(t, results[0].Err)
	require.Error(t, results[1].Err, "잔고가 없는데 hold가 성공했다")

	assert.EqualValues(t, 1, countIdemKeys(t, db, rich), "성공한 키가 남지 않았다")
	assert.EqualValues(t, 0, countIdemKeys(t, db, poor), "실패한 키가 소비됐다 — 재시도가 영구히 막힌다")
}

// 이미 커밋된 키의 재시도는 새 주문을 만들지 않고 저장된 레코드를 돌려준다.
func TestIntegrationHoldBatchExistingKeyReturnsStoredRecord(t *testing.T) {
	db := openServiceIntegrationDB(t)
	buyer := serviceTestUserID(766)
	seller := serviceTestUserID(767)
	defer cleanupServiceUsers(t, db, buyer, seller)
	seedHoldWallets(t, db, buyer, seller)
	defer cleanupIdemKeys(t, db, buyer)

	coordinator := newIdemHoldCoordinator(db)
	first := idemBuyOrder(buyer, 1)
	results, err := coordinator.HoldBatch([]holdRequest{idemHoldRequest(first, "retry", "fp")})
	require.NoError(t, err)
	require.NoError(t, results[0].Err)
	require.NotZero(t, first.ID)

	results, err = coordinator.HoldBatch([]holdRequest{
		idemHoldRequest(idemBuyOrder(buyer, 1), "retry", "fp"),
	})
	require.NoError(t, err)
	require.NoError(t, results[0].Err)

	assert.Equal(t, holdRoleFollower, results[0].Role)
	require.NotNil(t, results[0].Existing, "저장된 레코드를 돌려주지 않았다")
	require.NotNil(t, results[0].Existing.OrderID)
	assert.EqualValues(t, first.ID, *results[0].Existing.OrderID)
	assert.Equal(t, model.OrderIdempotencyOutcomePending, results[0].Existing.Outcome)

	assert.EqualValues(t, 1, countOrders(t, db, buyer), "재시도가 새 주문을 만들었다")
}

func cleanupIdemKeys(t *testing.T, db *gorm.DB, userID uint) {
	t.Helper()
	require.NoError(t, db.Where("user_id = ?", userID).
		Delete(&model.OrderIdempotencyKey{}).Error)
}
