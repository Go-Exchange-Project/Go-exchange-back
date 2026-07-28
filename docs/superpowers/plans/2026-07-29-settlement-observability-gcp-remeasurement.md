# 4차 축 1 — GCP 스케일 관측성 재측정 runbook (26번 하니스 + 신규 계측)

> **For agentic workers:** 측정 runbook이다. Phase 0(preflight) → Phase 1(전체 재현) →
> Phase 2(계산) → Phase 3(무결성) → Phase 4(판정) → Phase 5(문서) 순서로, 체크박스로 진행한다.
> **애플리케이션 코드 무수정.**

**Goal:** 27번이 GCP 스케일에서 확정한 **정산 경로 바인딩**의 내부 지배 원인을, 20번에서 추가한
계측 6종으로 **구간별 수치로 판정**한다.

**완료 조건은 "GCP 실행 완료"가 아니다:**
> **27번의 정산 큐 포화를 재현하고, batch 파편화 · 파티션 fence · DB transaction · worker
> scheduling 중 지배 기여도를 구간별 수치로 판정한다.**

**수정안·목표 TPS는 이 문서에 넣지 않는다.**

**선행**: [27번 재분석](../../benchmarks/27-2026-07-28-settlement-binding-reanalysis.md) ·
[20번 관측성 완료](../../refactor/20_4차축1_정산_관측성_완료.md) ·
[26번 runbook](2026-07-28-availability-spike-remeasurement.md)(하니스 원본)

## 비교 조건 (26번과 동일 고정)

- 서버 인스턴스·DB 구성(e2-highcpu-4, 서울), `GOEXCHANGE_SETTLEMENT_CONCURRENCY=4`
- load-gen **2대 수평 분할**, 사용자 범위·`USER_INDEX_OFFSET`(A=0 / B=5000), `LOAD_START_AT_MS` 동시 시작
- k6 stage·VU·주문 비율 동일, DB 초기화 및 `bootstrap loaded=0`
- `ExecutionCh`·settlement queue 용량, 워터마크 **0.75** 그대로
- **애플리케이션 차이는 관측성 패치만**

**사전 확인 결과(2026-07-29 시점)**: `git diff --stat 762a178..HEAD -- '*.go'` → `cmd/main.go`(+10),
`cmd/settlement_pipeline.go`(+32), `internal/metrics/metrics.go`(+54) + 테스트 3개. **기능 변경 0.**
**실행 시점에 이 diff를 다시 확인**하고, 기능 변경이 섞였으면 직접 비교가 약해짐을 문서에 명시한다.

---

### Phase 0: Preflight (낮은 VU 1~2분 — 실패 시 전체 실행 금지)

비싼 전체 실행을 계측 실수로 버리지 않기 위한 단계다.

- [x] **Step 1: 코드 동일성** — 위 `git diff --stat` 재확인(관측성 패치 외 Go 변경 없음).
- [x] **Step 2: 저부하 1~2분 실행** 후 다음을 **전부** 확인:
  - 신규 메트릭 **6종이 `/metrics`에 노출**
  - histogram의 **`_bucket`·`_sum`·`_count` 모두** 수집됨
  - **시작·중간·종료 스냅샷이 생성**됨
  - 두 load-gen **시작 skew ≤ 1초**
  - **`barriers_total`과 k6 Done/Cancel 수 일치**
  - **dispatch wait count = execution count = 완료 job 수**
  - 기존 SLI·정합성 검사 정상
- [x] **Step 3: 게이트** — 하나라도 어긋나면 **전체 실행 금지**. 원인 해결 후 Phase 0 재수행.
  (7항목 전부 통과 — [28번 문서](../../benchmarks/28-2026-07-29-settlement-observability-gcp.md) Phase 0 참고)

---

### Phase 1: 26번 동일 규모 재현

- [x] **Step 1: 리셋 + 기동** — 9테이블 TRUNCATE + `bootstrap loaded=0` + 기동 로그
  `settlement partitions=.. concurrency=4` 확인. `GOEXCHANGE_ENABLE_PPROF=true`.
- [x] **Step 2: 부하** — 26번과 동일 프로파일·분할·배리어(`LOAD_START_AT_MS`). 실제 시나리오 시작
  시각을 양쪽 k6 로그에서 확인·기록(skew ≤1초 목표, 2초 초과 시 폐기·재실행).
  (실측 skew ≈ 0.1초, 양쪽 6m35.8s/6m35.9s)
- [x] **Step 3: 구간별 스냅샷** — **hold·burst·recovery 각 구간의 최소 시작·종료 스냅샷**을 남긴다.
  **최종 누적값만으로 판정하지 않는다.** 각 스냅샷의 캡처 시각을 함께 기록.
  (기존 게이지·SLI·정합성 수집은 26번 그대로 병행.)
  (15초 간격 63개 스냅샷, 구간 경계 선택본 `picked_snapshots.json`)
