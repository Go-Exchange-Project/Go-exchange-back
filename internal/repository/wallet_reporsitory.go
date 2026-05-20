package repository

import (
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

func (r *WalletRepository) UpdateKRW(userID uint, krw decimal.Decimal) error {
	return requireRowsAffected(r.updateKRWDB(userID, krw), "wallet KRW update")
}

func (r *WalletRepository) UpdateCoinQuantity(userID uint, coinSymbol string, quantity decimal.Decimal) error {
	return requireRowsAffected(r.updateCoinQuantityDB(userID, coinSymbol, quantity), "wallet quantity update")
}

func (r *WalletRepository) UpdateBalances(userID uint, coinSymbol string, available decimal.Decimal, locked decimal.Decimal) error {
	return requireRowsAffected(r.updateBalancesDB(userID, coinSymbol, available, locked), "wallet balance update")
}

func (r *WalletRepository) walletByUserAndCoin(userID uint, coinSymbol string) *gorm.DB {
	query, args := walletByUserAndCoinScope(userID, coinSymbol)
	return r.DB.Where(query, args...)
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

	return r.DB.Model(&model.Wallet{}).
		Where(query, args...).
		Updates(updates)
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
