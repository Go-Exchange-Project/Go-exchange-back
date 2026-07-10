package service

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
)

const (
	defaultReconciliationInterval = time.Hour
	reconciliationPageSize        = 500
	staleMarketOrderThreshold     = 5 * time.Minute
	maxReconciliationDetailLength = 2048
)

type reconciliationRepository interface {
	CheckLedgerWalletPage(afterWalletID uint, limit int) ([]repository.LedgerWalletRow, error)
	CheckAssetConservation() ([]repository.AssetConservationRow, error)
	CheckStaleMarketOrders(staleAfter time.Duration) ([]repository.StaleMarketOrderRow, error)
	CreateViolations(violations []model.ReconciliationViolation) error
}

// ReconciliationWorker는 원장-지갑 정합성, 자산별 총량 보존, 오래된 시장가 주문 잔존을
// 주기적으로 검사하고 위반을 내구 기록 + 메트릭으로 보고합니다. 자동 교정은 하지 않습니다 —
// 탐지/보고만 합니다.
type ReconciliationWorker struct {
	Repository reconciliationRepository
	Interval   time.Duration
	Logger     *log.Logger
}

func (w *ReconciliationWorker) Run(ctx context.Context) {
	w.RunOnce() // 기동 직후 1회 — 배포/재시작 직후가 정합성이 가장 의심스러운 시점
	ticker := time.NewTicker(w.interval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.RunOnce()
		}
	}
}

func (w *ReconciliationWorker) RunOnce() {
	w.runLedgerWalletCheck()
	w.runAssetConservationCheck()
	w.runStaleMarketOrderCheck()
	metrics.ReconciliationLastRunTimestamp.Set(float64(time.Now().UTC().Unix()))
}

func (w *ReconciliationWorker) runLedgerWalletCheck() {
	var violations []model.ReconciliationViolation
	var lastWalletID uint

	for {
		rows, err := w.Repository.CheckLedgerWalletPage(lastWalletID, reconciliationPageSize)
		if err != nil {
			w.logf("reconciliation: ledger_wallet check failed: %v", err)
			metrics.ReconciliationCheckErrorsTotal.WithLabelValues("ledger_wallet").Inc()
			return
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			if checkName, violated := classifyLedgerWalletRow(row); violated {
				violations = append(violations, model.ReconciliationViolation{
					CheckName:  checkName,
					SubjectKey: fmt.Sprintf("wallet:%d", row.WalletID),
					Detail:     ledgerWalletViolationDetail(row),
					DetectedAt: time.Now().UTC(),
				})
			}
			lastWalletID = row.WalletID
		}
		if len(rows) < reconciliationPageSize {
			break
		}
	}

	w.persist(violations)
	ledgerWalletCount := 0
	legacyMismatchCount := 0
	for _, v := range violations {
		if v.CheckName == "legacy_mismatch" {
			legacyMismatchCount++
		} else {
			ledgerWalletCount++
		}
	}
	metrics.ReconciliationViolations.WithLabelValues("ledger_wallet").Set(float64(ledgerWalletCount))
	metrics.ReconciliationViolations.WithLabelValues("legacy_mismatch").Set(float64(legacyMismatchCount))
}

func (w *ReconciliationWorker) runAssetConservationCheck() {
	rows, err := w.Repository.CheckAssetConservation()
	if err != nil {
		w.logf("reconciliation: asset_conservation check failed: %v", err)
		metrics.ReconciliationCheckErrorsTotal.WithLabelValues("asset_conservation").Inc()
		return
	}

	var violations []model.ReconciliationViolation
	for _, row := range rows {
		if classifyAssetConservationRow(row) {
			violations = append(violations, model.ReconciliationViolation{
				CheckName:  "asset_conservation",
				SubjectKey: fmt.Sprintf("coin:%s", row.CoinSymbol),
				Detail:     assetConservationViolationDetail(row),
				DetectedAt: time.Now().UTC(),
			})
		}
	}

	w.persist(violations)
	metrics.ReconciliationViolations.WithLabelValues("asset_conservation").Set(float64(len(violations)))
}

