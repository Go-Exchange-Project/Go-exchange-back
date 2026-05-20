package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokenManagerGenerateAndParse(t *testing.T) {
	tm, err := NewTokenManager("test-secret", time.Hour)
	require.NoError(t, err)
	tm.now = func() time.Time { return time.Unix(1000, 0) }

	token, err := tm.Generate(42)
	require.NoError(t, err)

	userID, err := tm.Parse(token)
	require.NoError(t, err)
	assert.Equal(t, uint(42), userID)
}

func TestTokenManagerRejectsTamperedToken(t *testing.T) {
	tm, err := NewTokenManager("test-secret", time.Hour)
	require.NoError(t, err)

	token, err := tm.Generate(42)
	require.NoError(t, err)

	_, err = tm.Parse(token + "x")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidToken))
}

func TestTokenManagerRejectsExpiredToken(t *testing.T) {
	tm, err := NewTokenManager("test-secret", time.Hour)
	require.NoError(t, err)
	tm.now = func() time.Time { return time.Unix(1000, 0) }

	token, err := tm.Generate(42)
	require.NoError(t, err)

	tm.now = func() time.Time { return time.Unix(5000, 0) }
	_, err = tm.Parse(token)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrExpiredToken))
}

func TestExtractBearerToken(t *testing.T) {
	token, err := ExtractBearerToken("Bearer abc.def.ghi")
	require.NoError(t, err)
	assert.Equal(t, "abc.def.ghi", token)

	_, err = ExtractBearerToken("abc.def.ghi")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMissingToken))
}
