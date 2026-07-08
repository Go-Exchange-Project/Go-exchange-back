# 매칭엔진 순수 TPS 벤치마크 설계

## 배경 (왜 필요한가)

사용자가 다른 거래소 프로젝트의 벤치마크 글(Rust `criterion` 기반, 순수 매칭엔진만 측정, Limit Order 113만 TPS)을 찾아보고, "이 프로젝트는 매칭엔진이 메인"이라는 관점에서 매칭엔진 성능을 우선순위로 다시 보고 싶어했다.

지금까지의 성능 측정(`docs/benchmarks/03~05`)은 전부 **API+DB+매칭+정산을 포함한 전체 스택**을 측정한 것이었다. 이번 GCP 스트레스 테스트의 "매칭엔진 TPS ≈ 55.2"는 순수 매칭 로직의 속도가 아니라, DB 트랜잭션·HTTP 오버헤드까지 다 포함된 수치라 그 블로그의 113만 TPS와 직접 비교하면 오해를 부른다(측정 대상이 다름). 이번 작업은 **순수 매칭엔진만 떼어내 측정하는 벤치마크**를 만들어, (1) 다른 프로젝트의 "엔진만" 수치와 정당하게 비교할 수 있는 기준점을 확보하고, (2) 이후 CPU 코어 핀닝 등 엔진 자체 최적화 작업의 전/후 비교 기준을 마련하는 것이다.

## 왜 이 방식을 선택했는지

Go 표준 `testing.B` 벤치마크를 그대로 쓰기로 했다 — 이미 `internal/matching/engine_bench_test.go`에 유사한 벤치마크(`BenchmarkMatch_ImmediateCross`, `BenchmarkOrderBookDepth`, `BenchmarkBulkFill`)가 있어서 같은 패턴을 확장하는 게 가장 적은 코드로 가능하다. 그 블로그가 쓴 Rust `criterion`과 정확히 같은 도구는 아니지만, 둘 다 "인메모리 마이크로벤치마크로 반복 실행 후 안정된 처리량을 재는" 같은 방법론이라 개념적으로 비교 가능하다.

TPS를 `b.ReportMetric`으로 직접 출력하기로 한 이유는, Go 기본 벤치마크 출력(ns/op)은 그 자체로는 다른 프로젝트의 "TPS" 표기와 바로 비교가 안 되기 때문이다 — 매번 수동 변환하지 않고 벤치마크 실행 결과에 바로 "X tps"가 찍히게 한다.

## 범위

- `internal/matching/engine_bench_test.go`에 벤치마크 3개를 추가한다 (기존 파일 확장, 새 파일 만들지 않음).
- CPU 코어 핀닝, `GOMAXPROCS` 튜닝 등 실제 개선 작업은 이번 스코프가 아니다 — 이번 벤치마크로 기준점을 잡은 뒤, 그 결과를 보고 별도 브레인스토밍으로 진행한다.
- 결과를 `docs/benchmarks/06-YYYY-MM-DD-matching-engine-pure-tps-benchmark.md`에 기록한다.

## 아키텍처

### 1. `BenchmarkTPS_LimitOrder`

단일 지정가 매도 주문(수량 `b.N+1`)을 미리 걸어두고, 수량 1짜리 매수 주문을 반복해서 즉시 체결시킨다 (`BenchmarkMatch_ImmediateCross`와 같은 구조). 매 반복이 트레이드 1건과 대응하므로, 반복 종료 후 `b.Elapsed()`와 `b.N`으로 TPS를 계산해 리포트한다.

```go
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
```

### 2. `BenchmarkTPS_MarketOrder`

시장가 매수 주문이 매도 벽을 소진하는 시나리오. 시장가 주문은 체결 후 오더북에 남지 않으므로(소모성), 매 반복마다 매도 주문 하나를 미리 보충한 뒤 시장가 매수로 소진시킨다.

```go
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
```

### 3. `BenchmarkTPS_MixedOrder`

지정가/시장가를 번갈아 섞은 워크로드. 짝수 반복은 `BenchmarkTPS_LimitOrder`와 같은 지정가 체결, 홀수 반복은 `BenchmarkTPS_MarketOrder`와 같은 시장가 소진을 수행한다.

### 4. TPS 계산/리포트 헬퍼

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
```

세 벤치마크 모두 마지막에 이 헬퍼를 호출해 `go test -bench`의 출력 테이블에 `ns/op`와 나란히 `tps` 컬럼이 찍히게 한다.

### 5. 결과 기록

`docs/benchmarks/06-YYYY-MM-DD-matching-engine-pure-tps-benchmark.md`에:
- `go test -bench=TPS -benchmem ./internal/matching/...` 원본 출력
- 세 시나리오의 TPS 요약 표
- **"전체 스택 TPS(05번 문서, 55.2)"와 "순수 엔진 TPS(이번 결과)"를 나란히 놓고, 측정 대상이 다르다는 점을 명시** (전체 스택 병목은 DB/네트워크에 있었지, 매칭 로직 자체가 아니었다는 04/05번 문서의 결론과 일관되게)
- 참고 비교 대상으로 그 블로그의 수치(Limit 113만, Market 11.3만, Mixed 25.15만 TPS, Rust `criterion`, 싱글스레드 1코어)를 인용하되, "다른 언어/도구/하드웨어"라는 점을 명시해 직접적인 우열 비교로 오독되지 않게 한다.

## 성공 기준

- `go test -bench=TPS ./internal/matching/...`로 세 시나리오의 TPS가 출력된다.
- 결과 문서에 "전체 스택 vs 순수 엔진"의 측정 범위 차이가 명확히 설명되어 있어, 이력서/포트폴리오에서 숫자만 보고 오해할 여지가 없다.

## 범위 밖 (Out of Scope)

- CPU 코어 핀닝(`runtime.LockOSThread`), `GOMAXPROCS` 튜닝, 실시간 스케줄링 등 실제 엔진 최적화 — 이번 벤치마크로 기준점을 잡은 뒤 별도 브레인스토밍.
- 벤치마크 결과에 따른 코드 변경 — 이번엔 측정만 한다.
