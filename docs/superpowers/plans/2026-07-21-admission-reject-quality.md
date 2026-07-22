# ④ 입장 거절 품질 구현 계획 (2차 리팩토링 · 가용성)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development(권장) 또는 superpowers:executing-plans로 태스크별 실행. Steps use checkbox (`- [ ]`) syntax.

**Goal:** 과부하 503 응답에 Retry-After 헤더(재시도 폭풍 방지)를 붙이고, 입장 거절을 stage별로 세는 셰딩 메트릭을 추가한다. 거절 동작 자체(①②의 503)는 무변경.

**Architecture:** `httpapi.WriteError`에서 503일 때 `Retry-After` 헤더를 중앙 세팅(모든 503이 자동 수령). `orders_admission_rejected_total{stage}` 카운터를 세 거절 지점(①게이트·①핸드오프·②코디네이터)에서 `Inc()`. 로직 무변경 — 헤더 + 카운트만 추가.

**Tech Stack:** Go, Gin, prometheus/client_golang.

**스펙 문서:** `docs/superpowers/specs/2026-07-21-admission-reject-quality-design.md`

## Global Constraints

- 로직 무변경 — 거절(503 반환·보상)은 이미 ①②가 수행. ④는 응답 헤더 + 메트릭만 더한다.
- 두 게이트 통합·히스테리시스는 범위 밖(스펙의 "의도적으로 하지 않는 것").
- 기존 ①②③ 테스트가 무수정 그린이어야 한다(거절 동작 불변의 증거).
- 셰딩률 실증은 ⑤(23번). **성공 기준은 "Retry-After 실림 + 메트릭 증분 + 회귀 그린"까지** — 수치 주장 금지.
- 통합 테스트: 테스트 DB + DSN(포트 55432), `-v`로 SKIP 0 확인. 커밋은 태스크 단위 commit-message 스킬(author→reviewer, 한글). Bash 실패 시 PowerShell.

---

### Task 1: Retry-After 헤더 (503 중앙화)

**Files:**
- Modify: `internal/httpapi/response.go`
- Test: `internal/httpapi/response_test.go`(없으면 생성)

**Interfaces:**
- 변경 없음(내부). `WriteError`/`AbortWithError` 시그니처 그대로 — 503일 때만 헤더 부가.

- [ ] **Step 1: 실패 테스트** — `internal/httpapi/response_test.go`:

```go
package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

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
```

Run: `go test ./internal/httpapi/... -run TestWriteError -v` → FAIL(Retry-After 헤더 없음).

- [ ] **Step 2: 구현** — `response.go`에 헬퍼 + `WriteError`/`AbortWithError`에 적용:

```go
// retryAfterSeconds: 503(과부하) 응답의 Retry-After 헤더 값(초). 클라이언트 백오프
// 힌트로 즉시 재시도(retry storm)를 막아 과부하 회복을 돕는다.
const retryAfterSeconds = "1"

func setRetryAfterForOverload(c *gin.Context, status int) {
	if status == http.StatusServiceUnavailable {
		c.Header("Retry-After", retryAfterSeconds)
	}
}
```

`WriteError`·`AbortWithError`의 `c.JSON`/`c.AbortWithStatusJSON` **앞에** `setRetryAfterForOverload(c, status)` 호출 추가(헤더는 바디 쓰기 전에 세팅해야 함).

- [ ] **Step 3: 통과 + 회귀** — Run: `go test ./internal/httpapi/... -count=1` → PASS. 기존 httpapi 테스트(있으면) 무영향.
- [ ] **Step 4: Commit** — 초안: `feat(httpapi): 503 응답에 Retry-After 헤더 추가 (2차 ④)`

---

### Task 2: 셰딩 메트릭 + 세 거절 지점 증분

**Files:**
- Modify: `internal/metrics/metrics.go` (+ 테스트 `internal/metrics/admission_test.go`)
- Modify: `internal/service/order_service.go` (metrics import + 두 지점 Inc)
- Modify: `internal/service/hold_coordinator.go` (한 지점 Inc)
- Test: 기존 거절 통합 테스트에 델타 단언 추가

**Interfaces:**
- Produces: `metrics.OrdersAdmissionRejectedTotal *prometheus.CounterVec`(라벨 `stage`). stage ∈ {`engine_gate`, `engine_handoff`, `coordinator`}.

- [ ] **Step 1: 메트릭 실패 테스트** — `internal/metrics/admission_test.go`:

```go
package metrics_test

import (
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

func TestOrdersAdmissionRejectedTotalIncrementsPerStage(t *testing.T) {
	before := testutil.ToFloat64(metrics.OrdersAdmissionRejectedTotal.WithLabelValues("engine_gate"))
	metrics.OrdersAdmissionRejectedTotal.WithLabelValues("engine_gate").Inc()
	after := testutil.ToFloat64(metrics.OrdersAdmissionRejectedTotal.WithLabelValues("engine_gate"))
	assert.Equal(t, before+1, after)
}
```

