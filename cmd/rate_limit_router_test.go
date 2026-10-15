package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/auth"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/handler"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/middleware"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/ws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Task 8 Step 3 — 실제 라우터 배선 검증. 아래 요청은 전부 handler의 JSON
// 필수 필드 바인딩에서 끝나므로(본문 "{}"), 서비스가 DB에 닿기 전에 응답이
// 결정된다. 배선 순서(인증 → rate limit → 핸들러)와 버킷 분리 자체가
// 검증 대상이라 실제 DB 없이도 실제 newRouter로 충분히 검증된다.
func newRateLimitTestRouterConfig(t *testing.T, tokenManager *auth.TokenManager, authRL, orderRL, transferRL *middleware.RateLimiter) routerConfig {
	t.Helper()
	orderRepo := repository.NewOrderRepository(nil)
	orderService := service.NewOrderService(orderRepo, nil)
	transferService := service.NewTransferService(nil, nil)
	authService := &service.AuthService{TokenManager: tokenManager}

	return routerConfig{
		corsOrigins:         []string{"http://localhost:3000"},
		hub:                 ws.NewHub(),
		authHandler:         handler.NewAuthHandler(authService),
		marketHandler:       handler.NewMarketHandler(),
		orderBookHandler:    handler.NewOrderBookHandler(nil),
		orderHandler:        handler.NewOrderHandler(orderService),
		transferHandler:     handler.NewTransferHandler(transferService),
		tokenManager:        tokenManager,
		authRateLimiter:     authRL,
		orderRateLimiter:    orderRL,
		transferRateLimiter: transferRL,
	}
}

func bearerToken(t *testing.T, tokenManager *auth.TokenManager, userID uint) string {
	t.Helper()
	token, err := tokenManager.Generate(userID)
	require.NoError(t, err)
	return token
}

func postJSON(router http.Handler, path, authorization string) int {
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.55:4321"
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	router.ServeHTTP(recorder, req)
	return recorder.Code
}

// 미인증 요청은 user limiter보다 먼저 401을 받는다(설계 §6.2 배치 순서).
func TestRouterUnauthenticatedOrderRequestsGet401NotRateLimited(t *testing.T) {
	tokenManager, err := auth.NewTokenManager("test-secret", time.Hour)
	require.NoError(t, err)
	orderRL := middleware.NewRateLimiter(middleware.RateLimiterConfig{RPS: 1, Burst: 1})
	router, err := newRouter(newRateLimitTestRouterConfig(t, tokenManager, nil, orderRL, nil))
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		code := postJSON(router, "/orders", "")
		assert.Equal(t, http.StatusUnauthorized, code,
			"미인증 요청은 rate limit(429)이 아니라 401이어야 한다(%d번째)", i)
	}
}

// 서로 다른 사용자 ID는 서로 다른 버킷을 쓴다.
func TestRouterOrderRateLimitBucketsAreSeparatePerUser(t *testing.T) {
	tokenManager, err := auth.NewTokenManager("test-secret", time.Hour)
	require.NoError(t, err)
	orderRL := middleware.NewRateLimiter(middleware.RateLimiterConfig{RPS: 1, Burst: 1})
	router, err := newRouter(newRateLimitTestRouterConfig(t, tokenManager, nil, orderRL, nil))
	require.NoError(t, err)

	userA := "Bearer " + bearerToken(t, tokenManager, 101)
	userB := "Bearer " + bearerToken(t, tokenManager, 202)

	assert.Equal(t, http.StatusUnprocessableEntity, postJSON(router, "/orders", userA),
		"user A의 첫 요청은 burst 안이라 통과해 바인딩 검증(422)까지 가야 한다")
	assert.Equal(t, http.StatusTooManyRequests, postJSON(router, "/orders", userA),
		"user A의 두 번째 요청은 burst(1) 초과라 429여야 한다")
	assert.Equal(t, http.StatusUnprocessableEntity, postJSON(router, "/orders", userB),
		"user B는 다른 버킷이라 영향받지 않아야 한다")
}

// login과 register가 실제 라우트에서도 별도 버킷이다.
func TestRouterLoginAndRegisterRateLimitBucketsAreSeparateAtRouterLevel(t *testing.T) {
	tokenManager, err := auth.NewTokenManager("test-secret", time.Hour)
	require.NoError(t, err)
	authRL := middleware.NewRateLimiter(middleware.RateLimiterConfig{RPS: 1, Burst: 1})
	router, err := newRouter(newRateLimitTestRouterConfig(t, tokenManager, authRL, nil, nil))
	require.NoError(t, err)

	assert.Equal(t, http.StatusUnprocessableEntity, postJSON(router, "/auth/login", ""),
		"login 첫 요청은 burst 안이라 통과해 바인딩 검증(422)까지 가야 한다")
	assert.Equal(t, http.StatusTooManyRequests, postJSON(router, "/auth/login", ""),
		"login 두 번째 요청은 429여야 한다")
	assert.Equal(t, http.StatusUnprocessableEntity, postJSON(router, "/auth/register", ""),
		"register는 login과 별도 버킷이라 영향받지 않아야 한다")
}

// 주문 생성·취소와 이체가 각각 지정된 한도를 쓴다 — 주문 한도로 이체가
// 제한되지 않는다.
func TestRouterOrderRateLimitDoesNotAffectTransferRateLimit(t *testing.T) {
	tokenManager, err := auth.NewTokenManager("test-secret", time.Hour)
	require.NoError(t, err)
	orderRL := middleware.NewRateLimiter(middleware.RateLimiterConfig{RPS: 1, Burst: 1})
	transferRL := middleware.NewRateLimiter(middleware.RateLimiterConfig{RPS: 1, Burst: 1})
	router, err := newRouter(newRateLimitTestRouterConfig(t, tokenManager, nil, orderRL, transferRL))
	require.NoError(t, err)

	token := "Bearer " + bearerToken(t, tokenManager, 303)

	assert.Equal(t, http.StatusUnprocessableEntity, postJSON(router, "/orders", token))
	assert.Equal(t, http.StatusTooManyRequests, postJSON(router, "/orders", token),
		"주문 burst(1) 초과는 429여야 한다")

	assert.Equal(t, http.StatusUnprocessableEntity, postJSON(router, "/transfers/deposits", token),
		"이체는 별도 limiter라 주문 한도로 막히면 안 된다")
}
