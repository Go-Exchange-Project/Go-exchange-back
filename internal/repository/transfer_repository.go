package repository

import (
	"errors"
	"fmt"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type TransferRepository struct {
	DB *gorm.DB
}

func NewTransferRepository(db *gorm.DB) *TransferRepository {
	return &TransferRepository{DB: db}
}

func (r *TransferRepository) WithTx(tx *gorm.DB) *TransferRepository {
	return &TransferRepository{DB: tx}
}

// LockByID는 요청 행을 SELECT ... FOR UPDATE 한다. 확정 경로와 미확정 경로가
// 모두 이것으로 시작해 서로를 직렬화한다 — 잠그지 않으면 상태를 읽은 뒤
// UPDATE하기까지 사이에 다른 트랜잭션이 끼어든다.
func (r *TransferRepository) LockByID(id uint) (*model.TransferRequest, error) {
	var request model.TransferRequest
	err := r.DB.
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", id).
		First(&request).Error
	if err != nil {
		return nil, err
	}
	return &request, nil
}

func (r *TransferRepository) FindByExternalRef(ref string) (*model.TransferRequest, error) {
	var request model.TransferRequest
	err := r.DB.Where("external_ref = ?", ref).First(&request).Error
	if err != nil {
		return nil, err
	}
	return &request, nil
}

// InsertOrGetByUserRequestKey는 접수 멱등의 근거다. 같은 (user_id,
// client_request_key)가 이미 있으면 만들지 않고 기존 요청을 돌려준다.
//
// 접수는 항상 이것으로 시작한다. 키 선점보다 잠금이 먼저면 같은 요청이 두 번 잠근다.
// 본문이 다른지 판단해 409로 바꾸는 것은 호출자 몫이다 — 리포지토리는 무엇이
// "같은 요청"인지 정의하지 않는다.
func (r *TransferRepository) InsertOrGetByUserRequestKey(request *model.TransferRequest) (bool, *model.TransferRequest, error) {
	if request == nil {
		return false, nil, fmt.Errorf("transfer request is required")
	}

	result := r.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}, {Name: "client_request_key"}},
		DoNothing: true,
	}).Create(request)
	if result.Error != nil {
		return false, nil, result.Error
	}
	if result.RowsAffected == 1 {
		return true, request, nil
	}

	var existing model.TransferRequest
	err := r.DB.
		Where("user_id = ? AND client_request_key = ?", request.UserID, request.ClientRequestKey).
		First(&existing).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil, fmt.Errorf("transfer request key %q conflicted but was not found", request.ClientRequestKey)
		}
		return false, nil, err
	}
	return false, &existing, nil
}

// SetHoldJournal은 출금 접수 트랜잭션에서 잠금 분개를 만든 직후 불린다.
// INSERT 시점에는 분개가 없어 NULL이었고, 커밋 시점에 이 값이 있는지는
// migration 009의 deferrable constraint trigger가 검사한다.
func (r *TransferRepository) SetHoldJournal(id uint, journalID uint) error {
	return requireRowsAffected(r.DB.Model(&model.TransferRequest{}).
		Where("id = ?", id).
		Updates(map[string]interface{}{
			"hold_journal_id": journalID,
			"updated_at":      time.Now().UTC(),
		}), "transfer hold journal update")
}

// SetDispatched는 외부 제출이 끝난 요청을 RECEIVED에서 PROCESSING으로 옮긴다.
// WHERE에 status를 넣어 이미 확정된 요청을 되돌리지 못하게 한다.
func (r *TransferRepository) SetDispatched(id uint, externalRef string) error {
	return requireRowsAffected(r.DB.Model(&model.TransferRequest{}).
		Where("id = ? AND status = ?", id, model.TransferStatusReceived).
		Updates(map[string]interface{}{
			"status":       model.TransferStatusProcessing,
			"external_ref": externalRef,
			"updated_at":   time.Now().UTC(),
		}), "transfer dispatch update")
}

// InsertEventIfAbsent는 외부에서 알게 된 사실을 기록한다. created가 false면
// 같은 event_key를 이미 봤다는 뜻이다.
//
// 알림과 조회는 event_key가 다르므로 둘 다 통과한다. 그것이 옳다 — 실제로 두 번
// 알게 된 것이니 기록도 두 줄이어야 한다. 돈이 두 번 움직이는 것은 분개
// 멱등성 키와 요청 행 잠금이 막는다.
func (r *TransferRepository) InsertEventIfAbsent(event *model.TransferStatusEvent) (bool, error) {
	if event == nil {
		return false, fmt.Errorf("transfer status event is required")
	}

	result := r.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "event_key"}},
		DoNothing: true,
	}).Create(event)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// DueForCheck는 상태 조회 대상을 고른다. RECEIVED(제출 재시도 대상)와
// PROCESSING(상태 조회 대상)을 함께 돌려준다 — worker가 둘을 모두 처리한다.
func (r *TransferRepository) DueForCheck(now time.Time, limit int) ([]model.TransferRequest, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("limit must be greater than zero")
	}

	var requests []model.TransferRequest
	err := r.DB.
		Where("status = ? OR (status = ? AND (next_check_at IS NULL OR next_check_at <= ?))",
			model.TransferStatusReceived, model.TransferStatusProcessing, now).
		Order("id ASC").
		Limit(limit).
		Find(&requests).Error
	return requests, err
}
