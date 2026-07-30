# 29번 (2026-07-30): 4차 축 1 — per-order fence + terminal durable defer GCP 재측정

## 요약(먼저 읽어야 할 것)

- **정합성·무결성 전항목 통과**(비협상 게이트). 계측 내부 무결성 9항목이 **오차 0으로 정확히 일치**했고,
  `settlement_duplicate_terminal_total` = 0, quarantine 잔존 0, fallback 0, 정합성 위반 0,
  취소 인프라 실패(500) 0건. 따라서 아래 성능 수치를 읽어도 된다.
- **판정: 파티션 전체 fence는 제거됐고, 바인딩 링크가 "fence 대기"에서 "정산 워커·DB 처리 용량"으로
  이동했다.** worker busy가 **13.1% → 99.95%**로 뛰었고 정산 처리율이 **≈109 → ≈372 trades/s**로,
  DB attempt 빈도가 **39.05 → 133.64 /s**로 올랐다. 28번이 지목한 "N=4 병렬도를 쓸 기회가 구조적으로
  주어지지 않는다"는 상태는 해소됐다.
- **그러나 runbook의 주 가설을 문자 그대로는 충족하지 않는다.** `settlement_terminal_wait_seconds`의
  `market_done` p50은 hold 구간 **19.25ms**로, 28번 배리어 대기 평균 13.48ms보다 **오히려 높다**.
  이유는 지표의 의미가 바뀌었기 때문이다(아래 "주 가설 재해석"). `cancel` 쪽은 평균 **0.31ms → 3.9µs**로
  사실상 사라졌다.
- **28번의 인과 주장 하나는 이번 실측이 반증했다.** 28번은 "배리어가 배치 누적을 구조적으로
  가로막기 때문에 평균 배치가 2.8에 머문다"고 결론냈는데, **배리어를 없애고 처리량이 3.4배로 올라간
  뒤에도 평균 배치 크기는 2.82 → 2.78로 그대로였다.** 배치 파편화의 원인은 fence가 아니다.
- **부작용 신호는 관측됐으나 dispatcher 병목은 아니다.** Gauge `settlement_outstanding_jobs`가
  hold·burst의 **2초 샘플 100%에서 `2N`(=8)** 에 붙어 있었다. 다만 같은 구간 worker busy가 99.95%이므로
  **파이프라인을 막고 있는 것은 dispatcher가 아니라 워커·DB 용량**이다(dispatcher가 병목이면 워커가 논다).
- 가용성 하드 보장 유지: `sli_order_response_availability` **100.00%**, `sli_cancel_success` **100.00%**,
  load-gen 2대 100% 완주, 시작 skew **0.20초**.

## 왜 이 측정을 했는지

28번은 GCP 스케일에서 **파티션 전체 fence가 지배 원인**이고 배치 파편화는 그 하류 결과라고 판정했다.
그 판정이 지목한 수정(A: per-order fence, C: terminal durable defer)을 구현한 뒤, **그 배리어 대기가
실제로 없어졌는지, 그 과정에서 처리량 회귀나 정합성 위반이 없는지**를 28번과 같은 규모·프로파일에서
구간별 수치로 판정한다.

## 비교 조건 동일성

### 측정 기준 SHA

```
82b4d7f6383676b11951fb81e8bea5e24eb73f59
```

- 원격 `origin/main`에 존재, Backend CI 4개 job green(run `30532255904`).
- **배포는 이 SHA의 `git archive` 트리를 그대로 업로드**해서 했다(tarball sha256
  `393e527f…f01e2a`가 로컬·VM 양쪽 일치). 로컬 HEAD `c05840b`는 문서 전용 커밋이며
  `git diff 82b4d7f..HEAD -- '*.go'`가 **비어 있음**을 실행 전 확인했다.
- `.env`는 서버 VM의 `~/go-exchange-back/.env`(canonical)만 복사해 사용했고, 과거 `bench-*`
  디렉터리는 배포 원본으로 쓰지 않았다.

### 이번 구현이 바꾼 범위

`git diff --stat 8685923..82b4d7f -- '*.go'` → **26 files, +1905/−303**. 변경 파일 26개 전부가
settlement / cancellation / outbox / metrics / testdb 경로다 — 정산 경로 외 기능 변경 없음.
(계획서에는 이 수치가 `+1886/−301`로 적혀 있으나 실행 시점 재확인 결과는 위와 같다. 파일 수·커밋
범위는 동일하고 계획서 수치가 마지막 커밋 확정 전에 기록된 것으로 보인다 — 직접 비교의 근거는 유지된다.)

