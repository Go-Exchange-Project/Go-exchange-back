# DB 인스턴스 4→8 vCPU급 증설 GCP 측정 runbook (31번)

> **For agentic workers:** 측정 runbook이다. Phase 0(preflight) → Phase 1(DB8/N4 안전 기준선) →
> **1차 안전 게이트** → Phase 2(DB8/N8) → Phase 3(최종 비교 판정) → Phase 4(문서·종료)
> 순서로 체크박스로 진행한다. **무결성·리컨실리에이션은 별도 Phase가 아니라 각 실행 안에서
> 그 실행의 TRUNCATE 이전에 수행한다.**
> **이 문서는 계획만 담는다 — GCP 실행은 별도 측정 세션에서 수행한다.**

**Goal:** 30번이 남긴 질문 — **"DB 인스턴스를 키우면 N=8의 처리율 이득을 응답 가용성 계약을
깨지 않고 회수할 수 있는가"** — 를 판정한다.

**핵심 계약 (이 문서 전체를 지배한다):**

> **DB8/N8의 성공은 1초 계약 초과 0건과 모든 안전 게이트 통과를 먼저 요구한다.
> 그 후 DB8/N4 대비 업무 성공률·정산 처리율 상승이 확인되어야 채택 후보가 된다.
> p95는 run-to-run 분산이 확인됐으므로 설명용으로만 보고한다.**

**선행**: [30번 결과](../../benchmarks/30-2026-07-31-settlement-concurrency-sweep.md) ·
[30번 runbook](2026-07-31-settlement-concurrency-sweep.md)(하니스 원본) ·
[29번 결과](../../benchmarks/29-2026-07-30-per-order-fence-gcp-remeasurement.md)

---

## 측정 기준 SHA (고정)

```
82b4d7f6383676b11951fb81e8bea5e24eb73f59
```

29·30번과 **동일한 바이너리**다. 이후 커밋은 전부 문서 전용이며 실행 전
`git diff 82b4d7f..HEAD -- '*.go'`가 비어 있음을 재확인한다.
**이번 사이클에서도 코드는 변경하지 않는다** — 계측이 부족하다고 판단되면 중단·보고한다.

## 실행 구조 — 새로 도는 것은 2개뿐

| # | 구성 | 출처 | 목적 |
|---|---|---|---|
| 기준 | **DB4 / N=4** | **30번 재사용**(새로 돌리지 않음) | 기존 채택 구성 |
| 기준 | **DB4 / N=8** | **30번 재사용** | 기각된 구성(1초 초과 568건) |
| **1** | **DB8 / N=4** | 이번에 실행 | **DB만 바꾼 안전 기준선** |
| **2** | **DB8 / N=8** | 이번에 실행(1차 게이트 통과 시) | **실제 가설 검증** |

**DB4 계열을 재실행하지 않는 근거**: 처리율 계열이 29·30번에서 안정적으로 재현됐고
(30번 N=4가 29번을 최대 편차 −0.9%로 재현), 1초 초과 건수도 **두 번 연속 0건**이었다.

**단, DB8/N4의 성능 개선이 작아도 중단하지 않는다.** N=4는 이미 worker가 포화(busy 99.97%)인
반면 DB CPU에는 여유가 있었으므로(hold `us+sy` p95 61) **DB만 키워도 처리율이 거의 안 변할 수 있다.**
그 결과로 가설을 기각하면 오판이다. 1단계의 목적은 **회귀 여부 확인과 안전 기준선 확보**다.

## 비교 조건

**동일 고정**: 서버 `e2-highcpu-4` · load-gen 2대 `e2-standard-8`(A `OFFSET=0` / B `OFFSET=5000`,
각 `TOTAL_USERS=5000`) · 같은 `order-spike-availability.js` · 같은 stage·VU·주문 비율 ·
`LOAD_START_AT_MS` 배리어 · `--summary-export` · `GOEXCHANGE_DB_MAX_OPEN_CONNS` 25 ·
워터마크 0.75 · 파티션 10

**바꾸는 것**

```
DB VM 머신 타입:  e2-highcpu-4  →  e2-highcpu-8
GOEXCHANGE_SETTLEMENT_CONCURRENCY:  4 (Phase 1)  →  8 (Phase 2)
```

