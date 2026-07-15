package repository

import (
	"fmt"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/testdb"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func openRepositoryIntegrationDB(t *testing.T) *gorm.DB {
	t.Helper()

	return testdb.OpenIntegrationDB(t)
}

func repositoryTestUserID(offset uint) uint {
	return uint(time.Now().UnixNano()%1_000_000_000) + 100_000 + offset
}

func cleanupRepositoryUsers(t *testing.T, db *gorm.DB, userIDs ...uint) {
	t.Helper()

	if len(userIDs) == 0 {
		return
	}

	var orders []model.Order
	require.NoError(t, db.Where("user_id IN ?", userIDs).Find(&orders).Error)

	orderIDs := make([]uint, 0, len(orders))
	for _, order := range orders {
		orderIDs = append(orderIDs, order.ID)
	}
	if len(orderIDs) > 0 {
		require.NoError(t, db.Where("buy_order_id IN ? OR sell_order_id IN ?", orderIDs, orderIDs).Delete(&model.FailedSettlement{}).Error)
		require.NoError(t, db.Where("buy_order_id IN ? OR sell_order_id IN ?", orderIDs, orderIDs).Delete(&model.Trade{}).Error)
	}

	require.NoError(t, db.Where("user_id IN ?", userIDs).Delete(&model.LedgerEntry{}).Error)
	require.NoError(t, db.Where("user_id IN ?", userIDs).Delete(&model.Order{}).Error)
	require.NoError(t, db.Where("user_id IN ?", userIDs).Delete(&model.Wallet{}).Error)
	require.NoError(t, db.Where("id IN ?", userIDs).Delete(&model.User{}).Error)
}

func TestIntegrationUpdateBalancesUpdatesExistingWallet(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(1)
	defer cleanupRepositoryUsers(t, db, userID)

	repo := NewWalletRepository(db)
	require.NoError(t, db.Create(&model.Wallet{
		UserID:           userID,
		CoinSymbol:       "BTC",
		Quantity:         decimal.NewFromInt(10),
		AvailableBalance: decimal.NewFromInt(10),
		LockedBalance:    decimal.Zero,
	}).Error)

	require.NoError(t, repo.UpdateBalances(userID, "BTC", decimal.NewFromInt(7), decimal.NewFromInt(3)))

	wallet, err := repo.FindByUserIDAndCoinSymbol(userID, "BTC")
	require.NoError(t, err)
	assert.True(t, wallet.AvailableBalance.Equal(decimal.NewFromInt(7)))
	assert.True(t, wallet.LockedBalance.Equal(decimal.NewFromInt(3)))
	assert.True(t, wallet.Quantity.Equal(decimal.NewFromInt(10)))
}

func TestIntegrationUpdateBalancesAndAvgBuyPriceUpdatesExistingWallet(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(10)
	defer cleanupRepositoryUsers(t, db, userID)

	repo := NewWalletRepository(db)
	require.NoError(t, db.Create(&model.Wallet{
		UserID:           userID,
		CoinSymbol:       "BTC",
		Quantity:         decimal.NewFromInt(10),
		AvailableBalance: decimal.NewFromInt(10),
		LockedBalance:    decimal.Zero,
		AvgBuyPrice:      decimal.NewFromInt(90),
	}).Error)

	require.NoError(t, repo.UpdateBalancesAndAvgBuyPrice(userID, "BTC", decimal.NewFromInt(7), decimal.NewFromInt(3), decimal.NewFromInt(100)))

	wallet, err := repo.FindByUserIDAndCoinSymbol(userID, "BTC")
	require.NoError(t, err)
	assert.True(t, wallet.AvailableBalance.Equal(decimal.NewFromInt(7)))
	assert.True(t, wallet.LockedBalance.Equal(decimal.NewFromInt(3)))
	assert.True(t, wallet.Quantity.Equal(decimal.NewFromInt(10)))
	assert.True(t, wallet.AvgBuyPrice.Equal(decimal.NewFromInt(100)))
}

