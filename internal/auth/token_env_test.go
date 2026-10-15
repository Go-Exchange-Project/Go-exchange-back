package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A1: GOEXCHANGE_JWT_SECRET이 없으면 기본값으로 폴백하지 않고 즉시 실패한다.
// 값이 있으면 성공한다(대조군 — 없으면 항상 실패하는 구현도 통과하는 것을
// 막는다).
func TestNewTokenManagerFromEnvRequiresSecret(t *testing.T) {
	t.Run("unset", func(t *testing.T) {
		t.Setenv(EnvJWTSecret, "")

		_, err := NewTokenManagerFromEnv()
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidJWTSecret)
	})

	t.Run("blank", func(t *testing.T) {
		t.Setenv(EnvJWTSecret, "   ")

		_, err := NewTokenManagerFromEnv()
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidJWTSecret)
	})

	t.Run("present", func(t *testing.T) {
		t.Setenv(EnvJWTSecret, "a-real-secret")

		tm, err := NewTokenManagerFromEnv()
		require.NoError(t, err)
		assert.NotNil(t, tm)
	})
}
