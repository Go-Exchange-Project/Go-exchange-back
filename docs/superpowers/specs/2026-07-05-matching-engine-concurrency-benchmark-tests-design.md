# 매칭엔진 동시성/레이스 검증 및 성능 벤치마크 테스트 설계

## 배경

`internal/matching` 패키지에는 이미 `engine_test.go`에 단위 테스트(가격/시간 우선순위, 셀프 트레이드 방지, 시장가 주문, 부분 체결, 취소, 스냅샷 뎁스 등)가 갖춰져 있고, `go.mod`에 `testify v1.11.1`이 등록되어 있다. 이번 작업의 목표는 기존 단위 테스트는 건드리지 않고, 다음 두 가지를 새로 추가하는 것이다.

1. 동시성/레이스 검증
2. 성능 벤치마크

## 범위

- 새 파일 2개만 추가한다: `internal/matching/engine_concurrency_test.go`, `internal/matching/engine_bench_test.go`
- 기존 `engine_test.go`는 수정하지 않는다 (CLAUDE.md의 "외과수술처럼 정교한 변경" 원칙)
- 기존 테스트 헬퍼(`testOrder`, `testUserOrder`, `submitAndWaitSnapshot`, `requireNextTrade`, `requireNextExecutionEvent`, `assertNoTrade`, `requireCancelSnapshot`)는 같은 패키지이므로 새 파일에서 그대로 재사용한다. 별도 `testutil` 패키지 추출과 같은 리팩터링은 이번 범위에 포함하지 않는다.

## 아키텍처 관찰

`MatchingEngine.Start()`는 `OrderCh`/`CancelCh`/`SnapshotReq`를 단일 컨슈머 고루틴에서 순차 처리한다. 즉 매칭 로직(`Match`, `GetOrderBook`, `OrderBooks` 맵 접근)은 이미 직렬화되어 있어 그 내부에서는 데이터 레이스가 발생하지 않는다. 실제 동시성 위험은 **여러 프로듀서 고루틴이 동시에 채널에 쓰는 지점**에 있다. 따라서 동시성 테스트는 `me.Match()`를 여러 고루틴에서 직접 호출하는 방식이 아니라, 항상 `me.OrderCh <-` 채널 제출을 통해 실사용 패턴 그대로 스트레스를 준다.

## 1. 동시성/레이스 테스트 (`engine_concurrency_test.go`)

### `TestConcurrentOrderSubmission_NoRaceAndConsistentState`
- N개의 고루틴이 동시에 BTC 심볼로 매수/매도 주문을 `OrderCh`에 제출한다.
- 별도의 드레인 고루틴이 `SnapshotCh`/`TradeCh`/`ExecutionCh`를 계속 소비한다. (버퍼가 각각 256/1024/1024이므로, 드레인하지 않으면 버퍼가 가득 차 단일 컨슈머 루프가 블로킹되고 테스트가 멈춘다.)
- 모든 제출이 끝난 뒤 불변식을 검증한다: 총 제출 수량 = 체결된 수량 합 + 오더북에 남은 수량 합.

### `TestConcurrentMultiSymbolAccess_NoRace`
- 여러 고루틴이 서로 다른 심볼(BTC/ETH/AVAX 등)로 동시에 `OrderCh`에 제출한다.
- 각 심볼의 오더북에 다른 심볼의 주문이 섞여 들어가지 않았는지 검증한다.

### `TestMultipleEngineInstances_Isolated`
- 별도의 `MatchingEngine` 인스턴스 여러 개를 각각 고루틴에서 `Start()`시켜 동시에 구동한다.
- 인스턴스 간 오더북/트레이드 시퀀스가 서로 간섭하지 않는지 검증한다.

### 실행
```
go test -race -run TestConcurrent -v ./internal/matching/...
```
(테스트 함수명은 `TestConcurrent*` / `TestMultipleEngineInstances*` 접두사로 통일해 위 커맨드로 선택 실행 가능하게 한다.)

### 안전장치
드레인 고루틴이나 프로듀서 고루틴이 멈추면 테스트가 무한 대기할 수 있으므로, 각 테스트에 `time.After` 기반 타임아웃(예: 5초)을 두어 시간 내 완료되지 않으면 `t.Fatal`로 명확히 실패시킨다.

## 2. 성능 벤치마크 (`engine_bench_test.go`)

모든 벤치마크는 `b.ReportAllocs()`를 사용해 할당량도 함께 리포트한다. 채널 오버헤드를 배제하고 순수 매칭 비용을 측정하기 위해 `me.Match(order)`를 직접 호출한다 (`Start()` 고루틴 우회).

### `BenchmarkMatch_ImmediateCross`
- 셋업에서 대량 수량의 매도 주문 하나를 미리 넣어둔다.
- 루프마다 소량 매수 주문을 보내 즉시 체결시킨다.
- `matchBuy` 내부 루프의 순수 처리 비용(ops/sec)을 측정하는 기준 벤치마크.

### `BenchmarkOrderBookDepth`
- `b.Run`으로 오더북 깊이 100 / 1,000 / 10,000 케이스를 서브벤치마크로 분리한다.
- 각 깊이만큼 서로 다른 가격대에 매도 주문을 미리 채워둔 뒤, 최우선가에 체결되는 매수 주문 하나를 반복 실행한다.
- BTree 탐색 비용이 깊이에 따라 어떻게 변하는지(이론상 O(log n)) 확인한다.

### `BenchmarkBulkFill`
- "벽 주문" 시나리오: 하나의 대형 주문이 오더북 여러 단계를 훑고 지나가며 체결된다.
- 매 반복(`b.N`)마다 `b.StopTimer()`로 오더북을 재구성(예: 100단계 매도 물량 재생성)한 뒤 `b.StartTimer()`로 측정 구간을 연다.
- 셋업 비용이 타이머에 섞이지 않도록 Stop/Start를 명시적으로 감싼다.

### 실행
```
go test -bench=. -benchmem -run=^$ ./internal/matching/...
```

### 트레이드오프
`BenchmarkBulkFill`처럼 반복마다 재구성이 필요한 벤치마크는 `b.N`이 커질수록 셋업 비용이 누적되어 벤치마크 자체가 느려질 수 있다. 이는 허용 가능한 트레이드오프로 간주한다.

## 성공 기준

특정 성능 수치(예: N ops/sec 이상)를 목표로 하지 않는다. 이번 작업의 목표는 "측정 가능하게 만드는 것"이다.

- 동시성 테스트 3종: `go test -race`가 레이스 경고 없이 통과하고, 각 테스트의 불변식 assertion이 통과한다.
- 벤치마크 3종: `go test -bench=.`가 에러 없이 완료되고 ops/sec, ns/op, allocs/op 수치가 출력된다.

## 범위 밖 (Out of Scope)

- 기존 `engine_test.go` 수정
- 테스트 헬퍼의 별도 패키지 추출
- CI 파이프라인/Makefile 신규 추가
- 특정 성능 목표치 설정 또는 성능 회귀 방지 장치
