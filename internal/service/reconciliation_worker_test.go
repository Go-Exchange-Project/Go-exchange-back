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

// classifyLedgerWalletRow

func TestClassifyLedgerWalletRowNoGapIsNotViolated(t *testing.T) {
	row := repository.LedgerWalletRow{
		AvailableBalance:   decimal.NewFromInt(100),
		LockedBalance:      decimal.Zero,
		LedgerAvailableSum: decimal.NewFromInt(100),
		LedgerLockedSum:    decimal.Zero,
	}
	checkName, violated := classifyLedgerWalletRow(row)
	assert.False(t, violated)
	assert.Empty(t, checkName)
}

func TestClassifyLedgerWalletRowLegacyGapExplainedByImpliedInitial(t *testing.T) {
	row := repository.LedgerWalletRow{
		AvailableBalance:        decimal.NewFromInt(700),
		LockedBalance:           decimal.Zero,
		LedgerAvailableSum:      decimal.NewFromInt(500),
		LedgerLockedSum:         decimal.Zero,
		ImpliedInitialAvailable: decimal.NewNullDecimal(decimal.NewFromInt(200)),
	}
	checkName, violated := classifyLedgerWalletRow(row)
	assert.True(t, violated)
	assert.Equal(t, "legacy_mismatch", checkName)
}

func TestClassifyLedgerWalletRowLockedGapAlwaysMeansRealViolation(t *testing.T) {
	row := repository.LedgerWalletRow{
		AvailableBalance:        decimal.NewFromInt(700),
		LockedBalance:           decimal.NewFromInt(50),
		LedgerAvailableSum:      decimal.NewFromInt(500),
		LedgerLockedSum:         decimal.NewFromInt(10),
		ImpliedInitialAvailable: decimal.NewNullDecimal(decimal.NewFromInt(200)),
	}
	checkName, violated := classifyLedgerWalletRow(row)
	assert.True(t, violated)
	assert.Equal(t, "ledger_wallet", checkName, "available gap matches legacy but locked gap is non-zero, so this is a real bug overlapping legacy data")
}

func TestClassifyLedgerWalletRowGapNotExplainedByImpliedInitialIsRealViolation(t *testing.T) {
	row := repository.LedgerWalletRow{
		AvailableBalance:        decimal.NewFromInt(700),
		LockedBalance:           decimal.Zero,
		LedgerAvailableSum:      decimal.NewFromInt(500),
		LedgerLockedSum:         decimal.Zero,
		ImpliedInitialAvailable: decimal.NewNullDecimal(decimal.NewFromInt(100)),
	}
	checkName, violated := classifyLedgerWalletRow(row)
	assert.True(t, violated)
	assert.Equal(t, "ledger_wallet", checkName)
}

func TestClassifyLedgerWalletRowNullImpliedInitialTreatedAsZero(t *testing.T) {
	row := repository.LedgerWalletRow{
		AvailableBalance:   decimal.NewFromInt(50),
		LockedBalance:      decimal.Zero,
		LedgerAvailableSum: decimal.Zero,
		LedgerLockedSum:    decimal.Zero,
		// ImpliedInitialAvailable left zero-value: Valid=false (NULL)
	}
	checkName, violated := classifyLedgerWalletRow(row)
	assert.True(t, violated)
	assert.Equal(t, "ledger_wallet", checkName, "a wallet with no ledger history but a nonzero balance cannot be distinguished from a bug, so it must NOT be classified as legacy")
}

// classifyAssetConservationRow

func TestClassifyAssetConservationRowBalancedIsNotViolated(t *testing.T) {
	row := repository.AssetConservationRow{
		WalletTotal: decimal.NewFromInt(100),
		FeeTotal:    decimal.NewFromInt(5),
		FundedTotal: decimal.NewFromInt(105),
	}
	assert.False(t, classifyAssetConservationRow(row))
}

func TestClassifyAssetConservationRowImbalancedIsViolated(t *testing.T) {
	row := repository.AssetConservationRow{
		WalletTotal: decimal.NewFromInt(100),
		FeeTotal:    decimal.NewFromInt(5),
		FundedTotal: decimal.NewFromInt(200),
	}
	assert.True(t, classifyAssetConservationRow(row))
}

// RunOnce orchestration with fakes

type fakeReconciliationRepository struct {
	ledgerWalletRows      []repository.LedgerWalletRow
	ledgerWalletErr       error
	assetConservationRows []repository.AssetConservationRow
	assetConservationErr  error
	staleMarketOrderRows  []repository.StaleMarketOrderRow
	staleMarketOrderErr   error
	createViolationsCalls [][]model.ReconciliationViolation
	createViolationsErr   error
	ledgerWalletPageCalls []uint
}

