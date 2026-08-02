# 고정 부하 정산 비용 상승 진단 하니스 — 구현 계획

> **설계**: [2026-08-02-settlement-cost-growth-diagnostic-design.md](../specs/2026-08-02-settlement-cost-growth-diagnostic-design.md)
> **선행 결과**: [32-B](../../benchmarks/32-2026-08-01-capacity-boundary-session-b.md)
> **진행 상태 (2026-08-02)**: 계획 수립. **구현 미착수.**

**Goal:** **"고정 부하에서 작업당 정산 비용이 왜 오르는가"** 의 원인 후보를 **데이터 크기 축과
동시성 축으로 분리**한다. 수정은 이 계획의 범위가 아니다.

---

## 비협상 제약

| 제약 | 이유 |
|---|---|
| **production 함수를 그대로 호출한다** | mock을 재면 아무것도 알 수 없다 |
| **`GOEXCHANGE_RUN_SETTLEMENT_DIAGNOSTIC=1` 없으면 `t.Skip`** | CI의 `Postgres integration tests` job이 이미 `GOEXCHANGE_TEST_DATABASE_DSN`을 설정한다. **DSN 단독 게이팅이면 CI에서 돈다** |
| **`b.N` 금지, 고정 반복 횟수** | `b.N` 동안 DB가 커져 **크기 축이 오염된다** |
| **셀마다 동일 스냅샷에서 독립 시작** | N=1 뒤에 N=8을 이어 돌리면 앞 실행의 데이터 증가·캐시가 섞인다 |
| **정합성 위반이 하나라도 나오면 성능 수치를 읽지 않는다** | 프로젝트 전역 규칙 |
| **`internal/metrics`를 건드리지 않는다** | 앱 코드가 바뀌면 29~32번의 `82b4d7f` 기준선이 깨진다 |
| **`cmd/`에 파일을 추가하지 않는다** | 배포 표면을 늘리지 않는다 |

---

## Phase 0 — 준비

- [ ] 로컬 Docker Postgres 기동, `GOEXCHANGE_TEST_DATABASE_DSN` 설정
      → **검증**: 기존 `go test ./internal/service/ -run Integration`이 통과한다
- [ ] `pg_stat_statements` 확장 활성화 여부 확인
      → **검증**: `SELECT * FROM pg_stat_statements LIMIT 1`이 에러 없이 돈다.
      없으면 `shared_preload_libraries`에 추가 후 재기동
- [ ] 32-B 종료 규모 확정치를 상수로 옮긴다: `orders` **546,783** · `ledger_entries` **1,886,343** ·
      `trades` **311,207**
      → **검증**: 32-B 문서의 무결성 교차검증 표와 일치

---

## Phase 1 — 합성 시드 빌더

**위치**: `internal/service/settlement_diagnostic_seed_test.go`

- [ ] **고정 fixture 생성** — 32-B와 동일한 사용자 수, 고정 seed
      - 측정 대상 **active 주문·지갑**은 세 크기에서 **완전히 동일**
      - 사용자별 주문·체결·원장 **분포 유지**(균등으로 뭉개지 않는다)
      → **검증**: 세 크기로 시드한 뒤 **fixture 행을 해시해 세 값이 같음을 단언**하는 테스트
- [ ] **과거 이력만으로 크기 차이를 만든다** — `orders` / `trades` / `ledger_entries` / outbox의
      **종결된 과거 행만** 추가. active fixture는 건드리지 않는다
      → **검증**: 세 크기에서 `status IN ('PENDING','PARTIAL')` 행 수가 **동일**
- [ ] 세 크기 정의: **초기**(과거 이력 0) / **중간** / **32-B 종료 규모**
      → **검증**: 각 크기에서 실제 행 수가 목표의 ±1% 이내

> **⚠ "초기"는 빈 DB가 아니다.** fixture는 있고 **과거 거래 이력만 없는** DB다.

---

## Phase 2 — `TEMPLATE` 스냅샷 복원

- [ ] 크기별 template DB를 1회 생성하고, 셀 시작마다
      `CREATE DATABASE <cell> TEMPLATE <size>` 로 복제
      → **검증**: 복제 직후 행 수와 fixture 해시가 template과 **정확히 일치**
- [ ] 복제 전 template DB에 **접속이 없음**을 보장(있으면 `CREATE DATABASE ... TEMPLATE`이 실패한다)
      → **검증**: 6셀을 연속 복제해도 실패 0
- [ ] 복제 후 **`ANALYZE`**
      → **검증**: `pg_stat_user_tables.last_analyze`가 복제 이후 시각

---

## Phase 3 — terminal 실작업 검증 게이트 ⚠ 이것이 없으면 측정이 무의미하다

`ProcessOrderCancellation` · `CompleteMarketOrder`는 **이미 종결된 주문이면 멱등 no-op**이다.
no-op을 재면 아주 빠른 숫자가 나오지만 **아무것도 재지 않은 것**이다.

- [ ] 반복마다 다음을 확인하고, **하나라도 어긋나면 그 셀의 결과를 버린다**
      - 시작 상태가 **`PENDING` 또는 `PARTIAL`**
      - **실제 locked balance가 존재**
      - terminal 이후 **상태 · 잔고 · 원장 행이 실제로 변경됨**
      - **no-op · failure · fallback 수가 전부 0**
      - trade는 **매번 새 idempotency key로 실제 커밋됨**
      → **검증**: **일부러 이미 종결된 주문을 넣으면 게이트가 실패하는** 음성 테스트를 함께 작성한다.
      게이트가 통과만 하는 것은 게이트가 아니다