> **⚠ 이것은 "DB CPU 단독 실험"이 아니다.** 머신 타입 변경은 **인스턴스 ID와 디스크를 유지**하지만
> **실행 호스트가 바뀌고 CPU와 RAM이 함께 증가**한다(4 vCPU/4 GB → 8 vCPU/8 GB — **실행 시 실제
> 값을 확인해 기록**할 것). 따라서 **31번의 결론은 "DB 인스턴스 증설 효과"로 한정**한다.
> CPU만 분리하려면 동일 메모리의 custom machine type이 필요하며 이번 범위 밖이다.

**15번의 2.46배(2→4 vCPU)는 가능성을 지지하는 선례일 뿐 예상 배율이 아니다.**

## 캐시·프로세스 상태 공정성 (필수)

**첫 실행만 cold cache이고 두 번째가 warm cache면 N 효과와 캐시 효과가 섞인다.**
DB8/N4와 DB8/N8 **각각의 실행 전에 동일한 절차**를 거친다:

```
1. DB VM 재기동 (양쪽 실행 모두 — 한쪽만 재기동하면 안 된다)
2. DB 기동 완료 확인
3. 10테이블 TRUNCATE
4. 서버 재기동 (의도한 concurrency 기동 로그 확인)
5. CPU 수집기 기동            ← 여기서 한 번만 띄운다
6. k6 전체 실행 (setup 포함)   ← setup과 부하 스테이지는 한 프로세스다
7. 아래 "각 실행의 종료 순서" 8단계
```

**두 실행의 1~7단계가 동일해야 한다.** 순서를 바꾸거나 한쪽만 건너뛰면 비교가 오염된다.

### 각 실행의 종료 순서 (필수 — 순서를 바꾸면 결과를 잃는다)

**`ReconciliationWorker`를 1회 강제 실행하려면 서버를 재기동해야 하는데, 재기동은 Prometheus
프로세스 카운터와 히스토그램을 전부 초기화한다.** "드레인 → 리컨실리에이션"만 적으면 실행자가
재기동 뒤에 final metrics를 저장해 **workload 결과를 통째로 잃는다.** 순서를 못박는다:

```
1. 부하 종료 → 드레인 → 모든 파티션 outstanding_jobs = 0 확인
2. ★ 재기동 전에 final /metrics · 서버 로그 · k6 결과(stdout·summary) 저장
3. workload 무결성 검사 — dispatch/execution count, batch 합계, terminal count,
   k6↔DB 주문 수, 풀 구간 델타 등 (전부 2번에서 저장한 metrics 기준)
4. 서버 재기동 → ReconciliationWorker 1회 실행
5. reconciliation_last_run_timestamp_seconds가 이번 실행 이후 값인지 확인 + 4항목 0 확인
6. ★ post-restart metrics는 reconciliation 증거로만 쓴다 — 처리율·지연 계산에 쓰지 않는다
7. CPU 수집기 종료 (kill → kill -0 실패 확인)
8. ★ 그 뒤에만 다음 실행을 위한 TRUNCATE
```

**2번과 4번의 순서가 뒤집히면 그 실행의 성능·무결성 근거가 사라진다.**
**8번이 앞당겨지면 리컨실리에이션이 빈 DB를 검사한다**(30번에서 실제로 일어난 일이다).

> **수집기는 이 절차에서만 기동한다.** k6의 `setup()`(계정 생성·펀딩)과 부하 스테이지는
> **한 프로세스 안에서 이어지므로** "setup 완주 후 외부에서 수집기 기동"은 실행할 수 없다.
> **setup 구간과 부하 구간의 분리는 수집기를 두 번 띄우는 것이 아니라 UTC 구간으로 집계**해서 한다
> (Phase 0 Step 1-f에서 각 구간의 UTC 시작·종료를 기록하는 이유가 이것이다).

---

## 이번에 수집하는 것 (30번과 동일 방식)

### CPU 시계열 (서버·DB **분리**, 5초 간격)

```bash
loginctl enable-linger
FILE=~/cpu-<role>-<phase>.txt     # role=server|db, phase=db8n4|db8n8
stdbuf -oL vmstat -t 5 > "$FILE" & echo $! > "$FILE.pid"
```

**집계 규칙** — `us+sy`와 `100-id`는 동등하지 않다:

