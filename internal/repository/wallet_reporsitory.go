package repository

import (
    "gorm.io/gorm"
    "github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
)

type WalletRepository struct {
    DB *gorm.DB
}

func NewWalletRepository(db *gorm.DB) *WalletRepository {
    return &WalletRepository{DB: db}
}

func (r *WalletRepository) FindByUserID(userID uint) (*model.Wallet, error) {
    var wallet model.Wallet
    err := r.DB.Where("user_id = ?", userID).First(&wallet).Error
    return &wallet, err
}

func (r *WalletRepository) UpdateKRW(userID uint, krw decimal.Decimal) error {
    return r.DB.Model(&model.Wallet{}).Where("user_id = ?", userID).Update("krw", krw).Error
}

func (r *WalletRepository) UpdateCoinQuantity(userID uint, coinSymbol string, quantity decimal.Decimal) error {
    return r.DB.Model(&model.Wallet{}).Where("user_id = ? AND coin_symbol = ?", userID, coinSymbol).Update("quantity", quantity).Error
}