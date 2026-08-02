# 고정 부하 정산 비용 상승 진단 하니스 — 구현 계획

> **설계**: [2026-08-02-settlement-cost-growth-diagnostic-design.md](../specs/2026-08-02-settlement-cost-growth-diagnostic-design.md)
> **선행 결과**: [32-B](../../benchmarks/32-2026-08-01-capacity-boundary-session-b.md)
> **진행 상태 (2026-08-02)**: **Phase 0~6 구현 완료, 하니스 동작 확인.**
> **6셀 실측과 Phase 7 판정은 다음 세션이다.**
>
> 구현: `internal/service/settlement_diagnostic_seed_test.go`(시드 + TEMPLATE) ·
> `internal/service/settlement_diagnostic_test.go`(러너 + 게이트 + 수집).
> 결과 스키마 예시: `_workspace/settlement-cost-diagnostic/cell-full-n{1,8}.json`.
>
> **실행 방법** — 두 환경변수가 **모두** 있어야 돈다:
> ```
> GOEXCHANGE_TEST_DATABASE_DSN=... GOEXCHANGE_RUN_SETTLEMENT_DIAGNOSTIC=1 \
>   go test ./internal/service/ -run TestSettlementDiagnostic -timeout 60m
> ```
> `GOEXCHANGE_SETTLEMENT_DIAGNOSTIC_SCALE=smoke`를 주면 1/200 규모로 빠르게 돈다(하니스 점검용).
>
> **다음 세션이 알아야 할 시간 예산**: template 3개 시드가 **약 2분**
> (initial 1.3s / mid 35~73s / full 69~81s), 셀 1개 실행이 **15~30초**.
> Postgres에 `goex_diag_tmpl_{initial,mid,full}` 데이터베이스가 남아 있다(합계 약 1.8GB).
> 재실행 시 자동으로 다시 만들어지므로 지워도 된다.

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

- [x] 로컬 Docker Postgres 기동, `GOEXCHANGE_TEST_DATABASE_DSN` 설정
      → **검증**: 기존 `go test ./internal/service/ -run Integration`이 통과한다
      → 통과(repository 44s / service 81s). 컨테이너 `goexchange-postgres-test`(postgres:16.14, 포트 55432)
- [x] `pg_stat_statements` 확장 활성화 여부 확인
      → **검증**: `SELECT * FROM pg_stat_statements LIMIT 1`이 에러 없이 돈다.
      없으면 `shared_preload_libraries`에 추가 후 재기동
      → **없었다.** `ALTER SYSTEM SET shared_preload_libraries='pg_stat_statements'` + 컨테이너 재기동으로 활성화.
      셀 DB마다 `CREATE EXTENSION IF NOT EXISTS`를 걸어 template 복제본이 물려받게 했다
- [x] 32-B 종료 규모 확정치를 상수로 옮긴다: `orders` **546,783** · `ledger_entries` **1,886,343** ·
      `trades` **311,207**
      → **검증**: 32-B 문서의 무결성 교차검증 표와 일치
      → 일치. `trade_outbox_events` **459,346**도 함께 넣었다(계획 본문이 outbox를 크기 축에 포함한다)

---

## Phase 1 — 합성 시드 빌더

**위치**: `internal/service/settlement_diagnostic_seed_test.go`

- [x] **고정 fixture 생성** — 32-B와 동일한 사용자 수, 고정 seed
      - 측정 대상 **active 주문·지갑**은 세 크기에서 **완전히 동일**
      - 사용자별 주문·체결·원장 **분포 유지**(균등으로 뭉개지 않는다)
      → **검증**: 세 크기로 시드한 뒤 **fixture 행을 해시해 세 값이 같음을 단언**하는 테스트
      → `TestSettlementDiagnosticSeedIsolatesSizeAxis` 통과. 사용자 750명, seed 20260802,
      해시 `c23007f1614177f801d8d777ff1c546d`가 세 크기에서 동일.
      분포는 LCG + `power(x, 1.5)`로 기울여 균등을 피했다
- [x] **과거 이력만으로 크기 차이를 만든다** — `orders` / `trades` / `ledger_entries` / outbox의
      **종결된 과거 행만** 추가. active fixture는 건드리지 않는다
      → **검증**: 세 크기에서 `status IN ('PENDING','PARTIAL')` 행 수가 **동일**
      → 세 크기 모두 **3,575행**으로 동일
