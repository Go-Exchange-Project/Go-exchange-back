package repository

import (
	"errors"
	"fmt"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type FailedSettlementRepository struct {
	DB *gorm.DB
}

const (
	DefaultFailedSettlementListLimit = 50
	MaxFailedSettlementListLimit     = 200
)

func NewFailedSettlementRepository(db *gorm.DB) *FailedSettlementRepository {
	return &FailedSettlementRepository{DB: db}
}

func (r *FailedSettlementRepository) RecordFailure(failure *model.FailedSettlement) (*model.FailedSettlement, error) {
	if failure == nil {
		return nil, fmt.Errorf("failed settlement is required")
	}
	if r == nil || r.DB == nil {
		return nil, fmt.Errorf("failed settlement repository DB is required")
	}

	now := time.Now().UTC()
	result := r.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "trade_idempotency_key"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"error_message": failure.ErrorMessage,
			"status":        model.FailedSettlementStatusOpen,
			"retry_count":   gorm.Expr("failed_settlements.retry_count + ?", 1),
			"resolution":    "",
			"resolved_by":   "",
			"notes":         "",
			"resolved_at":   nil,
			"updated_at":    now,
		}),
	}).Create(failure)
	if result.Error != nil {
		return nil, result.Error
	}

	var persisted model.FailedSettlement
	if err := r.DB.Where("trade_idempotency_key = ?", failure.TradeIdempotencyKey).First(&persisted).Error; err != nil {
		return nil, err
	}
	return &persisted, nil
}

func (r *FailedSettlementRepository) FindOpen(limit int) ([]model.FailedSettlement, error) {
	if r == nil || r.DB == nil {
		return nil, fmt.Errorf("failed settlement repository DB is required")
	}

	var failures []model.FailedSettlement
	err := r.DB.
		Where("status = ?", model.FailedSettlementStatusOpen).
		Order("occurred_at ASC").
		Order("id ASC").
		Limit(NormalizeFailedSettlementListLimit(limit)).
		Find(&failures).Error
	return failures, err
}

func (r *FailedSettlementRepository) FindByID(id uint) (*model.FailedSettlement, error) {
	if r == nil || r.DB == nil {
		return nil, fmt.Errorf("failed settlement repository DB is required")
	}
	if id == 0 {
		return nil, fmt.Errorf("failed settlement id is required")
	}

	var failure model.FailedSettlement
	if err := r.DB.First(&failure, id).Error; err != nil {
		return nil, err
	}
	return &failure, nil
}

func (r *FailedSettlementRepository) MarkResolved(id uint, resolution string, resolvedBy string, notes string) error {
	if r == nil || r.DB == nil {
		return fmt.Errorf("failed settlement repository DB is required")
	}
	if id == 0 {
		return fmt.Errorf("failed settlement id is required")
	}

	existing, err := r.FindByID(id)
	if err != nil {
		return err
	}
	if existing.Status == model.FailedSettlementStatusResolved {
		return nil
	}
	if existing.Status != model.FailedSettlementStatusOpen {
		return fmt.Errorf("failed settlement %d has unsupported status %s", id, existing.Status)
	}

	now := time.Now().UTC()
	result := r.DB.Model(&model.FailedSettlement{}).
		Where("id = ? AND status = ?", id, model.FailedSettlementStatusOpen).
		Updates(map[string]interface{}{
			"status":      model.FailedSettlementStatusResolved,
			"resolution":  resolution,
			"resolved_by": resolvedBy,
			"notes":       notes,
			"resolved_at": &now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New("failed settlement resolve affected no rows")
	}
	return nil
}

func NormalizeFailedSettlementListLimit(limit int) int {
	if limit <= 0 {
		return DefaultFailedSettlementListLimit
	}
	if limit > MaxFailedSettlementListLimit {
		return MaxFailedSettlementListLimit
	}
	return limit
}