Run: `go test ./internal/metrics/... -run TestOrdersAdmissionRejected -v` → FAIL(undefined).

- [ ] **Step 2: 메트릭 추가** — `metrics.go`의 `var (...)` 블록에:

```go
	OrdersAdmissionRejectedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "orders_admission_rejected_total",
		Help: "Total orders fast-rejected by admission control (503), labeled by shedding stage.",
	}, []string{"stage"})
```

Run: `go test ./internal/metrics/... -count=1` → PASS.

- [ ] **Step 3: 세 거절 지점 증분** — 각 지점이 자기 stage를 `Inc()`:

`order_service.go` — import에 `"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"` 추가. `CreateOrder`의 두 거절 지점:

```go
	// ① 입장 게이트
	if s.MatchingEngine != nil && !s.MatchingEngine.IsIntakeAdmissible(order.CoinSymbol) {
		metrics.OrdersAdmissionRejectedTotal.WithLabelValues("engine_gate").Inc()
		return nil, NewUnavailableErrorf("order intake is saturated, please retry shortly")
	}
```
```go
		// ① 바운디드 핸드오프 타임아웃 → 보상
		if !submitted {
			metrics.OrdersAdmissionRejectedTotal.WithLabelValues("engine_handoff").Inc()
			if rerr := s.rejectAcceptedOrder(order); rerr != nil {
				return nil, fmt.Errorf("order intake saturated and hold release failed for order %d: %w", order.ID, rerr)
			}
			return nil, NewUnavailableErrorf("order intake is saturated, please retry shortly")
		}
```

`hold_coordinator.go` — `Submit`의 입력 만석 분기:

```go
	select {
	case c.input <- req:
	default:
		metrics.OrdersAdmissionRejectedTotal.WithLabelValues("coordinator").Inc()
		return nil, NewUnavailableErrorf("order intake is saturated, please retry shortly")
	}
```

- [ ] **Step 4: 거절 테스트에 델타 단언 추가** — 기존 통합 테스트를 확장(카운터는 전역이라 before/after 델타로):
  - `TestIntegrationCreateOrderFastRejectsWhenIntakeSaturated`(①게이트)에 `engine_gate` +1 단언.
  - `TestIntegrationCreateOrderCompensatesWhenHandoffTimesOut`(①핸드오프)에 `engine_handoff` +1 단언.
  - `TestHoldCoordinatorSubmitReturnsUnavailableWhenInputFull`(②코디네이터)에 `coordinator` +1 단언.

  패턴:
```go
	before := testutil.ToFloat64(metrics.OrdersAdmissionRejectedTotal.WithLabelValues("engine_gate"))
	// ... 기존 거절 시나리오 실행 ...
	after := testutil.ToFloat64(metrics.OrdersAdmissionRejectedTotal.WithLabelValues("engine_gate"))
	assert.Equal(t, before+1, after)
```

- [ ] **Step 5: 통과 + 회귀** — `go build ./...`; Run: `go test ./internal/service/... ./internal/metrics/... -count=1` → PASS. 기존 ①②③ 테스트 무수정 그린(거절 동작 불변). `go test ./internal/service/... -race -count=1` PASS.
- [ ] **Step 6: Commit** — 초안: `feat(order): 입장 거절 셰딩 메트릭 orders_admission_rejected_total 추가 (2차 ④)`

---

### Task 3: 전체 검증 + 완료 문서 + README

**Files:**
- Create: `docs/refactor/14_2차④_입장_거절_품질_완료.md`
- Modify: `docs/refactor/README.md`(2차 ④ ✅)

- [ ] **Step 1: 전체 검증** — `go build ./...` + `go vet` + `go test ./... -count=1`(통합 SKIP 0) + `go test ./internal/service/... ./cmd/... -race -count=1` → 전부 PASS. 기존 ①②③·정산·부트스트랩 통합 무수정 그린.
- [ ] **Step 2: 완료 문서** — `14_2차④_입장_거절_품질_완료.md`: 왜(하드 보장은 ①②가 delivered, ④는 거절 품질 — Retry-After 부재로 retry storm, 셰딩 관측 부재) / 어떻게(WriteError 503 헤더 중앙화, stage 3종 카운터, 통합·히스테리시스 의도적 제외) / 결과(헤더·메트릭 테스트, 회귀 그린, **셰딩률 실증은 ⑤/23번 병기 — 수치 주장 금지**).
- [ ] **Step 3: README** — 2차 표 ④ 🔨→✅ + 완료 문서 링크.
- [ ] **Step 4: Commit + 푸시 + CI** — author→reviewer, `gh run watch` 그린.

---

## 다음 (범위 밖)

⑤(23번 실증 — 스파이크에서 취소 실패율·주문 접수 p95·`orders_admission_rejected_total{stage}` 셰딩 분포를 ①②③④ 종합으로 측정). 이로써 2차 리팩토링(가용성 100%)의 코드가 모두 완성되고, 남는 것은 GCP 실측뿐. 히스테리시스는 ⑤에서 flapping이 관측되면 조건부 승격.
