# 고정 부하 정산 비용 상승 진단 하니스 — 설계

> **상태**: 설계 확정. 구현·측정 없음. 열린 결정 없음(6절 전부 닫힘).
> **선행**: [32-B 결과](../../benchmarks/32-2026-08-01-capacity-boundary-session-b.md)
> **앱 코드 기준**: `82b4d7f`(29~32번 측정 바이너리)

---

## 1. 왜 필요한가

32-B에서 **부하가 일정한데 작업당 정산 비용이 계속 올랐다.** 이것이 "이 구성에서 검증된 최고 VU"를
hold 길이와 분리할 수 없게 만들었고, **증설 판단을 막고 있다.**

**확정된 것:**

| 관측 | 값 |
|---|---|
| 작업당 정산 실행 시간 | **9.19 → 23.75ms** (2.6배) |
| DB attempt 시간 | 10.98 → 15.23ms (1.4배) |
| DB CPU median | 32.5(3분) → **85**(10분 실패 시점, p95 92) |
| worker busy | 59.4% → 99.95% |
| 배치 크기 | 2.42 → 2.69 — **평평. 파편화는 원인이 아니다**(29·30·31에 이은 네 번째 반증) |

**확정되지 않은 것 — 아래 어느 것도 배제되지 않았다:**

원장 크기 · 인덱스 · WAL/checkpoint/autovacuum 같은 쓰기 비용 · 동일 사용자 지갑의 행 락 경합 ·
terminal/trade job 구성 변화 · 오더북 상태 변화 · **더 오래 돌리면 평형점에 도달하는지**.

> **⚠ "원장 성장 때문이다"라고 쓰지 않는다.** 시간이 흐르면 같이 커지는 양은 여럿이고,
> 상관을 인과로 옮겨 쓰는 것이다.

### 왜 증설보다 이것이 먼저인가

32-A는 "서버·DB에 헤드룸이 남고 워커만 포화되므로 `N=16`이 먼저"라고 적었으나, 그 근거는
**3분 관측의 DB CPU median 32.5%** 였다. 10분 실행에서 750이 실패하는 시점의 DB CPU median은
**85(p95 92)** 다. **원인이 행 락이나 WAL이라면 `N=16`은 개선이 아니라 지연 악화를 만든다.**
원인을 모르는 채 자원을 늘리면 도달 시점만 옮긴다.

---

## 2. 무엇을 답하는가 — 판정 분기

| 결과 | 지목되는 원인 |
|---|---|
| **N=1부터** DB 크기에 따라 느려짐 | 데이터 크기 · 인덱스 · WAL · 쓰기 증폭 |
| **N=1은 평평, N=8만** 느려짐 | **행 락 · 동시성 경합** |
| **batch만** 느려짐 | trade 정산 SQL |
| **terminal만** 느려짐 | 완료 · 취소 쿼리 |
| **전부 평평** | 실제 부하의 오더북 · job mix · 사용자 상태 변화 |

마지막 칸이 나와도 실패가 아니다 — **"DB 데이터 크기는 원인이 아니다"를 배제로 확정**하고
탐색을 매칭·오더북 쪽으로 옮긴다.

---

## 3. terminal을 대조군으로 반드시 함께 잰다

**`SettleTradeBatch`만 재면 job 실행 시간 2.6배 상승의 절반도 설명하지 못한다.**

### 근거 (1) — 코드: 두 종류가 같은 메트릭 한 줄을 통과한다

`cmd/settlement_pipeline.go:58-83`의 worker는 `jobKindTrade`와 `jobKindTerminal`을 모두 처리하고,
**둘 다 같은 `metrics.SettlementJobSuccess.Observe()`(80행)** 로 관측된다.
`settlement_job_execution_seconds`의 라벨은 `result`뿐이고(`internal/metrics/metrics.go:167-171`)
**job 종류 라벨이 없다.** `settlement_job_dispatch_wait_seconds`(64행)도 같다.

### 근거 (2) — 실측: terminal이 job의 과반이다

