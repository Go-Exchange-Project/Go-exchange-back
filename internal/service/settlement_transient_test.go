package service

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsTransientSettlementError(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		transient bool
	}{
		{"deadlock detected", &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}, true},
		{"serialization failure", &pgconn.PgError{Code: "40001", Message: "could not serialize access"}, true},
		{"lock not available", &pgconn.PgError{Code: "55P03", Message: "lock timeout"}, true},
		{"wrapped deadlock", fmt.Errorf("settle: %w", &pgconn.PgError{Code: "40P01"}), true},
		{"unique violation is permanent", &pgconn.PgError{Code: "23505"}, false},
		{"plain error", errors.New("deadlock detected"), false},
		{"nil", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.transient, IsTransientSettlementError(tt.err))
		})
	}
}

func TestClassifyFailedSettlementTransientCategories(t *testing.T) {
	tests := []struct {
		message  string
		category FailedSettlementCategory
	}{
		{"[SQLSTATE 40P01] settle: deadlock detected", FailedSettlementCategoryDeadlock},
		{"[SQLSTATE 40001] could not serialize access", FailedSettlementCategorySerializationFailure},
		{"[SQLSTATE 55P03] canceling statement due to lock timeout", FailedSettlementCategoryLockTimeout},
		{"buy order 1 status CANCELLED cannot be settled", FailedSettlementCategoryCancelledOrder},
		{"idempotency key conflict for \"k\"", FailedSettlementCategoryIdempotencyConflict},
		{"buyer has insufficient locked KRW balance", FailedSettlementCategoryInsufficientLockedBalance},
		{"some other failure", FailedSettlementCategoryUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.message, func(t *testing.T) {
			category := ClassifyFailedSettlement(&model.FailedSettlement{ErrorMessage: tt.message})
			assert.Equal(t, tt.category, category)
		})
	}
}

func TestIsTransientFailedSettlementCategory(t *testing.T) {
	assert.True(t, IsTransientFailedSettlementCategory(FailedSettlementCategoryDeadlock))
	assert.True(t, IsTransientFailedSettlementCategory(FailedSettlementCategorySerializationFailure))
	assert.True(t, IsTransientFailedSettlementCategory(FailedSettlementCategoryLockTimeout))
	assert.False(t, IsTransientFailedSettlementCategory(FailedSettlementCategoryCancelledOrder))
	assert.False(t, IsTransientFailedSettlementCategory(FailedSettlementCategoryIdempotencyConflict))
	assert.False(t, IsTransientFailedSettlementCategory(FailedSettlementCategoryInsufficientLockedBalance))
	assert.False(t, IsTransientFailedSettlementCategory(FailedSettlementCategoryUnknown))
}

func TestSettlementErrorMessageTagsSQLState(t *testing.T) {
	tagged := settlementErrorMessage(fmt.Errorf("settle: %w", &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}))
	assert.Contains(t, tagged, "[SQLSTATE 40P01]")

	plain := settlementErrorMessage(errors.New("boom"))
	assert.Equal(t, "boom", plain)
}

func TestFailedSettlementFromTradeCarriesEngineMetadata(t *testing.T) {
	tradedAt := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	trade := &model.Trade{
		EngineSequence: 7,
		EngineEventID:  "engine-abc-7",
		CoinSymbol:     "BTC",
		Price:          decimal.NewFromInt(100),
		Quantity:       decimal.NewFromInt(1),
		TradedAt:       tradedAt,
		BuyOrderID:     1,
		SellOrderID:    2,
	}

	failure, err := failedSettlementFromTrade(trade, errors.New("boom"), time.Now().UTC())
	require.NoError(t, err)
	assert.Equal(t, int64(7), failure.EngineSequence)
	assert.Equal(t, "engine-abc-7", failure.EngineEventID)
	require.NotNil(t, failure.TradedAt)
	assert.True(t, failure.TradedAt.Equal(tradedAt))
}

func TestFailedSettlementFromTradeZeroTradedAtStaysNil(t *testing.T) {
	trade := &model.Trade{
		CoinSymbol:  "BTC",
		Price:       decimal.NewFromInt(100),
		Quantity:    decimal.NewFromInt(1),
		BuyOrderID:  1,
		SellOrderID: 2,
	}

	failure, err := failedSettlementFromTrade(trade, errors.New("boom"), time.Now().UTC())
	require.NoError(t, err)
	assert.Nil(t, failure.TradedAt)
}
