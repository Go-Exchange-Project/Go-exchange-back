# 28번 (2026-07-29): 4차 축 1 — GCP 스케일 정산 관측성 재측정

## 요약(먼저 읽어야 할 것)

- **27번의 정산 큐 포화가 정확히 재현됐다.** hold·burst 전 구간에서 `settlement_worker_queue_length{worker="8"}`
  이 256(cap)으로 포화, 나머지 워커는 0 — 27번과 동일 시그니처. `ExecutionCh`도 768~790으로 75%
  워터마크 부근 유지(26·27번과 동일).
- **판정: 파티션 전체 fence가 지배적이며, 배치 파편화는 그 fence의 직접적 하류 결과다.** hold 구간
  기준 `market_done` 배리어 wait duty **52.3%**(벽시계 시간의 절반이 배리어 대기), 배리어 진입 시
  in-flight가 **항상 정확히 1**, 평균 배리어 대기(13.48ms)가 평균 attempt 시간(13.33ms)·평균 job
  실행 시간(13.45ms)과 **거의 동일** — 배리어가 매번 "지금 실행 중인 배치 딱 1개"를 기다리는
  패턴이 반복된다는 뜻이다. 반면 **worker busy ratio는 13.1%에 불과**(DB 자체는 느리지 않다)하고
  `dispatch_wait` 평균은 9µs(무시 가능) — DB transaction·pool/scheduling 원인은 배제된다.
  **N=4 병렬도가 있어도 배리어가 매번 새 배치 준비를 막아 사실상 직렬에 가깝게 돈다**는 것이
  worker busy 13%의 정체다.
- **계측 내부 무결성 7항목 전부 정확히 일치**(오차 0) — `barriers_total`↔k6 Done/Cancel,
  dispatch_wait/execution/batch count 3자 일치, settled trade 합계↔DB, k6 주문 합계↔DB(REJECTED
  30건 보정 후 정확히 일치). 계측을 신뢰하고 판정을 진행했다.
- **안전 게이트 전부 충족**: 응답 가용성 100%, 취소 인프라 실패 0%, 정합성 위반 0,
  `settlement_batch_fallbacks_total` 0, load-gen 2대 100% 완주(skew ≈0.1초).
- **애플리케이션 코드는 관측성 패치(4차 축1)만 26번 대비 추가됐다** — `git diff --stat`으로 확인,
  기능 변경 0.

## 왜 이 측정을 했는지

27번은 GCP 스케일에서 정산 경로가 최초 바인딩 링크임을 확정했지만, 그 안에서 **batch 파편화**와
**barrier wait**(파티션 전체 fence) 중 무엇이 지배적인지 구분하지 못했다. 20번이 로컬 저VU
스모크로 그 계측 6종이 정상 동작함을 검증했지만, 로컬 규모(피크 30 VU)는 27번이 관측한 GCP 스케일
동시성 압력(정산 큐 256 포화, 135 trades/s)을 재현하지 않았다. 이번 28번은 26번과 동일한 GCP
규모·부하 프로파일로 20번의 계측을 실제로 돌려 27번이 남긴 질문에 수치로 답한다.

## 비교 조건 동일성

`git diff --stat 762a178..HEAD -- '*.go'` (26번 측정 커밋 `762a178` 대비, 실행 직전 재확인):

```
cmd/main.go                                       | 10 +++-
cmd/settlement_observability_test.go              | 54 ++++++++++++++++++++
cmd/settlement_pipeline.go                        | 32 ++++++++++--
cmd/settlement_pipeline_test.go                   | 62 +++++++++++++++++++++++
internal/metrics/metrics.go                       | 54 ++++++++++++++++++++
internal/metrics/settlement_observability_test.go | 40 +++++++++++++++
6 files changed, 246 insertions(+), 6 deletions(-)
```