func TestIntegrationUpdateBalancesMissingWalletReturnsError(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(2)
	defer cleanupRepositoryUsers(t, db, userID)

	repo := NewWalletRepository(db)

	err := repo.UpdateBalances(userID, "BTC", decimal.NewFromInt(1), decimal.Zero)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "affected no rows")
}

func TestIntegrationUpdateBalancesScopesUserAndCoinSymbol(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(3)
	otherUserID := repositoryTestUserID(4)
	defer cleanupRepositoryUsers(t, db, userID, otherUserID)

	repo := NewWalletRepository(db)
	wallets := []model.Wallet{
		{UserID: userID, CoinSymbol: "BTC", Quantity: decimal.NewFromInt(10), AvailableBalance: decimal.NewFromInt(10)},
		{UserID: userID, CoinSymbol: "ETH", Quantity: decimal.NewFromInt(20), AvailableBalance: decimal.NewFromInt(20)},
		{UserID: otherUserID, CoinSymbol: "BTC", Quantity: decimal.NewFromInt(30), AvailableBalance: decimal.NewFromInt(30)},
	}
	require.NoError(t, db.Create(&wallets).Error)

	require.NoError(t, repo.UpdateBalances(userID, "BTC", decimal.NewFromInt(7), decimal.NewFromInt(3)))

	target, err := repo.FindByUserIDAndCoinSymbol(userID, "BTC")
	require.NoError(t, err)
	sameUserOtherCoin, err := repo.FindByUserIDAndCoinSymbol(userID, "ETH")
	require.NoError(t, err)
	otherUserSameCoin, err := repo.FindByUserIDAndCoinSymbol(otherUserID, "BTC")
	require.NoError(t, err)

	assert.True(t, target.AvailableBalance.Equal(decimal.NewFromInt(7)))
	assert.True(t, target.LockedBalance.Equal(decimal.NewFromInt(3)))
	assert.True(t, sameUserOtherCoin.AvailableBalance.Equal(decimal.NewFromInt(20)))
	assert.True(t, sameUserOtherCoin.LockedBalance.Equal(decimal.Zero))
	assert.True(t, otherUserSameCoin.AvailableBalance.Equal(decimal.NewFromInt(30)))
	assert.True(t, otherUserSameCoin.LockedBalance.Equal(decimal.Zero))
}

func TestIntegrationListWalletsByUserIDScopesAndOrders(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(8)
	otherUserID := repositoryTestUserID(9)
	defer cleanupRepositoryUsers(t, db, userID, otherUserID)

	require.NoError(t, db.Create(&[]model.Wallet{
		{UserID: userID, CoinSymbol: "ETH", Quantity: decimal.NewFromInt(2), AvailableBalance: decimal.NewFromInt(2)},
		{UserID: userID, CoinSymbol: "BTC", Quantity: decimal.NewFromInt(1), AvailableBalance: decimal.NewFromInt(1)},
		{UserID: otherUserID, CoinSymbol: "ADA", Quantity: decimal.NewFromInt(3), AvailableBalance: decimal.NewFromInt(3)},
	}).Error)

	wallets, err := NewWalletRepository(db).ListByUserID(userID)
	require.NoError(t, err)

	require.Len(t, wallets, 2)
	assert.Equal(t, "BTC", wallets[0].CoinSymbol)
	assert.Equal(t, "ETH", wallets[1].CoinSymbol)
}

func TestIntegrationTradeIdempotencyKeyUniqueIndex(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	key := fmt.Sprintf("repo-trade-key-%d", time.Now().UnixNano())
	defer func() {
		require.NoError(t, db.Where("idempotency_key = ?", key).Delete(&model.Trade{}).Error)
	}()

	first := model.Trade{
		IdempotencyKey: key,
		CoinSymbol:     "BTC",
		Price:          decimal.NewFromInt(90),
		Quantity:       decimal.NewFromInt(5),
		TradedAt:       time.Now(),
		BuyOrderID:     1,
		SellOrderID:    2,
	}
	second := first
	second.ID = 0
	second.Price = decimal.NewFromInt(91)

	require.NoError(t, db.Create(&first).Error)
	require.Error(t, db.Create(&second).Error)
}