### 나머지는 26번·28번과 동일

서버·DB `e2-highcpu-4`(서울) · `GOEXCHANGE_SETTLEMENT_CONCURRENCY=4`(기동 로그
`settlement partitions=10 concurrency=4` 확인) · load-gen 2대 `e2-standard-8`(A `OFFSET=0` /
B `OFFSET=5000`, 각 `TOTAL_USERS=5000`) · 같은 `order-spike-availability.js`(양쪽 VM sha256이 로컬
원본 `ae31c14d…4661f`와 일치, `sli-classify.js`도 일치, k6 v2.1.0 동일) · 같은 스테이지 프로파일
(150→2,500→2,500→5,000→150→150, 1m/30s/3m/45s/30s/2m) · `LOAD_START_AT_MS` 배리어 ·
**10테이블 TRUNCATE**(`failed_order_cancellations` 추가) + `bootstrap loaded=0` · `--summary-export`.

## Phase 0: Preflight (게이트 — 통과)

VM당 peak 10 VU / 45초 저부하로 14항목 전부 확인했다. 상세는
`_workspace/per-order-fence-29/phase0/PHASE0-RESULT.md`.

| # | 항목 | 결과 |
|---|---|---|
| 1 | 신규 메트릭 5종 노출 | ✅ terminal_wait · outstanding_jobs · quarantined_orders · dependency_record_failed · duplicate_terminal |
| 2 | 폐기 메트릭 3종 미노출 | ✅ `settlement_barriers_total` / `_barrier_wait_seconds` / `_barrier_inflight_batches` **부하 후에도 0건** |
| 3 | histogram bucket·sum·count | ✅ |
| 4 | 시작·중간·종료 스냅샷 | ✅ 10초 간격 16개 |
| 5 | load-gen 시작 skew | ✅ 0.09초 |
| 6 | terminal_wait count = terminal 수 | ✅ cancel 80=80, market_done 170=170 |
| 7 | dispatch wait = execution count | ✅ 557=557 |
| 8 | batch attempt ≥ batch job | ✅ 307=307(재시도 0) |
| 9 | 기존 SLI·정합성 | ✅ SLI 3종 100%, DB orders 790=790, trades 433=433 |

**스키마 게이트(TRUNCATE 이전)** — 계획서가 경고한 대로 **migration 005는 GCP DB에 미적용
상태였고, 이번 기동에서 처음 적용**됐다(`OK 005_terminal_durable_defer.sql`,
`successfully migrated database to version: 5`).

| 항목 | 결과 |
|---|---|
| `failed_order_cancellations` 존재 · `order_id` uniqueIndex | ✅ `idx_failed_order_cancellations_order_id` UNIQUE |
| `failed_order_cancellations.retry_count` default 0 + CHECK `>= 0` | ✅ |
| `failed_market_completions.retry_count` default 0 | ✅ |
| 신 제약 `ck_..._retry_count_non_negative` 존재 | ✅ |
| 구 제약 `ck_..._retry_count_positive` 부재 | ✅ 0건 |

**preflight에서 해소한 문제 1건**: 두 load-gen의 `dev_tools_token.txt`가 서버 `.env`의
`GOEXCHANGE_DEV_TOOLS_TOKEN`과 불일치해 첫 시도가 `403 DEV_TOOLS_FORBIDDEN`으로 실패했다.
runbook의 secret 절차대로 canonical `.env`에서 재동기화했고(값은 파이프로만 전달, 로그·문서 미노출),
재검증은 값 비교가 아니라 **setup 성공**으로 했다. 애플리케이션 코드는 손대지 않았다.

## Phase 1: 26번·28번 동일 규모 재현

- **완주**: 두 VM 모두 프로파일 100% 완주(`✓ [100%] 0000/5000 VUs 7m45s`), k6 에러 0건,
  interrupted iteration 0. 전체 소요 19m46s(= setup+배리어 대기 11m59s + 스테이지 7m45s).
- **시작 정렬**: 배리어 `LOAD_START_AT_MS=1785419460.638`. 양쪽 첫 non-zero VU 절대시각
  A `1785419460.841` / B `1785419460.640` → **skew 0.201초**(목표 ≤1초).
