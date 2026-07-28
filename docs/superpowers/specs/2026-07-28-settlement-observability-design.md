# 4차 축 1 — 정산 경로 관측성 패치 설계 (관측성-only)

- **날짜**: 2026-07-28
- **성격**: **관측성-only.** 기능 설계가 아니다.
- **선행**: [27번 재분석](../../benchmarks/27-2026-07-28-settlement-binding-reanalysis.md)

## 원칙 (이 패치의 전부)

> **정산 스케줄링·배리어·재시도 동작은 바꾸지 않고, 27번에서 남은 두 원인인 batch 파편화와
> barrier wait의 기여도만 구분한다.**

성공 기준은 "원인을 고쳤다"가 아니라 **"다음 수정이 주문별 fence / batch scheduling / DB
transaction / worker scheduling 중 어디를 향해야 하는지 확정할 수 있다"**이다.

## 메트릭 계약

| 메트릭 | 타입 | 의미 |
|---|---|---|
| `settlement_attempt_duration_seconds{path}` | Histogram | **DB 호출 1회**(트랜잭션 시도)의 시간 — **신규** |
| `settlement_barriers_total{type}` | Counter | 종결 이벤트 배리어 **진입 횟수** |
| `settlement_barrier_wait_seconds{type}` | Histogram | 선행 in-flight batch를 **기다린 시간** |
| `settlement_barrier_inflight_batches{type}` | Histogram | 배리어 진입 **당시 in-flight 수** |
| `settlement_job_dispatch_wait_seconds` | Histogram | dispatcher가 **job 송신을 시도한 시점** → worker **실행 시작** |
| `settlement_job_execution_seconds{result}` | Histogram | worker 시작 → **논리적 job 완료** |

**라벨은 고정값만.** `path="batch|single"`, `type="market_done|cancel"`,
`result="success|fallback|failed"`. **`symbol`·`order_id`·`user_id`·`batch_seq` 등 고카디널리티
라벨 금지.**

### 기존 메트릭은 건드리지 않는다

**`order_settlement_duration_seconds`는 현재 의미(논리적 단건 정산 전체 — 재시도 포함,
main.go:684-690)로 그대로 보존한다.** 관측성-only 패치에서 기존 메트릭의 의미를 조용히 바꾸면
Prometheus에 남은 **과거 시계열을 운영자가 잘못 해석**하게 되고, 라벨을 새로 붙이면 무라벨 시계열과
신규 시계열이 공존해 혼란이 커진다. 대신 **신규 `settlement_attempt_duration_seconds{path}`**를
추가해 DB 호출 1회마다 관측한다. (기존 메트릭이 정말 어디서도 안 쓰인다고 확인되면, **이름·의미
변경을 명시한 별도 폐기 절차**로 처리한다 — 이번 범위 밖.)

### 시간 의미 (재시도가 있으므로 스펙에서 고정)

- **`settlement_attempt_duration_seconds` = DB 시도 단위.** 재시도가 3회면 **샘플 3개**.
  배치 경로(`SettleTradeBatch` 호출 1회)는 `path="batch"`, 단건 경로는 `path="single"`.
- **`settlement_job_execution_seconds` = 논리적 job 단위.** worker가 job을 받은 뒤 **재시도와
  fallback까지 포함해 최종 결과가 날 때까지**.
- **두 히스토그램을 직접 차감하지 않는다.** 개별 job과 attempt 샘플이 연결되지 않으므로 p95끼리
  빼는 것은 의미가 없다. **정확한 사용법**: attempt와 logical job의 **count·sum·분포를 함께 관측해
  DB 실행 시간, 재시도 빈도, 비-DB 오버헤드의 기여도를 추정**한다. (`_sum` 델타는 동일 job 집합이라는
  조건에서 대략 비교 가능하나, 재시도로 attempt 수가 달라진다는 점을 감안한다.)
- **`settlement_job_dispatch_wait_seconds`** = dispatcher가 **송신을 시도한 시점**부터 worker가
  실행을 시작할 때까지. 이름대로 **송신 대기까지 의도적으로 포함**한다: jobs 채널 송신 대기 +
  채널 내부 대기 + worker 스케줄링 대기. (순수 "enqueue 이후 대기"가 아니며, 이번 진단에는 이
  합계가 더 유용하다.)
- **`settlement_barrier_wait_seconds`** = 종결 이벤트를 **만난 시점**부터 필요한 선행 in-flight가
  **모두 완료될 때까지**. in-flight가 이미 0이면 ~0이 기록되지만 `barriers_total`은 증가한다
  (**"배리어는 빈발하나 대기는 0" = 파편화 지배**를 구분하는 핵심).

## 판정 연결 (27번 분기표)

| 관측 | 결론 |
|---|---|
| `barrier_wait` 큼 | 파티션 전체 fence 지배 → 주문별 dependency fence |
| `barrier_wait` 작은데 `attempt_duration` 샘플 수 많음 | batch 파편화 지배 → batch 구성 변경 |
| 작은 batch인데 `attempt_duration` 자체가 큼 | DB 왕복·락 지배 → 트랜잭션/SQL 최적화 |
| `job_dispatch_wait` 큼 | pool·스케줄링 한계 → pool·dispatcher 조정 |

## TDD 범위 (시간 값은 검증하지 않는다)

시간의 **정확한 값**을 단언하면 flaky해진다. **histogram `_count`와 라벨 선택**만 결정론적으로 검증:

1. 라이브 trade batch 경로에서 `settlement_attempt_duration_seconds{path="batch"}` count 증가
   (+ 기존 `order_settlement_duration_seconds`의 의미·관측 지점이 **변하지 않았음**도 확인)
2. Done과 Cancel이 **각각 올바른** `settlement_barriers_total{type}` 증가
3. in-flight가 존재하는 배리어에서 `settlement_barrier_wait_seconds` count 증가
4. job 하나를 의도적으로 worker 앞에서 대기시켜 `settlement_job_dispatch_wait_seconds` 관측
5. job 완료 시 `settlement_job_execution_seconds` count 증가
6. **기존 순서·정산·종료·race 테스트가 모두 그대로 통과**(동작 무변경의 증거)

## 성능 회귀 조건 (hot path에 들어가므로)

- **라벨 collector를 이벤트마다 조회하지 않는다** — 고정 라벨 조합은 초기화 시 1회 resolve해 재사용.
- **batch/job당 필요한 `time.Now` 호출만** 추가.
- **trade 한 건마다 기록하지 않는다** — 가능하면 batch 단위.
- 기존 matching/settlement 벤치마크에서 **유의미한 할당 증가 없음**.

## 명시적 비범위 (섞으면 판정 목적이 흐려진다)

- 배리어 범위 축소 · batch 구성 변경 · worker concurrency 변경 · 종결 이벤트 재배열 · 워터마크 변경
- **축 2의 `executions_per_order`·매칭 quantum 계측** — 중요하지만 이번 정산 진단 패치에 섞지 않는다.

## 완료 판정 (코드 테스트만으로 끝내지 않는다)

짧은 **통제 부하** 후 확인:

1. 신규 메트릭이 **전부 0이 아닌 값**을 가진다.
2. `settlement_barriers_total`이 실제 Done/Cancel 처리 건수와 **대략 일치**.
3. 완료 job 수와 `job_queue_wait`·`job_execution` histogram count가 **일치**.
4. **정합성·fallback 결과가 변경 전과 동일**.
5. 위 판정표의 **네 분기 중 하나를 실제로 선택할 수 있다.**
