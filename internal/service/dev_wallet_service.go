package service

import (
	"fmt"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type DevWalletService struct {
	DB *gorm.DB
}

type FundWalletInput struct {
	UserID     uint
	CoinSymbol string
	Amount     string
}

func NewDevWalletService(db *gorm.DB) *DevWalletService {
	return &DevWalletService{DB: db}
}

func (s *DevWalletService) FundWallet(input FundWalletInput) (*model.Wallet, error) {
	if s == nil || s.DB == nil {
		return nil, fmt.Errorf("database is required")
	}

	userID, coinSymbol, amount, err := normalizeFundWalletInput(input)
	if err != nil {
		return nil, err
	}

	var wallet model.Wallet
	if err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := upsertFundedWallet(tx, userID, coinSymbol, amount); err != nil {
			return err
		}
		if err := tx.
			Where("user_id = ? AND coin_symbol = ?", userID, coinSymbol).
			First(&wallet).Error; err != nil {
			return err
		}
		entry := devFundLedgerEntry(&wallet, amount)
		return repository.NewLedgerRepository(tx).Create(&entry)
	}); err != nil {
		return nil, err
	}

	return &wallet, nil
}

func normalizeFundWalletInput(input FundWalletInput) (uint, string, decimal.Decimal, error) {
	if input.UserID == 0 {
		return 0, "", decimal.Zero, NewValidationErrorf("user_id is required")
	}

	coinSymbol := normalizeCoinSymbol(input.CoinSymbol)
	if coinSymbol == "" {
		return 0, "", decimal.Zero, NewValidationErrorf("coin_symbol is required")
	}

	amount, err := parsePositiveDecimal(input.Amount, "amount")
	if err != nil {
		return 0, "", decimal.Zero, err
	}

	return input.UserID, coinSymbol, amount, nil
}

func upsertFundedWallet(tx *gorm.DB, userID uint, coinSymbol string, amount decimal.Decimal) error {
	wallet := model.Wallet{
		UserID:           userID,
		CoinSymbol:       coinSymbol,
		AvailableBalance: amount,
		LockedBalance:    decimal.Zero,
		AvgBuyPrice:      decimal.Zero,
	}

	assignments := map[string]interface{}{
		"available_balance": gorm.Expr(`"wallets"."available_balance" + ?`, amount),
	}
	if coinSymbol == model.KRWAssetSymbol {
		wallet.KRW = amount
		wallet.Quantity = decimal.Zero
		assignments["krw"] = gorm.Expr(`"wallets"."krw" + ?`, amount)
	} else {
		wallet.KRW = decimal.Zero
		wallet.Quantity = amount
		assignments["quantity"] = gorm.Expr(`"wallets"."quantity" + ?`, amount)
	}

	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "user_id"},
			{Name: "coin_symbol"},
		},
		DoUpdates: clause.Assignments(assignments),
	}).Create(&wallet).Error
}
