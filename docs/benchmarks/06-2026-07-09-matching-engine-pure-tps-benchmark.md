# 6번째 테스트 (2026-07-09): 순수 매칭엔진 TPS 벤치마크

## 커밋 해시

`b025055` (test(matching): 지정가/시장가/혼합 TPS 벤치마크 추가)

## 왜 이 테스트를 했는지

다른 거래소 프로젝트의 벤치마크 글(Rust `criterion` 기반, 순수 매칭엔진만 측정, Limit Order 113만 TPS)을 찾아본 뒤, "이 프로젝트는 매칭엔진이 메인"이라는 관점에서 매칭엔진 성능을 재조명하고 싶어했다. 지금까지의 GCP 스트레스 테스트(`03~05`번 문서)는 전부 API+DB+매칭+정산을 포함한 **전체 스택**을 측정한 것이라, 순수 매칭 로직만 뗀 다른 프로젝트의 수치와 직접 비교하면 오해를 부른다(그 프로젝트가 100만 TPS인데 우리는 55 TPS라서 "느리다"고 오해하기 쉽지만, 애초에 재는 대상이 다르다). 이번 테스트는 GoExchange 매칭엔진도 **API/DB 없이 순수 엔진만** 떼어내 측정해서, 정당하게 비교 가능한 기준점을 만들었다.

## 왜 이 방식을 선택했는지

`docs/superpowers/specs/2026-07-09-matching-engine-tps-benchmark-design.md`에서 결정한 대로, Go 표준 `testing.B` 벤치마크를 그대로 썼다. 이미 `internal/matching/engine_bench_test.go`에 유사한 마이크로벤치마크가 있어 같은 패턴을 확장하는 게 가장 적은 코드로 가능했고, `b.ReportMetric(tps, "tps")`로 TPS를 직접 출력하게 해서 매번 ns/op를 수동으로 환산할 필요가 없게 했다.

## 실행한 정확한 커맨드

```bash
go test -bench=TPS -benchmem -run=^$ ./internal/matching/...
```

## 환경

로컬 개발 머신에서 실행 (이 벤치마크는 API/DB/네트워크가 전혀 없는 순수 인메모리 연산이라, GCP 인스턴스가 아니어도 측정 방법론상 문제없다 — `01-2026-07-05-matching-engine-benchmarks.md`도 같은 방식으로 로컬에서 측정했다):

- CPU: 13th Gen Intel(R) Core(TM) i7-1360P
- OS: Windows
- Go: go.mod 기준 1.25.7

## 원본 출력

```
goos: windows
goarch: amd64
pkg: github.com/Go-Exchange-Project/Go-exchange-back/internal/matching
cpu: 13th Gen Intel(R) Core(TM) i7-1360P
BenchmarkTPS_LimitOrder-16     	  559630	      2150 ns/op	    465036 tps	    1040 B/op	      30 allocs/op
BenchmarkTPS_MarketOrder-16    	  207771	      6100 ns/op	    163926 tps	    3389 B/op	      89 allocs/op
BenchmarkTPS_MixedOrder-16     	  274785	      4614 ns/op	    216731 tps	    2528 B/op	      67 allocs/op
PASS
ok  	github.com/Go-Exchange-Project/Go-exchange-back/internal/matching	5.145s
```

## 요약 테이블

| 시나리오 | ns/op | **TPS** | B/op | allocs/op |
|---|---|---|---|---|
| 지정가(Limit) 즉시체결 | 2,150 | **465,036** | 1,040 | 30 |
| 시장가(Market) 매수 | 6,100 | **163,926** | 3,389 | 89 |
| 혼합(Mixed, 지정가+시장가 50:50) | 4,614 | **216,731** | 2,528 | 67 |

## 세 시나리오 간 비교: 전체 스택 vs 순수 엔진 vs 참고(타 프로젝트)

| | 측정 대상 | Limit/지정가 | Market/시장가 | Mixed/혼합 |
|---|---|---|---|---|
| **GoExchange (05번 문서)** | API+DB+매칭+정산 전체 스택 | — | — | **55.2 TPS** (체결 기준) |
| **GoExchange (이번, 06번)** | 순수 매칭엔진만 | **465,036 TPS** | **163,926 TPS** | **216,731 TPS** |
| 참고: 타 프로젝트 블로그 (Rust, `criterion`) | 순수 매칭엔진만, 싱글스레드 1코어 | 1,136,000 TPS | 113,000 TPS | 251,500 TPS |

**중요:** 위 세 줄 중 첫 번째 행(전체 스택 55.2 TPS)과 두 번째/세 번째 행(순수 엔진 수십만 TPS)은 **완전히 다른 것을 잰 것**이다. 순수 엔진만 떼면 GoExchange도 초당 수십만 건을 처리할 수 있다는 게 이번 테스트로 확인됐다 — `03~05`번 문서에서 찾은 병목(DB 커넥션 풀 미설정, CPU 스케줄링 경쟁)은 전부 매칭 로직 자체가 아니라 그걸 감싸는 API+DB+인프라 레이어에 있었다는 결론과 정확히 일치한다.

## 해석

1. **매칭 로직 자체는 결코 느리지 않다.** 지정가 체결 기준 초당 46.5만 건 — 이는 `01-2026-07-05-matching-engine-benchmarks.md`에서 확인했던 마이크로초 단위 처리 속도와 일관된다.
2. **시장가 주문이 지정가보다 약 2.8배 느리다(163,926 vs 465,036 TPS).** 시장가 매칭(`matchMarketBuy`)이 매 반복마다 오더북 순회와 가격 나눗셈(`QuoteAmount.Div(price)`)을 더 하고, 벤치마크 자체도 매 반복 매도 주문을 새로 보충해야 해서 그만큼 할당(alloc)도 더 많다(89 vs 30 allocs/op).
3. **타 프로젝트(Rust, 싱글스레드) 대비:** Limit Order 기준 우리(46.5만)가 그쪽(113.6만)의 약 41% 수준이다. 언어(Go vs Rust), 벤치마크 방법론(testing.B vs criterion), 하드웨어가 전부 다르므로 이 격차를 "우리가 못한다"는 결론으로 바로 연결하긴 어렵지만, Go GC/할당 오버헤드가 존재하는 건 사실이라 다음 최적화 후보로 참고할 만하다.
4. **다음 단계(이번 스코프 밖):** 그 블로그가 언급한 CPU 코어 핀닝/실시간 스케줄링 같은 최적화를 실제로 적용했을 때 이 벤치마크 수치가 얼마나 움직이는지, 별도 브레인스토밍으로 진행한다.

## 범위 밖 (Out of Scope)

- CPU 코어 핀닝, `GOMAXPROCS` 튜닝 등 실제 엔진 최적화 — 이번 기준점을 보고 별도 브레인스토밍으로 진행.
- Rust/`criterion` 프로젝트와의 직접적인 성능 우열 비교 — 측정 도구/언어/하드웨어가 달라 참고 수치로만 취급.
