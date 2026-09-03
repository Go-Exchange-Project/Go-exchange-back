package repository

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type JournalRepository struct {
	DB *gorm.DB
}

// BalanceDelta는 잔액 캐시 한 행에 더할 변화량이다.
type BalanceDelta struct {
	AccountID     uint
	Delta         decimal.Decimal
	LastPostingID uint64
}

func NewJournalRepository(db *gorm.DB) *JournalRepository {
	return &JournalRepository{DB: db}
}

func (r *JournalRepository) WithTx(tx *gorm.DB) *JournalRepository {
	return &JournalRepository{DB: tx}
}

// InsertOrGet은 분개를 넣거나, 같은 idempotency_key가 이미 있으면 기존 것을 읽어
// entry에 채운다. created가 false면 "이미 기록된 사건"이라는 뜻이고, 그것은 오류가
// 아니라 우리가 원하는 상태다.
//
// ON CONFLICT DO NOTHING RETURNING을 쓰는 이유: Postgres에서 유니크 위반은
// 트랜잭션 전체를 abort 상태로 만들어 그 뒤 아무 문장도 실행할 수 없게 한다.
// 위반을 일으킨 뒤 잡는 방식으로는 같은 트랜잭션에서 복구할 수 없다.
func (r *JournalRepository) InsertOrGet(entry *model.JournalEntry) (bool, error) {
	if entry == nil {
		return false, fmt.Errorf("journal entry is required")
	}

	result := r.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "idempotency_key"}},
		DoNothing: true,
	}).Create(entry)
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 1 {
		return true, nil
	}

	// 반환 행이 없다 = 같은 키가 이미 있다. 기존 분개를 읽어 호출자가 그것으로
	// 진행하게 한다.
	var existing model.JournalEntry
	if err := r.DB.Where("idempotency_key = ?", entry.IdempotencyKey).First(&existing).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, fmt.Errorf("journal %q conflicted but was not found", entry.IdempotencyKey)
		}
		return false, err
	}
	*entry = existing
	return false, nil
}

func (r *JournalRepository) CreatePostings(postings []model.Posting) error {
	if len(postings) == 0 {
		return fmt.Errorf("at least one posting is required")
	}
	return r.DB.Create(&postings).Error
}

// ApplyBalanceDeltas는 잔액 캐시를 1왕복으로 더한다. 행은 EnsureAccounts가 이미
// 만들어 두므로 여기서는 UPDATE만 한다 — 갱신된 행 수가 다르면 오류다.
func (r *JournalRepository) ApplyBalanceDeltas(deltas []BalanceDelta) error {
	if len(deltas) == 0 {
		return nil
	}

	rows := make([]string, 0, len(deltas))
	args := make([]interface{}, 0, len(deltas)*3+1)
	args = append(args, time.Now().UTC())
	for i, d := range deltas {
		base := i*3 + 2
		rows = append(rows, fmt.Sprintf("($%d::bigint, $%d::numeric, $%d::bigint)", base, base+1, base+2))
		args = append(args, d.AccountID, d.Delta, d.LastPostingID)
	}

	sql := fmt.Sprintf(`
		UPDATE account_balances AS b
		SET balance = b.balance + v.delta,
		    last_posting_id = GREATEST(b.last_posting_id, v.last_posting_id),
		    updated_at = $1
		FROM (VALUES %s) AS v(account_id, delta, last_posting_id)
		WHERE b.account_id = v.account_id`,
		strings.Join(rows, ", "),
	)

	result := r.DB.Exec(sql, args...)
	if result.Error != nil {
		return result.Error
	}
	if int(result.RowsAffected) != len(deltas) {
		return fmt.Errorf("balance delta update affected %d rows, expected %d", result.RowsAffected, len(deltas))
	}
	return nil
}

// FindPostingsByJournalID는 역분개가 원본 전기를 읽을 때 쓴다.
func (r *JournalRepository) FindPostingsByJournalID(journalID uint) ([]model.Posting, error) {
	var postings []model.Posting
	err := r.DB.Where("journal_id = ?", journalID).Order("id ASC").Find(&postings).Error
	return postings, err
}