func TestIntegrationWalletUserCoinUniqueConstraint(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(5)
	defer cleanupRepositoryUsers(t, db, userID)

	first := model.Wallet{
		UserID:           userID,
		CoinSymbol:       "BTC",
		Quantity:         decimal.NewFromInt(1),
		AvailableBalance: decimal.NewFromInt(1),
		LockedBalance:    decimal.Zero,
	}
	second := first

	require.NoError(t, db.Create(&first).Error)
	require.Error(t, db.Create(&second).Error)
}

func TestIntegrationTradeIdempotencyKeyCannotBeBlankOrNull(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	defer func() {
		require.NoError(t, db.Where("buy_order_id IN ? OR sell_order_id IN ?", []uint{31, 32}, []uint{31, 32}).Delete(&model.Trade{}).Error)
	}()

	blankKeyTrade := model.Trade{
		IdempotencyKey: "",
		CoinSymbol:     "BTC",
		Price:          decimal.NewFromInt(90),
		Quantity:       decimal.NewFromInt(5),
		TradedAt:       time.Now(),
		BuyOrderID:     31,
		SellOrderID:    32,
	}
	require.Error(t, db.Create(&blankKeyTrade).Error)

	require.Error(t, db.Exec(
		"INSERT INTO trades (idempotency_key, coin_symbol, price, quantity, traded_at, buy_order_id, sell_order_id) VALUES (?, ?, ?, ?, ?, ?, ?)",
		nil,
		"BTC",
		90,
		5,
		time.Now(),
		31,
		32,
	).Error)
}

func TestIntegrationTradeEngineEventIDUniqueIndex(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	eventID := fmt.Sprintf("engine-event-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		require.NoError(t, db.Where("engine_event_id = ?", eventID).Delete(&model.Trade{}).Error)
	})

	first := model.Trade{
		IdempotencyKey: "trade-engine-event-first-" + eventID,
		EngineSequence: 1,
		EngineEventID:  eventID,
		CoinSymbol:     "BTC",
		Price:          decimal.NewFromInt(1),
		Quantity:       decimal.NewFromInt(1),
		TradedAt:       time.Now(),
		BuyOrderID:     51,
		SellOrderID:    52,
	}
	second := first
	second.ID = 0
	second.IdempotencyKey = "trade-engine-event-second-" + eventID

	require.NoError(t, db.Create(&first).Error)
	require.Error(t, db.Create(&second).Error)
}

func TestIntegrationTradeEngineSequenceCannotBeNegative(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	key := fmt.Sprintf("trade-negative-engine-sequence-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		require.NoError(t, db.Where("idempotency_key = ?", key).Delete(&model.Trade{}).Error)
	})

	trade := model.Trade{
		IdempotencyKey: key,
		EngineSequence: -1,
		CoinSymbol:     "BTC",
		Price:          decimal.NewFromInt(1),
		Quantity:       decimal.NewFromInt(1),
		TradedAt:       time.Now(),
		BuyOrderID:     53,
		SellOrderID:    54,
	}

	require.Error(t, db.Create(&trade).Error)
}

func TestIntegrationWalletNegativeBalanceConstraint(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(6)
	defer cleanupRepositoryUsers(t, db, userID)

	wallet := model.Wallet{
		UserID:           userID,
		CoinSymbol:       "BTC",
		Quantity:         decimal.NewFromInt(1),
		AvailableBalance: decimal.NewFromInt(1),
		LockedBalance:    decimal.Zero,
	}
	require.NoError(t, db.Create(&wallet).Error)

	require.Error(t, db.Model(&model.Wallet{}).
		Where("id = ?", wallet.ID).
		Update("available_balance", decimal.NewFromInt(-1)).Error)
}

