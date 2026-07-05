# 매칭엔진 벤치마크 결과 — 2026-07-05

**커밋:** `24f8cb2` (refactor: extract sell-wall helper and document flat depth benchmark)
**패키지:** `internal/matching`
**실행 커맨드:** `go test -bench=. -benchmem -run=^$ .`
**환경:** Windows, 13th Gen Intel(R) Core(TM) i7-1360P

## 원본 출력

```
goos: windows
goarch: amd64
pkg: github.com/Go-Exchange-Project/Go-exchange-back/internal/matching
cpu: 13th Gen Intel(R) Core(TM) i7-1360P
BenchmarkMatch_ImmediateCross-16              768595       1576 ns/op    1024 B/op    30 allocs/op
BenchmarkOrderBookDepth/depth=100-16          499662       2646 ns/op    1633 B/op    46 allocs/op
BenchmarkOrderBookDepth/depth=1000-16         403149       2723 ns/op    1633 B/op    46 allocs/op
BenchmarkOrderBookDepth/depth=10000-16        418250       2933 ns/op    1632 B/op    46 allocs/op
BenchmarkBulkFill-16                            9559     132133 ns/op   64515 B/op  1812 allocs/op
PASS
ok      github.com/Go-Exchange-Project/Go-exchange-back/internal/matching    11.714s
```

## 요약 테이블

| 벤치마크 | 반복 횟수 | ns/op | B/op | allocs/op | 측정 대상 |
|---|---|---|---|---|---|
| `BenchmarkMatch_ImmediateCross` | 768,595 | 1,576 | 1,024 | 30 | 대량 유동성 대비 단건 즉시 체결 비용 (순수 매칭 로직 기준선) |
| `BenchmarkOrderBookDepth/depth=100` | 499,662 | 2,646 | 1,633 | 46 | 오더북 깊이 100단에서 최우선가 체결+보충 비용 |
| `BenchmarkOrderBookDepth/depth=1000` | 403,149 | 2,723 | 1,633 | 46 | 오더북 깊이 1,000단 |
| `BenchmarkOrderBookDepth/depth=10000` | 418,250 | 2,933 | 1,632 | 46 | 오더북 깊이 10,000단 |
| `BenchmarkBulkFill` | 9,559 | 132,133 | 64,515 | 1,812 | 벽 주문(100단계 매도 물량을 통째로 쓸어담는 매수 주문) 1건 처리 비용 |

## 해석

- **깊이별 벤치마크는 의도적으로 평평함**: `depth=100/1000/10000` 전부 ns/op가 2,600~2,900대로 거의 변화 없음. 타이머 안쪽 루프는 항상 최우선가(50000) 레벨 하나만 소진→보충을 반복하므로, 벽에 쌓인 나머지 레벨 수는 측정에 영향을 주지 않는 설계상 당연한 결과 (`engine_bench_test.go`의 `BenchmarkOrderBookDepth` 코드 주석 참고).
- **벽 체결(BulkFill)은 압도적으로 비쌈**: 단건 매칭(~1.6~2.9μs) 대비 100단계를 한 번에 쓸어담는 벽 주문은 132μs — 약 50~80배 차이. 매 레벨마다 트레이드 생성/이벤트 발행이 반복되기 때문 (1,812 allocs/op).
- **allocs/op**: 단건 체결(30)보다 깊이별 벤치마크(46)가 약간 더 높음 — 매 반복마다 소진된 레벨을 재생성(`buildSellWall`)하는 오버헤드 포함.

## 재현 방법

```bash
cd internal/matching
go test -bench=. -benchmem -run=^$ .
```
