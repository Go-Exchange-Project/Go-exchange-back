package repository

import (
	"fmt"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"gorm.io/gorm"
)

type TradeOutboxRepository struct {
	DB *gorm.DB
}

func NewTradeOutboxRepository(db *gorm.DB) *TradeOutboxRepository {
	return &TradeOutboxRepository{DB: db}
}

// InsertBatch는 한 트랜잭션(단일 INSERT)으로 이벤트 배치를 커밋합니다.
// 성공 시 각 이벤트의 ID가 채워집니다.
func (r *TradeOutboxRepository) InsertBatch(events []*model.TradeOutboxEvent) error {
	if len(events) == 0 {
		return nil
	}
	return r.DB.Create(&events).Error
}

// FindPendingAfter는 부팅 리플레이용으로 PENDING 이벤트를 ID 순으로 페이지 조회합니다.
func (r *TradeOutboxRepository) FindPendingAfter(afterID uint64, limit int) ([]model.TradeOutboxEvent, error) {
	var events []model.TradeOutboxEvent
	err := r.DB.
		Where("status = ?", model.TradeOutboxStatusPending).
		Where("id > ?", afterID).
		Order("id ASC").
		Limit(limit).
		Find(&events).Error
	return events, err
}

// MarkProcessed는 처리(정산 성공, 멱등 no-op, 또는 실패의 내구 기록 완료)가 끝난
// 이벤트를 PROCESSED로 마킹합니다. 마킹 실패는 유실이 아니라 다음 리플레이의
// 중복 처리로 이어질 뿐이며, 정산 멱등성 키가 이를 no-op으로 만듭니다.
func (r *TradeOutboxRepository) MarkProcessed(id uint64) error {
	now := time.Now().UTC()
	result := r.DB.Model(&model.TradeOutboxEvent{}).
		Where("id = ?", id).
		Updates(map[string]interface{}{
			"status":       model.TradeOutboxStatusProcessed,
			"processed_at": now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("mark trade outbox event %d processed affected no rows", id)
	}
	return nil
}
