package repository

import (
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

type ReconciliationRepository struct {
	DB *gorm.DB
}

func NewReconciliationRepository(db *gorm.DB) *ReconciliationRepository {
	return &ReconciliationRepository{DB: db}
}

// LedgerWalletRow는 검사 1(ledger_wallet)의 한 지갑에 대한 원장-지갑 비교 결과입니다.
type LedgerWalletRow struct {
	WalletID                uint
	UserID                  uint
	CoinSymbol              string
	AvailableBalance        decimal.Decimal
	LockedBalance           decimal.Decimal
	LedgerAvailableSum      decimal.Decimal
	LedgerLockedSum         decimal.Decimal
	ImpliedInitialAvailable decimal.NullDecimal
}

// CheckLedgerWalletPage는 지갑 ID 기준 keyset 페이지네이션으로 지갑별 원장 델타 합계와
// 레거시 초기 잔액 후보(가장 이른 원장 항목의 available_balance_after - available_delta)를
// 단일 SQL(하나의 스냅샷)로 조회합니다. 지갑 읽기와 원장 집계를 별도 쿼리로 하면 그 사이에
// 정산이 끼어들어 가짜 위반이 뜰 수 있습니다(tolerance 0 설계라 위양성이 그대로 알람이 됨).
func (r *ReconciliationRepository) CheckLedgerWalletPage(afterWalletID uint, limit int) ([]LedgerWalletRow, error) {
	var rows []LedgerWalletRow
	err := r.DB.Raw(`
		SELECT
			w.id AS wallet_id, w.user_id, w.coin_symbol,
			w.available_balance, w.locked_balance,
			COALESCE(agg.available_delta_sum, 0) AS ledger_available_sum,
			COALESCE(agg.locked_delta_sum, 0) AS ledger_locked_sum,
			first_entry.implied_initial_available
		FROM wallets w
		LEFT JOIN LATERAL (
			SELECT SUM(available_delta) AS available_delta_sum,
			       SUM(locked_delta)    AS locked_delta_sum
			FROM ledger_entries l
			WHERE l.user_id = w.user_id AND l.coin_symbol = w.coin_symbol
		) agg ON true
		LEFT JOIN LATERAL (
			SELECT available_balance_after - available_delta AS implied_initial_available
			FROM ledger_entries l
			WHERE l.user_id = w.user_id AND l.coin_symbol = w.coin_symbol
			ORDER BY l.id ASC
			LIMIT 1
		) first_entry ON true
		WHERE w.id > ?
		ORDER BY w.id
		LIMIT ?
	`, afterWalletID, limit).Scan(&rows).Error
	return rows, err
}
