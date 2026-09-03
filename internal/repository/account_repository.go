package repository

import (
	"fmt"
	"strings"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type AccountRepository struct {
	DB *gorm.DB
}

// AccountSpec은 계정 하나를 지목한다. 시스템 계정은 OwnerUserID가 nil이다.
type AccountSpec struct {
	AccountType model.AccountType
	OwnerUserID *uint
	Asset       string
}

// UserAssetBalance는 잔액 조회 API가 쓰는 자산 한 줄이다.
//
// AvailableAccountID는 WalletResponse.id로 그대로 나간다. 자산마다 계정이
// 둘이므로 어느 쪽을 쓸지 정해야 하고, 정하지 않으면 응답의 id가 실행마다 달라진다.
type UserAssetBalance struct {
	AvailableAccountID uint
	Asset              string
	Available          decimal.Decimal
	Locked             decimal.Decimal
	AvgBuyPrice        decimal.Decimal
}

func NewAccountRepository(db *gorm.DB) *AccountRepository {
	return &AccountRepository{DB: db}
}

func (r *AccountRepository) WithTx(tx *gorm.DB) *AccountRepository {
	return &AccountRepository{DB: tx}
}

// EnsureAccounts는 지목된 계정들을 확보한다. 없으면 만들고, 잔액 캐시 행도 함께
// 0으로 만든다. 캐시 행을 여기서 만들어 두어야 LockBalances가 항상 잠글 것을 찾는다.
//
// 반환 순서는 입력 순서와 무관하다 — 호출자는 (종류, 소유자, 자산)으로 찾는다.
func (r *AccountRepository) EnsureAccounts(specs []AccountSpec) ([]model.Account, error) {
	if len(specs) == 0 {
		return nil, nil
	}

	// 중복 제거는 값으로 한다. AccountSpec을 그대로 map 키로 쓰면 OwnerUserID가
	// 포인터라 값이 같아도 주소가 다르면 다른 키가 되고, 같은 계정이 여러 번
	// 남아 아래 개수 검사가 어긋난다.
	type dedupeKey struct {
		accountType model.AccountType
		ownerUserID uint // 시스템 계정은 0
		asset       string
	}
	unique := make([]AccountSpec, 0, len(specs))
	seen := make(map[dedupeKey]bool, len(specs))
	for _, spec := range specs {
		key := dedupeKey{accountType: spec.AccountType, asset: spec.Asset}
		if spec.OwnerUserID != nil {
			key.ownerUserID = *spec.OwnerUserID
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		unique = append(unique, spec)
	}

	rows := make([]model.Account, 0, len(unique))
	for _, spec := range unique {
		rows = append(rows, model.Account{
			AccountType:    spec.AccountType,
			OwnerUserID:    spec.OwnerUserID,
			Asset:          spec.Asset,
			AllowsNegative: model.AllowsNegativeBalance(spec.AccountType),
		})
	}

	// 표현식 유니크 인덱스(accounts_type_owner_asset_unique)를 추론시킨다.
	// 유니크 위반을 일으켜 잡지 않는다 — Postgres에서 그 위반은 트랜잭션 전체를
	// abort시켜 이후 문장을 전부 막는다.
	if err := r.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&rows).Error; err != nil {
		return nil, err
	}

	accounts, err := r.findBySpecs(unique)
	if err != nil {
		return nil, err
	}
	if len(accounts) != len(unique) {
		return nil, fmt.Errorf("ensure accounts expected %d rows, found %d", len(unique), len(accounts))
	}

	ids := make([]uint, 0, len(accounts))
	for i := range accounts {
		ids = append(ids, accounts[i].ID)
	}
	if err := r.ensureBalanceRows(ids); err != nil {
		return nil, err
	}
	return accounts, nil
}

// LockBalances는 잔액 캐시 행을 항상 account_id 오름차순으로 SELECT ... FOR UPDATE
// 한다. 모든 트랜잭션이 같은 순서로 잠그므로 계정 간 AB-BA 데드락이 성립하지 않는다.
func (r *AccountRepository) LockBalances(accountIDs []uint) ([]model.AccountBalance, error) {
	if len(accountIDs) == 0 {
		return nil, nil
	}

	var balances []model.AccountBalance
	err := r.DB.
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("account_id IN ?", accountIDs).
		Order("account_id ASC").
		Find(&balances).Error
	if err != nil {
		return nil, err
	}
	if len(balances) != len(accountIDs) {
		return nil, fmt.Errorf("balance lock expected %d rows, locked %d", len(accountIDs), len(balances))
	}
	return balances, nil
}

// ListUserBalances는 자산별로 USER_AVAILABLE·USER_LOCKED를 합쳐 한 줄로 만들고
// user_asset_stats를 LEFT JOIN 한다. 통계 행이 없으면 평단가는 0이다 —
// 매수한 적 없는 자산이 정상적으로 그렇다.
func (r *AccountRepository) ListUserBalances(userID uint) ([]UserAssetBalance, error) {
	var rows []UserAssetBalance
	err := r.DB.Raw(`
		SELECT
			COALESCE(MAX(a.id) FILTER (WHERE a.account_type = 'USER_AVAILABLE'), 0) AS available_account_id,
			a.asset                                                            AS asset,
			COALESCE(SUM(b.balance) FILTER (WHERE a.account_type = 'USER_AVAILABLE'), 0) AS available,
			COALESCE(SUM(b.balance) FILTER (WHERE a.account_type = 'USER_LOCKED'), 0)    AS locked,
			COALESCE(MAX(s.avg_buy_price), 0)                                  AS avg_buy_price
		FROM accounts a
		LEFT JOIN account_balances b ON b.account_id = a.id
		LEFT JOIN user_asset_stats s ON s.user_id = a.owner_user_id AND s.asset = a.asset
		WHERE a.owner_user_id = ?
		  AND a.account_type IN ('USER_AVAILABLE','USER_LOCKED')
		GROUP BY a.asset
		ORDER BY a.asset ASC`, userID).Scan(&rows).Error
	return rows, err
}

func (r *AccountRepository) findBySpecs(specs []AccountSpec) ([]model.Account, error) {
	conditions := make([]string, 0, len(specs))
	args := make([]interface{}, 0, len(specs)*3)
	for _, spec := range specs {
		conditions = append(conditions, "(account_type = ? AND COALESCE(owner_user_id, 0) = ? AND asset = ?)")
		owner := uint(0)
		if spec.OwnerUserID != nil {
			owner = *spec.OwnerUserID
		}
		args = append(args, string(spec.AccountType), owner, spec.Asset)
	}

	var accounts []model.Account
	err := r.DB.Where(strings.Join(conditions, " OR "), args...).Find(&accounts).Error
	return accounts, err
}

// ensureBalanceRows는 계정마다 잔액 캐시 행이 0으로 존재하게 만든다.
func (r *AccountRepository) ensureBalanceRows(accountIDs []uint) error {
	rows := make([]model.AccountBalance, 0, len(accountIDs))
	for _, id := range accountIDs {
		rows = append(rows, model.AccountBalance{
			AccountID:     id,
			Balance:       decimal.Zero,
			LastPostingID: 0,
		})
	}
	return r.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "account_id"}},
		DoNothing: true,
	}).Create(&rows).Error
}