- [x] 세 크기 정의: **초기**(과거 이력 0) / **중간** / **32-B 종료 규모**
      → **검증**: 각 크기에서 실제 행 수가 목표의 ±1% 이내
      → full 기준 orders 550,358(이력 546,783 + fixture 3,575) · trades 311,207 ·
      ledger 1,889,340(이력 1,886,340 + fixture 3,000) · outbox 459,346. 전부 ±1% 이내

> **⚠ 합성 시드가 원장 불변식을 지켜야 한다(구현 중 실제로 걸렸다).** `ReconciliationWorker`의
> `ledger_wallet` 검사는 지갑 잔고와 원장 델타 합계를 **tolerance 0**으로 비교하고,
> `asset_conservation`은 `Σ(available+locked) + 수수료 == Σ(DEV_FUND delta)`를 요구한다.
> 잔고만 심으면 **지갑 수만큼 위반**이 뜬다. 그래서 (1) fixture 지갑마다 DEV_FUND + ORDER_HOLD
> 원장을 만들어 잔고를 설명하고, (2) 과거 이력 원장은 **4행 그룹이 델타 합 0**이 되게 하고,
> (3) 과거 이력 체결의 **수수료를 0**으로 두었다. 시드 직후 정합성 0을 테스트가 단언한다.

> **⚠ "초기"는 빈 DB가 아니다.** fixture는 있고 **과거 거래 이력만 없는** DB다.

---

## Phase 2 — `TEMPLATE` 스냅샷 복원

- [x] 크기별 template DB를 1회 생성하고, 셀 시작마다
      `CREATE DATABASE <cell> TEMPLATE <size>` 로 복제
      → **검증**: 복제 직후 행 수와 fixture 해시가 template과 **정확히 일치**
      → `TestSettlementDiagnosticTemplateCloneMatchesTemplate` 통과
- [x] 복제 전 template DB에 **접속이 없음**을 보장(있으면 `CREATE DATABASE ... TEMPLATE`이 실패한다)
      → **검증**: 6셀을 연속 복제해도 실패 0
      → **6회 연속 복제 실패 0.** 두 겹으로 막았다 — template 시드는 subtest 스코프에서 열고 닫아
      `t.Cleanup`이 접속을 닫게 하고, 복제 직전 `pg_terminate_backend`로 잔여 접속을 끊는다
- [x] 복제 후 **`ANALYZE`**
      → **검증**: `pg_stat_user_tables.last_analyze`가 복제 이후 시각
      → orders·trades·ledger_entries·wallets 4개 테이블에서 확인

---

## Phase 3 — terminal 실작업 검증 게이트 ⚠ 이것이 없으면 측정이 무의미하다

`ProcessOrderCancellation` · `CompleteMarketOrder`는 **이미 종결된 주문이면 멱등 no-op**이다.
no-op을 재면 아주 빠른 숫자가 나오지만 **아무것도 재지 않은 것**이다.

- [x] 반복마다 다음을 확인하고, **하나라도 어긋나면 그 셀의 결과를 버린다**
      - 시작 상태가 **`PENDING` 또는 `PARTIAL`**
      - **실제 locked balance가 존재**
      - terminal 이후 **상태 · 잔고 · 원장 행이 실제로 변경됨**
      - **no-op · failure · fallback 수가 전부 0**
      - trade는 **매번 새 idempotency key로 실제 커밋됨**
      → **검증**: **일부러 이미 종결된 주문을 넣으면 게이트가 실패하는** 음성 테스트를 함께 작성한다.
      게이트가 통과만 하는 것은 게이트가 아니다
      → `TestSettlementDiagnosticGateRejectsAlreadyTerminalOrder` 통과. **정상 fixture는 통과하고**
      (이 단언이 없으면 "항상 실패하는 게이트"와 구분되지 않는다) 이미 `CANCELLED`인 주문을 넣으면
      시작 상태 게이트와 실작업 게이트가 **둘 다** 실패한다

**no-op 판별 기준은 상태가 아니라 `ORDER_RELEASE` 원장 1건이다.** 상태만 보면 "원래 CANCELLED였던
주문"과 구분되지 않는다. `ProcessOrderCancellation`은 no-op일 때도 **에러 없이 성공**하므로
지연만으로는 절대 알 수 없다.

> **검증 쿼리는 측정 구간 밖에 둔다.** 반복마다 DB를 조회하면 그 부하가 측정 대상 경합에 섞인다.
> 시작 상태는 실행 **전에**, 실작업은 실행 **후에** 일괄 조회하되 **반복 하나하나를 개별 판정**한다.

