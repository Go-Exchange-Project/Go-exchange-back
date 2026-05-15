package repository

import (
	"errors"
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestWalletScopeUsesUserIDAndCoinSymbol(t *testing.T) {
	query, args := walletByUserAndCoinScope(1, "BTC")

	assert.Equal(t, "user_id = ? AND coin_symbol = ?", query)
	assert.Equal(t, []interface{}{uint(1), "BTC"}, args)
}

func TestKRWAssetSymbolIsExplicit(t *testing.T) {
	query, args := walletByUserAndCoinScope(2, model.KRWAssetSymbol)

	assert.Equal(t, "user_id = ? AND coin_symbol = ?", query)
	assert.Equal(t, []interface{}{uint(2), model.KRWAssetSymbol}, args)
}

func TestRequireRowsAffectedReturnsDBError(t *testing.T) {
	err := errors.New("db failed")
	result := &gorm.DB{Error: err}

	require.ErrorIs(t, requireRowsAffected(result, "test operation"), err)
}

func TestRequireRowsAffectedRejectsZeroRows(t *testing.T) {
	result := &gorm.DB{RowsAffected: 0}

	err := requireRowsAffected(result, "test operation")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "affected no rows")
}

func TestRequireRowsAffectedAcceptsUpdatedRows(t *testing.T) {
	result := &gorm.DB{RowsAffected: 1}

	require.NoError(t, requireRowsAffected(result, "test operation"))
}
