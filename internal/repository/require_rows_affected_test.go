package repository

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

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