| 판단 대상 | 컬럼 |
|---|---|
| **CPU 병목** | **`us + sy`** |
| **스토리지 대기** | **`wa`** |
| **호스트 경합(VM steal)** | **`st`** |

**`100-id`를 CPU 사용률로 쓰지 않는다.** 세 값을 각각 **max / p95 / median**으로 보고한다.
**`vmstat`의 첫 데이터 행(부팅 이후 누적 평균)은 분포 계산에서 제외**한다.

**수집기 수명 관리**:

```
시작 전:  pgrep -af '[v]mstat -t 5'  → 결과 없음
시작:     단계별 고유 파일 + PID 파일
종료:     kill "$(cat "$FILE.pid")"
종료 확인: kill -0 실패 + pgrep 결과 없음 + 파일 크기·마지막 타임스탬프
```

### 풀 지표 구간별 델타

`go_sql_in_use_connections` / `go_sql_wait_count_total` / `go_sql_wait_duration_seconds_total`을
**setup · ramp · hold · burst · recovery** 구간으로 나눠 — 누적 카운터는 **델타**,
게이지는 구간별 **max / p95 / median**(단일 시점으로 대표하지 않는다).

30번 기준선: setup에 대기 집중(`wait_count` ~26.6k), **부하 스테이지 구간 델타 0**,
부하 구간 `in_use` 최대 12/25.

---

### Phase 0: Preflight (저부하 1~2분 — 실패 시 전체 실행 금지)

- [ ] **Step 1: 코드 동일성** — 배포 커밋 = `82b4d7f`, `git diff 82b4d7f..HEAD -- '*.go'` 비어 있음
- [ ] **Step 1-a: DB 머신 타입 확인·기록** — `gcloud compute instances describe`로 **변경 후 실제
  머신 타입·vCPU·RAM**을 확인해 기록. 인스턴스 ID·디스크가 유지됐는지도 확인
- [ ] **Step 1-b: DB 접속 유효성** — 현재 `go-exchange-back` 환경이 canonical source.
  **과거 `bench-*` 값을 복사하지 않는다.** 값 비교가 아니라 **실제 연결 성공**으로 검증.
  **비밀번호 값·hash·fingerprint를 로그·문서·아티팩트에 출력하지 않는다**
- [ ] **Step 1-c: 스키마 확인 (TRUNCATE보다 먼저)** — `failed_order_cancellations`(존재 +
  `order_id` uniqueIndex + `retry_count` default 0 + CHECK `>= 0`), `failed_market_completions`
  (default 0, 신 제약 존재 + 구 제약 `_retry_count_positive` 부재). **CHECK/default까지** 확인
- [ ] **Step 1-d: Secret preflight** — 기존(폐기) 토큰 **거부** 확인 / 현재 secret **인증 통과** 확인 /
  **값·hash·fingerprint 미출력** / 실패 시 **부하 실행 금지**
- [ ] **Step 1-e: CPU 수집기** — 시작 전 잔존 0개 → `stdbuf -oL` 실시간 기록 → SSH 끊어도 생존 →
  `kill -0` 실패까지 확인
- [ ] **Step 1-f: 시간 정렬** — **서버·DB·load-gen 2대의 UTC 시계 차이 ≤ 1초**(`date -u`).
  **각 구간의 UTC 시작·종료 시각을 기록**한다(없으면 CPU를 구간에 귀속시킬 수 없다).
  두 load-gen 시작 skew ≤ 1초
- [ ] **Step 2: 저부하 실행** 후 전부 확인:
  - 기동 로그가 **의도한 `concurrency=N`** 출력
  - `go_sql_*` 3종 · `settlement_terminal_wait_seconds{kind}` · `settlement_outstanding_jobs{partition}` ·
    `settlement_quarantined_orders{partition}` · `settlement_dependency_record_failed_total` ·
    `settlement_duplicate_terminal_total` 노출
  - **dispatch wait count = execution count** — **저부하 종료 후 드레인이 끝나고
    `settlement_outstanding_jobs`가 모든 파티션에서 0인 최종 스냅샷에서만** 검사(도중에는
    in-flight 때문에 어긋나는 것이 정상)
  - 기존 SLI·정합성 검사 정상