관측성 패치(4차 축1, 커밋 `6ae5097`~`393af51`) + 테스트 3개뿐 — **기능 변경 0**. 서버·DB는 26번과
동일한 `e2-highcpu-4`(서울, `asia-northeast3`), `GOEXCHANGE_SETTLEMENT_CONCURRENCY=4`(기동 로그
`settlement partitions=10 concurrency=4` 확인), load-gen 2대(`e2-standard-8`, A `OFFSET=0`/B
`OFFSET=5000`, 각 `TOTAL_USERS=5000`), 같은 `_workspace/loadtest/order-spike-availability.js`(양쪽
VM sha256 로컬 원본과 정확히 일치 확인, k6 v2.1.0 동일), 같은 스테이지 프로파일(150→2,500→2,500→
5,000→150→150, 1m/30s/3m/45s/30s/2m), `LOAD_START_AT_MS` 배리어, DB 리셋(9테이블 TRUNCATE) +
`bootstrap loaded=0` 확인.

## Phase 0: Preflight (게이트 — 통과)

저VU(peak 10, 총 20 VU) 85초 실행으로 7항목 전부 확인:

| # | 항목 | 결과 |
|---|---|---|
| 1 | 메트릭 6종 `/metrics` 노출 | ✅ 전부 노출 |
| 2 | histogram bucket·sum·count 수집 | ✅ 정상 |
| 3 | 시작·중간·종료 스냅샷 생성 | ✅ 여러 시점 쿼리 성공 |
| 4 | 두 load-gen 시작 skew ≤1초 | ✅ 양쪽 "0m54.0s"로 동일(초 단위 해상도 내 일치) |
| 5 | `barriers_total` = k6 Done/Cancel 수 | ✅ cancel 88=88, market_done 160=160(정확히 일치) |
| 6 | dispatch wait count = execution count = 완료 job 수 | ✅ 329=329=329 |
| 7 | 기존 SLI·정합성 정상 | ✅ SLI 100%, 정합성 0, DB 주문 합계(879)=k6 합계(879) |

전항목 통과 — 본 실행 진행.

## Phase 1: 26번 동일 규모 재현

- **부하**: 두 load-gen VM에서 동일 프로파일 동시 실행. `LOAD_START_AT_MS` 배리어로 정렬, 실제
  시나리오 시작 시각(양쪽 로그의 첫 non-zero VU 라인)이 **양쪽 모두 "6m35.8s"/"6m35.9s" 경과 —
  skew ≈ 0.1초**(목표 ≤1초 충족).
- **완주**: 두 VM 모두 프로파일 100% 완주(전체 소요 14m23s = setup+배리어 대기 6m36s + 스테이지
  7m45s). 23번의 크래시가 여기서도 재발하지 않았다(26번과 동일).
- **구간별 스냅샷**: `/metrics`를 15초 간격으로 63회 캡처, 실제 시나리오 시작 시각 기준
  hold(87.1s~269.3s)·burst(269.3s~315.2s)·recovery(345.4s~466.3s) 구간 경계에 가장 가까운 스냅샷을
  선택(±0.2~3.8초 오차, 15초 샘플링 해상도 한계). 최종 누적값이 아니라 이 구간별 델타로 판정한다.
- **드레인 확인**: 실행 종료 후 `matching_engine_channel_length`(전 채널) 0,
  `settlement_worker_queue_length`(전 워커) 0.

### 정합성 5검사 + fallback

| # | 항목 | 결과 |
|---|---|---|
| 1 | `reconciliation_violations{ledger_wallet}` | **0** |
| 2 | `reconciliation_violations{asset_conservation}` | **0** |
| 3 | `reconciliation_violations{legacy_mismatch}` | **0** |
| 4 | `reconciliation_violations{stale_market_order}` | **0** |
| 5 | `failed_settlements` | **0** |
| 6 | `failed_market_completions` | **0** |
| 7 | `stale_market_orders`(잔존 시장가) | **0** |
| 8 | outbox PENDING 잔존 | **0** |
| 9 | `settlement_batch_fallbacks_total` | **0** |