func (w *ReconciliationWorker) runStaleMarketOrderCheck() {
	rows, err := w.Repository.CheckStaleMarketOrders(staleMarketOrderThreshold)
	if err != nil {
		w.logf("reconciliation: stale_market_order check failed: %v", err)
		metrics.ReconciliationCheckErrorsTotal.WithLabelValues("stale_market_order").Inc()
		return
	}

	violations := make([]model.ReconciliationViolation, 0, len(rows))
	for _, row := range rows {
		violations = append(violations, model.ReconciliationViolation{
			CheckName:  "stale_market_order",
			SubjectKey: fmt.Sprintf("order:%d", row.OrderID),
			Detail:     staleMarketOrderViolationDetail(row),
			DetectedAt: time.Now().UTC(),
		})
	}

	w.persist(violations)
	metrics.ReconciliationViolations.WithLabelValues("stale_market_order").Set(float64(len(violations)))
}

func (w *ReconciliationWorker) persist(violations []model.ReconciliationViolation) {
	if len(violations) == 0 {
		return
	}
	if err := w.Repository.CreateViolations(violations); err != nil {
		w.logf("reconciliation: persist violations failed: %v", err)
	}
}

// classifyLedgerWalletRow는 위반이 레거시 데이터로 완전히 설명되는지(legacy_mismatch) 아니면
// 진짜 버그인지(ledger_wallet) 판정합니다. locked_delta는 레거시 구체화의 영향을 받지 않으므로
// (ledger.go의 ledgerEntryFromWalletUpdate가 available만 폴백 기준으로 계산) locked gap이
// 0이 아니면 항상 ledger_wallet입니다. 원장 항목이 하나도 없는 지갑(implied가 NULL)은
// 레거시 패턴과 구분할 근거가 없으므로 0으로 취급해 안전하게 ledger_wallet으로 분류합니다.
func classifyLedgerWalletRow(row repository.LedgerWalletRow) (checkName string, violated bool) {
	availableGap := row.AvailableBalance.Sub(row.LedgerAvailableSum)
	lockedGap := row.LockedBalance.Sub(row.LedgerLockedSum)
	if availableGap.IsZero() && lockedGap.IsZero() {
		return "", false
	}

	implied := decimal.Zero
	if row.ImpliedInitialAvailable.Valid {
		implied = row.ImpliedInitialAvailable.Decimal
	}
	if availableGap.Equal(implied) && lockedGap.IsZero() {
		return "legacy_mismatch", true
	}
	return "ledger_wallet", true
}

func classifyAssetConservationRow(row repository.AssetConservationRow) bool {
	return !row.WalletTotal.Add(row.FeeTotal).Equal(row.FundedTotal)
}

func ledgerWalletViolationDetail(row repository.LedgerWalletRow) string {
	implied := "null"
	if row.ImpliedInitialAvailable.Valid {
		implied = row.ImpliedInitialAvailable.Decimal.String()
	}
	return truncateReconciliationDetail(fmt.Sprintf(
		"wallet_id=%d user_id=%d coin_symbol=%s available_balance=%s locked_balance=%s ledger_available_sum=%s ledger_locked_sum=%s implied_initial_available=%s",
		row.WalletID, row.UserID, row.CoinSymbol,
		row.AvailableBalance.String(), row.LockedBalance.String(),
		row.LedgerAvailableSum.String(), row.LedgerLockedSum.String(), implied,
	))
}

func assetConservationViolationDetail(row repository.AssetConservationRow) string {
	return truncateReconciliationDetail(fmt.Sprintf(
		"coin_symbol=%s wallet_total=%s fee_total=%s funded_total=%s",
		row.CoinSymbol, row.WalletTotal.String(), row.FeeTotal.String(), row.FundedTotal.String(),
	))
}

func staleMarketOrderViolationDetail(row repository.StaleMarketOrderRow) string {
	return truncateReconciliationDetail(fmt.Sprintf(
		"order_id=%d user_id=%d coin_symbol=%s status=%s created_at=%s",
		row.OrderID, row.UserID, row.CoinSymbol, row.Status, row.CreatedAt.Format(time.RFC3339),
	))
}

func truncateReconciliationDetail(s string) string {
	if len(s) <= maxReconciliationDetailLength {
		return s
	}
	return s[:maxReconciliationDetailLength]
}

func (w *ReconciliationWorker) interval() time.Duration {
	if w.Interval > 0 {
		return w.Interval
	}
	return defaultReconciliationInterval
}

func (w *ReconciliationWorker) logf(format string, args ...interface{}) {
	logger := w.Logger
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf(format, args...)
}