- [ ] **Step 3: 게이트** — 하나라도 어긋나면 **전체 실행 금지**

---

### Phase 1: DB8 / N=4 — 안전 기준선

`GOEXCHANGE_SETTLEMENT_CONCURRENCY=4`. **캐시 공정성 절차 1~8을 그대로 수행.**

> **⚠ 실행 순서가 정합성 근거를 결정한다.** 무결성·리컨실리에이션을 뒤로 미루면
> **Phase 2의 TRUNCATE가 DB8/N4의 데이터를 지운 뒤에 실행되어 빈 DB를 검사**하게 된다 —
> 30번에서 N=4 리컨실리에이션을 잃은 원인이 정확히 이것이다. **각 실행의 검증은 그 실행 안에서
> 끝낸다.**

```
DB8/N4 실행 → 드레인 → DB8/N4 무결성·리컨실리에이션 → DB8/N4 안전 게이트
  → (통과한 경우에만) DB 재기동·TRUNCATE → DB8/N8 …
```

- [ ] **Step 1: 실행** — 캐시 공정성 절차 1~6(수집기 기동 후 k6 전체 실행)
- [ ] **Step 2: 종료 순서 8단계** — **"각 실행의 종료 순서"를 그대로 수행**한다.
  특히 **재기동 전에 final metrics를 저장**(2번)하고, 아래 "무결성 체크리스트"를 그 저장본 기준으로
  수행(3번)한 뒤에 재기동·리컨실리에이션(4~5번)으로 넘어간다. **TRUNCATE는 8번이다**
- [ ] **Step 3: 회귀 확인** — 30번 DB4/N4 대비

| 지표 | 30번 DB4/N4 | 판정 |
|---|---|---|
| `sli_order_response_availability` | 100.00% (0/1,071,434) | **100%·0건 유지** |
| `sli_cancel_success` | 100.00% | 유지 |
| 정합성·fallback·quarantine·duplicate | 0 | 0 유지 |
| settled trades/s (hold) | 368.35 | **회귀 없음**(참고) |
| `sli_order_business_success` | 22.43% | **회귀 없음**(참고) |

> **개선이 작아도 중단하지 않는다.** 이 단계의 목적은 **안전 기준선 확보**이지 성능 증명이 아니다.
> **회귀가 있으면** 중단하고 원인을 규명한다.

- [ ] **Step 4: 1차 안전 게이트** — 아래 "1차 안전 게이트" 기준을 DB8/N4에 적용해 통과 확인.
  **실패 시 Phase 2 진행 금지.**
- [ ] **Step 5: 원본 보존** — `_workspace/db-scaleup-31/db8n4/`에 스냅샷·k6 stdout·
  `summary-*.json`·`vmstat`·구간 UTC 시각·리컨실리에이션 결과. **토큰·외부 IP 제거.**

---

### Phase 2: DB8 / N=8 — 실제 가설 검증

**Phase 1의 안전 게이트를 통과한 경우에만 진행한다.** 여기서 처음으로 DB 재기동·TRUNCATE를 한다.

`GOEXCHANGE_SETTLEMENT_CONCURRENCY=8`. **캐시 공정성 절차 1~8을 Phase 1과 동일하게 수행.**

- [ ] **Step 1: 기동 로그에서 `concurrency=8` 확인**
- [ ] **Step 2: 실행** — Phase 1과 같은 절차·같은 수집
- [ ] **Step 3: 종료 순서 8단계** — Phase 1 Step 2와 동일. **재기동 전 final metrics 저장 →
  무결성 검사 → 재기동·리컨실리에이션 → 수집기 종료 → TRUNCATE** 순서를 지킨다
- [ ] **Step 4: 1차 안전 게이트** — DB8/N8에 적용. **실패 시 처리율 수치를 읽기 전에 기각**
- [ ] **Step 5: 원본 보존** — `_workspace/db-scaleup-31/db8n8/`에 동일 구성

---

### 무결성 체크리스트 (종료 순서 3번·5번에서 사용 — 해석 **전에** 검증)

**두 실행 각각에 대해, 그 실행의 TRUNCATE 이전에 수행한다.**
**workload 항목은 재기동 전에 저장한 final metrics 기준**이고, 리컨실리에이션 항목만 재기동
이후 값을 쓴다:

