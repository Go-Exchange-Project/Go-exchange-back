package middleware

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/auth"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/httpapi"
	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

type RateLimiterConfig struct {
	RPS   int
	Burst int
}

type limiterEntry struct {
	mu       sync.Mutex
	limiter  *rate.Limiter
	lastUsed time.Time
}

// RateLimiter는 키별 토큰 버킷 저장소다(설계 §6.2). 저장소 자체는 rl.mu로,
// 개별 버킷의 토큰 연산·lastUsed 갱신은 그 entry의 mu로 보호한다. 락 획득
// 순서는 항상 map lock → entry lock이다(반대 순서는 두지 않는다) — 요청이
// entry를 찾은 뒤 entry lock을 얻기 전에 map lock을 놓으면, 그 사이 cleanup이
// entry를 지우고 다음 요청이 같은 키로 새 entry를 만들어 한 키에 limiter가
// 둘 생길 수 있다.
type RateLimiter struct {
	mu      sync.Mutex
	entries map[string]*limiterEntry
	rate    rate.Limit
	burst   int
	now     func() time.Time

	// testHookAfterEntryLock은 nil이 아니면 Allow가 map lock과 entry lock을
	// 모두 쥔 직후(아직 map lock을 놓기 전) 호출된다. cleanup과의 락 순서를
	// 결정적으로 검증하는 테스트 전용 훅이다.
	testHookAfterEntryLock func()
}

func NewRateLimiter(cfg RateLimiterConfig) *RateLimiter {
	return &RateLimiter{
		entries: make(map[string]*limiterEntry),
		rate:    rate.Limit(cfg.RPS),
		burst:   cfg.Burst,
		now:     time.Now,
	}
}

// Allow는 key에 대한 요청 하나를 허용할지 판정한다. 거절이면 재시도까지
// 기다려야 할 초(올림, 최소 1)를 함께 돌려주며, 거절 자체는 토큰을 추가로
// 소비하지 않는다(Reserve로 판정한 뒤 거절 시 Cancel로 되돌린다).
func (rl *RateLimiter) Allow(key string) (allowed bool, retryAfterSeconds int) {
	now := rl.now()

	rl.mu.Lock()
	entry, ok := rl.entries[key]
	if !ok {
		entry = &limiterEntry{limiter: rate.NewLimiter(rl.rate, rl.burst)}
		rl.entries[key] = entry
	}
	entry.mu.Lock()
	if rl.testHookAfterEntryLock != nil {
		rl.testHookAfterEntryLock()
	}
	rl.mu.Unlock()
	defer entry.mu.Unlock()

	reservation := entry.limiter.ReserveN(now, 1)
	delay := reservation.DelayFrom(now)
	entry.lastUsed = now
	if delay <= 0 {
		return true, 0
	}
	reservation.CancelAt(now)
	seconds := int(delay / time.Second)
	if delay%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	return false, seconds
}

// Cleanup은 마지막 사용 후 idleAfter 이상 유휴인 버킷을 지운다. 요청 경로와
// 같은 락 순서(map lock → entry lock)를 따라 entry별로 재확인한 뒤 지운다.
func (rl *RateLimiter) Cleanup(idleAfter time.Duration) {
	now := rl.now()
	rl.mu.Lock()
	defer rl.mu.Unlock()
	for key, entry := range rl.entries {
		entry.mu.Lock()
		stale := now.Sub(entry.lastUsed) >= idleAfter
		entry.mu.Unlock()
		if stale {
			delete(rl.entries, key)
		}
	}
}

// RunCleanup은 ctx가 끝날 때까지 주기적으로 Cleanup을 돈다(설계 §6.2 — 종료
// 시 중단된다).
func (rl *RateLimiter) RunCleanup(ctx context.Context, interval, idleAfter time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rl.Cleanup(idleAfter)
		}
	}
}

func rejectRateLimited(c *gin.Context, retryAfterSeconds int) {
	c.Header("Retry-After", strconv.Itoa(retryAfterSeconds))
	httpapi.AbortWithError(c, http.StatusTooManyRequests, httpapi.CodeRateLimited, "rate limit exceeded, retry later")
}

// RateLimitByIP는 클라이언트 IP(gin의 ClientIP — SetTrustedProxies 설정을
// 그대로 따른다) 기준으로 제한한다. bucketPrefix가 다르면 같은 IP라도 서로
// 다른 버킷을 쓴다(예: login과 register). rl이 nil이면 통과시킨다(A6, 설계
// §6.2 — limiter 비활성).
func RateLimitByIP(rl *RateLimiter, bucketPrefix string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rl == nil {
			c.Next()
			return
		}
		key := bucketPrefix + ":" + c.ClientIP()
		if allowed, retryAfter := rl.Allow(key); !allowed {
			rejectRateLimited(c, retryAfter)
			return
		}
		c.Next()
	}
}

// RateLimitByUser는 인증된 사용자 ID 기준으로 제한한다. AuthRequired 뒤에
// 배치해야 한다(사용자 ID가 컨텍스트에 있어야 키가 나온다). rl이 nil이면
// 통과시킨다.
func RateLimitByUser(rl *RateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rl == nil {
			c.Next()
			return
		}
		raw, ok := c.Get(auth.UserIDContextKey)
		userID, isUint := raw.(uint)
		if !ok || !isUint {
			c.Next()
			return
		}
		key := strconv.FormatUint(uint64(userID), 10)
		if allowed, retryAfter := rl.Allow(key); !allowed {
			rejectRateLimited(c, retryAfter)
			return
		}
		c.Next()
	}
}
