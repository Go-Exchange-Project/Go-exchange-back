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

// EnsureDeferred는 terminal을 실행하지 않고 내구적으로 미룰 때 쓴다.
// RecordFailure와 달리 ON CONFLICT DO NOTHING 의미론이라 기존 행의 status·
// resolved_at·retry_count를 건드리지 않는다 — 특히 RESOLVED를 OPEN으로 되돌리지
// 않는다(이미 실행된 terminal을 재실행 대상으로 되살리게 된다).
func (r *FailedMarketCompletionRepository) EnsureDeferred(failure *model.FailedMarketCompletion) (*model.FailedMarketCompletion, error) {
	if failure == nil {
		return nil, fmt.Errorf("failed market completion is required")
	}
	if r == nil || r.DB == nil {
		return nil, fmt.Errorf("failed market completion repository DB is required")
	}

	if err := r.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "order_id"}},
		DoNothing: true,
	}).Create(failure).Error; err != nil {
		return nil, err
	}

	// DoNothing은 기존 행을 돌려주지 않으므로 조회가 필요하다.
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