- [ ] `settlement_terminal_wait_seconds{kind}` count = 정상 처리된 terminal 수
- [ ] **dispatch wait count = job execution count** — 드레인 완료·모든 파티션 `outstanding_jobs`=0인
  **최종 스냅샷에서만**
- [ ] batch attempt count ≥ logical batch job count, 차이가 실제 retry 로그와 일치
- [ ] settlement batch의 trade 합계 = settled trade 증가량
- [ ] k6 주문 합계 = 서버·DB 주문 합계
- [ ] **리컨실리에이션(ledger_wallet·asset_conservation)을 이 실행의 최종 데이터에 대해 완주**시켜
  4항목 0 확인. `ReconciliationWorker` 기본 주기가 1시간이므로 재기동으로 1회 강제 실행하되,
  **반드시 TRUNCATE 이전**이어야 한다(30번에서 N=4 쪽을 놓친 원인이 TRUNCATE 후 재기동이었다)
- [ ] CPU 시계열이 부하 구간 전체를 덮고, **첫 데이터 행이 제외**됐는지
- [ ] 각 구간의 **UTC 시작·종료 기록**이 CPU·Prometheus 스냅샷과 정렬되는지
- [ ] 두 실행의 수집기 파일이 **단계별로 분리**됐는지
- [ ] 하나라도 불일치면 **계측 문제로 보고 판정 보류**

---

### Phase 3: 최종 비교 판정 — **1차 안전이 2차 효과보다 먼저**

> 1차 안전 게이트는 **각 실행 직후**(Phase 1 Step 4 / Phase 2 Step 4)에 이미 적용한다.
> 여기서는 두 실행을 **비교**해 2차 효과를 판정한다.

#### 1차 안전 게이트 (하드 계약 — 판정값은 **0건**)

| 항목 | 기준 |
|---|---|
| `sli_order_response_availability` | **= 100%** |
| `duration > 1,000ms` 요청 | **A 0건 · B 0건 · 합계 0건** |
| 취소 인프라 실패(500) | **0** |
| 정합성 위반 | **0** |
| `settlement_batch_fallbacks_total` | **0** |
| quarantine 잔존 · duplicate terminal | **0** |

- **count와 비율을 함께 기록**한다(표본이 약 107만 건으로 거의 고정이므로 비율만으로는 오독하기 쉽다).
- **하드 계약의 판정값은 0건이다.** 30번의 **568건 → 10건처럼 크게 줄어도 개선은 맞지만 채택은
  실패**다. 부분 개선을 통과로 재분류하지 않는다.
- [ ] **한 항목이라도 실패하면 처리율 수치를 읽기 전에 기각.**

#### 2차 효과 게이트 (1차 통과 시에만)

**단순 `>`는 노이즈도 통과시킨다**(22.43% → 22.44%도 "상승"이다). 판정선을 수치로 고정한다:

| 항목 | 기준 (DB8/N4 대비) |
|---|---|
| settled trades/s (hold) | **+10% 초과 상승** |
| `sli_order_business_success` | **+2%p 초과 상승** |
| 절대 하한 | DB8/N8이 **30번 DB4/N8(467.29 trades/s · 26.62%)을 밑돌지 않을 것** |

> **이 값은 통계적 신뢰구간이 아니다.** [30번 runbook](2026-07-31-settlement-concurrency-sweep.md)의
> 재현 허용폭(처리율 ±10%, 성공률 ±2%p)을 **운영상 materiality 기준으로 재사용한 실무 판정선**이다.
> 지연과 달리 처리율 계열은 29·30번에서 재현이 확인됐으므로(최대 편차 −0.9%) 이 폭 밖의 변화는
> 실질적 차이로 읽을 수 있다.

**"기존 이득이 사라지지 않았는지"를 `+16.9%`·`+2.19%p`(= 30번 이득 −허용폭)로 고정하지 않은 이유**:
**DB8/N4 기준선 자체가 올라갈 수 있다.** 예컨대 DB8이 N=4를 368 → 420으로 올리고 N=8이 480이 되면
증분은 +14.3%로 그 기준에 미달하지만, **총 처리율은 30번 DB4/N8(467)보다 높고 안전하다** — 이를
실패로 판정하면 오판이다. 그래서 **증분은 materiality(+10%/+2%p)로 보고, 이득 유지 여부는 절대
하한(30번 DB4/N8 미달 금지)으로 따로 확인**한다.

