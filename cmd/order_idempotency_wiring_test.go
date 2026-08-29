package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/testdb"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 배선이 없으면 monitor 구현이 아무리 옳아도 운영에서 영원히 실행되지 않는다.
// main()은 실행해 볼 수 없으므로 호출 자체를 소스로 고정한다.
func TestMainStartsOrderIdempotencyMonitor(t *testing.T) {
	source, err := os.ReadFile("main.go")
	require.NoError(t, err)

	assert.Contains(t, string(source), "startOrderIdempotencyMonitor(backgroundCtx, config.DB)",
		"main()이 stale PENDING monitor를 기동하지 않는다")
}

// 핸들러가 Idempotency-Key를 필수로 요구하는데 CORS가 그 헤더를 허용하지 않으면
// preflight 단계에서 막혀 브라우저 주문이 전부 실패한다. 서버 로그에는 아무것도 남지
// 않아 원인을 찾기 어렵다(E2E에서 실제로 겪었다).
func TestCORSAllowsTheHeadersOrderCreationRequires(t *testing.T) {
	assert.Contains(t, corsAllowedHeaders, "Idempotency-Key",
		"주문 생성이 요구하는 헤더가 CORS 허용 목록에 없다 — 브라우저에서 주문이 되지 않는다")
	assert.Contains(t, corsAllowedHeaders, "Authorization")
	assert.Contains(t, corsAllowedHeaders, "Content-Type")
}

// 배선이 실제 DB까지 이어지는지 본다 — repository 생성, goroutine 기동, gauge 반영이
// 한 번에 깨진다.
func TestIntegrationStartOrderIdempotencyMonitorPublishesGauge(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)

	userID := uint(time.Now().UnixNano() % 1_000_000_000)
	stale := &model.OrderIdempotencyKey{
		UserID:             userID,
		IdempotencyKey:     "wiring",
		Fingerprint:        "fp-wiring",
		FingerprintVersion: 1,
		Outcome:            model.OrderIdempotencyOutcomePending,
	}
	require.NoError(t, db.Create(stale).Error)
	t.Cleanup(func() {
		require.NoError(t, db.Where("user_id = ?", userID).Delete(&model.OrderIdempotencyKey{}).Error)
	})

	// 기본 임계(5분)보다 오래된 PENDING이어야 gauge에 잡힌다.
	require.NoError(t, db.Model(&model.OrderIdempotencyKey{}).Where("id = ?", stale.ID).
		Update("updated_at", time.Now().Add(-time.Hour)).Error)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	monitor := startOrderIdempotencyMonitor(ctx, db)

	require.Eventually(t, func() bool {
		return testutil.ToFloat64(metrics.OrderIdempotencyStalePending) >= 1
	}, 5*time.Second, 10*time.Millisecond, "배선된 monitor가 gauge를 채우지 않았다")
	assert.Zero(t, monitor.ErrorCount(), "실제 DB 조회가 실패했다")
}
