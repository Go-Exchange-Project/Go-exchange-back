package service

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFailedSettlementFromTradeBuildsPayload(t *testing.T) {
	occurredAt := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	trade := &model.Trade{
		IdempotencyKey: " trade-key ",
		CoinSymbol:     " btc ",
		BuyOrderID:     10,
		SellOrderID:    20,
		Price:          decimal.NewFromInt(90),
		Quantity:       decimal.NewFromInt(5),
	}

	failure, err := failedSettlementFromTrade(trade, errors.New("buy order 10 status CANCELLED cannot be settled"), occurredAt)

	require.NoError(t, err)
	assert.Equal(t, "trade-key", failure.TradeIdempotencyKey)
	assert.Equal(t, "BTC", failure.CoinSymbol)
	assert.Equal(t, uint(10), failure.BuyOrderID)
	assert.Equal(t, uint(20), failure.SellOrderID)
	assert.True(t, failure.Price.Equal(decimal.NewFromInt(90)))
	assert.True(t, failure.Quantity.Equal(decimal.NewFromInt(5)))
	assert.Equal(t, "buy order 10 status CANCELLED cannot be settled", failure.ErrorMessage)
	assert.Equal(t, model.FailedSettlementStatusOpen, failure.Status)
	assert.Equal(t, uint(1), failure.RetryCount)
	assert.Equal(t, occurredAt, failure.OccurredAt)
}

func TestFailedSettlementFromTradeFillsDeterministicIdempotencyKey(t *testing.T) {
	trade := &model.Trade{
		CoinSymbol:  "BTC",
		BuyOrderID:  10,
		SellOrderID: 20,
		Price:       decimal.NewFromInt(90),
		Quantity:    decimal.NewFromInt(5),
	}

	failure, err := failedSettlementFromTrade(trade, errors.New("settlement failed"), time.Now())

	require.NoError(t, err)
	assert.NotEmpty(t, failure.TradeIdempotencyKey)
	assert.Equal(t, deterministicTradeIdempotencyKey(trade), failure.TradeIdempotencyKey)
	assert.Equal(t, failure.TradeIdempotencyKey, trade.IdempotencyKey)
}

func TestSettlementErrorMessageDefaultsWhenEmpty(t *testing.T) {
	assert.Equal(t, "unknown settlement failure", settlementErrorMessage(nil))
	assert.Equal(t, "unknown settlement failure", settlementErrorMessage(errors.New("  ")))
}

func TestSettlementErrorMessageIsBounded(t *testing.T) {
	message := strings.Repeat("x", maxFailedSettlementErrorLength+10)

	got := settlementErrorMessage(errors.New(message))

	assert.Len(t, got, maxFailedSettlementErrorLength)
}

func TestValidateResolveFailureInput(t *testing.T) {
	tests := []struct {
		name  string
		input ResolveFailureInput
	}{
		{
			name:  "missing id",
			input: ResolveFailureInput{Resolution: "investigated"},
		},
		{
			name:  "missing resolution",
			input: ResolveFailureInput{ID: 1, Resolution: "  "},
		},
		{
			name:  "long resolution",
			input: ResolveFailureInput{ID: 1, Resolution: strings.Repeat("x", maxFailedSettlementResolutionLength+1)},
		},
		{
			name:  "long resolved by",
			input: ResolveFailureInput{ID: 1, Resolution: "done", ResolvedBy: strings.Repeat("x", maxFailedSettlementResolvedByLength+1)},
		},
		{
			name:  "long notes",
			input: ResolveFailureInput{ID: 1, Resolution: "done", Notes: strings.Repeat("x", maxFailedSettlementNotesLength+1)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Error(t, validateResolveFailureInput(tt.input))
		})
	}

	require.NoError(t, validateResolveFailureInput(ResolveFailureInput{
		ID:         1,
		Resolution: "investigated stale engine event",
		ResolvedBy: "ops",
		Notes:      "no wallet mutation required",
	}))
}

func TestClassifyFailedSettlement(t *testing.T) {
	tests := []struct {
		name     string
		message  string
		category FailedSettlementCategory
	}{
		{
			name:     "cancelled order",
			message:  "buy order 10 status CANCELLED cannot be settled",
			category: FailedSettlementCategoryCancelledOrder,
		},
		{
			name:     "idempotency conflict",
			message:  "idempotency key conflict for trade",
			category: FailedSettlementCategoryIdempotencyConflict,
		},
		{
			name:     "insufficient locked balance",
			message:  "insufficient locked balance for release",
			category: FailedSettlementCategoryInsufficientLockedBalance,
		},
		{
			name:     "unknown",
			message:  "database timeout",
			category: FailedSettlementCategoryUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failure := &model.FailedSettlement{ErrorMessage: tt.message}

			assert.Equal(t, tt.category, ClassifyFailedSettlement(failure))
		})
	}

	assert.Equal(t, FailedSettlementCategoryUnknown, ClassifyFailedSettlement(nil))
}

func TestListOpenFailuresNormalizesLimit(t *testing.T) {
	repo := &fakeFailedSettlementRepository{}
	service := &FailedSettlementService{Repository: repo}

	_, err := service.ListOpenFailures(0)
	require.NoError(t, err)
	assert.Equal(t, repository.DefaultFailedSettlementListLimit, repo.findOpenLimit)

	_, err = service.ListOpenFailures(repository.MaxFailedSettlementListLimit + 1)
	require.NoError(t, err)
	assert.Equal(t, repository.MaxFailedSettlementListLimit, repo.findOpenLimit)
}

func TestResolveFailureTrimsAuditFields(t *testing.T) {
	repo := &fakeFailedSettlementRepository{
		findByIDResult: &model.FailedSettlement{ID: 42, Status: model.FailedSettlementStatusResolved},
	}
	service := &FailedSettlementService{Repository: repo}

	result, err := service.ResolveFailure(ResolveFailureInput{
		ID:         42,
		Resolution: "  stale engine event reviewed  ",
		ResolvedBy: " ops-user ",
		Notes:      " no retry ",
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, repo.markResolvedCalled)
	assert.Equal(t, uint(42), repo.markResolvedID)
	assert.Equal(t, "stale engine event reviewed", repo.markResolvedResolution)
	assert.Equal(t, "ops-user", repo.markResolvedBy)
	assert.Equal(t, "no retry", repo.markResolvedNotes)
}

type fakeFailedSettlementRepository struct {
	findOpenLimit          int
	findByIDResult         *model.FailedSettlement
	markResolvedCalled     bool
	markResolvedID         uint
	markResolvedResolution string
	markResolvedBy         string
	markResolvedNotes      string
}

func (r *fakeFailedSettlementRepository) RecordFailure(failure *model.FailedSettlement) (*model.FailedSettlement, error) {
	return failure, nil
}

func (r *fakeFailedSettlementRepository) FindOpen(limit int) ([]model.FailedSettlement, error) {
	r.findOpenLimit = limit
	return nil, nil
}

func (r *fakeFailedSettlementRepository) FindByID(uint) (*model.FailedSettlement, error) {
	if r.findByIDResult != nil {
		return r.findByIDResult, nil
	}
	return &model.FailedSettlement{ID: r.markResolvedID}, nil
}

func (r *fakeFailedSettlementRepository) MarkResolved(id uint, resolution string, resolvedBy string, notes string) error {
	r.markResolvedCalled = true
	r.markResolvedID = id
	r.markResolvedResolution = resolution
	r.markResolvedBy = resolvedBy
	r.markResolvedNotes = notes
	return nil
}