32-B Run 1(750) 스냅샷을 1분 버킷으로 분해한 값이다.
trade job 수는 `settlement_batch_size_count`, terminal job 수는 `총 job − trade job`이다.

| hold 분 | trade job/s | terminal job/s | **terminal 비율** | 평균 job |
|---|---|---|---|---|
| 1.2 | 242.2 | 278.2 | **53.5%** | 9.22ms |
| 3.2 | 239.4 | 277.8 | 53.7% | 12.04ms |
| 5.2 | 194.9 | 244.5 | 55.6% | 18.25ms |
| 7.2 | 171.3 | 218.4 | 56.0% | 20.55ms |
| 9.2 | 153.0 | 194.0 | **55.9%** | 22.73ms |

두 가지가 나온다.

1. **terminal이 전체 job의 53~56%다.** `SettleTradeBatch`만 재면 **모집단의 절반 이하**만 본다.
2. **mix 이동은 2.4%p로 작다.** 따라서 **"job 구성이 바뀌어서 평균이 올랐다"는 단독 설명은 약하다** —
   두 종류 중 최소 하나가 실제로 느려졌다. **어느 쪽인지는 현재 계측으로 알 수 없다.**

### 측정 대상 세 종류

| 종류 | production 함수 | 위치 |
|---|---|---|
| trade batch | `(*SettlementService).SettleTradeBatch` | `internal/service/settlement_batch.go:31` |
| 취소 terminal | `(*OrderService).ProcessOrderCancellation` | `internal/service/order_service.go:336` |
| 시장가 완료 terminal | `(*OrderService).CompleteMarketOrder` | `internal/service/order_service.go:373` |

**세 종류를 각각 별도 결과로 저장한다.** 합산 평균은 32-B가 이미 갖고 있고, 그것이 부족해서 하는 진단이다.

---

## 4. 왜 이 형태인가 — 네 안 비교

| 안 | 채택 | 이유 |
|---|---|---|
| **opt-in 통합 진단 테스트**(`internal/service/*_test.go`) | ✅ | 실제 PostgreSQL + production 함수. 기존 `testdb.OpenIntegrationDB` 재사용. **배포 표면 증가 없음** |
| `cmd/` 진단 바이너리 | ❌ | **배포 표면만 늘린다.** 진단이 끝나면 남는 부채다 |
| 일반 `testing.B` | ❌ | **`b.N` 동안 DB가 계속 커져 데이터 크기 축이 오염된다.** 반복 횟수를 고정할 수 없다 |
| 기존 서버 + k6 | ❌(이 단계에서는) | 오더북·job mix·사용자 상태가 같이 변해 **DB 크기 축을 통제할 수 없다.** **수정 후 최종 재검증에 쓴다** |

기존 자산이 그대로 맞는다.

- `internal/testdb/integration.go` — `OpenIntegrationDB(t testing.TB)`가 **`testing.TB`** 를 받고
  `GOEXCHANGE_TEST_DATABASE_DSN`으로 게이팅되며 AutoMigrate + goose까지 수행한다.
- `internal/service/settlement_batch_integration_test.go` · `settlement_batch_concurrency_integration_test.go` ·
  `order_cancellation_integration_test.go` — 시드 픽스처와 동시성 실행 선례가 이미 있다.

> **⚠ 게이트가 두 겹이어야 한다.** CI의 `Postgres integration tests` job은 이미
> `GOEXCHANGE_TEST_DATABASE_DSN`을 설정한다. **DSN만으로 게이팅하면 진단이 CI에서 돌아간다.**
> **`GOEXCHANGE_RUN_SETTLEMENT_DIAGNOSTIC=1`을 추가 조건으로 두고, 없으면 `t.Skip`** 한다.

---

## 5. 계약

### 5.1 측정 행렬 — 6셀

| DB 크기 | 동시성 |
|---|---|
| 초기 | N=1, N=8 |
| 중간 | N=1, N=8 |
| 32-B 종료 규모 | N=1, N=8 |

