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

// InsertBatch는 한 트랜잭션으로 이벤트 배치를 커밋합니다.
// 성공 시 각 이벤트의 ID가 채워집니다.
func (r *TradeOutboxRepository) InsertBatch(events []*model.TradeOutboxEvent) error {
	return r.InsertBatchAndMarkCancelCommands(events, nil)
}

// InsertBatchAndMarkCancelCommands는 이벤트 배치 INSERT와 해당 취소 command의
// PROCESSED 전환을 한 커밋(단일 fsync)에 묶습니다. SQL은 두 문장이지만 둘 중
// 하나만 남는 상태가 없어야 합니다 — 갈라지면 크래시 복구가 "재실행"으로도
// "outbox replay"로도 덮지 못하는 구간이 생깁니다.
//
// commandIDs가 비어 있으면 UPDATE를 아예 실행하지 않습니다. 취소가 섞이지 않은
// 대다수 배치에 문장을 추가하지 않기 위해서입니다.
func (r *TradeOutboxRepository) InsertBatchAndMarkCancelCommands(events []*model.TradeOutboxEvent, commandIDs []uint64) error {
	if len(events) == 0 && len(commandIDs) == 0 {
		return nil
	}

	// 한 배치에 같은 command의 이벤트가 두 번 들어올 수 있고, command 없이 들어온
	// 취소는 0으로 온다. 그대로 두면 행 수 검사가 어긋나 정상 배치가 rollback된다.
	ids := dedupeNonzeroUint64(commandIDs)

	return r.DB.Transaction(func(tx *gorm.DB) error {
		if len(events) > 0 {
			if err := tx.Create(&events).Error; err != nil {
				return err
			}
		}
		if len(ids) == 0 {
			return nil
		}

		result := tx.Model(&model.CancelCommand{}).
			Where("id IN ? AND status = ?", ids, model.CancelCommandStatusPending).
			Updates(map[string]any{
				"status":     model.CancelCommandStatusProcessed,
				"updated_at": time.Now().UTC(),
			})
		if result.Error != nil {
			return result.Error
		}
		// 불일치는 "이미 처리됐다" 또는 "command가 없다"이며, 둘 다 조용히 넘기면
		// 안 되는 상태다. 트랜잭션을 통째로 되돌린다.
		if int(result.RowsAffected) != len(ids) {
			return fmt.Errorf("mark cancel commands processed affected %d rows, expected %d", result.RowsAffected, len(ids))
		}
		return nil
	})
}

func dedupeNonzeroUint64(values []uint64) []uint64 {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[uint64]struct{}, len(values))
	deduped := make([]uint64, 0, len(values))
	for _, value := range values {
		if value == 0 {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		deduped = append(deduped, value)
	}
	return deduped
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
