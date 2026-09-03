package repository

import (
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// LedgerReconciliationRepository는 원장만으로 자산을 검산한다. 지갑 표를 보지
// 않으므로 §1.6의 "사라진 수수료를 따로 더해서 맞추는" 보정항이 필요 없다.
type LedgerReconciliationRepository struct {
	DB *gorm.DB
}

func NewLedgerReconciliationRepository(db *gorm.DB) *LedgerReconciliationRepository {
	return &LedgerReconciliationRepository{DB: db}
}

// UnbalancedJournalRow는 자산별 합이 0이 아닌 분개다.
type UnbalancedJournalRow struct {
	JournalID uint
	Asset     string
	Sum       decimal.Decimal
}

// BalanceDriftRow는 잔액 캐시와 전기 합이 어긋난 계정이다.
type BalanceDriftRow struct {
	AccountID uint
	Cached    decimal.Decimal
	Computed  decimal.Decimal
}

// AssetTotalRow는 자산 전체 합이 0이 아닌 자산이다.
type AssetTotalRow struct {
	Asset string
	Sum   decimal.Decimal
}

// NegativeAccountRow는 음수가 되면 안 되는데 음수인 계정이다.
type NegativeAccountRow struct {
	AccountID   uint
	AccountType string
	OwnerUserID *uint
	Asset       string
	Balance     decimal.Decimal
}

// CheckUnbalancedJournals는 검사 1이다. 한 줄이라도 나오면 심각한 오류다 —
// 돈이 생겼거나 사라졌다는 뜻이다.
func (r *LedgerReconciliationRepository) CheckUnbalancedJournals(afterJournalID uint, limit int) ([]UnbalancedJournalRow, error) {
	var rows []UnbalancedJournalRow
	err := r.DB.Raw(`
		SELECT journal_id, asset, SUM(amount) AS sum
		FROM postings
		WHERE journal_id > ?
		GROUP BY journal_id, asset
		HAVING SUM(amount) <> 0
		ORDER BY journal_id
		LIMIT ?`, afterJournalID, limit).Scan(&rows).Error
	return rows, err
}

// CheckBalanceCacheDrift는 검사 2다. 이것이 "원장만 다시 합산해 잔액을 검산"이다.
//
// 캐시가 진실이 아니라 전기가 진실이므로, 어긋나면 캐시를 다시 계산해 덮어쓴다.
func (r *LedgerReconciliationRepository) CheckBalanceCacheDrift(afterAccountID uint, limit int) ([]BalanceDriftRow, error) {
	var rows []BalanceDriftRow
	err := r.DB.Raw(`
		SELECT a.id AS account_id,
		       COALESCE(b.balance, 0) AS cached,
		       COALESCE(p.computed, 0) AS computed
		FROM accounts a
		LEFT JOIN account_balances b ON b.account_id = a.id
		LEFT JOIN LATERAL (
			SELECT SUM(amount) AS computed FROM postings WHERE account_id = a.id
		) p ON true
		WHERE a.id > ?
		  AND COALESCE(b.balance, 0) IS DISTINCT FROM COALESCE(p.computed, 0)
		ORDER BY a.id
		LIMIT ?`, afterAccountID, limit).Scan(&rows).Error
	return rows, err
}

// CheckAssetTotals는 검사 3이다. 외부·MINT 계정이 반대편을 받아 주므로 자산별
// 전체 합은 항상 정확히 0이어야 한다.
//
// 페이지네이션하지 않는다 — 자산 종류만큼만 나오고, 한 줄이라도 나오면 즉시
// 조사해야 하는 상황이다.
func (r *LedgerReconciliationRepository) CheckAssetTotals() ([]AssetTotalRow, error) {
	var rows []AssetTotalRow
	err := r.DB.Raw(`
		SELECT asset, SUM(amount) AS sum
		FROM postings
		GROUP BY asset
		HAVING SUM(amount) <> 0
		ORDER BY asset`).Scan(&rows).Error
	return rows, err
}

// CheckNegativeAccounts는 검사 4다. allows_negative가 false인 계정이 음수면
// 없는 돈을 쓴 것이다.
func (r *LedgerReconciliationRepository) CheckNegativeAccounts(afterAccountID uint, limit int) ([]NegativeAccountRow, error) {
	var rows []NegativeAccountRow
	err := r.DB.Raw(`
		SELECT a.id AS account_id, a.account_type, a.owner_user_id, a.asset, b.balance
		FROM accounts a
		JOIN account_balances b ON b.account_id = a.id
		WHERE a.id > ?
		  AND a.allows_negative = false
		  AND b.balance < 0
		ORDER BY a.id
		LIMIT ?`, afterAccountID, limit).Scan(&rows).Error
	return rows, err
}
