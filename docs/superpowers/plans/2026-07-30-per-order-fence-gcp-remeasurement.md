# 4차 축 1 — per-order fence GCP 재측정 runbook (29번)

> **For agentic workers:** 측정 runbook이다. Phase 0(preflight) → Phase 1(전체 재현) →
> Phase 2(계산) → Phase 3(무결성) → Phase 4(판정) → Phase 5(문서) 순서로, 체크박스로 진행한다.
> **이 문서는 계획만 담는다 — 29번 GCP 실행은 별도 측정 세션에서 수행한다.**

**Goal:** 28번이 GCP 스케일에서 확정한 **"파티션 전체 fence가 지배적"** 판정에 대해,
per-order fence + terminal durable defer(A+C, 이번 구현)가 실제로 그 배리어 대기를
없애는지 **구간별 수치로 판정**한다.

**완료 조건은 "GCP 실행 완료"가 아니다:**
> **28번이 실측한 배리어 대기(p50 13.48ms)가 per-order fence 도입 후 유의하게 줄었는지,
> 그리고 그 과정에서 처리량 회귀나 정합성 위반이 없는지 구간별 수치로 판정한다.**

**수정안 확대·목표 TPS는 이 문서에 넣지 않는다.**

**선행**: [28번 GCP 관측성 판정](../../benchmarks/28-2026-07-29-settlement-observability-gcp.md)
(파티션 전체 fence가 지배 원인) ·
[설계 스펙](../specs/2026-07-30-per-order-fence-and-terminal-durable-defer-design.md)
("실측 (29번)" 절) · [26번 runbook](2026-07-28-availability-spike-remeasurement.md)(하니스 원본)

## 비교 조건 (26번·28번과 동일 고정)

- 서버 인스턴스·DB 구성(e2-highcpu-4, 서울), `GOEXCHANGE_SETTLEMENT_CONCURRENCY=4`
- load-gen **2대 수평 분할**(e2-standard-8), 사용자 범위·`USER_INDEX_OFFSET`(A=0 / B=5000),
  `LOAD_START_AT_MS` 동시 시작
- k6 stage·VU·주문 비율 동일, DB 초기화 및 `bootstrap loaded=0`
- `ExecutionCh`·settlement queue 용량, 워터마크 **0.75** 그대로
- `--summary-export` 유지
- **애플리케이션 차이는 이번 A+C 구현(per-order fence + terminal durable defer)뿐**

**사전 확인(2026-07-30 시점, 실행 전 반드시 재확인)**:
`git diff --stat 8685923..HEAD -- '*.go'` → 26개 파일, +1886/-301. 이 범위는 B(dependency
guard)와 A+C(per-order fence + terminal durable defer) 전체를 포함하며, 정산 경로 외의
기능 변경은 없다. **실행 시점에 이 diff를 다시 확인**하고, 무관한 변경이 섞였으면 직접
비교가 약해짐을 문서에 명시한다.

---

### Secret preflight

[26번/28번 runbook](2026-07-28-availability-spike-remeasurement.md)과 동일한 secret
교체·검증 절차를 그대로 따른다 — 실제 토큰 값과 fingerprint는 문서 및 아티팩트에 기록하지
않는다. 검증 실패 시 전체 부하 실행을 금지한다.

### Deployment source guard

- 배포 원본은 **현재 `go-exchange-back` 디렉터리와 그 환경 파일로 고정**한다.
- **과거 `bench-*` 디렉터리는 배포 원본으로 사용하지 않는다.**
- secret 값을 비교하거나 출력하지 말고 **실제 DB 연결 성공으로 유효성을 검증**한다.
- **bootstrap 및 DB 연결 검증 실패 시 부하 실행을 금지**한다.

---

### Phase 0: Preflight (낮은 VU 1~2분 — 실패 시 전체 실행 금지)

- [ ] **Step 1: 코드 동일성** — 위 `git diff --stat` 재확인(A+C 구현 외 Go 변경 없음).
- [ ] **Step 2: 저부하 1~2분 실행** 후 다음을 **전부** 확인:
  - 신규 메트릭이 `/metrics`에 노출: `settlement_terminal_wait_seconds{kind}`,
    `settlement_outstanding_jobs{partition}`, `settlement_quarantined_orders{partition}`,
    `settlement_dependency_record_failed_total`, `settlement_duplicate_terminal_total`
  - `settlement_barriers_total`/`settlement_barrier_wait_seconds`/
    `settlement_barrier_inflight_batches`가 **더 이상 노출되지 않음**(폐기 확인)
  - histogram의 `_bucket`·`_sum`·`_count` 모두 수집됨
  - 시작·중간·종료 스냅샷이 생성됨
  - 두 load-gen 시작 skew ≤ 1초
  - `settlement_terminal_wait_seconds{kind}` count = 정상 처리된 terminal(cancel+market_done) 수
  - dispatch wait count = execution count = 완료 job 수
  - 기존 SLI·정합성 검사 정상
- [ ] **Step 3: 게이트** — 하나라도 어긋나면 **전체 실행 금지**. 원인 해결 후 Phase 0 재수행.

---

### Phase 1: 26번·28번 동일 규모 재현

- [ ] **Step 1: 리셋 + 기동** — 9(+2)테이블 TRUNCATE + `bootstrap loaded=0` + 기동 로그
  `settlement partitions=.. concurrency=4` 확인. `GOEXCHANGE_ENABLE_PPROF=true`.
  (테이블 수는 이번 구현이 추가한 `failed_order_cancellations`를 포함해 기존 9개에서 갱신)
