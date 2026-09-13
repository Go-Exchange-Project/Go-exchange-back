package repository

import (
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"gorm.io/gorm"
)

// ReconciliationRepository는 검산 워커가 쓰는 질의를 모은다. 원장 검산 4종은
// LedgerReconciliationRepository가 가지고 있으므로 그대로 묻어 쓴다 — 워커가
// 의존성 두 개를 따로 들고 다닐 이유가 없다.
type ReconciliationRepository struct {
	DB *gorm.DB
	*LedgerReconciliationRepository
}

func NewReconciliationRepository(db *gorm.DB) *ReconciliationRepository {
	return &ReconciliationRepository{
		DB:                             db,
		LedgerReconciliationRepository: NewLedgerReconciliationRepository(db),
	}
}

// StaleMarketOrderRow는 stale_market_order 검사에서 발견된, 완료되지 않고 오래 남은
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
