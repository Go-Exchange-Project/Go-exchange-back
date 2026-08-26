package repository

import (
	"strings"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"gorm.io/gorm"
)

type OrderIdempotencyRepository struct {
	DB *gorm.DB
}

type UserKeyPair struct {
	UserID uint
	Key    string
}

func NewOrderIdempotencyRepository(db *gorm.DB) *OrderIdempotencyRepository {
	return &OrderIdempotencyRepository{DB: db}
}

func (r *OrderIdempotencyRepository) WithTx(tx *gorm.DB) *OrderIdempotencyRepository {
	return &OrderIdempotencyRepository{DB: tx}
}

// InsertNew는 배치를 한 문장으로 넣고 실제로 삽입된 레코드의 ID만 돌려줍니다.
//
// 반환되지 않은 요청은 기존 키(follower)입니다. 요청마다 INSERT하면 배치의 존재
// 이유(왕복 절감)가 사라지므로 ON CONFLICT DO NOTHING + RETURNING을 씁니다.
//
// GORM의 Create(&records)를 쓰지 않는 이유: DO NOTHING으로 일부 행이 빠지면 반환 행 수가
// 구조체 수보다 적은데, GORM은 반환 행을 슬라이스 **순서대로** 채운다. 그러면 충돌해서
// 삽입되지 않은 앞쪽 구조체가 뒤쪽 행의 ID를 갖게 되고, follower가 owner의 ID를 들고
// 다니게 된다(통합 테스트로 실제 확인). 그래서 (user_id, idempotency_key)로 명시적으로
// 되짚어 채운다.
func (r *OrderIdempotencyRepository) InsertNew(records []*model.OrderIdempotencyKey) ([]uint64, error) {
	if len(records) == 0 {
		return nil, nil
	}

	placeholders := make([]string, 0, len(records))
	args := make([]any, 0, len(records)*5)
	for _, record := range records {
		placeholders = append(placeholders, "(?, ?, ?, ?, ?)")
		args = append(args, record.UserID, record.IdempotencyKey,
			record.Fingerprint, record.FingerprintVersion, record.Outcome)
	}

	// created_at·updated_at은 DB 기본값(now())에 맡긴다.
	query := `
INSERT INTO order_idempotency_keys (user_id, idempotency_key, fingerprint, fingerprint_version, outcome)
VALUES ` + strings.Join(placeholders, ", ") + `
ON CONFLICT (user_id, idempotency_key) DO NOTHING
RETURNING id, user_id, idempotency_key`

	var rows []struct {
		ID             uint64
		UserID         uint
		IdempotencyKey string
	}
	if err := r.DB.Raw(query, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}

	byKey := make(map[UserKeyPair]uint64, len(rows))
	for _, row := range rows {
		byKey[UserKeyPair{UserID: row.UserID, Key: row.IdempotencyKey}] = row.ID
	}

	inserted := make([]uint64, 0, len(rows))
	for _, record := range records {
		id, ok := byKey[UserKeyPair{UserID: record.UserID, Key: record.IdempotencyKey}]
		if !ok {
			continue
		}
		// 같은 배치에 같은 키가 두 번 있으면 한 행만 삽입된다. 그 ID는 앞선 구조체가
		// 이미 가져갔으므로 뒤쪽 구조체는 follower로 남겨 둔다.
		delete(byKey, UserKeyPair{UserID: record.UserID, Key: record.IdempotencyKey})
		record.ID = id
		inserted = append(inserted, id)
	}
	return inserted, nil
}

func (r *OrderIdempotencyRepository) FindByUserKeys(pairs []UserKeyPair) ([]model.OrderIdempotencyKey, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	tuples := make([][]any, 0, len(pairs))
	for _, pair := range pairs {
		tuples = append(tuples, []any{pair.UserID, pair.Key})
	}

	var records []model.OrderIdempotencyKey
	err := r.DB.Where("(user_id, idempotency_key) IN ?", tuples).Find(&records).Error
	return records, err
}

// SetOrderAndOutcome은 order_id·outcome·updated_at을 한 UPDATE 문에서 갱신합니다.
func (r *OrderIdempotencyRepository) SetOrderAndOutcome(id uint64, orderID uint, outcome model.OrderIdempotencyOutcome) error {
	return r.DB.Model(&model.OrderIdempotencyKey{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"order_id":   orderID,
			"outcome":    outcome,
			"updated_at": time.Now().UTC(),
		}).Error
}

// UpdateOutcome은 outcome과 updated_at을 한 UPDATE 문에서 갱신합니다.
// outcome이 바뀌면 부분 인덱스에서 빠지고, updated_at은 전이 시각을 보존합니다.
func (r *OrderIdempotencyRepository) UpdateOutcome(id uint64, outcome model.OrderIdempotencyOutcome) error {
	return r.DB.Model(&model.OrderIdempotencyKey{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"outcome":    outcome,
			"updated_at": time.Now().UTC(),
		}).Error
}

// DeleteByIDs는 이번 트랜잭션에서 삽입했지만 hold 검증에 실패한 키를 지웁니다.
// 커밋된 주문을 가리키는 키에는 절대 쓰지 않습니다.
func (r *OrderIdempotencyRepository) DeleteByIDs(ids []uint64) error {
	if len(ids) == 0 {
		return nil
	}
	return r.DB.Where("id IN ?", ids).Delete(&model.OrderIdempotencyKey{}).Error
}

// CountStalePending은 stale PENDING gauge의 원천입니다.
// order_idempotency_pending_updated_at 부분 인덱스가 이 조회를 받칩니다.
func (r *OrderIdempotencyRepository) CountStalePending(olderThan time.Time) (int64, error) {
	var count int64
	err := r.DB.Model(&model.OrderIdempotencyKey{}).
		Where("outcome = ? AND updated_at < ?", model.OrderIdempotencyOutcomePending, olderThan).
		Count(&count).Error
	return count, err
}
