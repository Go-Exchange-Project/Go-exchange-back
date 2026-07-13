package service

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 두 유저가 서로 반대 역할(A가 buyer인 체결과 B가 buyer인 체결)인 정산을 동시에
// 실행하는 회귀 테스트. 지갑 락을 ID 오름차순으로 정렬하기 전에는 AB-BA 데드락으로
// 한쪽 정산이 abort될 수 있었다. 수정 후에는 모든 라운드가 오류 없이 정산되어야 한다.
func TestIntegrationConcurrentReversedSettlementsDoNotDeadlock(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userA := serviceTestUserID(70)
	userB := serviceTestUserID(71)
	defer cleanupServiceUsers(t, db, userA, userB)

	seedDeadlockWallets(t, db, userA, userB)
	buyAB, sellAB := seedDeadlockOrderPair(t, db, userA, userB)
	buyBA, sellBA := seedDeadlockOrderPair(t, db, userB, userA)

	settlementService := NewSettlementService(db, repository.NewOrderRepository(db), repository.NewWalletRepository(db))

	const rounds = 30
	testRunID := time.Now().UnixNano()
	errs := make(chan error, rounds*2)
	for round := 0; round < rounds; round++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func(round int) {
			defer wg.Done()
			_, err := settlementService.SettleTrade(deadlockTestTrade(buyAB.ID, sellAB.ID, testRunID, round, "ab"), 0)
			errs <- err
		}(round)
		go func(round int) {
			defer wg.Done()
			_, err := settlementService.SettleTrade(deadlockTestTrade(buyBA.ID, sellBA.ID, testRunID, round, "ba"), 0)
			errs <- err
		}(round)
		wg.Wait()
	}
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	var tradeCount int64
	require.NoError(t, db.Model(&model.Trade{}).
		Where("buy_order_id IN ?", []uint{buyAB.ID, buyBA.ID}).
		Count(&tradeCount).Error)
	assert.Equal(t, int64(rounds*2), tradeCount)
}

func seedDeadlockWallets(t *testing.T, db *gorm.DB, userA uint, userB uint) {
	t.Helper()

	lockedKRW := decimal.NewFromInt(1_000_000)
	lockedBTC := decimal.NewFromInt(1_000)
	wallets := []model.Wallet{
		{UserID: userA, CoinSymbol: model.KRWAssetSymbol, KRW: lockedKRW, AvailableBalance: decimal.Zero, LockedBalance: lockedKRW},
		{UserID: userA, CoinSymbol: "BTC", Quantity: lockedBTC, AvailableBalance: decimal.Zero, LockedBalance: lockedBTC},
		{UserID: userB, CoinSymbol: model.KRWAssetSymbol, KRW: lockedKRW, AvailableBalance: decimal.Zero, LockedBalance: lockedKRW},
		{UserID: userB, CoinSymbol: "BTC", Quantity: lockedBTC, AvailableBalance: decimal.Zero, LockedBalance: lockedBTC},
	}
	require.NoError(t, db.Create(&wallets).Error)
}

func seedDeadlockOrderPair(t *testing.T, db *gorm.DB, buyerID uint, sellerID uint) (model.Order, model.Order) {
	t.Helper()

	orderAmount := decimal.NewFromInt(1_000)
	buyOrder := model.Order{
		UserID:       buyerID,
		CoinSymbol:   "BTC",
		Side:         model.OrderSideBuy,
		OrderType:    model.OrderTypeLimit,
		Price:        decimal.NewFromInt(100),
		Amount:       orderAmount,
		Status:       model.OrderStatusPending,
		FilledAmount: decimal.Zero,
	}
	sellOrder := model.Order{
		UserID:       sellerID,
		CoinSymbol:   "BTC",
		Side:         model.OrderSideSell,
		OrderType:    model.OrderTypeLimit,
		Price:        decimal.NewFromInt(100),
		Amount:       orderAmount,
		Status:       model.OrderStatusPending,
		FilledAmount: decimal.Zero,
	}
	require.NoError(t, db.Create(&buyOrder).Error)
	require.NoError(t, db.Create(&sellOrder).Error)
	return buyOrder, sellOrder
}

func deadlockTestTrade(buyOrderID uint, sellOrderID uint, testRunID int64, round int, pair string) *model.Trade {
	return &model.Trade{
		EngineSequence: int64(round + 1),
		EngineEventID:  fmt.Sprintf("deadlock-test-%d-%s-%d", testRunID, pair, round),
		CoinSymbol:     "BTC",
		Price:          decimal.NewFromInt(100),
		Quantity:       decimal.NewFromInt(1),
		TradedAt:       time.Now(),
		BuyOrderID:     buyOrderID,
		SellOrderID:    sellOrderID,
	}
}