- **구간별 스냅샷**: `/metrics` 15초 간격 90회. 시나리오 시작 기준 hold(96.3~276.7s) ·
  burst(276.7~321.8s) · recovery(351.8~472.0s) 경계에 가장 가까운 스냅샷을 선택(15초 해상도 한계로
  목표 대비 +6.3~+7.0초 이동). **최종 누적값이 아니라 이 구간 델타로 판정한다.**
- **`settlement_outstanding_jobs` 전용 2초 샘플러 추가**: 15초 스냅샷으로는 "Gauge가 `2N`에 **상시**
  붙어 있는가"(Phase 4 부작용 판정)를 답할 수 없어 이 Gauge만 2초 간격으로 6,760 샘플을 따로 받았다.
  28번에는 존재하지 않던 지표라 비교 가능성 손실은 없다.
- **드레인**: 종료 후 10초 만에 `matching_engine_channel_length` 4채널 0,
  `settlement_worker_queue_length` 전 워커 0, `settlement_outstanding_jobs` 전 파티션 0.

### 정합성 검사 + fallback

| # | 항목 | 결과 |
|---|---|---|
| 1 | `reconciliation_violations{ledger_wallet}` | **0** |
| 2 | `reconciliation_violations{asset_conservation}` | **0** |
| 3 | `reconciliation_violations{legacy_mismatch}` | **0** |
| 4 | `reconciliation_violations{stale_market_order}` | **0** |
| 5 | `failed_settlements` | **0** |
| 6 | `failed_market_completions` | **0** |
| 7 | `failed_order_cancellations`(이번 구현이 추가) | **0** |
| 8 | 잔존 시장가(`MARKET` & `PENDING`/`PARTIAL`) | **0** |
| 9 | outbox PENDING 잔존 | **0**(전량 `PROCESSED` 205,358건) |
| 10 | `settlement_batch_fallbacks_total` | **0** |
| 11 | `settlement_completion_blocked_total` | **0** |
| 12 | `reconciliation_violations` 테이블 행 | **0** |

> **리컨실리에이션 실행 방식(한계 겸 방법 기록)**: `ReconciliationWorker`의 기본 주기는 1시간이라
> 부하 직후 `/metrics`의 게이지는 **기동 시점(빈 DB)** 값이다. 그래서 최종 `/metrics`를 먼저
> 보존한 뒤 백엔드를 재기동해 **실제 최종 데이터를 대상으로 1회 완주**시켰다
> (`reconciliation_last_run_timestamp_seconds = 1785420114`, 재기동 14:01:48 이후 값 → 완주 확인,
> `reconciliation_check_errors_total` 미발생). 위 4개 게이지는 그 결과다.

취소 인프라 실패: 서버 로그 `DELETE /orders/:id` 상태 분포 **200=17,605 / 409=26,663 / 500=0**
→ 실패율 **0.00%**(26·28번과 동일). `POST /orders` **200=244,299 / 503=831,411 / 500=0**.

## Phase 2: 구간별 계산

| 지표 | hold(180.3s) | burst(45.1s) | recovery(120.2s) |
|---|---|---|---|
| terminal 빈도 — cancel (/s) | 46.67 | 34.64 | 29.44 |
| terminal 빈도 — market_done (/s) | 130.36 | 93.14 | 80.69 |
| **terminal wait 평균 — cancel** | **3.9µs** | 4.2µs | 4.0µs |
| **terminal wait 평균 — market_done** | **20.85ms** | 27.94ms | 15.19ms |
| **terminal wait p50 — market_done** | **19.25ms** | 22.93ms | 14.82ms |
| terminal wait p99 — market_done | 81.42ms | 180.72ms | 47.64ms |
| 평균 trade batch | **2.781** | 2.831 | 2.191 |
| settled trades (/s) | **371.7** | 267.8 | 230.5 |
| DB attempt 빈도 (/s) | **133.64** | 94.56 | 105.25 |
| 재시도 추정치(attempt−batch job) | **0** | 0 | 0 |
| 평균 attempt 시간(batch) | 13.80ms | 17.89ms | 10.41ms |
| **worker busy ratio**(N=4) | **99.95%** | 99.93% | 73.22% |
| 총 job 실행 수 | 56,022 | 10,032 | 25,882 |
| 평균 job 실행 시간 | 12.87ms | 17.98ms | 13.60ms |
| **dispatch wait 평균** | **12.79ms** | 17.89ms | 6.47ms |
| `outstanding_jobs` 평균(2초 샘플) | **8.00 / 8** | **8.00 / 8** | 4.51 / 8 |
| `outstanding_jobs`가 `2N`인 샘플 비율 | **100.0%** | **100.0%** | 30.5% |
| quarantine 잔존 | 0 | 0 | 0 |
| dependency record 실패 | 0 | 0 | 0 |