func (f *fakeReconciliationRepository) CheckLedgerWalletPage(afterWalletID uint, limit int) ([]repository.LedgerWalletRow, error) {
	f.ledgerWalletPageCalls = append(f.ledgerWalletPageCalls, afterWalletID)
	if f.ledgerWalletErr != nil {
		return nil, f.ledgerWalletErr
	}
	if len(f.ledgerWalletPageCalls) > 1 {
		return nil, nil // second page is always empty in these tests
	}
	return f.ledgerWalletRows, nil
}

func (f *fakeReconciliationRepository) CheckAssetConservation() ([]repository.AssetConservationRow, error) {
	return f.assetConservationRows, f.assetConservationErr
}

func (f *fakeReconciliationRepository) CheckStaleMarketOrders(time.Duration) ([]repository.StaleMarketOrderRow, error) {
	return f.staleMarketOrderRows, f.staleMarketOrderErr
}

func (f *fakeReconciliationRepository) CreateViolations(violations []model.ReconciliationViolation) error {
	f.createViolationsCalls = append(f.createViolationsCalls, violations)
	return f.createViolationsErr
}

func TestRunOnceRecordsLedgerWalletViolationAndSetsGauges(t *testing.T) {
	repo := &fakeReconciliationRepository{
		ledgerWalletRows: []repository.LedgerWalletRow{{
			WalletID:           42,
			UserID:             7,
			CoinSymbol:         "BTC",
			AvailableBalance:   decimal.NewFromInt(700),
			LockedBalance:      decimal.Zero,
			LedgerAvailableSum: decimal.NewFromInt(500),
			LedgerLockedSum:    decimal.Zero,
		}},
	}
	worker := &ReconciliationWorker{Repository: repo, Logger: discardServiceLogger()}

	worker.RunOnce()

	require.Len(t, repo.createViolationsCalls, 1)
	require.Len(t, repo.createViolationsCalls[0], 1)
	assert.Equal(t, "ledger_wallet", repo.createViolationsCalls[0][0].CheckName)
	assert.Equal(t, "wallet:42", repo.createViolationsCalls[0][0].SubjectKey)
	assert.Equal(t, float64(1), testutil.ToFloat64(metrics.ReconciliationViolations.WithLabelValues("ledger_wallet")))
	assert.Equal(t, float64(0), testutil.ToFloat64(metrics.ReconciliationViolations.WithLabelValues("legacy_mismatch")))
}

func TestRunOnceSetsZeroGaugeWhenNoViolations(t *testing.T) {
	repo := &fakeReconciliationRepository{}
	worker := &ReconciliationWorker{Repository: repo, Logger: discardServiceLogger()}

	worker.RunOnce()

	assert.Empty(t, repo.createViolationsCalls, "no violations found, so CreateViolations must not be called with an empty slice inside RunOnce's own persist step")
	assert.Equal(t, float64(0), testutil.ToFloat64(metrics.ReconciliationViolations.WithLabelValues("ledger_wallet")))
	assert.Equal(t, float64(0), testutil.ToFloat64(metrics.ReconciliationViolations.WithLabelValues("asset_conservation")))
	assert.Equal(t, float64(0), testutil.ToFloat64(metrics.ReconciliationViolations.WithLabelValues("stale_market_order")))
}

func TestRunOnceIncrementsErrorCounterAndSkipsGaugeOnQueryFailure(t *testing.T) {
	repo := &fakeReconciliationRepository{assetConservationErr: errors.New("db unavailable")}
	worker := &ReconciliationWorker{Repository: repo, Logger: discardServiceLogger()}

	before := testutil.ToFloat64(metrics.ReconciliationCheckErrorsTotal.WithLabelValues("asset_conservation"))
	beforeGauge := testutil.ToFloat64(metrics.ReconciliationViolations.WithLabelValues("asset_conservation"))

	worker.RunOnce()

	after := testutil.ToFloat64(metrics.ReconciliationCheckErrorsTotal.WithLabelValues("asset_conservation"))
	afterGauge := testutil.ToFloat64(metrics.ReconciliationViolations.WithLabelValues("asset_conservation"))
	assert.Equal(t, before+1, after)
	assert.Equal(t, beforeGauge, afterGauge, "gauge must not be overwritten when the check itself failed")
}

func TestRunOncePaginatesLedgerWalletCheckUntilPageIsShort(t *testing.T) {
	fullPage := make([]repository.LedgerWalletRow, reconciliationPageSize)
	for i := range fullPage {
		fullPage[i] = repository.LedgerWalletRow{WalletID: uint(i + 1)}
	}
	repo := &fakeReconciliationRepository{ledgerWalletRows: fullPage}
	worker := &ReconciliationWorker{Repository: repo, Logger: discardServiceLogger()}

	worker.RunOnce()

	require.Len(t, repo.ledgerWalletPageCalls, 2, "a full first page must trigger a second page request")
	assert.Equal(t, uint(0), repo.ledgerWalletPageCalls[0])
	assert.Equal(t, uint(reconciliationPageSize), repo.ledgerWalletPageCalls[1])
}