---

## Phase 4 — 실행 러너

**위치**: `internal/service/settlement_diagnostic_test.go`

- [ ] 세 종류를 **각각 별도로** 측정 — 합산하지 않는다

| 종류 | 함수 | 위치 |
|---|---|---|
| trade batch | `(*SettlementService).SettleTradeBatch` | `internal/service/settlement_batch.go:31` |
| 취소 terminal | `(*OrderService).ProcessOrderCancellation` | `internal/service/order_service.go:336` |
| 시장가 완료 terminal | `(*OrderService).CompleteMarketOrder` | `internal/service/order_service.go:373` |

- [ ] 고정 조건: **batch 크기 정확히 3**(32-B hold 평균 2.549에 가장 가까운 정수) ·
      terminal 비율 **53~56%** · **고정 seed** · **고정 반복 횟수**
      → **검증**: 결과 파일에 실제 batch 크기 분포가 기록되고 **전부 3**
- [ ] **고정 warm-up 후 측정 시작**(warm-up은 결과에서 제외)
      → **검증**: warm-up 구간이 결과에 포함되지 않음을 파일로 확인
- [ ] 동시성 N=1 / N=8
      → **검증**: N=8에서 실제로 8개가 동시 실행됨(진행 중 카운터 최댓값 확인)

---

## Phase 5 — 수집

- [ ] 종류별 **p50 / p95 / p99**, 처리율
- [ ] `pg_stat_statements` **호출 수 · 평균 · 누적 시간** (셀 시작 시 `pg_stat_statements_reset()`)
- [ ] **lock wait** · **WAL bytes** · **buffer hit / read** · **DB CPU**
- [ ] **실패 · fallback · 정합성 위반**
      → **검증**: 6셀 전부에서 값이 수집되고, **정합성 위반이 0**. 0이 아니면 **성능 수치를 읽지 않는다**
- [ ] 결과를 **`_workspace/settlement-cost-diagnostic/`** 에 JSON/CSV로 저장
      → **검증**: 6개 셀 파일 + 환경 메타(Postgres 버전·하드웨어·설정) 파일이 생성된다

---

## Phase 6 — 정적 DB 조사 (Phase 1~5와 병행 가능)

- [ ] 테이블·인덱스 크기, **인덱스 정의와 사용량**(`pg_stat_user_indexes`)
- [ ] autovacuum · dead tuple · checkpoint · WAL 통계
- [ ] 정산 SQL의 **`EXPLAIN (ANALYZE, BUFFERS, WAL)`**
      → **검증**: 32-B 종료 규모 DB에서 **seq scan이 뜨는 정산 쿼리 목록**을 산출한다(없으면 "없음"으로 기록)

---

## Phase 7 — 판정

| 결과 | 지목되는 원인 |
|---|---|
| **N=1부터** 크기에 따라 느려짐 | 데이터 크기 · 인덱스 · WAL · 쓰기 증폭 |
| **N=1 평평, N=8만** 느려짐 | **행 락 · 동시성 경합** |
| **batch만** 느려짐 | trade 정산 SQL |
| **terminal만** 느려짐 | 완료 · 취소 쿼리 |
| **전부 평평** | 실제 부하의 오더북 · job mix · 사용자 상태 변화 |

### 로컬 결과의 해석 범위 — 넘기지 않는다

> **로컬 실험은 논리적 데이터 크기와 동시성 효과를 선별한다.
> 환경 의존적인 WAL · 스토리지 · autovacuum 원인의 최종 기각은 GCP에서만 가능하다.**

- 로컬에서 **기울기가 나타남** → 논리적 데이터 크기 · SQL · 락 후보를 **강하게 지지**
- 로컬에서 **평평함** → **합성 cardinality만으로는 재현되지 않았다**는 뜻일 뿐
- **평평하다고 GCP의 WAL · 디스크 · checkpoint 원인을 기각할 수 없다** ⚠

### 평평할 때의 승격 — `pg_dump`가 아니다

`pg_dump`/`pg_restore`는 논리적 행·분포·관계는 보존하지만 **dead tuple · bloat · autovacuum 진행
상태 · buffer cache · checkpoint/WAL 누적 · 물리적 인덱스 배치를 보존하지 않는다.**
따라서 승격은 둘 중 하나다 — **(1) 실제 부하 직후 GCP DB에서 live profiling**,
**(2) persistent disk / 물리 DB 스냅샷 복제.**

---

## 종료

- [ ] **결과 문서** — `docs/benchmarks/33-<날짜>-settlement-cost-growth-diagnostic.md`
      (환경 · 합성 시드의 한계 · 판정 · **말할 수 없는 것**을 반드시 포함)
- [ ] `docs/refactor/README.md` · `docs/ENGINEERING-SUMMARY.md` 갱신
- [ ] commit + 푸시 + **CI green** — **진단 테스트가 CI에서 skip되는 것까지 확인**

---

## 범위 밖

- **수정** — 이 계획은 **원인을 좁히는 것까지**다
- **리컨실리에이션 실행시간** — 사용자·지갑별 LATERAL 집계라는 별도 쿼리다. **독립 진단 항목**
- **`N=16`** — 이 진단이 **동시성 부족을 가리킬 때만** 간다
- 축 2 매칭 quantum 계약
