package repository

import (
	"fmt"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type FailedOrderCancellationRepository struct {
	DB *gorm.DB
}

func NewFailedOrderCancellationRepository(db *gorm.DB) *FailedOrderCancellationRepository {
	return &FailedOrderCancellationRepository{DB: db}
}

func (r *FailedOrderCancellationRepository) RecordFailure(failure *model.FailedOrderCancellation) (*model.FailedOrderCancellation, error) {
	if failure == nil {
		return nil, fmt.Errorf("failed order cancellation is required")
	}
	if r == nil || r.DB == nil {
		return nil, fmt.Errorf("failed order cancellation repository DB is required")
	}

	now := time.Now().UTC()
	if err := r.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "order_id"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"error_message": failure.ErrorMessage,
			"status":        model.FailedSettlementStatusOpen,
			"retry_count":   gorm.Expr("failed_order_cancellations.retry_count + ?", 1),
			"resolution":    "",
			"resolved_at":   nil,
			"updated_at":    now,
		}),
	}).Create(failure).Error; err != nil {
		return nil, err
	}

	var persisted model.FailedOrderCancellation
	if err := r.DB.Where("order_id = ?", failure.OrderID).First(&persisted).Error; err != nil {
		return nil, err
	}
	return &persisted, nil
}

// EnsureDeferred는 실행하지 않고 미룰 때 쓴다 — 기존 행의 status·retry_count를
// 건드리지 않는다(RESOLVED를 OPEN으로 되돌리지 않는다).
func (r *FailedOrderCancellationRepository) EnsureDeferred(failure *model.FailedOrderCancellation) (*model.FailedOrderCancellation, error) {
	if failure == nil {
		return nil, fmt.Errorf("failed order cancellation is required")
	}
	if r == nil || r.DB == nil {
		return nil, fmt.Errorf("failed order cancellation repository DB is required")
	}

	if err := r.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "order_id"}},
		DoNothing: true,
	}).Create(failure).Error; err != nil {
		return nil, err
	}

	var persisted model.FailedOrderCancellation
	if err := r.DB.Where("order_id = ?", failure.OrderID).First(&persisted).Error; err != nil {
		return nil, err
	}
	return &persisted, nil
}

func (r *FailedOrderCancellationRepository) FindOpen(limit int) ([]model.FailedOrderCancellation, error) {
	if r == nil || r.DB == nil {
		return nil, fmt.Errorf("failed order cancellation repository DB is required")
	}

	var failures []model.FailedOrderCancellation
	err := r.DB.
		Where("status = ?", model.FailedSettlementStatusOpen).
		Order("occurred_at ASC").
		Order("id ASC").
		Limit(NormalizeFailedSettlementListLimit(limit)).
		Find(&failures).Error
	return failures, err
}

func (r *FailedOrderCancellationRepository) MarkResolved(id uint, resolution string) error {
	if r == nil || r.DB == nil {
		return fmt.Errorf("failed order cancellation repository DB is required")
	}
	if id == 0 {
		return fmt.Errorf("failed order cancellation id is required")
	}

	now := time.Now().UTC()
	result := r.DB.Model(&model.FailedOrderCancellation{}).
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
		return fmt.Errorf("failed order cancellation %d resolve affected no rows", id)
	}
	return nil
}
