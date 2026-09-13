package service

import (
	"errors"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeReconciliationRepository는 검산 4종 + 시장가 주문 검사를 흉내 낸다.
// 페이지네이션을 보는 검사는 첫 페이지만 돌려주고 두 번째부터는 비운다.
type fakeReconciliationRepository struct {
	unbalancedRows []repository.UnbalancedJournalRow
	unbalancedErr  error
	unbalancedArgs []uint

	driftRows []repository.BalanceDriftRow
	driftErr  error

	assetTotalRows []repository.AssetTotalRow
	assetTotalErr  error

	negativeRows []repository.NegativeAccountRow
	negativeErr  error

	staleMarketOrderRows []repository.StaleMarketOrderRow
	staleMarketOrderErr  error

	createViolationsCalls [][]model.ReconciliationViolation
	createViolationsErr   error
}

func (f *fakeReconciliationRepository) CheckUnbalancedJournals(afterJournalID uint, limit int) ([]repository.UnbalancedJournalRow, error) {
	f.unbalancedArgs = append(f.unbalancedArgs, afterJournalID)
	if f.unbalancedErr != nil {
		return nil, f.unbalancedErr
	}
	if len(f.unbalancedArgs) > 1 {
		return nil, nil
	}
	return f.unbalancedRows, nil
}

func (f *fakeReconciliationRepository) CheckBalanceCacheDrift(afterAccountID uint, limit int) ([]repository.BalanceDriftRow, error) {
	if f.driftErr != nil {
		return nil, f.driftErr
	}
	if afterAccountID != 0 {
		return nil, nil
	}
	return f.driftRows, nil
}

func (f *fakeReconciliationRepository) CheckAssetTotals() ([]repository.AssetTotalRow, error) {
	return f.assetTotalRows, f.assetTotalErr
}

func (f *fakeReconciliationRepository) CheckNegativeAccounts(afterAccountID uint, limit int) ([]repository.NegativeAccountRow, error) {
	if f.negativeErr != nil {
		return nil, f.negativeErr
	}
	if afterAccountID != 0 {
		return nil, nil
	}
	return f.negativeRows, nil
}

func (f *fakeReconciliationRepository) CheckStaleMarketOrders(time.Duration) ([]repository.StaleMarketOrderRow, error) {
	return f.staleMarketOrderRows, f.staleMarketOrderErr
}

func (f *fakeReconciliationRepository) CreateViolations(violations []model.ReconciliationViolation) error {
	f.createViolationsCalls = append(f.createViolationsCalls, violations)
	return f.createViolationsErr
}

func TestRunOnceRecordsLedgerViolationsAndSetsGauges(t *testing.T) {
	owner := uint(7)
	repo := &fakeReconciliationRepository{
		unbalancedRows: []repository.UnbalancedJournalRow{{
			JournalID: 42, Asset: model.KRWAssetSymbol, Sum: decimal.NewFromInt(5),
		}},
		driftRows: []repository.BalanceDriftRow{{
			AccountID: 11, Cached: decimal.NewFromInt(100), Computed: decimal.NewFromInt(90),
		}},
		assetTotalRows: []repository.AssetTotalRow{{
			Asset: "BTC", Sum: decimal.NewFromInt(-2),
		}},
		negativeRows: []repository.NegativeAccountRow{{
			AccountID: 13, AccountType: "USER_AVAILABLE", OwnerUserID: &owner,
			Asset: model.KRWAssetSymbol, Balance: decimal.NewFromInt(-1),
		}},
	}
	worker := &ReconciliationWorker{Repository: repo, Logger: discardServiceLogger()}

	worker.RunOnce()

	require.Len(t, repo.createViolationsCalls, 4, "검사 4종이 각각 위반을 기록해야 한다")
	subjects := map[string]string{}
	for _, call := range repo.createViolationsCalls {
		require.Len(t, call, 1)
		subjects[call[0].CheckName] = call[0].SubjectKey
	}
	assert.Equal(t, "journal:42", subjects["unbalanced_journal"])
	assert.Equal(t, "account:11", subjects["balance_cache_drift"])
	assert.Equal(t, "asset:BTC", subjects["asset_totals"])
	assert.Equal(t, "account:13", subjects["negative_account"])

	for _, name := range []string{"unbalanced_journal", "balance_cache_drift", "asset_totals", "negative_account"} {
		assert.Equal(t, float64(1), testutil.ToFloat64(metrics.ReconciliationViolations.WithLabelValues(name)), name)
	}
}

func TestRunOnceSetsZeroGaugeWhenNoViolations(t *testing.T) {
	repo := &fakeReconciliationRepository{}
	worker := &ReconciliationWorker{Repository: repo, Logger: discardServiceLogger()}

	worker.RunOnce()

	assert.Empty(t, repo.createViolationsCalls, "위반이 없으면 빈 슬라이스로 CreateViolations를 부르지 않는다")
	for _, name := range []string{"unbalanced_journal", "balance_cache_drift", "asset_totals", "negative_account", "stale_market_order"} {
		assert.Equal(t, float64(0), testutil.ToFloat64(metrics.ReconciliationViolations.WithLabelValues(name)), name)
	}
}

func TestRunOnceIncrementsErrorCounterAndSkipsGaugeOnQueryFailure(t *testing.T) {
	repo := &fakeReconciliationRepository{assetTotalErr: errors.New("db unavailable")}
	worker := &ReconciliationWorker{Repository: repo, Logger: discardServiceLogger()}

	before := testutil.ToFloat64(metrics.ReconciliationCheckErrorsTotal.WithLabelValues("asset_totals"))
	beforeGauge := testutil.ToFloat64(metrics.ReconciliationViolations.WithLabelValues("asset_totals"))

	worker.RunOnce()

	after := testutil.ToFloat64(metrics.ReconciliationCheckErrorsTotal.WithLabelValues("asset_totals"))
	afterGauge := testutil.ToFloat64(metrics.ReconciliationViolations.WithLabelValues("asset_totals"))
	assert.Equal(t, before+1, after)
	assert.Equal(t, beforeGauge, afterGauge, "검사 자체가 실패하면 게이지를 덮어쓰지 않는다")
}

func TestRunOncePaginatesUnbalancedJournalCheckUntilPageIsShort(t *testing.T) {
	fullPage := make([]repository.UnbalancedJournalRow, reconciliationPageSize)
	for i := range fullPage {
		fullPage[i] = repository.UnbalancedJournalRow{JournalID: uint(i + 1), Asset: model.KRWAssetSymbol, Sum: decimal.NewFromInt(1)}
	}
	repo := &fakeReconciliationRepository{unbalancedRows: fullPage}
	worker := &ReconciliationWorker{Repository: repo, Logger: discardServiceLogger()}

	worker.RunOnce()

	require.Len(t, repo.unbalancedArgs, 2, "첫 페이지가 가득 차면 다음 페이지를 요청해야 한다")
	assert.Equal(t, uint(0), repo.unbalancedArgs[0])
	assert.Equal(t, uint(reconciliationPageSize), repo.unbalancedArgs[1])
}