**각 셀은 동일한 초기 데이터에서 독립 실행한다.** N=1을 돌린 DB에 이어 N=8을 돌리면
**앞 실행의 데이터 증가와 캐시가 섞인다.**

> **⚠ "초기"는 빈 DB가 아니다.** **사용자·지갑·측정 fixture는 세 크기에서 모두 동일**하고,
> **과거 거래 이력만 없는** DB다. 빈 DB로 시작하면 크기 축이 아니라 "fixture 유무"를 재게 된다.

### 5.2 셀 1개의 절차

1. **DB를 목표 크기로 시드** (또는 스냅샷 복원)
2. **`ANALYZE`**
3. **고정 warm-up 실행 — 측정에서 제외**
4. **production 함수로 고정 횟수 실행** (`b.N` 아님)
5. **결과를 `_workspace/`에 JSON/CSV로 저장**
6. **다음 셀 전에 DB를 동일 스냅샷으로 복원**

### 5.3 고정할 조건 — "행 수만 같은 데이터"로 만들면 안 된다

크기 축만 움직이고 **나머지는 전부 같아야** 인과가 분리된다.

| 항목 | 고정 방식 |
|---|---|
| 사용자 집합 | **32-B와 동일한 고정 사용자 수**, 세 크기에서 동일 |
| 사용자별 주문·체결·원장 분포 | **유지**. 균등 분포로 뭉개지 않는다 |
| 측정 대상 active 주문·지갑 | **세 크기에서 완전히 동일한 fixture** |
| 크기 차이를 만드는 것 | **과거 `orders` / `trades` / `ledger_entries` / outbox 행만** |
| batch 크기 | **정확한 정수로 고정한다 — 평균 2.5가 아니다.** 권장 **3**(32-B hold 평균 2.549에 가장 가까운 정수). 2와 3의 차이를 보고 싶으면 **별도 축**으로 뺀다 |
| terminal 비율 | 32-B 실측 **53~56%** 를 고정값으로 |
| random seed | **고정** |
| 반복 횟수 | 고정(`b.N` 아님) |
| DB 재기동 · warm-up | 고정 절차 |

### 5.4 terminal이 실제로 일을 했는지 검증한다 — 없으면 측정이 무의미하다

**`ProcessOrderCancellation` · `CompleteMarketOrder`는 이미 종결된 주문이면 멱등 no-op이다.**
no-op을 벤치마크하면 아주 빠른 숫자가 나오지만 **아무것도 재지 않은 것**이다.

**각 반복마다 다음을 확인한다. 하나라도 어긋나면 그 셀의 결과를 버린다.**

- 시작 상태가 **`PENDING` 또는 `PARTIAL`**
- **실제 locked balance가 존재**
- terminal 이후 **상태 · 잔고 · 원장 행이 실제로 변경됨**
- **no-op · failure · fallback 수가 전부 0**
- trade는 **매번 새 idempotency key로 실제 커밋됨**

### 5.5 수집값

| 분류 | 항목 |
|---|---|
| 지연 | **작업 종류별** p50 / p95 / p99 |
| 처리율 | 종류별 ops/s |
| DB | `pg_stat_statements` 호출 수 · 평균 · 누적 시간 |
| 경합 | **lock wait** |
| 쓰기 | **WAL bytes** |
| 캐시 | buffer hit / read |
| 자원 | DB CPU |
| 무결성 | 실패 · fallback · **정합성 위반** |

> **정합성 게이트는 여기서도 유효하다** — 위반이 하나라도 나오면 **성능 수치를 읽지 않는다.**

### 5.6 정적 DB 조사 (선행, DB 1대로 끝난다)

테이블·인덱스 크기 · **실제 인덱스 정의와 사용량**(`pg_stat_user_tables` / `pg_stat_user_indexes`) ·
autovacuum · dead tuple · checkpoint · WAL 통계 · 정산 SQL의
**`EXPLAIN (ANALYZE, BUFFERS, WAL)`** · `pg_stat_statements` 호출당 시간.