> **`cancel`의 p50/p99는 읽지 않는다.** `settlement_terminal_wait_seconds`의 최저 버킷이 `le=0.005`(5ms)인데
> cancel 관측치 **17,605건이 전부 그 첫 버킷에 들어갔다.** 따라서 계산되는 p50 2.5ms·p99 4.95ms는
> 버킷 내 선형보간 산물일 뿐 실제 값이 아니다. **cancel의 실제 신호는 평균 3.9µs**다.
> market_done은 버킷이 분포를 가르므로 p50·p99를 그대로 쓸 수 있다.

## Phase 3: 계측 내부 무결성 — 9항목 전부 정확히 일치

| # | 항목 | 결과 |
|---|---|---|
| 1 | `terminal_wait{cancel}` count = 처리된 cancel terminal 수 | **17,605 = 17,605**(k6 `sli_cancel_success` A 8,814 + B 8,791) |
| 2 | `terminal_wait{market_done}` count = market done 수 | **48,744 = 48,744**(k6 `custom_market_success` A 24,381 + B 24,363) |
| 3 | dispatch wait count = job execution count | **120,682 = 120,682** |
| 4 | batch attempt count ≥ logical batch job count | **54,333 = 54,333**(등호, 재시도 없음) |
| 5 | 그 차이가 실제 retry 로그와 일치 | 차이 0 — 재시도 로그도 0건 |
| 6 | job 수 내부 정합 | **120,682 = batch 54,333 + terminal 66,349**(17,605 + 48,744) |
| 7 | settlement batch의 trade 합계 = settled trade 증가량 | **139,009 = 139,009**(DB `trades`) |
| 8 | k6 주문 합계 = 서버·DB 주문 합계 | k6 성공 244,299 + DB `REJECTED` 191 = **244,490 = 244,490**(DB `orders`) |
| 9 | 서버 로그 ↔ k6 상태 분포 | `POST /orders` 200 **244,299 = 244,299**, 503 **831,411 = 831,411**; `DELETE` 200 **17,605 = 17,605**, 409 **26,663 = 26,663** — 네 값 모두 정확히 일치 |

**계측을 신뢰하고 판정을 진행한다.**

> **`settlement_duplicate_terminal_total = 0`의 범위(반드시 함께 읽을 것)**:
> 이 값은 **dispatcher waiting 상태에서 관측 가능한** 중복 terminal이 없었다는 뜻이며,
> **저장소 수준의 전역 중복 부재를 증명하지 않는다.** 첫 terminal이 이미 dispatch된 뒤 도착하는
> 중복은 waiting이 비어 있어 이 counter에 잡히지 않는다. 저장소 수준 탐지는 설계 스펙 범위 밖이다.

> **서버 로그 대조 시 Phase 0 분량 보정이 필요했다**: 백엔드 컨테이너를 재생성이 아니라
> stop/start로만 다뤄서 `docker logs`에 Phase 0 preflight 요청(주문 790건·취소 80/74건)이 함께
> 남아 있었다. 위 #9의 수치는 그 분량을 뺀 값이며, 뺀 뒤 네 항목이 **전부 정확히 일치**한다.

## Phase 4: 판정

| 판정표 행 | 기준 | 이번 측정 | 판정 |
|---|---|---|---|
| **주 가설** | terminal wait p50이 28번 배리어 대기 13.48ms 대비 유의하게 감소 | cancel 평균 **0.31ms → 3.9µs**(≈79배 감소) · market_done p50 **19.25ms**(28번 평균 13.48ms 대비 **증가**) | **사전 기준상 미충족**(아래 재해석 — 통과로 재분류하지 않는다) |
| **처리량** | worker busy 13.1%에서 상승 | **13.13% → 99.95%** | ✅ 충족 |
| **처리량** | 배치 크기 2.82에서 상승 | **2.80 → 2.781**(사실상 무변화) | ❌ **미충족** |
| **정합성(비협상)** | 무결성 전항목 + quarantine 0 + duplicate terminal 0 | 무결성 9항목 오차 0 · quarantine 0 · duplicate 0 | ✅ 충족 |
| **회귀 없음** | `sli_cancel_success` 100% 유지, 가용성 하드 보장 유지 | 100.00%(17,605/17,605) · 응답 가용성 100.00%(1,075,710/1,075,710) | ✅ 충족 |
| **부작용 감시** | `outstanding_jobs`가 `2N`에 상시 붙으면 dispatcher가 새 병목 | hold·burst **2초 샘플 100%가 `2N`(8)** | ⚠️ 신호 관측 — 단 dispatcher 병목은 아님(아래) |

