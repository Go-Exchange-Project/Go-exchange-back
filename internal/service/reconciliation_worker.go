package service

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
)

const (
	defaultReconciliationInterval = time.Hour
	reconciliationPageSize        = 500
	staleMarketOrderThreshold     = 5 * time.Minute
	maxReconciliationDetailLength = 2048
)

type reconciliationRepository interface {
	CheckUnbalancedJournals(afterJournalID uint, limit int) ([]repository.UnbalancedJournalRow, error)
	CheckBalanceCacheDrift(afterAccountID uint, limit int) ([]repository.BalanceDriftRow, error)
	CheckAssetTotals() ([]repository.AssetTotalRow, error)
	CheckNegativeAccounts(afterAccountID uint, limit int) ([]repository.NegativeAccountRow, error)
	CheckStaleMarketOrders(staleAfter time.Duration) ([]repository.StaleMarketOrderRow, error)
	CreateViolations(violations []model.ReconciliationViolation) error
}

// ReconciliationWorker는 원장 검산 4종과 오래된 시장가 주문 잔존을 주기적으로 검사하고
// 위반을 내구 기록 + 메트릭으로 보고합니다. 자동 교정은 하지 않습니다 — 탐지/보고만 합니다.
//
// 검산은 원장만 본다. 지갑 표를 보던 시절에는 "사라진 수수료"를 따로 더해서 맞추는
// 보정항이 필요했지만, 수수료가 FEE_INCOME 계정으로 들어가면서 그 보정이 사라졌다 —
// 이제 전기의 합은 보정 없이 0이어야 한다.
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
	w.runUnbalancedJournalCheck()
	w.runBalanceCacheDriftCheck()
	w.runAssetTotalsCheck()
	w.runNegativeAccountCheck()
	w.runStaleMarketOrderCheck()
	metrics.ReconciliationLastRunTimestamp.Set(float64(time.Now().UTC().Unix()))
}

// runUnbalancedJournalCheck는 검사 1이다. 자산별 합이 0이 아닌 분개가 하나라도
// 나오면 어딘가에서 돈이 생기거나 사라진 것이다.
//
// CheckUnbalancedJournals의 LIMIT은 분개 수에 걸리므로, 한 페이지의 행 수는
// pageSize보다 많을 수도(분개 하나가 자산 여러 종을 가지면) 적을 수도 있다.
// 종료 판정은 행 수가 아니라 그 페이지에 담긴 서로 다른 분개 수로 해야 한다 —
// 행 수로 판정하면 조기 종료로 뒷 페이지를 놓치거나, 같은 페이지를 무한 반복한다.
func (w *ReconciliationWorker) runUnbalancedJournalCheck() {
	var violations []model.ReconciliationViolation
	var afterJournalID uint

	for {
		rows, err := w.Repository.CheckUnbalancedJournals(afterJournalID, reconciliationPageSize)
		if err != nil {
			w.logf("reconciliation: unbalanced_journal check failed: %v", err)
			metrics.ReconciliationCheckErrorsTotal.WithLabelValues("unbalanced_journal").Inc()
			return
		}
		if len(rows) == 0 {
			break
		}
		journalCount := 0
		var lastJournalID uint
		for i, row := range rows {
			if i == 0 || row.JournalID != lastJournalID {
				journalCount++
				lastJournalID = row.JournalID
			}
			violations = append(violations, model.ReconciliationViolation{
				CheckName:  "unbalanced_journal",
				SubjectKey: fmt.Sprintf("journal:%d", row.JournalID),
				Detail: truncateReconciliationDetail(fmt.Sprintf(
					"journal_id=%d asset=%s sum=%s", row.JournalID, row.Asset, row.Sum.String())),
				DetectedAt: time.Now().UTC(),
			})
			afterJournalID = row.JournalID
		}
		if journalCount < reconciliationPageSize {
			break
		}
	}

	w.persist(violations)
	metrics.ReconciliationViolations.WithLabelValues("unbalanced_journal").Set(float64(len(violations)))
}