---

## Phase 4 — 실행 러너

**위치**: `internal/service/settlement_diagnostic_test.go`

- [x] 세 종류를 **각각 별도로** 측정 — 합산하지 않는다

| 종류 | 함수 | 위치 |
|---|---|---|
| trade batch | `(*SettlementService).SettleTradeBatch` | `internal/service/settlement_batch.go:31` |
| 취소 terminal | `(*OrderService).ProcessOrderCancellation` | `internal/service/order_service.go:336` |
| 시장가 완료 terminal | `(*OrderService).CompleteMarketOrder` | `internal/service/order_service.go:373` |

- [x] 고정 조건: **batch 크기 정확히 3**(32-B hold 평균 2.549에 가장 가까운 정수) ·
      terminal 비율 **53~56%** · **고정 seed** · **고정 반복 횟수**
      → **검증**: 결과 파일에 실제 batch 크기 분포가 기록되고 **전부 3**
      → `batch_sizes = {"3": 454}`. `b.N`을 쓰지 않고 warm-up 100 + 측정 1,000 job으로 고정.
      terminal 비율 0.55(= 605 job)이고, **취소/시장가 완료를 반씩 나눈 것은 32-B에서 분해되지 않은
      값이라 가정**이므로 결과 파일의 `terminal_split_note`에 그대로 적는다
- [x] **고정 warm-up 후 측정 시작**(warm-up은 결과에서 제외)
      → **검증**: warm-up 구간이 결과에 포함되지 않음을 파일로 확인
      → 측정 job 1,000건 중 batch 454 + cancel 278 + market 268 = **1,000**(warm-up 100건 미포함)
- [x] 동시성 N=1 / N=8
      → **검증**: N=8에서 실제로 8개가 동시 실행됨(진행 중 카운터 최댓값 확인)
      → `max_in_flight`가 N=1에서 1, N=8에서 8. 양쪽 모두 32-B 종료 규모에서 실행해 확인했다

---

## Phase 5 — 수집

- [x] 종류별 **p50 / p95 / p99**, 처리율
- [x] `pg_stat_statements` **호출 수 · 평균 · 누적 시간** (셀 시작 시 `pg_stat_statements_reset()`)
      → 상위 25개 statement를 `total_exec_time` 내림차순으로 저장(WAL bytes·buffer·temp 포함)
- [x] **lock wait** · **WAL bytes** · **buffer hit / read** · ~~**DB CPU**~~
      → lock wait은 50ms 간격으로 `pg_locks WHERE NOT granted`를 표본(최대·평균·표본 수).
      WAL은 `pg_wal_lsn_diff` 델타, buffer는 `pg_stat_database` 델타.
      **⚠ DB 호스트 CPU%는 로컬 Docker에서 수집하지 않는다** — 대신 `pg_stat_statements`의
      누적 실행시간(`db_exec_ms_total`)을 DB 측 작업량으로 쓴다. 결과 파일의 `db_cpu_note`에 명시.
      설계 (d)가 이미 "환경 의존 원인의 최종 기각은 GCP에서만 가능하다"고 못박은 범위 안이다
- [x] **실패 · fallback · 정합성 위반**
      → **검증**: 6셀 전부에서 값이 수집되고, **정합성 위반이 0**. 0이 아니면 **성능 수치를 읽지 않는다**
      → 수집·게이트 구현 완료. 실행한 2셀(full/N1, full/N8)에서 전부 0.
      **6셀 전부에 대한 확인은 다음 세션**이다. production `ReconciliationWorker.RunOnce()`를
      그대로 호출해 검사한다
- [x] 결과를 **`_workspace/settlement-cost-diagnostic/`** 에 JSON/CSV로 저장
      → **검증**: 6개 셀 파일 + 환경 메타(Postgres 버전·하드웨어·설정) 파일이 생성된다
      → 셀 파일에 환경 메타를 **함께** 넣었다(`env`: `postgres_version`, `shared_buffers`,
      `work_mem`, `max_wal_size`, `checkpoint_timeout`, `synchronous_commit`, `autovacuum`,
      `wal_level`, `max_connections`, `effective_cache_size`) — 셀마다 자족적이라 별도 메타 파일을 두지 않았다.
      **6개 파일은 다음 세션에 생긴다**(현재 2개: `cell-full-n1.json`, `cell-full-n8.json`)

---

