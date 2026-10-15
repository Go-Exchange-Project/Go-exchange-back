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

// SettlementFailureHandoff는 RecordFailuresAndMarkOutboxProcessed의 입력 항목이다.
// failure와 outbox ID를 병렬 슬라이스 두 개가 아니라 한 구조체로 묶어 대응 오류
// 자체를 없앤다(설계 §4.3).
type SettlementFailureHandoff struct {
	Failure  *model.FailedSettlement
	OutboxID uint64
}

// RecordFailuresAndMarkOutboxProcessed는 배치 57014 인계(설계 §4.3)를 위한 원자적
// 메서드다. failed_settlements 배치 upsert(RecordFailure와 같은 conflict 갱신
// 범위)와 그 outbox 행들의 PENDING→PROCESSED 배치 UPDATE를 한 트랜잭션으로 묶는다
// — "기록은 됐는데 마킹만 실패"라는 중간 상태를 이 경로에서는 만들지 않는다.
//
// 입력 계약: 빈 입력·nil failure·OutboxID==0·중복 OutboxID·중복
// trade_idempotency_key는 전부 거부한다(조용한 부분 성공 방지).
// InsertBatchAndMarkCancelCommands(trade_outbox_repository.go)의 중복 제거는
// 복사하지 않는다 — 취소 command는 중복이 정상이지만 여기서 중복은 입력
// 불변식 위반이다.
func (r *FailedSettlementRepository) RecordFailuresAndMarkOutboxProcessed(items []SettlementFailureHandoff) error {
	if r == nil || r.DB == nil {
		return fmt.Errorf("failed settlement repository DB is required")
	}
	if len(items) == 0 {
		return fmt.Errorf("items is required")
	}

	failures := make([]*model.FailedSettlement, 0, len(items))
	outboxIDs := make([]uint64, 0, len(items))
	seenOutboxIDs := make(map[uint64]struct{}, len(items))
	seenKeys := make(map[string]struct{}, len(items))
	for i, item := range items {
		if item.Failure == nil {
			return fmt.Errorf("items[%d].Failure is required", i)
		}
		if item.OutboxID == 0 {
			return fmt.Errorf("items[%d].OutboxID is required", i)
		}
		if _, dup := seenOutboxIDs[item.OutboxID]; dup {
			return fmt.Errorf("duplicate outbox id %d", item.OutboxID)
		}
		seenOutboxIDs[item.OutboxID] = struct{}{}
		if _, dup := seenKeys[item.Failure.TradeIdempotencyKey]; dup {
			return fmt.Errorf("duplicate trade idempotency key %q", item.Failure.TradeIdempotencyKey)
		}
		seenKeys[item.Failure.TradeIdempotencyKey] = struct{}{}
		failures = append(failures, item.Failure)
		outboxIDs = append(outboxIDs, item.OutboxID)
	}

	now := time.Now().UTC()
	return r.DB.Transaction(func(tx *gorm.DB) error {
		// 다중 행 INSERT ... ON CONFLICT DO UPDATE라 EXCLUDED로 "이번에 들어온
		// 그 행"의 값을 가리킨다 — RecordFailure의 단건 clause.Assignments(리터럴
		// 값 하나)와 달리 항목마다 다른 값을 반영해야 하므로 clause.Set을 직접 쓴다.
		insertResult := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "trade_idempotency_key"}},
			DoUpdates: clause.Set{
				{Column: clause.Column{Name: "error_message"}, Value: gorm.Expr("EXCLUDED.error_message")},
				{Column: clause.Column{Name: "status"}, Value: model.FailedSettlementStatusOpen},
				{Column: clause.Column{Name: "retry_count"}, Value: gorm.Expr("failed_settlements.retry_count + 1")},
				{Column: clause.Column{Name: "resolution"}, Value: ""},
				{Column: clause.Column{Name: "resolved_by"}, Value: ""},
				{Column: clause.Column{Name: "notes"}, Value: ""},
				{Column: clause.Column{Name: "resolved_at"}, Value: nil},
				{Column: clause.Column{Name: "updated_at"}, Value: now},
			},
		}).Create(&failures)
		if insertResult.Error != nil {
			return insertResult.Error
		}
		if int(insertResult.RowsAffected) != len(failures) {
			return fmt.Errorf("upsert failed settlements affected %d rows, expected %d", insertResult.RowsAffected, len(failures))
		}

		outboxResult := tx.Model(&model.TradeOutboxEvent{}).
			Where("id IN ? AND status = ?", outboxIDs, model.TradeOutboxStatusPending).
			Updates(map[string]any{
				"status":       model.TradeOutboxStatusProcessed,
				"processed_at": now,
			})
		if outboxResult.Error != nil {
			return outboxResult.Error
		}
		if int(outboxResult.RowsAffected) != len(outboxIDs) {
			return fmt.Errorf("mark outbox processed affected %d rows, expected %d", outboxResult.RowsAffected, len(outboxIDs))
		}
		return nil
	})
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

// HasOpenFailureForOrder는 해당 주문을 maker 또는 taker로 참조하는 OPEN 실패가 있는지
// DB에서 EXISTS로 판정한다. ListOpenFailures(limit) 결과를 메모리에서 검색하면 batch
// limit 밖의 dependency를 놓쳐 fail-open이 되므로 반드시 이 경로를 쓴다.
func (r *FailedSettlementRepository) HasOpenFailureForOrder(orderID uint) (bool, error) {
	if r == nil || r.DB == nil {
		return false, fmt.Errorf("failed settlement repository DB is required")
	}
	if orderID == 0 {
		return false, fmt.Errorf("order id is required")
	}

	var exists bool
	err := r.DB.Raw(
		`SELECT EXISTS (
			SELECT 1 FROM failed_settlements
			WHERE status = ? AND (buy_order_id = ? OR sell_order_id = ?)
		)`,
		model.FailedSettlementStatusOpen, orderID, orderID,
	).Scan(&exists).Error
	return exists, err
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