// runBalanceCacheDriftCheck는 검사 2다. 잔액 캐시는 전기의 합과 항상 같아야 한다 —
// 어긋나면 캐시를 갱신하지 않은 경로가 있다는 뜻이다.
func (w *ReconciliationWorker) runBalanceCacheDriftCheck() {
	var violations []model.ReconciliationViolation
	var afterAccountID uint

	for {
		rows, err := w.Repository.CheckBalanceCacheDrift(afterAccountID, reconciliationPageSize)
		if err != nil {
			w.logf("reconciliation: balance_cache_drift check failed: %v", err)
			metrics.ReconciliationCheckErrorsTotal.WithLabelValues("balance_cache_drift").Inc()
			return
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			violations = append(violations, model.ReconciliationViolation{
				CheckName:  "balance_cache_drift",
				SubjectKey: fmt.Sprintf("account:%d", row.AccountID),
				Detail: truncateReconciliationDetail(fmt.Sprintf(
					"account_id=%d cached=%s computed=%s",
					row.AccountID, row.Cached.String(), row.Computed.String())),
				DetectedAt: time.Now().UTC(),
			})
			afterAccountID = row.AccountID
		}
		if len(rows) < reconciliationPageSize {
			break
		}
	}

	w.persist(violations)
	metrics.ReconciliationViolations.WithLabelValues("balance_cache_drift").Set(float64(len(violations)))
}

// runAssetTotalsCheck는 검사 3이다. 자산 하나의 전기를 전부 더하면 0이어야 한다.
// 지갑 시절의 수수료 보정항은 없다 — 수수료도 FEE_INCOME 계정에 남아 있다.
func (w *ReconciliationWorker) runAssetTotalsCheck() {
	rows, err := w.Repository.CheckAssetTotals()
	if err != nil {
		w.logf("reconciliation: asset_totals check failed: %v", err)
		metrics.ReconciliationCheckErrorsTotal.WithLabelValues("asset_totals").Inc()
		return
	}

	violations := make([]model.ReconciliationViolation, 0, len(rows))
	for _, row := range rows {
		violations = append(violations, model.ReconciliationViolation{
			CheckName:  "asset_totals",
			SubjectKey: fmt.Sprintf("asset:%s", row.Asset),
			Detail: truncateReconciliationDetail(fmt.Sprintf(
				"asset=%s sum=%s", row.Asset, row.Sum.String())),
			DetectedAt: time.Now().UTC(),
		})
	}

	w.persist(violations)
	metrics.ReconciliationViolations.WithLabelValues("asset_totals").Set(float64(len(violations)))
}

// runNegativeAccountCheck는 검사 4다. allows_negative가 false인 계정이 음수면
// 없는 돈을 쓴 것이다.
func (w *ReconciliationWorker) runNegativeAccountCheck() {
	var violations []model.ReconciliationViolation
	var afterAccountID uint

	for {
		rows, err := w.Repository.CheckNegativeAccounts(afterAccountID, reconciliationPageSize)
		if err != nil {
			w.logf("reconciliation: negative_account check failed: %v", err)
			metrics.ReconciliationCheckErrorsTotal.WithLabelValues("negative_account").Inc()
			return
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			owner := "system"
			if row.OwnerUserID != nil {
				owner = fmt.Sprintf("%d", *row.OwnerUserID)
			}
			violations = append(violations, model.ReconciliationViolation{
				CheckName:  "negative_account",
				SubjectKey: fmt.Sprintf("account:%d", row.AccountID),
				Detail: truncateReconciliationDetail(fmt.Sprintf(
					"account_id=%d account_type=%s owner=%s asset=%s balance=%s",
					row.AccountID, row.AccountType, owner, row.Asset, row.Balance.String())),
				DetectedAt: time.Now().UTC(),
			})
			afterAccountID = row.AccountID
		}
		if len(rows) < reconciliationPageSize {
			break
		}
	}

	w.persist(violations)
	metrics.ReconciliationViolations.WithLabelValues("negative_account").Set(float64(len(violations)))
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
