package repository

import (
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
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

// AssetConservationRow는 검사 2(asset_conservation)의 자산 하나에 대한 총량 보존 결과입니다.
type AssetConservationRow struct {
	CoinSymbol  string
	WalletTotal decimal.Decimal
	FeeTotal    decimal.Decimal
	FundedTotal decimal.Decimal
}

// CheckAssetConservation은 자산(coin_symbol)별로 Σ(available+locked) + (KRW일 때만 누적 수수료)
// == Σ(DEV_FUND delta)인지 확인합니다. 수수료는 internal/service/fee.go에서 항상 KRW로만
// 부과되므로 코인 자산은 fee_total이 0이 되어 동일한 쿼리 형태로 일반화됩니다. 지갑 배치
// 순회와 무관하게 매 실행 1회만 수행합니다.
func (r *ReconciliationRepository) CheckAssetConservation() ([]AssetConservationRow, error) {
	var rows []AssetConservationRow
	err := r.DB.Raw(`
		WITH wallet_totals AS (
			SELECT coin_symbol, SUM(available_balance + locked_balance) AS total
			FROM wallets
			GROUP BY coin_symbol
		),
		fee_totals AS (
			SELECT 'KRW' AS coin_symbol,
			       COALESCE(SUM(buyer_fee), 0) + COALESCE(SUM(seller_fee), 0) AS total
			FROM trades
		),
		funded_totals AS (
			SELECT coin_symbol, SUM(available_delta) AS total
			FROM ledger_entries
			WHERE entry_type = 'DEV_FUND'
			GROUP BY coin_symbol
		)
		SELECT
			COALESCE(w.coin_symbol, f.coin_symbol) AS coin_symbol,
			COALESCE(w.total, 0) AS wallet_total,
			COALESCE(fee.total, 0) AS fee_total,
			COALESCE(f.total, 0) AS funded_total
		FROM wallet_totals w
		FULL OUTER JOIN funded_totals f ON f.coin_symbol = w.coin_symbol
		LEFT JOIN fee_totals fee ON fee.coin_symbol = COALESCE(w.coin_symbol, f.coin_symbol)
	`).Scan(&rows).Error
	return rows, err
}

// StaleMarketOrderRow는 검사 3(stale_market_order)에서 발견된, 완료되지 않고 오래 남은
// 시장가 주문입니다.
type StaleMarketOrderRow struct {
	OrderID    uint
	UserID     uint
	CoinSymbol string
	Status     string
	CreatedAt  time.Time
}

// CheckStaleMarketOrders는 5분 넘게 PENDING/PARTIAL 상태로 남은 시장가 주문을 찾습니다.
// 시장가는 오더북에 rest하지 않으므로 이 상태가 지속되면 완료(MarketOrderDone 처리 또는
// 그 재시도)가 어딘가에서 유실됐다는 뜻입니다. SettlementRetryWorker의 RetryCount 소진 등으로
// 놓친 케이스의 최종 안전망입니다.
func (r *ReconciliationRepository) CheckStaleMarketOrders(staleAfter time.Duration) ([]StaleMarketOrderRow, error) {
	var rows []StaleMarketOrderRow
	err := r.DB.Raw(`
		SELECT id AS order_id, user_id, coin_symbol, status, created_at
		FROM orders
		WHERE order_type = 'MARKET'
		  AND status IN ('PENDING', 'PARTIAL')
		  AND created_at < ?
	`, time.Now().UTC().Add(-staleAfter)).Scan(&rows).Error
	return rows, err
}

// CreateViolations는 발견된 위반만 일괄 insert합니다. 위반이 없으면 아무것도 쓰지 않습니다.
func (r *ReconciliationRepository) CreateViolations(violations []model.ReconciliationViolation) error {
	if len(violations) == 0 {
		return nil
	}
	return r.DB.Create(&violations).Error
}