### 주 가설 재해석 — 지표의 의미가 바뀌었다

28번의 `settlement_barrier_wait_seconds`는 **파티션 dispatcher가 멈춰 있던 시간**이었다.
그래서 그 값이 크면 곧 처리량 손실이었고, wait duty가 벽시계의 52%였다.
29번의 `settlement_terminal_wait_seconds`는 **한 terminal이 자기 주문의 의존 배치를 기다린 시간**이며
**dispatcher를 막지 않는다** — 기다리는 동안 dispatcher는 다른 job을 계속 내보낸다.

따라서 두 숫자를 직접 빼서 "줄었다/늘었다"로 읽으면 안 된다. market_done terminal wait이 13.48ms에서
19.25ms로 커진 것은 **워커가 100% 포화됐기 때문**이다 — 28번에서는 워커가 13%만 일하고 있었으니
"in-flight 정확히 1개"를 기다리면 곧 끝났지만, 29번에서는 의존 배치가 **꽉 찬 큐를 통과해야** 한다.
대기가 길어졌지만 그 대기가 더 이상 파이프라인을 막지 않는다.

**fence 제거의 실제 효과는 처리량 쪽 수치가 증언한다:**

| 신호 | 28번(hold) | 29번(hold) | 변화 |
|---|---|---|---|
| worker busy ratio(N=4) | 13.13% | **99.95%** | ×7.6 |
| DB attempt 빈도 | 39.05 /s | **133.64 /s** | ×3.4 |
| 정산 처리율 | ≈109 trades/s | **≈372 trades/s** | ×3.4 |
| dispatch wait 평균 | 9µs | 12.79ms | 워커 포화의 결과 |

**결론: 파티션 전체 fence는 제거됐다.** 28번이 "N=4 병렬도가 있어도 배리어가 매번 새 배치 준비를
막아 사실상 직렬에 가깝게 돈다"고 진단한 상태는 해소됐고, **바인딩 링크가 fence 대기에서
정산 워커·DB 처리 용량(N=4)으로 이동**했다.

### 28번의 인과 주장 중 하나는 반증됐다

28번은 평균 배치 크기 2.80을 **"배리어가 배치 누적을 구조적으로 가로막기 때문"** 으로 설명했다.
이번 측정은 배리어를 없애고 처리량을 3.4배로 올렸는데도 **평균 배치가 2.781로 그대로**다
(burst 2.831, recovery 2.191). **배치 파편화는 fence의 하류 결과가 아니다** — 별도의 원인이 있고,
28번의 그 문장은 이번 실측으로 철회해야 한다.

#### 새 가설 — terminal 경계가 batch run length를 제한한다

`collectTradeBatch`는 **비-trade 이벤트를 만나면 배치를 끊는다**(cmd/main.go, `event.Event.Trade == nil`
→ `pending`으로 반환). [27번](27-2026-07-28-settlement-binding-reanalysis.md)에서 terminal은
전체 outbox 이벤트의 **31.9%**였다. 이벤트가 서로 독립적으로 섞인다고 **가정**하면 연속 trade의
기대 run length는

```
1 / 0.319 ≈ 3.13   ← 독립 혼합 가정 아래의 근사치이지 정확한 예측값이 아니다
```

이고, 관측값 2.78~2.83이 이에 근접한다. 파생 수치들도 정합적이다:

```
trade batch job/s = 371.7 settled ÷ 2.781 = 133.7/s   → 측정된 DB attempt 133.64/s 와 일치
```

**다만 이것은 수치상 강하게 지지되는 가설이지 확정된 인과가 아니다.** 실제 이벤트 배치는 독립
혼합이 아닐 수 있고(심볼·주문 쏠림), 큐가 순간적으로 비어 `default`로 배치가 끊기는 경로도 있다.
**다음 측정에서 실제 연속 trade run-length 분포를 직접 계측해 확인해야 한다.**

