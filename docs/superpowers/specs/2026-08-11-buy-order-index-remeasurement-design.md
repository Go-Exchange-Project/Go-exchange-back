# `trades.buy_order_id` 인덱스 단독 수정·재측정 설계

> **상태**: 설계 확정. 구현·측정 없음.
> **선행**: [32-B 용량 경계](../../benchmarks/32-2026-08-01-capacity-boundary-session-b.md) ·
> [33 정산 비용 성장 진단](../../benchmarks/33-2026-08-02-settlement-cost-growth-diagnostic.md)
> **비교 앱 코드 기준**: `82b4d7f`

## 1. 목표

33번 진단은 시장가 매수 완료 시 실행되는 다음 쿼리가 `trades` 전체를 스캔한다는 사실을 확인했다.

```sql
SELECT COALESCE(SUM(buyer_fee), 0)
FROM trades
WHERE buy_order_id = $1;
```

`trades.buy_order_id`를 선두 컬럼으로 갖는 인덱스가 없고, 6셀 진단에서 데이터 크기만 늘렸을 때
`market_terminal` p50은 4.9~6.3배 증가했다. `full/N8`에서는 이 쿼리 하나가 DB 실행시간의
86.1~86.4%를 차지했다.

이번 작업의 목표는 하나다.

> **`trades.buy_order_id` 인덱스만 추가한 뒤 같은 로컬 6셀과 같은 GCP 용량 프로파일로 재측정하여,
> 진단한 원인이 제거됐는지와 10분 용량 경계가 실제로 이동했는지를 분리해 판정한다.**

인덱스·멱등성·취소 outbox·매칭 quantum·DB timeout을 한 바이너리에 함께 넣지 않는다. 먼저 인덱스
하나의 효과를 닫아야 이후 변경의 성능 효과와 정확성 효과를 구분할 수 있다.

## 2. 범위

### 포함

- `trades (buy_order_id)` 단일 컬럼 B-tree 인덱스용 goose migration 1개
- migration 적용·복구·카탈로그 검증
- 33번과 같은 로컬 6셀 스윕 2회
- 로컬 게이트 통과 시 32-B와 같은 GCP 500 VU 1회, 750 VU 최소 1회·최대 2회 독립 실행
- 원본 산출물 보존과 최종 벤치마크 보고서

### 제외

- 주문 생성 `Idempotency-Key`
- 취소 command outbox와 worker
- `maxConsecutiveCancels`, `maxMatchesPerTurn`
- DB statement/lock timeout
- `/live`, `/ready`, healthcheck 배선
- shutdown drain 수정
- 프런트엔드, k6 workload, API 계약 변경
- matching/settlement 동시성 변경
- 앱 서버 또는 DB 사양 변경

즉 GCP에 배포되는 애플리케이션 로직은 `82b4d7f`와 같고, DB schema에 인덱스 하나만 추가된다.
33번의 두 진단 `_test.go` 파일은 로컬 재측정에만 쓰이며 배포 바이너리에 포함되지 않는다.

## 3. 인덱스 설계

### 3.1 정의

새 migration은 다음 인덱스를 만든다.

```sql
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_trades_buy_order_id
    ON trades (buy_order_id);
```

- `buy_order_id = $1` 동등 조건 하나만 지원하므로 단일 컬럼 B-tree면 충분하다.
- `buyer_fee`를 포함하는 covering index는 이번 진단에 필요하지 않다. 인덱스 크기와 체결 쓰기 비용을
  불필요하게 늘리지 않는다.
- GORM 모델에 `index` 태그를 추가하지 않는다. `AutoMigrate`가 운영 테이블에 비동시 인덱스 생성을
  시도하지 않게 하고, 인덱스 수명주기는 goose migration 하나가 소유한다.

### 3.2 온라인 적용

PostgreSQL의 `CREATE INDEX CONCURRENTLY`는 transaction block 안에서 실행할 수 없다. migration 파일
첫 줄에 `-- +goose NO TRANSACTION`을 두며, 현재 의존성인 goose v3.27.1이 이 지시자를 지원한다.

