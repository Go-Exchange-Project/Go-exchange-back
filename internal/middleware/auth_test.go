package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/auth"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuthRequiredAcceptsValidBearerToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tokenManager, err := auth.NewTokenManager("test-secret", time.Hour)
	require.NoError(t, err)
	token, err := tokenManager.Generate(7)
	require.NoError(t, err)

	router := gin.New()
	router.Use(AuthRequired(tokenManager))
	router.GET("/protected", func(c *gin.Context) {
		userID, exists := c.Get(auth.UserIDContextKey)
		require.True(t, exists)
		c.JSON(http.StatusOK, gin.H{"user_id": userID})
	})

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"user_id":7`)
}

func TestAuthRequiredRejectsMissingBearerToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tokenManager, err := auth.NewTokenManager("test-secret", time.Hour)
	require.NoError(t, err)

	router := gin.New()
	router.Use(AuthRequired(tokenManager))
	router.GET("/protected", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
