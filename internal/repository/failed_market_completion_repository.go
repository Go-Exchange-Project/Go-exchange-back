package repository

import (
	"fmt"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type FailedMarketCompletionRepository struct {
	DB *gorm.DB
}

func NewFailedMarketCompletionRepository(db *gorm.DB) *FailedMarketCompletionRepository {
	return &FailedMarketCompletionRepository{DB: db}
}

func (r *FailedMarketCompletionRepository) RecordFailure(failure *model.FailedMarketCompletion) (*model.FailedMarketCompletion, error) {
	if failure == nil {
		return nil, fmt.Errorf("failed market completion is required")
	}
	if r == nil || r.DB == nil {
		return nil, fmt.Errorf("failed market completion repository DB is required")
	}

	now := time.Now().UTC()
	result := r.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "order_id"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"error_message": failure.ErrorMessage,
			"status":        model.FailedSettlementStatusOpen,
			"retry_count":   gorm.Expr("failed_market_completions.retry_count + ?", 1),
			"resolution":    "",
			"resolved_at":   nil,
			"updated_at":    now,
		}),
	}).Create(failure)
	if result.Error != nil {
		return nil, result.Error
	}

	var persisted model.FailedMarketCompletion
	if err := r.DB.Where("order_id = ?", failure.OrderID).First(&persisted).Error; err != nil {
		return nil, err
	}
	return &persisted, nil
}

func (r *FailedMarketCompletionRepository) FindOpen(limit int) ([]model.FailedMarketCompletion, error) {
	if r == nil || r.DB == nil {
		return nil, fmt.Errorf("failed market completion repository DB is required")
	}

	var failures []model.FailedMarketCompletion
	err := r.DB.
		Where("status = ?", model.FailedSettlementStatusOpen).
		Order("occurred_at ASC").
		Order("id ASC").
		Limit(NormalizeFailedSettlementListLimit(limit)).
		Find(&failures).Error
	return failures, err
}

func (r *FailedMarketCompletionRepository) MarkResolved(id uint, resolution string) error {
	if r == nil || r.DB == nil {
		return fmt.Errorf("failed market completion repository DB is required")
	}
	if id == 0 {
		return fmt.Errorf("failed market completion id is required")
	}

	now := time.Now().UTC()
	result := r.DB.Model(&model.FailedMarketCompletion{}).
		Where("id = ? AND status = ?", id, model.FailedSettlementStatusOpen).
		Updates(map[string]interface{}{
			"status":      model.FailedSettlementStatusResolved,
			"resolution":  resolution,
			"resolved_at": &now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("failed market completion %d resolve affected no rows", id)
	}
	return nil
}
