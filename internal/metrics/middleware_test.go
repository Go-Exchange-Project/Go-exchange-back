package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTTPMiddlewareRecordsRequestsTotalAndDuration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(metrics.HTTPMiddleware())
	router.GET("/ping", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	before := testutil.ToFloat64(metrics.HTTPRequestsTotal.WithLabelValues(http.MethodGet, "/ping", "200"))

	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	after := testutil.ToFloat64(metrics.HTTPRequestsTotal.WithLabelValues(http.MethodGet, "/ping", "200"))
	assert.Equal(t, before+1, after)
}

func TestHTTPMiddlewareUsesUnmatchedPathForUnknownRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(metrics.HTTPMiddleware())

	before := testutil.ToFloat64(metrics.HTTPRequestsTotal.WithLabelValues(http.MethodGet, "unmatched", "404"))

	req := httptest.NewRequest(http.MethodGet, "/does-not-exist", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
	after := testutil.ToFloat64(metrics.HTTPRequestsTotal.WithLabelValues(http.MethodGet, "unmatched", "404"))
	assert.Equal(t, before+1, after)
}
