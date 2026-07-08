# 매칭/정산 지연 지표 버킷 해상도 확장 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `order_pipeline_match_latency_seconds`와 `order_settlement_duration_seconds` 히스토그램이 Prometheus 기본 버킷(최대 10초)을 써서 10초 초과 지연이 전부 10초로 잘려 보이는 문제를, 버킷을 60초까지 확장해서 해결한다.

**Architecture:** `internal/metrics/metrics.go`에 `matchLatencyBuckets` 패키지 변수를 새로 두고, 두 히스토그램이 이 변수를 공유하게 한다. Go 코드 변경만 있고 Grafana/docker-compose 변경은 없다.

**Tech Stack:** `github.com/prometheus/client_golang/prometheus`.

## Global Constraints

- `order_pipeline_match_latency_seconds`와 `order_settlement_duration_seconds`는 `prometheus.DefBuckets`에 `15, 20, 30, 45, 60` 초 버킷을 추가한 동일한 슬라이스를 공유한다.
- `HTTPRequestDuration`(`http_request_duration_seconds`)은 변경하지 않는다.
- 실제 GCP 재배포, k6 재실행, 결과 문서화(`docs/benchmarks/08-...md`)는 이 계획의 범위 밖이다.

---

### Task 1: 히스토그램 버킷 확장 + 테스트

**Files:**
- Modify: `internal/metrics/metrics.go`
- Create: `internal/metrics/metrics_test.go`

**Interfaces:**
- Produces: 패키지 변수 `matchLatencyBuckets []float64` — `internal/metrics` 패키지 내부에서만 쓰인다.

- [ ] **Step 1: 실패하는 테스트 작성**

`internal/metrics/metrics_test.go` 신규 생성:

```go
package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestMatchLatencyBucketsExtendDefaultUpTo60Seconds(t *testing.T) {
	want := append(append([]float64{}, prometheus.DefBuckets...), 15, 20, 30, 45, 60)

	if len(matchLatencyBuckets) != len(want) {
		t.Fatalf("len(matchLatencyBuckets) = %d, want %d", len(matchLatencyBuckets), len(want))
	}
	for i, v := range want {
		if matchLatencyBuckets[i] != v {
			t.Fatalf("matchLatencyBuckets[%d] = %v, want %v", i, matchLatencyBuckets[i], v)
		}
	}
}
```

- [ ] **Step 2: 테스트 실행해서 실패 확인**

Run: `go test ./internal/metrics/... -run TestMatchLatencyBucketsExtendDefaultUpTo60Seconds -v`
Expected: FAIL — `matchLatencyBuckets`가 아직 정의되지 않아 컴파일 에러 (`undefined: matchLatencyBuckets`)

- [ ] **Step 3: `internal/metrics/metrics.go`에 버킷 변수 추가 및 적용**

`internal/metrics/metrics.go` 전체, 현재:

```go
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	HTTPRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total HTTP requests processed, labeled by method, path, and status code.",
	}, []string{"method", "path", "status"})

	HTTPRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request duration in seconds, labeled by method, path, and status code.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path", "status"})

	OrderPipelineMatchLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "order_pipeline_match_latency_seconds",
		Help:    "Time from order enqueue into the matching engine to completion of matching for that order.",
		Buckets: prometheus.DefBuckets,
	})

	OrderSettlementDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "order_settlement_duration_seconds",
		Help:    "Time to persist trade settlement (wallet/ledger updates) after a match event.",
		Buckets: prometheus.DefBuckets,
	})
)
```

다음과 같이 전체 교체한다:

```go
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// matchLatencyBuckets extends the Prometheus default buckets (max 10s) with
// 15/20/30/45/60s so tail latencies above 10s are not all clipped into the
// +Inf bucket, which made histogram_quantile report a flat 10s ceiling.
var matchLatencyBuckets = append(append([]float64{}, prometheus.DefBuckets...), 15, 20, 30, 45, 60)

var (
	HTTPRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total HTTP requests processed, labeled by method, path, and status code.",
	}, []string{"method", "path", "status"})

	HTTPRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request duration in seconds, labeled by method, path, and status code.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path", "status"})

	OrderPipelineMatchLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "order_pipeline_match_latency_seconds",
		Help:    "Time from order enqueue into the matching engine to completion of matching for that order.",
		Buckets: matchLatencyBuckets,
	})

	OrderSettlementDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "order_settlement_duration_seconds",
		Help:    "Time to persist trade settlement (wallet/ledger updates) after a match event.",
		Buckets: matchLatencyBuckets,
	})
)
```

- [ ] **Step 4: 테스트 실행해서 통과 확인**

Run: `go test ./internal/metrics/... -run TestMatchLatencyBucketsExtendDefaultUpTo60Seconds -v`
Expected: PASS

- [ ] **Step 5: 빌드 및 전체 테스트 회귀 확인**

```bash
go build ./...
go vet ./...
go test ./... 2>&1 | tail -40
```

Expected: 전부 에러 없이 종료, 모든 패키지 PASS.

- [ ] **Step 6: 커밋**

```bash
git add internal/metrics/metrics.go internal/metrics/metrics_test.go
git commit -m "$(cat <<'MSG'
fix: 매칭/정산 지연 지표 히스토그램 버킷을 60초까지 확장

order_pipeline_match_latency_seconds와 order_settlement_duration_seconds가
Prometheus 기본 버킷(최대 10초)을 써서 10초 초과 지연이 전부 10초로 잘려
보이던 문제를 해결한다. 15/20/30/45/60초 버킷을 추가해 진짜 꼬리 지연을
구분할 수 있게 한다.
MSG
)"
```