func TestIntegrationTradePositivePriceAndQuantityConstraints(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	keyPrefix := fmt.Sprintf("repo-invalid-trade-%d", time.Now().UnixNano())
	defer func() {
		require.NoError(t, db.Where("idempotency_key LIKE ?", keyPrefix+"%").Delete(&model.Trade{}).Error)
	}()

	zeroPriceTrade := model.Trade{
		IdempotencyKey: keyPrefix + "-zero-price",
		CoinSymbol:     "BTC",
		Price:          decimal.Zero,
		Quantity:       decimal.NewFromInt(1),
		TradedAt:       time.Now(),
		BuyOrderID:     41,
		SellOrderID:    42,
	}
	require.Error(t, db.Create(&zeroPriceTrade).Error)

	negativeQuantityTrade := model.Trade{
		IdempotencyKey: keyPrefix + "-negative-quantity",
		CoinSymbol:     "BTC",
		Price:          decimal.NewFromInt(1),
		Quantity:       decimal.NewFromInt(-1),
		TradedAt:       time.Now(),
		BuyOrderID:     43,
		SellOrderID:    44,
	}
	require.Error(t, db.Create(&negativeQuantityTrade).Error)
}

func TestIntegrationLimitOrderPositiveAmountConstraint(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(7)
	defer cleanupRepositoryUsers(t, db, userID)

	order := model.Order{
		UserID:       userID,
		CoinSymbol:   "BTC",
		Side:         model.OrderSideBuy,
		OrderType:    model.OrderTypeLimit,
		Price:        decimal.NewFromInt(100),
		Amount:       decimal.Zero,
		Status:       model.OrderStatusPending,
		FilledAmount: decimal.Zero,
	}

	require.Error(t, db.Create(&order).Error)
}

func TestIntegrationBatchUpdateBalancesUpdatesMultipleWallets(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userA := repositoryTestUserID(20)
	userB := repositoryTestUserID(21)
	defer cleanupRepositoryUsers(t, db, userA, userB)

	repo := NewWalletRepository(db)
	walletA := model.Wallet{UserID: userA, CoinSymbol: "KRW", KRW: decimal.NewFromInt(1000), AvailableBalance: decimal.NewFromInt(1000), LockedBalance: decimal.Zero}
	walletB := model.Wallet{UserID: userB, CoinSymbol: "BTC", Quantity: decimal.NewFromInt(5), AvailableBalance: decimal.NewFromInt(5), LockedBalance: decimal.Zero, AvgBuyPrice: decimal.NewFromInt(90)}
	require.NoError(t, db.Create(&walletA).Error)
	require.NoError(t, db.Create(&walletB).Error)

	err := repo.BatchUpdateBalances([]WalletBatchUpdate{
		{WalletID: walletA.ID, AvailableBalance: decimal.NewFromInt(400), LockedBalance: decimal.NewFromInt(600), KRW: decimal.NewFromInt(1000), Quantity: decimal.Zero, AvgBuyPrice: decimal.Zero},
		{WalletID: walletB.ID, AvailableBalance: decimal.NewFromInt(2), LockedBalance: decimal.NewFromInt(3), KRW: decimal.Zero, Quantity: decimal.NewFromInt(5), AvgBuyPrice: decimal.NewFromInt(100)},
	})
	require.NoError(t, err)

	updatedA, err := repo.FindByUserIDAndCoinSymbol(userA, "KRW")
	require.NoError(t, err)
	assert.True(t, updatedA.AvailableBalance.Equal(decimal.NewFromInt(400)))
	assert.True(t, updatedA.LockedBalance.Equal(decimal.NewFromInt(600)))
	assert.True(t, updatedA.KRW.Equal(decimal.NewFromInt(1000)))

	updatedB, err := repo.FindByUserIDAndCoinSymbol(userB, "BTC")
	require.NoError(t, err)
	assert.True(t, updatedB.AvailableBalance.Equal(decimal.NewFromInt(2)))
	assert.True(t, updatedB.LockedBalance.Equal(decimal.NewFromInt(3)))
	assert.True(t, updatedB.Quantity.Equal(decimal.NewFromInt(5)))
	assert.True(t, updatedB.AvgBuyPrice.Equal(decimal.NewFromInt(100)))
}