## Phase 6 — 정적 DB 조사 (Phase 1~5와 병행 가능)

- [x] 테이블·인덱스 크기, **인덱스 정의와 사용량**(`pg_stat_user_indexes`)
      → 테이블 11개·인덱스 27개의 크기와 셀 구간 `seq_scan`/`idx_scan` 델타를 결과 파일에 저장
- [x] autovacuum · dead tuple · checkpoint · WAL 통계
      → `n_live_tup`/`n_dead_tup`/`autovacuum_count` + checkpointer 카운터.
      **`pg_stat_checkpointer`는 PG17부터**라 PG16에서는 `pg_stat_bgwriter`로 갈라진다(구현 중 걸렸다)
- [x] 정산 SQL의 **`EXPLAIN (ANALYZE, BUFFERS, WAL)`**
      → **검증**: 32-B 종료 규모 DB에서 **seq scan이 뜨는 정산 쿼리 목록**을 산출한다(없으면 "없음"으로 기록)
      → 정산 경로의 읽기 쿼리 5건을 **롤백하는 트랜잭션 안에서** EXPLAIN한다
      (`ANALYZE`는 실제로 실행하고 `FOR UPDATE`는 실제로 락을 잡는다).
      full 규모에서 seq scan이 뜬 쿼리: **`trades_sum_buyer_fee_by_buy_order` 1건**
      (`SumBuyerFeesByBuyOrderID` — `trades.buy_order_id`에 인덱스가 없다).
      나머지 4건은 인덱스를 탄다.

> **⚠ 작은 테이블의 seq scan은 근거가 아니다.** 플래너가 정상적으로 고르는 것이다.
> 이 목록은 **full 셀에서만** 판정에 쓴다 — 결과 파일의 `seq_scan_note`에 같은 경고를 넣었다.
>
> **⚠ EXPLAIN만으로 닫히지 않는다.** 정산 경로의 중심은 INSERT·UPDATE·행 락·COMMIT·WAL이고
> EXPLAIN은 읽기 계획만 잘 보여준다. **Phase 5의 크기·동시성 스윕이 본체다.**

---

## Phase 7 — 판정

> **아직 판정하지 않았다.** 아래는 하니스를 검증하며 **부수적으로 관측된 후보**일 뿐이고,
> 6셀 스윕 없이는 어느 칸에도 넣을 수 없다.
>
> `trades.buy_order_id`에 인덱스가 없어 `SumBuyerFeesByBuyOrderID`가 full 규모에서
> **seq scan**을 탄다(셀 구간 `trades` seq scan 681회, 누적 84.2M 튜플 읽기).
> 같은 셀에서 `market_terminal` p50이 118ms로 `cancel_terminal` 23.5ms보다 크게 높은데,
> `CompleteMarketOrder`만 이 쿼리를 호출한다. **하지만 이것은 크기 축 1점·동시성 축 2점의
> 단일 관측이다** — 초기·중간 크기와 비교해 기울기를 보기 전에는 "원인"이라고 쓰지 않는다.
> lock wait은 같은 셀에서 최대 1(평균 0.01)로 거의 없었다.

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
      (환경 · 합성 시드의 한계 · 판정 · **말할 수 없는 것**을 반드시 포함) — 6셀 실측 후
- [ ] `docs/refactor/README.md` · `docs/ENGINEERING-SUMMARY.md` 갱신 — 6셀 실측 후
- [x] commit + 푸시 + **CI green** — **진단 테스트가 CI에서 skip되는 것까지 확인**
      → 로컬에서 CI 두 job을 재현했다:
      `go test ./...`(DSN 없음) 통과 / `go test -run Integration -p 1 ./internal/repository ./internal/service`
      (DSN 있음, opt-in 없음) 통과. **진단 테스트 4개는 `-run Integration` 필터에 이름이 잡히지도 않고,
      잡히더라도 `GOEXCHANGE_RUN_SETTLEMENT_DIAGNOSTIC=1`이 없으면 skip한다**(두 겹).
      `go vet ./...` 무경고, `gofmt` 정리 완료

---

## 범위 밖

- **수정** — 이 계획은 **원인을 좁히는 것까지**다
- **리컨실리에이션 실행시간** — 사용자·지갑별 LATERAL 집계라는 별도 쿼리다. **독립 진단 항목**
- **`N=16`** — 이 진단이 **동시성 부족을 가리킬 때만** 간다
- 축 2 매칭 quantum 계약