> **0건만으로는 충분하지 않다.** 안전해졌지만 처리량 이득이 없다면 **DB 증설로 N=8을 채택할 이유가
> 없다.** 그 경우 결론은 "DB 증설이 N=8을 안전하게 만들었으나 이득이 없어 채택하지 않는다"이다.

#### 보고만 하고 판정에 쓰지 않는 것

- **p90 · p95 · max** — [30번 한계](../../benchmarks/30-2026-07-31-settlement-concurrency-sweep.md)에서
  **동일 구성의 run-to-run p95 분산이 약 35%**(29번 120.5ms vs 30번 N=4 162.7ms)로 확인됐다.
  **단독 통과·실패 기준으로 사용하지 않는다.** 설명용 보조 지표다.
- CPU `us+sy`/`wa`/`st`, 풀 구간 델타, terminal wait, 배치 크기 — **어디가 새 병목인지 설명**하는 데 쓴다.

---

### Phase 4: 문서 + 안전 종료

- [ ] **문서** — `docs/benchmarks/31-<날짜>-db-instance-scaleup.md`:
  머신 타입·vCPU·**RAM 변화 기록** / 캐시 공정성 절차 준수 여부 / 30번 재사용 기준값 명시 /
  **1차 안전 게이트 결과(count + 비율)** / 2차 효과 게이트 / 구간별 계산표 / CPU max·p95·median /
  한계.
- [ ] **결론 표현을 "DB 인스턴스 증설 효과"로 한정**(CPU 단독이 아님, RAM 동반 증가).
- [ ] **README**·완료 문서 갱신, commit + 푸시 + **CI green**.
- [ ] **VM 4대 정지 후 `gcloud compute instances list`로 `TERMINATED` 확인** —
  "정지 명령을 실행했다"가 아니라 **조회 결과**가 완료 조건이다.

```bash
gcloud compute instances stop goexchange-stress-server goexchange-stress-db goexchange-stress-load-gen goexchange-stress-load-gen-b --zone asia-northeast3-a
```

- [ ] **DB 머신 타입을 원상 복구할지 결정** — 채택하면 유지, 기각하면 `e2-highcpu-4`로 되돌린다
  (비용). 결정을 문서에 기록한다.

---

## 판정이 나온 뒤

| 결과 | 다음 |
|---|---|
| **1차 통과 + 2차 통과** | **DB8 + N=8 채택 후보.** 새 병목을 CPU·풀·배치로 재판정 |
| **1차 통과 + 2차 실패** | DB 증설이 안전은 회복했으나 이득 없음 → **채택하지 않는다.** 배치 파편화 진단으로 이동 |
| **1차 실패** | DB 증설로도 1초 계약을 못 지킨다 → **N=4 유지**, 병목을 다시 판정 |
| **DB8/N4에서 회귀** | 인스턴스 변경 자체가 문제 — 원인 규명 전까지 진행 금지 |

## 한계 (문서에 반드시 남길 것)

- **DB4 기준값은 다른 세션·다른 DB VM 호스트에서 얻은 값이다.** 머신 타입 변경은 실행 호스트를
  바꾸므로 DB4→DB8 비교에는 **호스트 분산이 섞여 있다.** 이번 설계로는 분리할 수 없다.
- **CPU와 RAM이 함께 증가**하므로 "CPU 증설 효과"가 아니라 **"인스턴스 증설 효과"** 다.
- **지연(p95)의 run-to-run 분산이 약 35%** 로 확인됐다 — 지연 개선·회귀를 p95로 주장할 수 없다.

## 범위 밖

- 배치 파편화 수정(`collectTradeBatch` terminal 건너뛰기 / terminal batching)
- `settlement_job_execution_seconds`의 job 종류별 라벨 — **코드 변경이므로 이번 사이클 아님**
- `GOEXCHANGE_DB_MAX_OPEN_CONNS` 조정 — 30번에서 **불필요**로 확인(부하 구간 `in_use` 최대 12/25)
- 서버 VM 증설 — 30번에서 서버는 여유(median 72.5)
- 축 2(매칭 quantum) — 독립 판정
