package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestWriteErrorUsesStructuredErrorShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/error", func(c *gin.Context) {
		WriteError(c, http.StatusConflict, CodeConflict, "conflict happened")
	})

	req := httptest.NewRequest(http.MethodGet, "/error", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.JSONEq(t, `{"error":{"code":"CONFLICT","message":"conflict happened"}}`, rec.Body.String())
}

func TestCodeForStatus(t *testing.T) {
	assert.Equal(t, CodeAuthRequired, CodeForStatus(http.StatusUnauthorized))
	assert.Equal(t, CodeForbidden, CodeForStatus(http.StatusForbidden))
	assert.Equal(t, CodeNotFound, CodeForStatus(http.StatusNotFound))
	assert.Equal(t, CodeConflict, CodeForStatus(http.StatusConflict))
	assert.Equal(t, CodeValidation, CodeForStatus(http.StatusUnprocessableEntity))
	assert.Equal(t, CodeInternal, CodeForStatus(http.StatusInternalServerError))
	assert.Equal(t, CodeBadRequest, CodeForStatus(http.StatusTeapot))
}