**job 개수 비중은 worker 시간 비중이 아니다.** 이번 실행에서 batch job 54,333건 대비 terminal job
66,349건으로 **terminal이 job 개수의 약 55%**를 차지했지만, `settlement_job_execution_seconds`가
job 종류를 구분하지 않으므로 **"worker 시간의 55%를 쓴다"거나 "용량의 절반을 쓴다"고 말할 수 없다.**
현재 말할 수 있는 것은 전체 평균(job 12.87ms)과 포화 상태(busy 99.95%)가 서로 정합적이라는 수준까지다.

### 부작용 — `2N` 점유는 dispatcher 병목이 아니다

`settlement_outstanding_jobs`가 hold·burst의 **2초 샘플 100%에서 `2N`=8**에 붙어 있었다.
runbook은 이를 "dispatcher가 새 병목"의 신호로 적어 뒀지만, **같은 구간의 worker busy가 99.95%**다.
dispatcher가 병목이라면 워커가 놀아야 한다 — 워커는 놀지 않았다. 즉 Gauge가 `2N`에 붙어 있는 것은
**dispatcher가 파이프라인을 항상 가득 채우고 있는데 워커가 그 속도로 job을 은퇴시키지 못한다**는
뜻이며, 제약은 **워커·DB 용량** 쪽이다. dispatch wait 평균이 9µs → 12.79ms로 커진 것도 같은 해석을
가리킨다(job이 큐에서 워커를 기다린다).

recovery 구간에서는 `2N` 점유가 30.5%로 떨어지고 worker busy도 73.2%로 내려가, 부하가 빠지면
포화가 풀린다는 것도 확인된다.

### 사용자 관점 효과 (26번 대비)

| 지표 | 26번 | 29번 | 변화 |
|---|---|---|---|
| `sli_order_response_availability` | 100.00%(1,007,654) | **100.00%**(1,075,710) | 유지 |
| `sli_cancel_success` | 100.00%(7,209) | **100.00%**(17,605) | 유지 |
| `sli_order_business_success` | 10.82%(109,033/1,007,654) | **22.70%**(244,299/1,075,710) | **×2.10** |
| DB `total_orders` | 109,033 | **244,490** | ×2.24 |

셰딩이 줄어 같은 부하에서 **업무 성공 주문이 2.2배**로 늘었다. 응답 가용성·취소 성공률의 하드 보장은
그대로 유지됐다.

## 안전 게이트 확인

| 게이트 | 결과 |
|---|---|
| 응답 가용성 유지 | ✅ 100.00%(양쪽) |
| 취소 인프라 실패율 0% | ✅ 0.00%(500 = 0 / 44,268) |
| 정합성 위반 0 | ✅ 12항목 전부 0 |
| fallback 0 | ✅ `settlement_batch_fallbacks_total` 0 |
| 회복 성능 악화 없음 | ✅ recovery의 attempt(10.41ms)·terminal wait p50(14.82ms)·worker busy(73.2%)가 hold·burst보다 낮음 |
| load-gen 완주 및 시작 skew | ✅ 양쪽 100% 완주, skew 0.201초 |
| 계측 내부 무결성 | ✅ Phase 3 9항목 전부 정확히 일치 |
| quarantine 잔존 0 | ✅ 전 파티션 0 |
| 중복 terminal 0 | ✅ 0(단, 위에 적은 범위 한정) |

전 게이트 충족 — 위 판정은 유효하다.

## 사후 분석 — DB 커넥션 풀 대기의 시간적 집중

> 이 절은 **판정 이후 원본 스냅샷을 다시 읽어** 추가한 것이다. 새 부하를 걸지 않았고,
> **이번 원본에서 새로 확정할 수 있는 것은 풀 대기의 시간적 집중 하나뿐**이다.

`config/database.go`가 `NewDBStatsCollector`를 등록하므로 `go_sql_*`가 이미 수집돼 있었다.
phase1 스냅샷 90개(약 10초 간격)의 누적 카운터:

| 시각 | `wait_count_total` | `wait_duration_seconds_total` | `in_use` |
|---|---|---|---|
| 13:39:01 (시작) | 0 | 0 | 0 |
| 13:39:16 (+15초) | 1,368 | 953.7 | **25/25** |
| 13:41:34 (+2.5분) | 17,026 | 9,028.0 | **25/25** |
| 13:46:36 (+7.5분) | 26,546 | 14,408.2 | 0 |
| 13:54:07 | **26,546**(정체) | 14,408.2 | 5 |
| 14:01:22 (종료) | 26,660 | 14,410.5 | 0 |

