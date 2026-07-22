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

func TestWriteDataUsesStructuredDataShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/data", func(c *gin.Context) {
		WriteData(c, http.StatusCreated, gin.H{
			"id":      7,
			"message": "created",
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/data", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.JSONEq(t, `{"data":{"id":7,"message":"created"}}`, rec.Body.String())
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

func TestWriteErrorSetsRetryAfterOn503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	WriteError(c, http.StatusServiceUnavailable, CodeUnavailable, "saturated")
	assert.Equal(t, "1", w.Header().Get("Retry-After"))
}

func TestWriteErrorNoRetryAfterOnNon503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	WriteError(c, http.StatusConflict, CodeConflict, "conflict")
	assert.Empty(t, w.Header().Get("Retry-After"))
}