`IF NOT EXISTS`는 이미 유효한 인덱스를 만든 뒤 goose version 기록만 실패한 경우의 안전한 재실행을
위해 사용한다. 단, 중단된 concurrent build가 같은 이름의 invalid index를 남길 수 있으므로 이름 존재만
성공으로 보지 않는다. 아래 속성 검증과 실패 시 `RAISE EXCEPTION`을 **같은 migration의 Up 절차에
포함**하여, 검증 실패 상태에서는 goose version이 기록되지 않게 한다.

- `idx_trades_buy_order_id`가 `trades`를 대상으로 한다.
- 첫 번째이자 유일한 key column이 `buy_order_id`다.
- `pg_index.indisready = true`
- `pg_index.indisvalid = true`
- unique index가 아니다.

검증 실패 시 애플리케이션을 준비 상태로 올리지 않는다. invalid/wrong-definition index는 정확한 대상을
확인한 뒤 `DROP INDEX CONCURRENTLY IF EXISTS idx_trades_buy_order_id`로 제거하고 migration을 재실행한다.

### 3.3 rollback

Down migration은 다음 한 문장만 수행하며 역시 transaction 밖에서 실행한다.

```sql
DROP INDEX CONCURRENTLY IF EXISTS idx_trades_buy_order_id;
```

인덱스 제거는 원본 데이터나 정산 결과를 되돌리지 않는다. 다만 제거 후 시장가 매수 완료 쿼리는 다시
전체 스캔으로 돌아가므로 성능 rollback임을 명시한다.

## 4. 변경 귀속 보호

로컬과 GCP 재측정 전 다음 조건을 기록한다.

- Git commit과 migration checksum
- 배포 이미지 digest
- `git diff 82b4d7f..HEAD -- '*.go' ':!**/*_test.go'` 결과
- `idx_trades_buy_order_id`의 `pg_get_indexdef`, `indisready`, `indisvalid`
- 서버·DB·load generator 사양과 settlement concurrency
- k6 스크립트 checksum

배포되는 Go 소스 diff가 비어 있지 않거나 k6 스크립트 checksum이 32-B와 다르면 32-B와의 직접 비교로
판정하지 않는다. 이 경우 원인을 제거하고 다시 시작한다.

## 5. 1차 게이트 — 로컬 6셀 재측정

### 5.1 고정 프로토콜

33번 진단을 변경 없이 반복한다.

| 항목 | 고정값 |
|---|---|
| 행렬 | `{initial, mid, full} × {N=1, N=8}` = 6셀 |
| 반복 | 전체 6셀 스윕 2회 |
| 셀 격리 | 각 셀은 자기 크기의 `TEMPLATE` 복제본에서 독립 시작 |
| fixture | seed `c23007f1614177f801d8d777ff1c546d`, 활성 주문 3,575개 |
| 과거 이력 크기 | initial `0/0/0/0`, mid `273,391/155,603/943,168/229,673`, full `546,783/311,207/1,886,340/459,346` |
| workload | 사용자 750명, random seed `20260802` |
| job | warm-up 100 제외, 측정 1,000 |
| job mix | `trade_batch=454`, `cancel_terminal=278`, `market_terminal=268` |
| batch | 정확히 3 |
| 정합성 | production reconciliation 포함, 모든 실패·fallback·위반 0 |

크기 표의 값 순서는 `orders / trades / ledger_entries / outbox`다. 환경은 33번과 같은 로컬 PostgreSQL
선별 환경을 사용한다. 절대 지연이 아니라 셀 사이의 기울기를 읽는다.

### 5.2 실행 전 증거

- full fixture에 `ANALYZE trades`를 실행한다.
- 대상 SUM 쿼리의 `EXPLAIN (ANALYZE, BUFFERS)`를 기록한다.
- mid와 full fixture에서 대상 조건이 `Index Scan` 또는 `Bitmap Index Scan`으로
  `idx_trades_buy_order_id`를 사용하는지 확인한다.
- initial fixture는 테이블이 작아 planner가 순차 스캔을 선택해도 실패로 보지 않는다.

### 5.3 사전 등록 성공 기준

두 반복의 p50을 먼저 셀별로 중앙값 처리하고, 동시성별 크기 기울기를 계산한다.

```text
market_slope(N) = median(full market_terminal p50 A/B)
                  / median(initial market_terminal p50 A/B)
```

