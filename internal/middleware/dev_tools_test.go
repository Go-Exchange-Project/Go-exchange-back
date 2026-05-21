package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestDevToolsRequiredAcceptsConfiguredToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(DevToolsRequired("dev-token"))
	router.POST("/dev/wallets/fund", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/dev/wallets/fund", nil)
	req.Header.Set(DevToolsTokenHeader, "dev-token")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestDevToolsRequiredRejectsMissingConfiguredToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(DevToolsRequired(""))
	router.POST("/dev/wallets/fund", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/dev/wallets/fund", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), `"code":"DEV_TOOLS_DISABLED"`)
}

func TestDevToolsRequiredRejectsInvalidToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(DevToolsRequired("dev-token"))
	router.POST("/dev/wallets/fund", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/dev/wallets/fund", nil)
	req.Header.Set(DevToolsTokenHeader, "wrong-token")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), `"code":"DEV_TOOLS_FORBIDDEN"`)
}