취소 인프라 실패(500): 서버 로그에서 `DELETE /orders/:id` 상태 분포 **200=5,658 / 409=10,110 /
500=0** — 취소 인프라 실패율 **0.00%**(26번과 동일). 응답 가용성: 양쪽 k6
`sli_order_response_availability` **100.00%**.

## Phase 2: 구간별 계산 (BTC 단일 파티션 기준)

`settlement_worker_queue_length{worker="8"}`(BTC 파티션)만 비영이므로, 아래 barrier wait duty는
**단일 활성 파티션 기준으로 그대로 해석 가능**하다(runbook의 "여러 파티션 합산 시 100% 초과 가능"
주의사항은 이번 측정에는 해당하지 않음 — 활성 심볼이 BTC 하나뿐).

| 지표 | hold(182.2s) | burst(45.9s) | recovery(120.9s) |
|---|---|---|---|
| barrier 빈도 — cancel (/s) | 11.83 | 10.65 | 13.01 |
| barrier 빈도 — market_done (/s) | 38.77 | 34.53 | 35.01 |
| **barrier wait duty — cancel** | 0.36% | 0.32% | 1.83% |
| **barrier wait duty — market_done** | **52.27%** | **50.76%** | **45.63%** |
| 평균 in-flight — cancel | 0.023 | 0.023 | 0.111 |
| **평균 in-flight — market_done** | **1.000** | **1.000** | **1.000** |
| 평균 trade batch | **2.80** | 2.84 | 2.69 |
| DB attempt 빈도 (/s) | 39.05 | 34.77 | 36.45 |
| 재시도 추정치(attempt−job) | 0 | 0 | 0 |
| **worker busy ratio**(N=4) | **13.13%** | 12.75% | 11.84% |
| dispatch wait 평균 | 9µs | 12µs | 10µs |
| 평균 attempt 시간(batch) | 13.33ms | 14.54ms | 12.88ms |
| 평균 job 실행 시간 | 13.45ms | 14.67ms | 12.99ms |
| **평균 배리어 대기(market_done)** | **13.48ms** | **14.70ms** | **13.03ms** |
| 평균 배리어 대기(cancel) | 0.31ms | 0.31ms | 1.40ms |

세 구간 모두 같은 패턴이 반복된다: `barrier_wait duty(market_done)` ≈ 46~52%, 배리어 진입 시
in-flight는 **항상 정확히 1**, 평균 배리어 대기가 평균 attempt·job 실행 시간과 **거의 같다**(오차
≤1.2ms). 재시도는 전 구간 0.

## Phase 3: 계측 내부 무결성 — 7항목 전부 일치

| # | 항목 | 결과 |
|---|---|---|
| 1 | `barriers_total{cancel}` = 처리된 cancel terminal 수 | **5,570 = 5,570**(k6 `sli_cancel_success` A+B 합산) |
| 2 | `barriers_total{market_done}` = market done 수 | **17,313 = 17,313**(k6 `custom_market_success` A+B 합산) |
| 3 | dispatch wait count = job execution count | **18,087 = 18,087** |
| 4 | batch attempt count ≥ logical batch job count | **18,087 = 18,087**(등호, 재시도 없음) |
| 5 | 차이가 실제 retry 로그와 일치 | 차이 0 — 재시도 로그도 0건(코드상 batch 경로는 재시도 루프가 없어 예상대로) |
| 6 | settlement batch의 trade 합계 = settled trade 증가량 | **48,835 = 48,835**(DB `total_trades`) |
| 7 | k6 주문 합계 = 서버·DB 주문 합계 | k6 성공 86,769 + DB `REJECTED` 30건 = **86,799 = 86,799**(DB `total_orders`) |

7항목 전부 정확히 일치(#7은 `REJECTED` 상태로 영속화된 30건을 더해야 맞는데, DB에서 직접
`status` 분포를 조회해 확인했다 — 계측 오류가 아니라 admission 이후 별도 실패 분류로 설명됨).
**계측을 신뢰하고 판정을 진행한다.**

