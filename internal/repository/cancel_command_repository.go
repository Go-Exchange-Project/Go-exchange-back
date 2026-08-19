package repository

import (
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type CancelCommandRepository struct {
	DB *gorm.DB
}

func NewCancelCommandRepository(db *gorm.DB) *CancelCommandRepository {
	return &CancelCommandRepository{DB: db}
}

func (r *CancelCommandRepository) WithTx(tx *gorm.DB) *CancelCommandRepository {
	return &CancelCommandRepository{DB: tx}
}

// CreateOrGet은 주문당 하나뿐인 command를 만들거나 기존 것을 돌려줍니다.
//
// unique violation을 그대로 내면 호출자의 트랜잭션이 abort되므로(주문 lock과 같은
// 트랜잭션에서 호출된다) ON CONFLICT DO NOTHING으로 에러 없이 넘긴 뒤 기존 행을
// 조회합니다.
func (r *CancelCommandRepository) CreateOrGet(command *model.CancelCommand) (*model.CancelCommand, bool, error) {
	result := r.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "order_id"}},
		DoNothing: true,
	}).Create(command)
	if result.Error != nil {
		return nil, false, result.Error
	}
	if result.RowsAffected == 1 {
		return command, true, nil
	}

	var existing model.CancelCommand
	if err := r.DB.Where("order_id = ?", command.OrderID).First(&existing).Error; err != nil {
		return nil, false, err
	}
	return &existing, false, nil
}

// FindPending은 worker의 dispatch 후보를 ID 오름차순으로 조회합니다.
func (r *CancelCommandRepository) FindPending(limit int) ([]model.CancelCommand, error) {
	var commands []model.CancelCommand
	err := r.DB.
		Where("status = ?", model.CancelCommandStatusPending).
		Order("id ASC").
		Limit(limit).
		Find(&commands).Error
	return commands, err
}

// FindStatuses는 worker가 awaiting_outbox 엔트리의 결말을 확인할 때 씁니다.
// PENDING 스캔에 LIMIT이 있으면 "안 보임"과 "완료됨"이 구분되지 않으므로 이렇게
// ID를 명시해 조회해야 합니다.
func (r *CancelCommandRepository) FindStatuses(ids []uint64) ([]model.CancelCommand, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var commands []model.CancelCommand
	err := r.DB.Where("id IN ?", ids).Find(&commands).Error
	return commands, err
}

// MarkNoop은 할 일이 없던 command를 종결합니다. 이미 PROCESSED인 행은 건드리지
// 않습니다 — outbox가 이미 만든 사실을 지우면 안 됩니다. 갱신된 행이 없으면
// nil을 돌려줍니다.
func (r *CancelCommandRepository) MarkNoop(id uint64) (*model.CancelCommand, error) {
	result := r.DB.Model(&model.CancelCommand{}).
		Where("id = ? AND status = ?", id, model.CancelCommandStatusPending).
		Updates(map[string]any{
			"status":     model.CancelCommandStatusNoop,
			"updated_at": time.Now().UTC(),
		})
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil
	}

	var updated model.CancelCommand
	if err := r.DB.Where("id = ?", id).First(&updated).Error; err != nil {
		return nil, err
	}
	return &updated, nil
}

// RecordAttempt는 재시도를 관측용으로 기록합니다. attempt_count는 재시도 예산이
// 아니므로 상태를 바꾸지 않습니다 — 취소는 포기하면 안 됩니다.
func (r *CancelCommandRepository) RecordAttempt(id uint64, message string) error {
	return r.DB.Model(&model.CancelCommand{}).
		Where("id = ? AND status = ?", id, model.CancelCommandStatusPending).
		Updates(map[string]any{
			"attempt_count": gorm.Expr("attempt_count + 1"),
			"last_error":    message,
			"updated_at":    time.Now().UTC(),
		}).Error
}

// CountPending은 부팅 장벽의 drain 완료 판정에 씁니다.
func (r *CancelCommandRepository) CountPending() (int64, error) {
	var count int64
	err := r.DB.Model(&model.CancelCommand{}).
		Where("status = ?", model.CancelCommandStatusPending).
		Count(&count).Error
	return count, err
}
