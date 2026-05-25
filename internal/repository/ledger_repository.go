package repository

import (
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"gorm.io/gorm"
)

type LedgerRepository struct {
	DB *gorm.DB
}

func NewLedgerRepository(db *gorm.DB) *LedgerRepository {
	return &LedgerRepository{DB: db}
}

func (r *LedgerRepository) WithTx(tx *gorm.DB) *LedgerRepository {
	return &LedgerRepository{DB: tx}
}

func (r *LedgerRepository) Create(entry *model.LedgerEntry) error {
	return r.DB.Create(entry).Error
}

func (r *LedgerRepository) CreateMany(entries []model.LedgerEntry) error {
	if len(entries) == 0 {
		return nil
	}
	return r.DB.Create(&entries).Error
}

func (r *LedgerRepository) ListByUserID(userID uint, limit int) ([]model.LedgerEntry, error) {
	var entries []model.LedgerEntry
	err := r.DB.
		Where("user_id = ?", userID).
		Order("created_at DESC").
		Order("id DESC").
		Limit(limit).
		Find(&entries).Error
	return entries, err
}