- [x] **Step 4: 드레인 + 정합성 5검사 + fallback** — 26번과 동일. (9항목 전부 0)
- [x] **Step 5: 원본 보존** — 스냅샷·k6 stdout·`summary-*.json`·시각 기록을 `_workspace/`에.
  **토큰·외부 IP 제거.** (26번과 동일하게 용량 문제로 로컬 보관, 리포 미커밋)

---

### Phase 2: 구간별 계산

각 구간(hold / burst / recovery)에서:

```
barrier 빈도        = Δsettlement_barriers_total / 구간 시간
barrier wait duty   = Δsettlement_barrier_wait_seconds_sum / 구간 시간
평균 in-flight      = Δinflight_sum / Δinflight_count
평균 trade batch    = Δsettled_trades / Δcompleted_batch_jobs
DB attempt 빈도     = Δsettlement_attempt_duration_seconds_count / 구간 시간
재시도 추정치       = batch attempt count − logical batch job count
worker busy ratio   = Δsettlement_job_execution_seconds_sum / (CONCURRENCY × 구간 시간)
dispatch wait 평균  = Δdispatch_wait_sum / Δdispatch_wait_count
```

**해석 주의**: BTC 단일 파티션에서는 barrier wait가 dispatcher에서 **순차적으로** 발생하므로
`wait_sum / 구간 시간`을 **배리어 대기 duty**로 해석할 수 있다. **여러 활성 파티션을 합산하면 이
값이 100%를 넘을 수 있으므로 파티션별로 해석**한다.

---

### Phase 3: 계측 내부 무결성 (결과 해석 **전에** 검증)

- [x] `barriers_total{cancel}` = 정상 처리된 cancel terminal 수 (5,570=5,570)
- [x] `barriers_total{market_done}` = market done 수 (17,313=17,313)
- [x] dispatch wait count = job execution count (18,087=18,087)
- [x] batch attempt count **≥** logical batch job count (18,087=18,087)
- [x] 그 차이가 **실제 retry 로그와 일치** (차이 0, 재시도 로그도 0)
- [x] settlement batch의 trade 합계 = settled trade 증가량 (48,835=48,835)
- [x] k6 주문 합계 = 서버·DB 주문 합계 (86,769+REJECTED 30=86,799=86,799)
- [x] **하나라도 불일치면 계측 문제로 보고 판정을 보류**하고 원인부터 규명한다.
  (7항목 전부 일치 — 판정 진행)

---

### Phase 4: 판정 분기

| 신호 조합 | 지배 원인 | (후속) 방향 |
|---|---|---|
| 평균 batch 계속 **2~3건** + barrier 빈도 **높음** + wait duty **낮음** + worker busy 낮음/중간 | **Batch 파편화** — 기다림이 아니라 terminal이 batch 경계를 자주 만든다 | 종결 의존성을 지키며 terminal 경계 너머 trade를 묶는 dependency-aware scheduling |
| wait duty **큼** + 진입 시 in-flight 자주 **≥1** + dispatcher 진행이 반복 정지 | **파티션 전체 fence** | 주문별 dependency fence |
| attempt duration **큼** + worker busy **높음** + DB 실행이 wait보다 지배 + dispatch wait도 포화와 함께 증가 | **DB transaction** | SQL·락·왕복 |
| dispatch wait **큼** + worker 실행 시간 상대적으로 작음 + jobs 채널/공용 pool 대기 두드러짐 | **Worker scheduling** | pool·dispatcher 연결 방식 |

- [x] **둘 이상의 신호가 동시에 강하면 단일 원인으로 몰지 말고 기여도를 함께 기록**한다.
  (파티션 전체 fence가 지배적 — 배치 파편화 신호는 그 fence의 직접적 하류 결과로 함께 기록.
  자세한 근거는 [28번 문서](../../benchmarks/28-2026-07-29-settlement-observability-gcp.md) Phase 4)

---

### Phase 5: 안전 게이트 + 문서

- [x] **성능 결과는 다음이 모두 만족될 때만 유효**: 응답 가용성 유지 · 취소 인프라 실패율 0% ·
  정합성 위반 0 · fallback 0 · 회복 성능 악화 없음 · load-gen 완주 및 시작 skew 충족 ·
  **계측 내부 무결성 충족**(Phase 3). (전 게이트 충족)
- [x] **문서** — `docs/benchmarks/28-2026-07-29-settlement-observability-gcp.md`:
  비교 조건 동일성(diff 확인 포함) / 구간별 계산표 / 무결성 검증 결과 / **판정(지배 기여도)** /
  27번 대비(정산 큐 포화 재현 여부) / 한계.
- [x] **README** 4차 현재 단계 갱신, 20번 완료 문서에 "GCP 스케일 판정" 링크 추가.
- [ ] **Commit + 푸시 + CI**, **모든 VM stop**(load-gen 2대 포함).

---

## 다음 (범위 밖)

Phase 4가 지목한 지배 원인에 대한 **수정 설계**(별도 스펙). 축 2(`executions_per_order`·매칭
quantum) 계측. `settlement_job_execution_seconds`의 `fallback`/`failed` 라벨 구분.