**로컬 PASS는 아래 조건을 모두 만족할 때만 성립한다.**

1. `market_slope(N=1) ≤ 1.40`
2. `market_slope(N=8) ≤ 1.40`
3. mid와 full에서 대상 SUM 쿼리가 새 인덱스를 사용한다.
4. 12개 셀의 정합성 실패·fallback·reconciliation 위반이 전부 0이다.

두 반복의 원시 p50과 각 회차의 `full/initial` 비율도 모두 보고한다. 중앙값을 주 판정값으로 쓰는 이유는
33번에서 같은 셀의 두 회차가 최대 34% 차이 났기 때문이다. `trade_batch`와 `cancel_terminal`의
크기 기울기는 회귀 관찰값으로 보고하되, 기존 1.2~1.4배가 run-to-run 분산과 구분되지 않았으므로
인덱스 인과 판정의 별도 게이트로 쓰지 않는다.

**주 판정은 위 중앙값 집계만 사용한다.** 중앙값 기준은 PASS지만 원시 회차 하나의 비율이 1.40을
넘는 경우에도 PASS를 유지하고, 회차 불일치를 결과의 한계로 기록한다. 이를 이유로 threshold를 바꾸거나
통과 회차를 고르기 위한 추가 반복을 하지 않는다.

### 5.4 실패 분기

위 네 조건 중 하나라도 깨지면 **GCP로 올라가지 않는다.** 이때의 결론은 다음으로 고정한다.

> 인덱스가 쿼리 계획에 적용되지 않았거나, 적용됐어도 33번에서 관찰한 `market_terminal` 크기 기울기를
> 1.4배 이하로 제거하지 못했다. `buy_order_id` 전수 스캔은 관찰된 원인이지만 제안한 단일 인덱스만으로
> 로컬 비용 성장을 닫았다고 말할 수 없다.

실패 결과를 숨기거나 threshold를 사후 완화하지 않는다. 쿼리 계획, fixture 동등성, 캐시·통계 상태부터
다시 진단한다.

## 6. 2차 게이트 — GCP 500 VU 10분

로컬 PASS 후에만 GCP로 간다. 500 실행은 기존 검증 용량에서 인덱스가 고정 부하 중 비용 성장 추세를
줄이는지 확인한다.

### 6.1 고정 프로파일

| 항목 | 값 |
|---|---|
| Server | `e2-highcpu-4` |
| DB | `e2-highcpu-8` |
| settlement concurrency | 8 |
| DB max open connections | 25 |
| load generator | `e2-standard-8` 2대 |
| 부하 | 합산 500 VU |
| ramp | 30초 |
| hold | 10분 |
| 반복 | 1회 |
| 시작 DB | 32-B와 같은 10개 테이블 초기화 절차 |
| 실행 관계 | 750 실행과 DB 상태·프로세스를 공유하지 않는 독립 실행 |

10,000 VU spike 프로파일은 사용하지 않는다. 32-B와 같은 capacity workload 및 checksum을 사용한다.

### 6.2 비용 성장 계산

`settlement_job_execution_seconds_sum / count`의 counter delta로 hold 구간을 1분 버킷화한다. ramp와
경계 표본을 제외한 첫 번째 완전한 1분 버킷과 마지막 완전한 1분 버킷을 사용한다.

```text
job_growth_500 = last_complete_minute_mean / first_complete_minute_mean
```

같은 `/metrics` 스냅샷에 아래 counter를 함께 보존하고 1분 버킷 delta를 계산한다. 이 수집은 500과
모든 750 실행에 공통 적용한다.

```text
trade_jobs       = delta(settlement_batch_size_count)
cancel_jobs      = delta(settlement_terminal_wait_seconds_count{kind="cancel"})
market_done_jobs = delta(settlement_terminal_wait_seconds_count{kind="market_done"})
total_jobs       = delta(settlement_job_execution_seconds_count{result="success"})
terminal_jobs    = total_jobs - trade_jobs
```

