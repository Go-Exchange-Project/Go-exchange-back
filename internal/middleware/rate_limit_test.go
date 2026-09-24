package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/auth"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A2: 버스트까지 통과 → 초과 시 429 + Retry-After ≥ 1 + RATE_LIMITED. 키가
// 다르면 서로 영향 없음. 거절이 토큰을 추가 소비하지 않는다(거절 후 기대
// 시점에 통과).
func TestRateLimiterAllowsBurstThenRejectsAndDoesNotConsumeExtraTokenOnReject(t *testing.T) {
	rl := NewRateLimiter(RateLimiterConfig{RPS: 1, Burst: 3})
	current := time.Unix(1000, 0)
	rl.now = func() time.Time { return current }

	for i := 0; i < 3; i++ {
		allowed, retryAfter := rl.Allow("k")
		require.True(t, allowed, "burst request %d should pass", i)
		assert.Equal(t, 0, retryAfter)
	}

	allowed, retryAfter := rl.Allow("k")
	assert.False(t, allowed)
	assert.GreaterOrEqual(t, retryAfter, 1)

	allowedOther, _ := rl.Allow("other-key")
	assert.True(t, allowedOther, "다른 키는 영향받지 않아야 한다")

	current = current.Add(time.Duration(retryAfter) * time.Second)
	allowedAfterWait, _ := rl.Allow("k")
	assert.True(t, allowedAfterWait, "거절이 토큰을 추가로 소비했다면 대기 후에도 통과하지 못한다")
}

// A3: 결정적 경쟁 — 요청이 map lock·entry lock을 모두 쥔 시점과 cleanup의
// 삭제 시도를 채널 장벽으로 맞물리게 해, cleanup이 그 창에서 끼어들 수
// 없음을 증명한다. 같은 키에 limiter가 둘 생기면 burst가 다시 차 통과할
// 것이므로, 그 부재로 중복 생성이 없었음을 확인한다.
func TestRateLimiterCleanupCannotRaceWithInFlightRequest(t *testing.T) {
	rl := NewRateLimiter(RateLimiterConfig{RPS: 1, Burst: 1})
	reached := make(chan struct{})
	proceed := make(chan struct{})
	rl.testHookAfterEntryLock = func() {
		close(reached)
		<-proceed
	}

	allowDone := make(chan struct{})
	var allowed bool
	go func() {
		allowed, _ = rl.Allow("k")
		close(allowDone)
	}()

	<-reached // Allow가 map lock과 entry lock을 모두 쥔 상태다.

	// 락 순서 검출: 정상 구현은 entry lock을 쥔 동안 map lock을 계속 쥔다
	// (map lock → entry lock → map unlock). `map unlock → entry lock` 변이는
	// 이 시점에 이미 map을 놓았으므로 TryLock이 성공한다.
	if rl.mu.TryLock() {
		rl.mu.Unlock()
		t.Fatal("entry lock을 쥔 요청이 map lock을 이미 놓았다 — 금지된 락 순서(map unlock → entry lock)")
	}

	cleanupDone := make(chan struct{})
	go func() {
		rl.Cleanup(10 * time.Minute)
		close(cleanupDone)
	}()

	select {
	case <-cleanupDone:
		t.Fatal("cleanup이 진행 중인 요청의 map lock을 우회해 완료됐다")
	case <-time.After(100 * time.Millisecond):
		// 기대: cleanup은 map lock에서 막혀 있다.
	}

	close(proceed)
	<-allowDone
	<-cleanupDone
	rl.testHookAfterEntryLock = nil

	require.True(t, allowed)

	rl.mu.Lock()
	entryCount := len(rl.entries)
	rl.mu.Unlock()
	assert.Equal(t, 1, entryCount, "같은 키에 entry가 둘 생기면 안 된다")

	allowedAgain, _ := rl.Allow("k")
	assert.False(t, allowedAgain, "entry가 중복 생성됐다면 burst가 다시 차 통과했을 것이다")
}

// A4: login과 register가 서로 다른 버킷을 쓴다 — 같은 RateLimiter, 같은 IP라도
// 버킷 접두사가 다르면 서로 영향이 없다.
func TestRateLimitByIPUsesSeparateBucketsPerRoutePrefix(t *testing.T) {
	rl := NewRateLimiter(RateLimiterConfig{RPS: 1, Burst: 1})
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("boundBucket", c.Request.URL.Path) })
	router.POST("/auth/login", RateLimitByIP(rl, "login"), func(c *gin.Context) { c.Status(http.StatusOK) })
	router.POST("/auth/register", RateLimitByIP(rl, "register"), func(c *gin.Context) { c.Status(http.StatusOK) })

	loginRecorder1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	req1.RemoteAddr = "203.0.113.1:5555"
	router.ServeHTTP(loginRecorder1, req1)
	assert.Equal(t, http.StatusOK, loginRecorder1.Code, "login 첫 요청은 통과해야 한다")

	loginRecorder2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	req2.RemoteAddr = "203.0.113.1:5555"
	router.ServeHTTP(loginRecorder2, req2)
	assert.Equal(t, http.StatusTooManyRequests, loginRecorder2.Code, "login burst(1) 초과는 429여야 한다")

	registerRecorder := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodPost, "/auth/register", nil)
	req3.RemoteAddr = "203.0.113.1:5555"
	router.ServeHTTP(registerRecorder, req3)
	assert.Equal(t, http.StatusOK, registerRecorder.Code, "register는 login과 별도 버킷이라 영향받지 않아야 한다")
}