- [ ] **Step 2: 부하** — 26번·28번과 동일 프로파일·분할·배리어(`LOAD_START_AT_MS`). 실제
  시나리오 시작 시각을 양쪽 k6 로그에서 확인·기록(skew ≤1초 목표, 2초 초과 시 폐기·재실행).
- [ ] **Step 3: 구간별 스냅샷** — **hold·burst·recovery 각 구간의 최소 시작·종료 스냅샷**을
  남긴다. **최종 누적값만으로 판정하지 않는다.** 각 스냅샷의 캡처 시각을 함께 기록.
- [ ] **Step 4: 드레인 + 정합성 5검사 + fallback** — 26번·28번과 동일.
- [ ] **Step 5: 원본 보존** — 스냅샷·k6 stdout·`summary-*.json`·시각 기록을 `_workspace/`에.
  **토큰·외부 IP 제거.**

---

### Phase 2: 구간별 계산

각 구간(hold / burst / recovery)에서:

```
terminal wait p50/p99     = settlement_terminal_wait_seconds{kind} 히스토그램에서 산출
outstanding jobs 평균/최대 = Δsettlement_outstanding_jobs{partition} 샘플 평균·최대
quarantine 누적            = 최종 settlement_quarantined_orders{partition}(0이어야 정상)
dependency record 실패     = Δsettlement_dependency_record_failed_total(0이어야 정상)
평균 trade batch           = Δsettled_trades / Δcompleted_batch_jobs
DB attempt 빈도            = Δsettlement_attempt_duration_seconds_count / 구간 시간
worker busy ratio          = Δsettlement_job_execution_seconds_sum / (CONCURRENCY × 구간 시간)
dispatch wait 평균         = Δdispatch_wait_sum / Δdispatch_wait_count
```

**해석 주의**: `settlement_outstanding_jobs`는 Gauge다 — "얼마나 오래 `2N`에 붙어 있었는가"를
봐야 한다(순간 분포가 아니라 지속 시간). 28번의 barrier wait duty와 달리 이번 지표는 파티션이
막힌 시간이 아니라 **동시 실행 중인 job 수**를 나타내므로 직접 비교하지 말고 별개 신호로 읽는다.

---

### Phase 3: 계측 내부 무결성 (결과 해석 **전에** 검증)

- [ ] `settlement_terminal_wait_seconds{cancel}` count = 정상 처리된 cancel terminal 수
- [ ] `settlement_terminal_wait_seconds{market_done}` count = market done 수
- [ ] dispatch wait count = job execution count
- [ ] batch attempt count **≥** logical batch job count
- [ ] 그 차이가 **실제 retry 로그와 일치**
- [ ] settlement batch의 trade 합계 = settled trade 증가량
- [ ] k6 주문 합계 = 서버·DB 주문 합계
- [ ] `settlement_quarantined_orders`가 실행 종료 시점에 **0**(0이 아니면 미해결 quarantine —
  원인 규명 전까지 판정 보류)
- [ ] **`settlement_duplicate_terminal_total` = 0 (비협상)** — 주문당 terminal 1개는 엔진
  불변식이다. 0이 아니면 **엔진이나 outbox 경로가 불변식을 깬 것**이므로 성능 수치를 읽지 않고
  원인을 먼저 규명한다. 종류는 `duplicate terminal for order` 오류 로그로 식별한다.
- [ ] **하나라도 불일치면 계측 문제로 보고 판정을 보류**하고 원인부터 규명한다.

---

### Phase 4: 판정 분기

설계 스펙의 "실측 (29번)" 절 그대로:

| 판정 | 기준 |
|---|---|
| **주 가설** | `settlement_terminal_wait_seconds` p50이 **배리어 대기 13.48ms(28번) 대비 유의하게 감소** |
| **처리량** | worker busy가 **13.1%(28번)에서 상승**, 배치 크기가 **2.82(28번)에서 상승** |
| **정합성(비협상)** | 무결성 검사 전항목 통과 — 이게 깨지면 다른 수치는 읽지 않는다 |
| **회귀 없음** | `sli_cancel_success` 100%(26번) 유지, 가용성 하드 보장 유지 |
| **부작용 감시** | Gauge `settlement_outstanding_jobs{partition}`가 `2N`에 **상시** 붙어 있으면 dispatcher가 새 병목 |

**해석 전 무결성 검사를 먼저 통과시킨다.** 통과 전 성능 수치는 읽지 않는다.

- [ ] 판정 수행 및 기록.

---

### Phase 5: 안전 게이트 + 문서

- [ ] **성능 결과는 다음이 모두 만족될 때만 유효**: 응답 가용성 유지 · 취소 인프라 실패율 0% ·
  정합성 위반 0 · fallback 0 · 회복 성능 악화 없음 · load-gen 완주 및 시작 skew 충족 ·
  **계측 내부 무결성 충족**(Phase 3) · quarantine 잔존 0 · **중복 terminal 0**.
- [ ] **문서** — `docs/benchmarks/29-<날짜>-per-order-fence-gcp-remeasurement.md`:
  비교 조건 동일성(diff 확인 포함) / 구간별 계산표 / 무결성 검증 결과 / **판정(주 가설·처리량·
  부작용)** / 28번 대비(배리어 대기 제거 여부) / 한계.
- [ ] **README** 4차 현재 단계 갱신, 완료 문서에 "GCP 스케일 최종 판정" 링크 추가.
- [ ] **Commit + 푸시 + CI**, **모든 VM stop**(load-gen 2대 포함).

---

## 다음 (범위 밖)

축 2(매칭 quantum) 계측. `settlement_job_execution_seconds`의 `fallback`/`failed` 라벨 구분.
`unsafeOrders` eviction 정책(gauge로 먼저 관측 후 판단). 저장소 수준의 중복 terminal 탐지.