func TestIntegrationBatchUpdateBalancesEmptySliceIsNoop(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewWalletRepository(db)

	require.NoError(t, repo.BatchUpdateBalances(nil))
}

func TestIntegrationBatchUpdateBalancesMissingWalletReturnsError(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewWalletRepository(db)

	err := repo.BatchUpdateBalances([]WalletBatchUpdate{
		{WalletID: 999999999, AvailableBalance: decimal.NewFromInt(1), LockedBalance: decimal.Zero, KRW: decimal.NewFromInt(1), Quantity: decimal.Zero, AvgBuyPrice: decimal.Zero},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected")
}

func TestIntegrationFindByKeysReturnsOnlyMatchingWallets(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userA := repositoryTestUserID(50)
	userB := repositoryTestUserID(51)
	defer cleanupRepositoryUsers(t, db, userA, userB)

	repo := NewWalletRepository(db)
	walletA_KRW := model.Wallet{UserID: userA, CoinSymbol: "KRW", KRW: decimal.NewFromInt(1000), AvailableBalance: decimal.NewFromInt(1000), LockedBalance: decimal.Zero}
	walletA_BTC := model.Wallet{UserID: userA, CoinSymbol: "BTC", Quantity: decimal.NewFromInt(5), AvailableBalance: decimal.NewFromInt(5), LockedBalance: decimal.Zero}
	walletB_KRW := model.Wallet{UserID: userB, CoinSymbol: "KRW", KRW: decimal.NewFromInt(2000), AvailableBalance: decimal.NewFromInt(2000), LockedBalance: decimal.Zero}

	require.NoError(t, db.Create(&walletA_KRW).Error)
	require.NoError(t, db.Create(&walletA_BTC).Error)
	require.NoError(t, db.Create(&walletB_KRW).Error)

	// Find only (userA, KRW) and (userB, KRW)
	got, err := repo.FindByKeys([]WalletKey{
		{UserID: userA, CoinSymbol: "KRW"},
		{UserID: userB, CoinSymbol: "KRW"},
	})
	require.NoError(t, err)

	require.Len(t, got, 2)
	// Verify we got the right wallets (exact matches only, not BTC)
	foundKRWCount := 0
	for _, w := range got {
		assert.Equal(t, "KRW", w.CoinSymbol)
		if w.UserID == userA || w.UserID == userB {
			foundKRWCount++
		}
	}
	assert.Equal(t, 2, foundKRWCount)
}

func TestIntegrationCreateZeroBalanceWalletsIsIdempotent(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(52)
	defer cleanupRepositoryUsers(t, db, userID)

	repo := NewWalletRepository(db)
	key := WalletKey{UserID: userID, CoinSymbol: "TEST"}

	// First call: create the wallet
	err := repo.CreateZeroBalanceWallets([]WalletKey{key})
	require.NoError(t, err)

	wallet1, err := repo.FindByUserIDAndCoinSymbol(userID, "TEST")
	require.NoError(t, err)
	walletID1 := wallet1.ID

	// Second call: try to create again (should be idempotent)
	err = repo.CreateZeroBalanceWallets([]WalletKey{key})
	require.NoError(t, err)

	wallet2, err := repo.FindByUserIDAndCoinSymbol(userID, "TEST")
	require.NoError(t, err)

	// Same wallet ID, not a duplicate
	assert.Equal(t, walletID1, wallet2.ID)
	// All balances are zero
	assert.True(t, wallet2.KRW.Equal(decimal.Zero))
	assert.True(t, wallet2.Quantity.Equal(decimal.Zero))
	assert.True(t, wallet2.AvailableBalance.Equal(decimal.Zero))
	assert.True(t, wallet2.LockedBalance.Equal(decimal.Zero))
	assert.True(t, wallet2.AvgBuyPrice.Equal(decimal.Zero))
}