**누적 대기의 99.6%가 초기 약 7.5분에 발생했고, 그 뒤 15분 동안 `wait_count`는 114건(+0.4%),
`wait_duration`은 2.2초만 늘었다.**

`in_use` 분포 — **단일 시점이 아니라 90개 샘플 전체**:

| 구간 | 샘플 | max | p95 | median |
|---|---|---|---|---|
| 전체 | 90 | 25 | 25 | 1 |
| **램프**(`wait_count` 증가 중) | 16 | **25** | **25** | **25** |
| **이후**(`wait_count` 정체) | 74 | **8** | **6** | 0 |

`in_use = 25`(포화) 샘플은 90개 중 14개이며 **전부 램프 구간**이다.

### 이 데이터로 말할 수 있는 것과 없는 것

**말할 수 있는 것**

> 29번에서 **DB 풀 고갈은 초기 램프 구간에 집중**됐고 이후 누적 대기가 거의 증가하지 않았다.
> 이는 **정상 구간에서 풀 크기 25가 지속 병목이 아니며, N=4보다 높은 정산 동시성을 시험할 여지가
> 있음을 강하게 지지**한다. 다만 **N=8의 안전성과 효과, CPU 병목 여부, 배치 확대 효과는 아직
> 실증되지 않았다.**

**말할 수 없는 것 (모두 미측정)**

- **평균 풀 대기 `14,408.2 / 26,546 ≈ 543ms`는 max 4.5초의 유력한 원인으로는 볼 수 있으나,
  전체 요청의 약 2.3%에 해당하므로 이것만으로 전체 p95 120ms를 설명하지 못한다.**
- **job 실행 12.87ms가 "대부분 DB 대기"라는 주장은 가설이다** — 서버·DB CPU도,
  `settlement_job_execution_seconds`의 job 종류별 분리도 없다.
- **배치 확대의 효과는 trade 배치 트랜잭션에만 적용된다.** 2.78 → 8이면 trade 배치 job은
  133.6/s → 46.5/s로 줄지만 **terminal job은 그대로**다. terminal이 전체 job의 약 55%
  (batch 54,333 : terminal 66,349)이므로 **총 job·DB 호출 감소는 약 29% 수준이지 3분의 2가
  아니다.** 배치가 커질 때의 **트랜잭션 시간 증가와 락 경합 증가도 미측정**이다.

### 이 발견이 지정하는 다음 순서

1. (완료) 29번 문서에 풀 시계열과 제한사항 추가
2. **같은 바이너리로 N=4 → 8 통제 비교**
3. 실행 중 **서버·DB CPU**와 **구간별 풀 `in_use`/`wait`** 수집
4. **안전 게이트 통과 후에만** N=8 성능 판정
5. 이후 **실제 run-length·job 종류별 실행 시간**으로 배치 수정 여부 결정

**CPU와 job 종류별 지표는 계측 코드를 추가한다고 값이 생기는 것이 아니라, 추가한 뒤 부하를 다시
실행해야 얻어진다.** 이번 원본만으로는 위 2~5를 대체할 수 없다.

## 한계

- **주 가설 지표의 비교 불가능성**: 28번의 barrier wait과 29번의 terminal wait은 **측정 대상이 다르다**
  (dispatcher 정지 시간 vs 주문별 비차단 대기).

  > **사전 기준상 미충족.** 다만 사후 검토에서 기존 partition-wide blocking 시간과 신규 per-order
  > dependency wait를 직접 비교한 **기준 자체가 의미적으로 부적절했음**이 확인됐다. **이를 통과로
  > 재분류하지 않으며**, 처리량·worker busy·batch size 결과는 각각 독립적으로 보고한다.

  기준을 그렇게 세운 것은 [설계 스펙](../superpowers/specs/2026-07-30-per-order-fence-and-terminal-durable-defer-design.md)의
  "실측 (29번)" 절이며, 다음 사이클의 판정 기준을 세울 때 이 점을 반영해야 한다.
- **`terminal_wait{cancel}`의 히스토그램 해상도 부족**: 최저 버킷이 5ms인데 cancel 관측치 17,605건이
  **전부 첫 버킷**에 들어가 분위수를 산출할 수 없다(평균 3.9µs). cancel 쪽 분포를 봐야 한다면
  버킷을 µs 스케일까지 내려야 한다.