`cancel_jobs + market_done_jobs`는 dispatch 기준이고 `terminal_jobs`는 worker 완료 기준이므로 1분 경계의
in-flight job만큼 작은 차이가 날 수 있다. 최종 drain 후 누적값으로 둘을 교차 확인하고, 각 버킷에서
trade/cancel/market_done 비중을 보고한다. 이 mix는 PASS threshold가 아니라, 혼합 평균의 변화가
`job_growth_500` 또는 `job_growth_750`을 만들었는지 판별할 진단 증거다.

32-B에서 보고된 `9.19ms → 23.75ms`는 **750 VU 실행의 2.58배 성장 기준**이다. 500과 750의
절대 지연을 직접 비교하지 않는다. 500에서는 같은 시간축 계산법으로 성장 배율이 로컬 잡음 범위까지
줄었는지를 판정한다.

### 6.3 사전 등록 성공 기준

**GCP 500 PASS는 아래 조건을 모두 만족할 때만 성립한다.**

1. `job_growth_500 ≤ 1.40`
2. `sli_order_response_availability = 100%`
3. `sli_order_business_success = 100%`
4. `sli_cancel_success = 100%`
5. 응답시간 `> 1,000ms`가 A/B 시나리오 모두 0건
6. admission rejection, 정산 실패, fallback, quarantine, duplicate, 정합성 위반이 전부 0건
7. 종료 후 settlement outstanding job이 0으로 drain되고, 재기동 reconciliation 4개 항목이 모두 0

첫·마지막 버킷 값, 절대 차이, 배율을 함께 보고한다. `≤1.40`은 33번에서 평평하다고 판정한
batch·cancel의 크기 기울기 범위에 맞춘 사전 기준이다.

### 6.4 실패 분기

500에서 업무 SLI 또는 정합성 게이트가 깨지거나 `job_growth_500 > 1.40`이면 750을 실행하지 않는다.
결론은 다음으로 고정한다.

> 로컬 전수 스캔 제거는 확인됐더라도, 인덱스 하나가 32-B의 GCP 비용 성장 또는 기존 500 VU 계약을
> 충분히 설명·보존하지 못했다. 용량 경계가 이동했다고 주장하지 않고 GCP의 남은 성장 원인을 진단한다.

## 7. 3차 게이트 — GCP 750 VU 10분

500 PASS 후 새로 초기화한 독립 환경에서 합산 750 VU를 30초 ramp + 10분 hold로 실행한다.
서버·DB·worker·pool·load generator와 workload는 32-B와 동일하다. 첫 실행이 통과하면 환경을 다시
초기화해 같은 조건으로 확증 실행을 한 번 더 수행한다. 따라서 750은 최소 1회, 최대 2회 실행한다.

### 7.1 사전 등록 성공 기준

각 실행은 500의 2~7번 조건을 모두 만족해야 한다. 추가로 1분 버킷의 job execution time, 종류별 job 수,
DB CPU, worker busy, dispatch wait, 체결/s를 보고해 시간에 따른 포화 여부를 설명한다.
`job_growth_750`은 원인 설명 지표이며 750 통과를 위한 별도 threshold로 쓰지 않는다.

- 첫 실행 FAIL: 확증 실행 없이 750을 탈락시킨다.
- 첫 실행 PASS: 깨끗한 DB의 독립 확증 실행으로 진행한다.
- 확증 실행 FAIL: 두 실행을 평균하지 않고 750을 탈락시킨다.
- 두 실행 모두 PASS: 이 workload와 고정 구성에서 **10분 hold로 검증된 최고 용량이 최소 750 VU로
  이동했다.**
  750보다 높은 값을 외삽하지 않는다.

750이 한 번이라도 실패하면 500은 그대로 최고 검증 용량이다. 인덱스가 비용을 줄였어도 경계가
750까지 이동한 것은 아니다.

750 실패와 함께 32-B와 같은 DB 포화 서명(DB CPU 고점, job 비용·worker busy 상승, admission shedding)이
재현되면 다음 결론을 사용한다.

> 전수 스캔 제거 후에도 DB가 이 구성의 실제 용량 벽으로 남았다. 다음 용량 작업은 앱 서버 추가가 아니라
> DB 읽기/정산 경로 분리, DB 자원 또는 데이터 접근 구조 쪽에서 설계한다.

다른 실패 서명이 나오면 그 증거를 우선하며 “DB가 벽”이라고 자동 결론 내리지 않는다.

