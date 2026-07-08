# 매칭엔진 순수 TPS 벤치마크 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 다른 거래소 프로젝트의 "순수 매칭엔진" 벤치마크(API/DB 제외, Rust 기준 100만+ TPS)와 정당하게 비교할 수 있는 기준점을 만들기 위해, GoExchange 매칭엔진만 떼어내 측정하는 Go 벤치마크 3개(지정가/시장가/혼합)를 추가한다.

**Architecture:** 기존 `internal/matching/engine_bench_test.go`(이미 `BenchmarkMatch_ImmediateCross`, `BenchmarkOrderBookDepth`, `BenchmarkBulkFill`이 있음)에 벤치마크 3개와 TPS 계산 헬퍼를 추가한다. 새 파일을 만들지 않는다.

**Tech Stack:** Go 표준 `testing.B`(`b.ReportMetric`), 기존 `matching` 패키지의 `testOrder` 헬퍼와 `drainEngineEvents` 패턴.

## Global Constraints

- 새 파일을 만들지 않는다 — `internal/matching/engine_bench_test.go`를 확장한다.
- 각 벤치마크는 `b.ReportMetric(tps, "tps")`로 TPS를 직접 리포트한다.
- CPU 코어 핀닝 등 실제 개선 작업은 이번 스코프가 아니다.
- 실제 벤치마크 실행 및 `docs/benchmarks/06-...md` 결과 문서화는 이 계획의 범위 밖이다 — 코드 작성까지만 다루고, 실제 실행/기록은 사용자와 함께 직접 진행한다.

---

### Task 1: TPS 벤치마크 3개 추가

**Files:**
- Modify: `internal/matching/engine_bench_test.go`

**Interfaces:**
- Produces: `BenchmarkTPS_LimitOrder`, `BenchmarkTPS_MarketOrder`, `BenchmarkTPS_MixedOrder`, `reportTPS(b *testing.B)` — 실행 시 사용할 정확한 이름(`go test -bench=TPS`로 3개 모두 매칭됨).

- [ ] **Step 1: import 추가**

`internal/matching/engine_bench_test.go`의 import 블록, 현재:

```go
import (
	"fmt"
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
)
```

다음과 같이 수정한다:

```go
import (
	"fmt"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
)
```

- [ ] **Step 2: TPS 리포트 헬퍼와 벤치마크 3개를 파일 끝에 추가**

파일 끝(`BenchmarkBulkFill` 다음)에 추가:

```go
func reportTPS(b *testing.B) {
	b.Helper()
	elapsed := b.Elapsed()
	if elapsed <= 0 || b.N == 0 {
		return
	}
	tps := float64(b.N) / elapsed.Seconds()
	b.ReportMetric(tps, "tps")
}

func BenchmarkTPS_LimitOrder(b *testing.B) {
	me := NewMatchingEngine()
	me.Match(testOrder(1, "BTC", model.OrderSideSell, 50000, int64(b.N)+1))

	done := make(chan struct{})
	go drainEngineEvents(me, done)
	defer close(done)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		me.Match(testOrder(uint(i+2), "BTC", model.OrderSideBuy, 50000, 1))
	}
	b.StopTimer()
	reportTPS(b)
}

func BenchmarkTPS_MarketOrder(b *testing.B) {
	me := NewMatchingEngine()

	done := make(chan struct{})
	go drainEngineEvents(me, done)
	defer close(done)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		orderID := uint(200000 + i*2)
		me.Match(testOrder(orderID, "BTC", model.OrderSideSell, 50000, 1))
		me.Match(&Order{
			ID:          orderID + 1,
			CoinSymbol:  "BTC",
			Side:        model.OrderSideBuy,
			OrderType:   model.OrderTypeMarket,
			QuoteAmount: decimal.NewFromInt(50000),
			CreatedAt:   time.Now(),
		})
	}
	b.StopTimer()
	reportTPS(b)
}

func BenchmarkTPS_MixedOrder(b *testing.B) {
	me := NewMatchingEngine()

	done := make(chan struct{})
	go drainEngineEvents(me, done)
	defer close(done)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		orderID := uint(400000 + i*3)
		me.Match(testOrder(orderID, "BTC", model.OrderSideSell, 50000, 1))
		if i%2 == 0 {
			me.Match(testOrder(orderID+1, "BTC", model.OrderSideBuy, 50000, 1))
		} else {
			me.Match(&Order{
				ID:          orderID + 1,
				CoinSymbol:  "BTC",
				Side:        model.OrderSideBuy,
				OrderType:   model.OrderTypeMarket,
				QuoteAmount: decimal.NewFromInt(50000),
				CreatedAt:   time.Now(),
			})
		}
	}
	b.StopTimer()
	reportTPS(b)
}
```

- [ ] **Step 3: 빌드 확인**

```bash
go build ./...
go vet ./...
```

Expected: 둘 다 에러 없이 종료.

- [ ] **Step 4: 벤치마크가 실제로 실행되고 TPS를 리포트하는지 짧게 확인**

```bash
go test -bench=TPS -benchtime=200x -run=^$ ./internal/matching/...
```

Expected: 세 벤치마크(`BenchmarkTPS_LimitOrder`, `BenchmarkTPS_MarketOrder`, `BenchmarkTPS_MixedOrder`) 각각 `ns/op` 옆에 `tps` 값이 출력되고, `PASS`로 종료. (`-benchtime=200x`는 반복 횟수를 200회로 고정해 빠르게 동작 여부만 확인하는 것 — 실제 측정 시에는 이 플래그 없이 기본 시간 기반으로 실행한다.)

- [ ] **Step 5: 기존 테스트 회귀 확인**

```bash
go test ./... 2>&1 | tail -40
```

Expected: 모든 패키지 PASS (기존 `internal/matching` 테스트 포함 — 새 코드는 `_test.go` 파일 안에만 있으므로 프로덕션 코드에 영향 없음).

- [ ] **Step 6: 커밋**

```bash
git add internal/matching/engine_bench_test.go
git commit -m "$(cat <<'MSG'
test: 순수 매칭엔진 TPS 벤치마크 3개 추가

다른 거래소 프로젝트의 순수 엔진 벤치마크와 비교 가능한 기준점을 만들기
위해, API/DB 없이 매칭 로직만 반복 실행하는 지정가/시장가/혼합 시나리오
벤치마크를 추가한다. b.ReportMetric으로 tps를 직접 리포트한다.
MSG
)"
```