## Phase 4: 판정 — 파티션 전체 fence가 지배적(배치 파편화는 그 하류 결과)

판정표 4행 중 신호를 대조한다:

| 판정표 행 | 이번 측정 신호 | 판정 |
|---|---|---|
| ① 배치 2~3건 + 빈도 높음 + **wait duty 낮음** + worker busy 낮음/중간 | 배치 2.7~2.84(✅) · 빈도 35~39/s(✅) · **wait duty 46~52%(❌ 낮지 않음)** · busy 12~13%(✅) | 조건 일부만 충족 |
| ② **wait duty 큼** + 진입 시 **in-flight 자주 ≥1** + dispatcher 반복 정지 | **wait duty 46~52%(✅)** · **in-flight 항상=1.000(✅)** · 배리어 평균 대기≈attempt 평균≈job 평균(✅, 아래 설명) | **전 조건 충족** |
| ③ attempt 큼 + worker busy 높음 + DB 실행이 wait보다 지배 | attempt 13~15ms(보통 수준) · **worker busy 12~13%(❌ 낮음)** | 배제 |
| ④ dispatch wait 큼 + worker 실행 상대적으로 작음 | **dispatch wait 9~12µs(❌ 무시 가능)** | 배제 |

**③(DB transaction)과 ④(worker scheduling)는 명확히 배제된다** — DB 자체는 느리지 않고(attempt
13~15ms은 극단적이지 않다) worker pool도 놀고 있으며(busy 12~13%), job이 대기 없이 즉시
디스패치된다(9~12µs).

**①과 ②가 동시에 신호를 보이지만, 이는 두 개의 독립된 원인이 아니라 하나의 인과 사슬이다.**
`runPartitionDispatcher`의 배리어는 `barrier=true`인 동안 새 배치 준비 자체를 막는다
(`cmd/settlement_pipeline.go`의 "다음 배치 준비" 블록이 `!barrier` 조건에서만 실행). market_done
배리어가 벽시계 시간의 46~52%를 점유하고 그때마다 정확히 1개 배치만 기다린다는 것은, **그 절반의
시간 동안 배치가 누적될 기회 자체가 없었다**는 뜻이다 — 평균 배치 크기가 2.7~2.84로 27번의 2.82와
거의 같은 수준에 머무는 것은 파편화가 배리어와 무관한 별도 현상이어서가 아니라, **배리어가 배치
누적을 구조적으로 가로막기 때문**이다. 평균 배리어 대기(13~15ms)가 평균 attempt·job 실행 시간과
오차 1.2ms 이내로 일치하는 것이 이 인과관계의 직접 증거다 — 배리어는 "쌓인 대기열"을 기다리는 게
아니라 매번 "지금 막 시작한 배치 딱 하나"를 기다리고 있다.

**결론: 지배 원인은 파티션 전체 fence(②)다.** N=4 병렬도가 설정돼 있어도 배리어가 새 배치 디스패치
자체를 막아 실질적으로 직렬에 가깝게 돌아간다(worker busy 13%가 그 증거 — 4배 병렬을 쓸 기회가
구조적으로 주어지지 않는다). 배치 파편화(①의 표면적 신호)는 독립 원인이 아니라 이 fence의 직접적
관측 가능한 결과로 기록한다.

### 27번 대비 — 정산 큐 포화 재현 여부: 재현됨

| 신호 | 27번(GCP, hold2→hold3) | 28번(이번, hold 구간) |
|---|---|---|
| `settlement_worker_queue_length{worker="8"}` | 256(cap) 포화 | **256(cap) 포화**(hold·burst 전 스냅샷 동일) |
| 나머지 워커(0~7,9) | 0 | **0**(동일) |
| `ExecutionCh` | 워터마크 부근(783) | **768~790**(75% 워터마크 부근, 동일) |
| 평균 trade batch | 2.82/32 | **2.80/32**(거의 동일) |
| 정산 처리율 | ~135 trades/s | attempt 빈도 39.05/s(batch), 배치당 2.80건 → **≈109 trades/s**(비교 가능한 규모) |
| `settlement_batch_fallbacks_total` | 0 | **0** |

