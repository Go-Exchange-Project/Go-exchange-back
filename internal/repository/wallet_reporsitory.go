package repository

import (
	"errors"
	"fmt"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type WalletRepository struct {
	DB *gorm.DB
}

func NewWalletRepository(db *gorm.DB) *WalletRepository {
	return &WalletRepository{DB: db}
}

func (r *WalletRepository) WithTx(tx *gorm.DB) *WalletRepository {
	return &WalletRepository{DB: tx}
}

func (r *WalletRepository) FindByUserID(userID uint) (*model.Wallet, error) {
	return r.FindKRWWalletByUserID(userID)
}

func (r *WalletRepository) FindKRWWalletByUserID(userID uint) (*model.Wallet, error) {
	return r.FindByUserIDAndCoinSymbol(userID, model.KRWAssetSymbol)
}

func (r *WalletRepository) FindByUserIDAndCoinSymbol(userID uint, coinSymbol string) (*model.Wallet, error) {
	var wallet model.Wallet
	err := r.walletByUserAndCoin(userID, coinSymbol).First(&wallet).Error
	return &wallet, err
}

func (r *WalletRepository) ListByUserID(userID uint) ([]model.Wallet, error) {
	var wallets []model.Wallet
	err := r.DB.
		Where("user_id = ?", userID).
		Order("coin_symbol ASC").
		Find(&wallets).Error
	return wallets, err
}

func (r *WalletRepository) FindByUserIDAndCoinSymbolForUpdate(userID uint, coinSymbol string) (*model.Wallet, error) {
	var wallet model.Wallet
	err := r.walletByUserAndCoin(userID, coinSymbol).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		First(&wallet).Error
	return &wallet, err
}

func (r *WalletRepository) FindKRWWalletByUserIDForUpdate(userID uint) (*model.Wallet, error) {
	return r.FindByUserIDAndCoinSymbolForUpdate(userID, model.KRWAssetSymbol)
}

func (r *WalletRepository) FindOrCreateByUserIDAndCoinSymbolForUpdate(userID uint, coinSymbol string) (*model.Wallet, error) {
	wallet, err := r.FindByUserIDAndCoinSymbolForUpdate(userID, coinSymbol)
	if err == nil {
		return wallet, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	if err := r.createZeroBalanceWallet(userID, coinSymbol); err != nil {
		return nil, err
	}
	return r.FindByUserIDAndCoinSymbolForUpdate(userID, coinSymbol)
}

func (r *WalletRepository) FindOrCreateKRWWalletByUserIDForUpdate(userID uint) (*model.Wallet, error) {
	return r.FindOrCreateByUserIDAndCoinSymbolForUpdate(userID, model.KRWAssetSymbol)
}

func (r *WalletRepository) UpdateKRW(userID uint, krw decimal.Decimal) error {
	return requireRowsAffected(r.updateKRWDB(userID, krw), "wallet KRW update")
}

func (r *WalletRepository) UpdateCoinQuantity(userID uint, coinSymbol string, quantity decimal.Decimal) error {
	return requireRowsAffected(r.updateCoinQuantityDB(userID, coinSymbol, quantity), "wallet quantity update")
}

func (r *WalletRepository) UpdateBalances(userID uint, coinSymbol string, available decimal.Decimal, locked decimal.Decimal) error {
	return requireRowsAffected(r.updateBalancesDB(userID, coinSymbol, available, locked), "wallet balance update")
}

func (r *WalletRepository) UpdateBalancesAndAvgBuyPrice(userID uint, coinSymbol string, available decimal.Decimal, locked decimal.Decimal, avgBuyPrice decimal.Decimal) error {
	return requireRowsAffected(r.updateBalancesAndAvgBuyPriceDB(userID, coinSymbol, available, locked, avgBuyPrice), "wallet balance and avg buy price update")
}

func (r *WalletRepository) walletByUserAndCoin(userID uint, coinSymbol string) *gorm.DB {
	query, args := walletByUserAndCoinScope(userID, coinSymbol)
	return r.DB.Where(query, args...)
}

func (r *WalletRepository) createZeroBalanceWallet(userID uint, coinSymbol string) error {
	wallet := model.Wallet{
		UserID:           userID,
		CoinSymbol:       coinSymbol,
		KRW:              decimal.Zero,
		Quantity:         decimal.Zero,
		AvailableBalance: decimal.Zero,
		LockedBalance:    decimal.Zero,
		AvgBuyPrice:      decimal.Zero,
	}

	return r.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "user_id"},
			{Name: "coin_symbol"},
		},
		DoNothing: true,
	}).Create(&wallet).Error
}

func (r *WalletRepository) updateKRWDB(userID uint, krw decimal.Decimal) *gorm.DB {
	query, args := walletByUserAndCoinScope(userID, model.KRWAssetSymbol)
	return r.DB.Model(&model.Wallet{}).
		Where(query, args...).
		Update("krw", krw)
}

func (r *WalletRepository) updateCoinQuantityDB(userID uint, coinSymbol string, quantity decimal.Decimal) *gorm.DB {
	query, args := walletByUserAndCoinScope(userID, coinSymbol)
	return r.DB.Model(&model.Wallet{}).
		Where(query, args...).
		Update("quantity", quantity)
}

func (r *WalletRepository) updateBalancesDB(userID uint, coinSymbol string, available decimal.Decimal, locked decimal.Decimal) *gorm.DB {
	query, args := walletByUserAndCoinScope(userID, coinSymbol)
	updates := walletBalanceUpdates(coinSymbol, available, locked)

	return r.DB.Model(&model.Wallet{}).
		Where(query, args...).
		Updates(updates)
}

func (r *WalletRepository) updateBalancesAndAvgBuyPriceDB(userID uint, coinSymbol string, available decimal.Decimal, locked decimal.Decimal, avgBuyPrice decimal.Decimal) *gorm.DB {
	query, args := walletByUserAndCoinScope(userID, coinSymbol)
	updates := walletBalanceUpdates(coinSymbol, available, locked)
	updates["avg_buy_price"] = avgBuyPrice

	return r.DB.Model(&model.Wallet{}).
		Where(query, args...).
		Updates(updates)
}

func walletBalanceUpdates(coinSymbol string, available decimal.Decimal, locked decimal.Decimal) map[string]interface{} {
	total := available.Add(locked)
	updates := map[string]interface{}{
		"available_balance": available,
		"locked_balance":    locked,
	}
	if coinSymbol == model.KRWAssetSymbol {
		updates["krw"] = total
	} else {
		updates["quantity"] = total
	}
	return updates
}

func walletByUserAndCoinScope(userID uint, coinSymbol string) (string, []interface{}) {
	return "user_id = ? AND coin_symbol = ?", []interface{}{userID, coinSymbol}
}

func requireRowsAffected(result *gorm.DB, operation string) error {
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("%s affected no rows", operation)
	}
	return nil
}