- **`settlement_job_execution_seconds`가 batch job과 terminal job을 구분하지 않는다**: 이번 worker busy
  99.95%에는 batch 54,333건과 terminal 66,349건이 섞여 있다. batch attempt 시간만 떼어 보면
  hold 기준 `13.80ms × 24,098 / (4 × 180.3s)` = **46.1%** — 나머지 약 54%는 terminal job이 쓴다.
  **28번의 13.1%와 같은 정의로 비교하려면 이 46.1%를 봐야 하며**(그래도 ×3.5), 100%라는 숫자는
  "워커 슬롯이 꽉 찼다"는 뜻이지 "batch DB 작업이 100%"는 아니다.
- **스냅샷 해상도(15초)**: 구간 경계가 목표 시각보다 +6.3~7.0초 이동했다(180s·45s·120s 구간 판정에는
  무시할 수준이나, burst 45초 구간에는 상대적으로 크다).
- **`REJECTED` 191건 보정**: 28번(30건)과 같은 현상이 이번에도 있었고(전체의 0.08%) 원인은
  이번 사이클 범위 밖이다. 정산 계측 무결성 항목은 이 보정과 별개로 전부 일치했다.
- **리컨실리에이션은 재기동으로 1회 실행**했다(위 방법 기록 참조) — 부하 중 연속 관측이 아니다.
- **원본 자료**는 용량 때문에 리포에 커밋하지 않고 로컬 `_workspace/`에만 둔다.

## 원본 자료

- k6 스크립트: `_workspace/loadtest/order-spike-availability.js`(26·28번과 sha256 동일, 무변경)
- `_workspace/per-order-fence-29/phase0/` — Phase 0 결과 문서, 양쪽 k6 stdout·summary,
  `/metrics` 스냅샷 16개
- `_workspace/per-order-fence-29/phase1/` — 양쪽 k6 stdout·`p1-summary-*.json`,
  `/metrics` 스냅샷 90개, `outstanding_jobs` 2초 샘플(`gauge29-p1.tsv`, 6,760행),
  최종 `/metrics`, 구간 계산 결과(`phase2_results.json`)와 계산 스크립트(`phase2.py`)
- `DEV_TOOLS_TOKEN`이 k6 stdout·summary에 포함되지 않았음을 확인(grep 0건)한 뒤 보관. 외부 IP 미기록.

## 다음 (범위 밖)

**한 줄 요약:** 29번은 **partition-wide fence 제거 후 worker 공급 부족이 해소됐음**을 보여준다.
반면 **trade batch size는 개선되지 않았으며, terminal 경계가 batch run length를 제한한다는 새 가설이
수치상 강하게 지지된다.** 이 가설의 수정은 **축 1 후속 사이클에서 별도로 검증한다.**

- **축 1 후속 — terminal 경계 배치 파편화 진단**(축 2와 **독립 판정**). fence가 원인이 아님은
  확인됐고 terminal 경계 가설이 남았다. **설계에 앞서 다음을 먼저 측정해야 한다:**
  - 실제 연속 trade **run-length 분포**(독립 혼합 가정의 검증)
  - `settlement_job_execution_seconds`의 **job 종류별(batch/terminal) 실행 시간 분리**
  - **terminal의 DB transaction 비중**

  후보 수정 2가지는 **측정 전에는 채택하지 않는다**:
  - `collectTradeBatch`가 terminal을 건너뛰고 trade를 계속 모으기
  - 독립 주문의 terminal 여러 개를 job 하나로 묶기 — **주의: job 수는 줄어도 DB transaction 수는
    줄지 않을 수 있고, 한 worker가 여러 terminal DB 호출을 직렬 수행해 오히려 병렬도를 낮출 수 있다.**
- **N=4 → 8 통제 비교**(같은 바이너리) — 바인딩 링크가 워커·DB 용량으로 이동했고, 사후 분석에서
  **정상 구간의 풀 `in_use`가 max 8 / p95 6(상한 25)**로 여유가 확인돼 시험할 여지가 있다.
  **실행 중 서버·DB CPU와 구간별 풀 `in_use`/`wait`를 함께 수집**해야 하며, 안전 게이트 통과 후에만
  성능을 판정한다.
- `settlement_job_execution_seconds`에 job 종류(batch/terminal) 라벨 추가.
- `settlement_terminal_wait_seconds{cancel}` 버킷 하한 축소.
- 저장소 수준의 중복 terminal 탐지.