명백한 missing index나 write amplification 후보를 먼저 걸러낸다.

> **⚠ `EXPLAIN`만으로는 닫히지 않는다.** 정산 경로의 중심은 INSERT · UPDATE · 행 락 · COMMIT · WAL이고
> `EXPLAIN`은 읽기 계획만 잘 보여준다. **5.1의 스윕이 본체다.**

---

## 6. 닫힌 결정

| # | 결정 |
|---|---|
| (a) | **합성 시드** — 5.3의 고정 조건 전부 적용 |
| (b) | **`TEMPLATE` 복원** |
| (c) | **production 메트릭 라벨 변경 없음** |
| (d) | **로컬은 선별(screening) 실험** — 아래 해석 범위 준수 |
| (e) | **평평하면 GCP live / 물리 상태 진단으로 승격** |

### (a) 합성 시드 — 확정

32-B의 종료 규모는 `orders` 546,783 · `ledger_entries` 1,886,343 · `trades` 311,207행이다.
**그 DB는 남아 있지 않다**(매 실행 TRUNCATE, VM 정지). 합성 시드가 **가장 싸고 크기 축을 정확히
통제**한다. 조건은 **5.3에 고정한 그대로** 만든다 — 행 수만 맞추면 안 된다.

### (b) `TEMPLATE` 복원 — 확정

`pg_dump`/`pg_restore`보다 **`CREATE DATABASE ... TEMPLATE ...` 가 훨씬 빠르다**(6셀 × 복원이라
누적된다). template DB는 복제 중 접속이 없어야 한다.

### (c) `kind` 라벨 추가 안 함 — 확정

추가하면 다음 GCP 실행에서 실제 부하의 종류별 분해를 직접 얻지만, **앱 코드가 바뀌어
`82b4d7f` 기준선이 깨진다**(29~32번이 모두 이 바이너리다). 진단 결과가 실제 경로 확인을
요구할 때 별도로 판단한다.

### (d) 로컬 Docker Postgres — **선별 실험이다.** 해석 범위를 넘기지 않는다

> **로컬 실험은 논리적 데이터 크기와 동시성 효과를 선별한다.
> 환경 의존적인 WAL · 스토리지 · autovacuum 원인의 최종 기각은 GCP에서만 가능하다.**

| 로컬 결과 | 말할 수 있는 것 |
|---|---|
| **기울기가 나타남** | 논리적 데이터 크기 · SQL · 락 후보를 **강하게 지지** |
| **평평함** | **합성 cardinality만으로는 재현되지 않았다**는 뜻일 뿐 |
| 평평함 | **GCP의 WAL · 디스크 · checkpoint 원인을 기각할 수 없다** ⚠ |

### (e) 평평할 때의 승격 경로 — `pg_dump`가 아니다

**`pg_dump`/`pg_restore`는 물리적 충실도가 높지 않다.**

| 보존한다 | 보존하지 않는다 |
|---|---|
| 논리적 행과 값 | **dead tuple과 실제 bloat** |
| 사용자별 데이터 분포 | **autovacuum 진행 상태** |
| 테이블 간 관계 | **buffer cache** |
| | **checkpoint / WAL 누적 상태** |
| | **물리적 인덱스 배치** |

따라서 합성 실험이 평평할 때의 다음 단계는 단순 dump 복원이 **아니라** 둘 중 하나다.

1. **실제 부하 직후 GCP DB에서 live profiling**
2. **persistent disk / 물리 DB 스냅샷 복제**

---

## 7. 범위 밖

- **리컨실리에이션 실행시간** — **정산 비용 상승과 섞지 않는다.** 사용자·지갑별 LATERAL 집계라는
  별도 쿼리로 비용 구조가 다르다. **독립 진단 항목**이다.
- `N=16` 경계 재측정 — **이 진단이 동시성 부족을 가리킬 때만** 간다.
- 축 2 매칭 quantum 계약.
- 수정 자체 — 이 문서는 **원인을 좁히는 것까지**다. 무엇을 고칠지는 결과를 보고 정한다.