// A5: 프록시 신뢰 — 신뢰 목록이 비면 X-Forwarded-For를 무시하고 RemoteAddr
// 기준으로만 판정한다. 신뢰 CIDR을 설정했을 때만 헤더가 반영된다.
func TestRateLimitByIPIgnoresForwardedForWithoutTrustedProxies(t *testing.T) {
	rl := NewRateLimiter(RateLimiterConfig{RPS: 1, Burst: 1})
	gin.SetMode(gin.TestMode)

	router := gin.New()
	require.NoError(t, router.SetTrustedProxies(nil))
	router.POST("/auth/login", RateLimitByIP(rl, "login"), func(c *gin.Context) { c.Status(http.StatusOK) })

	first := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	req1.RemoteAddr = "198.51.100.7:1111"
	req1.Header.Set("X-Forwarded-For", "1.1.1.1")
	router.ServeHTTP(first, req1)
	assert.Equal(t, http.StatusOK, first.Code)

	second := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	req2.RemoteAddr = "198.51.100.7:2222" // 같은 IP, 다른 포트
	req2.Header.Set("X-Forwarded-For", "2.2.2.2")
	router.ServeHTTP(second, req2)
	assert.Equal(t, http.StatusTooManyRequests, second.Code,
		"신뢰 목록이 비면 X-Forwarded-For가 달라도 RemoteAddr이 같으면 같은 버킷이다")
}

func TestRateLimitByIPHonorsForwardedForWhenProxyTrusted(t *testing.T) {
	rl := NewRateLimiter(RateLimiterConfig{RPS: 1, Burst: 1})
	gin.SetMode(gin.TestMode)

	router := gin.New()
	require.NoError(t, router.SetTrustedProxies([]string{"198.51.100.0/24"}))
	router.POST("/auth/login", RateLimitByIP(rl, "login"), func(c *gin.Context) { c.Status(http.StatusOK) })

	first := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	req1.RemoteAddr = "198.51.100.7:1111"
	req1.Header.Set("X-Forwarded-For", "1.1.1.1")
	router.ServeHTTP(first, req1)
	assert.Equal(t, http.StatusOK, first.Code)

	second := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	req2.RemoteAddr = "198.51.100.7:2222"
	req2.Header.Set("X-Forwarded-For", "2.2.2.2")
	router.ServeHTTP(second, req2)
	assert.Equal(t, http.StatusOK, second.Code,
		"신뢰 프록시 CIDR 안이면 X-Forwarded-For가 다르면 다른 버킷이어야 한다")
}

// A6: limiter가 비활성(nil)이면 통과한다.
func TestRateLimitMiddlewarePassesThroughWhenLimiterIsNil(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/auth/login", RateLimitByIP(nil, "login"), func(c *gin.Context) { c.Status(http.StatusOK) })

	for i := 0; i < 5; i++ {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
		req.RemoteAddr = "203.0.113.9:1234"
		router.ServeHTTP(recorder, req)
		assert.Equal(t, http.StatusOK, recorder.Code, "limiter가 nil이면 항상 통과해야 한다")
	}
}

func TestRateLimitByUserUsesUserIDFromContext(t *testing.T) {
	rl := NewRateLimiter(RateLimiterConfig{RPS: 1, Burst: 1})
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(func(c *gin.Context) {
		switch c.GetHeader("X-Test-User-ID") {
		case "42":
			c.Set(auth.UserIDContextKey, uint(42))
		case "99":
			c.Set(auth.UserIDContextKey, uint(99))
		}
	})
	router.POST("/orders", RateLimitByUser(rl), func(c *gin.Context) { c.Status(http.StatusOK) })

	recorder1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/orders", nil)
	req1.Header.Set("X-Test-User-ID", "42")
	router.ServeHTTP(recorder1, req1)
	assert.Equal(t, http.StatusOK, recorder1.Code)

	recorder2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/orders", nil)
	req2.Header.Set("X-Test-User-ID", "42")
	router.ServeHTTP(recorder2, req2)
	assert.Equal(t, http.StatusTooManyRequests, recorder2.Code, "같은 사용자는 같은 버킷을 써야 한다")

	recorder3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodPost, "/orders", nil)
	req3.Header.Set("X-Test-User-ID", "99")
	router.ServeHTTP(recorder3, req3)
	assert.Equal(t, http.StatusOK, recorder3.Code, "다른 사용자는 다른 버킷이라 영향받지 않아야 한다")
}

// RunCleanup은 ctx가 취소되면 종료된다(설계 §6.2 — 종료 시 중단). 짧은
// interval로 실제 tick이 돈 뒤에도 취소에 반응하는지 확인한다.
func TestRateLimiterRunCleanupStopsWhenContextIsCancelled(t *testing.T) {
	rl := NewRateLimiter(RateLimiterConfig{RPS: 1, Burst: 1})
	rl.Allow("k")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		rl.RunCleanup(ctx, 5*time.Millisecond, time.Nanosecond)
		close(done)
	}()

	require.Eventually(t, func() bool {
		rl.mu.Lock()
		defer rl.mu.Unlock()
		return len(rl.entries) == 0
	}, 2*time.Second, 5*time.Millisecond, "tick이 돌아 유휴 entry를 지워야 한다")

	select {
	case <-done:
		t.Fatal("ctx 취소 전에 RunCleanup이 종료됐다")
	default:
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 취소 뒤에도 RunCleanup이 종료되지 않았다")
	}
}