## 8. 전체 판정표

| 로컬 | GCP 500 | GCP 750 | 고정 결론 |
|---|---|---|---|
| FAIL | 실행 안 함 | 실행 안 함 | 단일 인덱스로 33번 원인을 닫지 못함 |
| PASS | FAIL | 실행 안 함 | 로컬 원인은 제거했지만 GCP 비용 성장·기존 계약을 설명하지 못함 |
| PASS | PASS | 1회 이상 FAIL | 비용 개선은 유효하나 검증 경계는 500; DB 포화 서명이면 다음은 DB 측 작업 |
| PASS | PASS | 2회 모두 PASS | 검증 경계가 최소 750으로 이동; A 완료 |

750 실패는 작업 실패를 뜻하지 않는다. 미리 고정한 조건에서 “인덱스가 제거한 비용”과 “남아 있는 시스템
경계”를 분리해 얻은 결론이다. threshold나 실행 시간을 결과를 본 뒤 바꾸지 않는다.

## 9. 산출물

- 중간 원본: `_workspace/buy-order-index-remeasurement/`
- 로컬 12셀 JSON/CSV와 환경 메타데이터
- migration 적용·카탈로그·`EXPLAIN` 증거
- GCP k6 summary, metrics snapshot, CPU 시계열, 정합성 SQL 결과
- 최종 보고서: 실행일에 맞춘 `docs/benchmarks/34-YYYY-MM-DD-buy-order-index-remeasurement.md`

최종 보고서는 PASS/FAIL뿐 아니라 판정표의 어느 행에 도달했는지, 실행하지 않은 다음 단계가 무엇인지도
명시한다.

## 10. A 이후 B 설계에 넘기는 확정 제약

아래 항목은 이번 구현 범위가 아니지만, 이미 합의된 의미를 후속 설계에서 바꾸지 않는다.

- 주문 생성 `Idempotency-Key`는 A 측정 후 optional-but-honored 단계 또는 하니스 호환 전환을 거쳐
  필수화한다. A의 k6 계약은 변경하지 않는다.
- 취소 command outbox worker를 matching quantum보다 먼저 구현한다. DB commit 직후 nonblocking
  in-process wake-up을 보내고 50ms polling은 crash/signal-loss backstop으로만 쓴다.
- command `PROCESSED`와 execution outbox 저장은 같은 DB transaction에서 commit한다.
- `maxConsecutiveCancels`는 최종 worker dispatch 패턴 위에서 결정한다. 초기 후보 64는 outbox 이후
  재측정 없이 고정하지 않는다.
- `maxMatchesPerTurn`은 취소 burst quantum과 별개다. 큰 aggressive order의 sweep을 slice하되 active
  incoming order는 새 주문보다 앞에 유지한다.
- **shutdown은 진행 중 sweep을 선점하지 않는다.** active order를 rest 또는 terminal 상태로 종결한 뒤
  `stopCh`를 처리한다.
- sweep slice 사이 취소를 허용하면 resting maker가 뒤의 체결을 피할 수 있다. 이는 기존의 incoming-order
  단위 체결 원자성을 포기하고 취소 진행성을 택한 명시적 대가다.
- DB timeout은 전역 5초 하나를 공유하지 않고 hot path, migration, settlement retry, reconciliation 등
  경로별 계약으로 정한다.
- Docker healthcheck는 `/live`, load balancer와 트래픽 admission은 `/ready`를 사용한다.

## 11. 완료 정의

A는 다음 조건을 만족할 때 끝난다.

1. concurrent index migration과 rollback/recovery 절차가 검증됐다.
2. 로컬 6셀 판정과, 통과한 경우 GCP 500/750 판정이 사전 등록 분기대로 기록됐다.
3. 원본과 최종 보고서가 재현 가능한 형태로 보존됐다.
4. 결과가 무엇이든 threshold를 사후 변경하지 않고, 판정표에 따른 결론을 명시했다.

A가 끝나면 “경계를 측정했고(32) → 원인을 특정했고(33) → 단일 원인을 수정했고 → 경계가 어디까지
움직였는지 확인했다(A)”는 성능 이야기가 독립적으로 완결된다. B는 이 결과와 분리된 정확성 부채 작업이다.