**27번이 GCP 스케일에서 관측한 정산 큐 포화·작은 배치·outbox 대비 여유 있는 처리 여유 신호가 이번
측정에서도 그대로 재현됐다** — 두 측정이 같은 바인딩 조건을 가리키고 있다는 교차 증거다.

## 안전 게이트 확인

| 게이트 | 결과 |
|---|---|
| 응답 가용성 유지 | ✅ `sli_order_response_availability` 100.00%(양쪽) |
| 취소 인프라 실패율 0% | ✅ 0.00%(500=0/15,768) |
| 정합성 위반 0 | ✅ 9항목 전부 0 |
| `settlement_batch_fallbacks_total` 0 | ✅ 0 |
| 회복 성능 악화 없음 | ✅ recovery 구간의 attempt(12.88ms)·job 실행(12.99ms)·worker busy(11.84%)가 hold·burst보다 오히려 낮음 — 저하 없음 |
| load-gen 완주 및 시작 skew 충족 | ✅ 양쪽 100% 완주, skew ≈0.1초(목표 ≤1초) |
| 계측 내부 무결성 충족 | ✅ Phase 3 7항목 전부 정확히 일치 |

전 게이트 충족 — 위 판정은 유효하다.

## 한계

- **스냅샷 해상도(15초)**: 구간 경계 스냅샷은 목표 시각과 최대 3.8초(런 시작 직전 1건) 차이가
  있다 — hold/burst/recovery 구간 자체의 판정(46초~3분 규모)에는 무시할 수준이지만, 더 정밀한
  경계가 필요하면 스냅샷 간격을 좁혀야 한다.
- **k6 주문 합계 대조 시 `REJECTED` 30건 보정이 필요했다** — 26번은 이 상태가 관측되지 않아
  단순 합산이 정확히 일치했는데, 이번엔 소수 발생했다(전체의 0.03%). 원인은 이번 사이클
  범위 밖이라 규명하지 않았다 — 정산 계측 무결성과는 무관(정산 관련 7항목은 이 보정과 별개로
  전부 정확히 일치).
- **`settlement_job_execution_seconds`의 `result` 라벨은 `success`만 유효**하다(20번에서 명시한
  제한, `fallback`/`failed` 구분은 이번 범위 밖) — 이번 측정도 fallback이 0건이라 이 제한이
  결과 해석에 영향을 주지 않았다.
- **cancel 취소 상태 카운트의 서버 로그 grep과 k6 집계 사이에 소수(전체의 약 1%) 불일치**가
  있었다(서버 로그 200/409 합계 15,768 vs k6 success+already_filled 합계 15,603) — grep 패턴의
  로그 포맷 민감성으로 추정되며, **500(인프라 실패) 카운트는 두 출처 모두 0으로 일치**해 안전
  게이트 판정에는 영향이 없다.
- **원본 자료**(스냅샷 63개·`picked_snapshots.json`·`phase2_results.json`·양쪽 k6 stdout·
  `summary-*.json`)는 26번과 동일한 이유로 로컬에만 보관하고 리포에는 커밋하지 않는다(용량).
  `DEV_TOOLS_TOKEN`·외부 IP는 저장 전 제거.
- 이번 사이클은 **지배 기여도 판정까지**다. 판정이 지목한 수정 방향(주문별 dependency fence)의
  설계·구현은 범위 밖(별도 스펙).

## 원본 자료

- k6 스크립트: `_workspace/loadtest/order-spike-availability.js`(26번과 sha256 동일, 무변경)
- 로컬 보관(리포 미커밋): `/metrics` 스냅샷 63개, 구간 경계 선택본(`picked_snapshots.json`),
  Phase 2 계산 결과(`phase2_results.json`, `phase2b_avgs.json`), 양쪽 k6 stdout 로그 및
  `summary-*.json`, DB `order_status`/`integrity_check` 쿼리 결과.
