# 3차 마무리: 정산 병렬화 처리량 재측정 runbook (24번 재실행)

> **For agentic workers:** 코드 TDD가 아니라 **측정 runbook**이다. Phase 0→4를 순서대로,
> 체크박스(`- [ ]`)로 진행하며 실행한다. **애플리케이션 코드 무수정.**

**Goal:** ②-수정(정산 병렬화)이 24번이 확정한 **정산 워커 바인딩을 실제로 풀었는지**를
`GOEXCHANGE_SETTLEMENT_CONCURRENCY` **1/2/4/8 스윕**으로 측정하고, **새 바인딩 링크가 어디로
옮겨갔는지**를 판정한다. ①②③·②-수정이 미뤄온 처리량 수치를 여기서 처음 보고한다.

**Architecture:** 24번과 **같은 하니스·같은 신호**(crossing-flood 드라이버 + 정산 큐/채널 게이지 +
반복 goroutine 덤프 + CPU). 바뀌는 것은 `CONCURRENCY` env 하나뿐 — same-binary 스윕이라 빌드·배포
변수가 0이다.

**선행 문서:**
- 진단: `docs/benchmarks/24-2026-07-26-throughput-binding-link-diagnosis.md`
- 구현: `docs/refactor/18_3차②_정산_병렬화_완료.md`, 스펙 `docs/superpowers/specs/2026-07-27-settlement-parallelization-design.md`

## Global Constraints

- **애플리케이션 코드 무수정.** env·드라이버(측정 도구)만.
- **same-binary 스윕**: 한 번 빌드/기동한 바이너리에 `CONCURRENCY`만 바꿔 재시작. 측정 간
  **리셋(9테이블 TRUNCATE + 재시작 + `bootstrap loaded=0`)** 필수.
- **N=1은 기준선(baseline)** — 24번(직렬)과 **같은 그림이 재현돼야** 스윕을 신뢰할 수 있다(아래 Phase 2 게이트).
- 수치는 측정값 그대로. 개선이 없거나 역효과여도 그대로 기록(22번 outbox 상한 사례처럼).
- **정합성 5검사는 각 측정 후 필수**(돈이 움직이는 경로를 병렬화했으므로 — 이번 재측정의 절반은 정합성 확인이다).
- 로컬 우선(24번이 로컬에서 깨끗한 신호를 얻었고 자원 경쟁도 배제됨). 신호가 흐리거나 자원 경쟁이
  개입하면 **즉시 GCP 승격**(23번과 동일 구성) 후 Phase 0부터 재수행.

---

### Phase 0: Preflight (실패 시 부하 시작 금지)

- [ ] **Step 1: 코드 확인** — HEAD가 ②-수정(`57663b1` 이후)을 포함하는지, `cmd/settlement_pipeline.go`
  존재 확인. `git log --oneline -3`.
- [ ] **Step 2: 기동(pprof on + 스윕 env 전달)** — `GOEXCHANGE_ENABLE_PPROF=true`로 기동.
  스트레스 compose를 쓸 경우 `docker-compose.stress.yml:29`가 `GOEXCHANGE_SETTLEMENT_CONCURRENCY`를
  전달하므로 **셸에서 그 값을 설정해야** 스윕이 반영된다(설정 안 하면 앱 기본값 4로만 돈다 — 최종
  리뷰가 잡은 함정).
- [ ] **Step 3: 도달 확인** — `curl http://127.0.0.1:6060/debug/pprof/` 200 +
  `/metrics`에 `settlement_worker_queue_length`·`matching_engine_channel_length` 노출 확인.
- [ ] **Step 4: 기동 로그로 구성 확인** — `settlement partitions=<P> concurrency=<N>` 로그가 **의도한
  N**인지 눈으로 확인(스윕마다 매번). 이게 스윕이 실제로 적용됐다는 유일한 직접 증거다.

---

### Phase 1: 측정 절차 (N마다 반복)

각 `N ∈ {1, 2, 4, 8}`에 대해:

- [ ] **Step 1: 리셋** — 9테이블 `TRUNCATE ... RESTART IDENTITY CASCADE` + backend 재시작(해당 N으로)
  + 기동 로그 `bootstrap loaded=0` 및 `concurrency=N` 확인.
- [ ] **Step 2: 부하** — `_workspace/loadtest/crossing-flood.js`를 24번과 **동일한 VU·지속시간**으로
  실행(24번 문서의 값을 그대로 따른다 — 비교 가능성이 핵심).
- [ ] **Step 3: 안정 상태 샘플(로드 중반, 15초 간격 수 회)**
  - **`settlement_worker_queue_length{worker="*"}`** ← **핵심 신호**(24번에서 worker 8이 256 고정)
  - `matching_engine_channel_length{channel="execution"|"order"}`
  - `orders_admission_rejected_total{stage="engine_gate"}` 증가율(셰딩 여부)
  - **처리율(trades/s)**: `SELECT count(*) FROM trades` **T0/T1 델타**(24번과 동일 출처 —
    `order_settlement_duration_seconds_count`는 24번이 확인한 **죽은 메트릭**이라 쓰지 않는다)
  - 서버·DB CPU(top), **DB 커넥션 사용량**(N=8에서 풀 25 경합 여부 — 새 병목 후보)
- [ ] **Step 4: 반복 goroutine 덤프** — `/debug/pprof/goroutine?debug=2`를 안정 상태에서
  **10~15초 간격 ≥3회** 저장. 정산 worker들이 어디서 막혀 있는지(pgx Exec? 락 대기? jobs 수신 대기?)
  **지속 패턴**으로 판정(단일 스냅샷 단정 금지).
- [ ] **Step 5: CPU pprof 30초** — `/debug/pprof/profile?seconds=30`.
- [ ] **Step 6: 드레인 + 정합성 5검사** — 채널·큐 0 확인 후: 리컨실리에이션 게이지 2종(원장-지갑,
  자산 총량) · `failed_settlements` · `failed_market_completions` · 시장가 PENDING/PARTIAL 잔존 ·
  outbox PENDING 잔존 · `settlement_batch_fallbacks_total`. **전부 0이어야 한다(비협상).**
- [ ] **Step 7: 기록** — 위 값을 N별 표로 저장(`_workspace/`).

---

### Phase 2: 기준선 게이트 (N=1)

- [ ] **Step 1: 24번 재현 확인** — N=1 결과가 24번 진단과 **같은 그림**인지:
  정산 큐 1개가 256 고정 포화 · 나머지 0 · 처리율 24번의 **~350 trades/s** 근방.
- [ ] **Step 2: 판정** — 재현되면 스윕을 신뢰하고 진행. **재현이 안 되면**(부하·환경이 달라졌다는 뜻)
  스윕 수치를 24번과 비교하지 말고, 원인을 먼저 규명하거나 GCP로 승격해 다시 잡는다.

---

### Phase 3: 판정

- [ ] **Step 1: 바인딩 해소 여부** — N이 커질 때 **정산 큐 깊이가 256 고정에서 풀리는지**.
  풀리면 "정산 워커 바인딩 해소" 확정.
- [ ] **Step 2: 처리량 스케일링** — trades/s를 N=1 대비로 표기. **N에 비례하지 않을 수 있다**
  (동일 지갑/주문 행 락 직렬화 — 스펙이 예고한 한계). 비례하지 않으면 **어디서 꺾이는지**를 기록.
- [ ] **Step 3: 새 바인딩 링크 식별** — 아래 중 무엇인지 goroutine 덤프·게이지·CPU로 판정:
  - `ExecutionCh` high-watermark 지속 + engine_gate 셰딩 → **엔진/emit 경로**
  - 정산 큐는 비었는데 처리량 정체 + DB CPU 상승 → **DB(진짜 용량)**
  - worker들이 락 대기에 몰림 → **행 락 경합(동일 지갑/주문)**
  - DB 커넥션 풀 고갈 징후 → **풀 25 상한**(→ `DB_MAX_OPEN_CONNS` 조정 후보)
- [ ] **Step 4: 정합성 판정** — 모든 N에서 5검사 0. **하나라도 위반이면 그것이 최우선 결과**이며,
  병렬화 롤백(`CONCURRENCY=1`)을 즉시 권고하고 원인 규명으로 전환한다.
- [ ] **Step 5: 권장 운영값** — 처리량·정합성·풀 여유를 종합해 **기본 `CONCURRENCY` 권장치**를 제시
  (현재 기본 4가 적절한지, 조정이 필요한지).

---

### Phase 4: 문서 + README

- [ ] **Step 1: 벤치마크 문서** — `docs/benchmarks/25-<측정일>-settlement-parallelization-remeasurement.md`:
  왜(24번이 확정한 바인딩을 ②-수정이 풀었는지) / 방법(same-binary 스윕, 24번과 동일 하니스) /
  **N별 표**(정산 큐 깊이·채널·trades/s·CPU·풀·정합성) / **판정 3종**(바인딩 해소·스케일링·새 병목) /
  권장 운영값 / 한계(로컬 vs GCP, 픽스처 특성). 24번과의 비교는 **N=1 기준선이 재현된 경우에만**.
- [ ] **Step 2: 완료 문서 보강** — `docs/refactor/18_3차②_정산_병렬화_완료.md`의 "처리량 실증은
  재측정" 문장에 **이 문서 링크와 결과 한 줄**을 추가(미뤄둔 수치를 닫는다).
- [ ] **Step 3: README** — 3차 표에 재측정 결과 반영 + **3차 리팩토링 완결** 표기(코드 ③①②-진단②-수정
  + 실증). 남은 병목은 백로그로 승격.
- [ ] **Step 4: Commit + 푸시 + CI** — 문서 커밋(Conventional Commits·한글, 스킬 불가 시 직접 작성).
- [ ] **Step 5: 정리** — GCP 썼으면 VM stop, 로컬이면 compose down.

---

## 다음 (범위 밖)

Phase 3에서 식별된 **새 바인딩 링크**에 대한 수정 설계(별도 스펙 — 안 재고 고치지 않는다는 원칙 유지).
관측성 후속: 라이브 배치 정산 경로를 관측하지 못하는 `order_settlement_duration_seconds_count`
(24번 발견). 23번 재실행(SLI 3종으로 취소 성공률·응답 가용성·업무 성공률 종합 판정)은 별도 사이클.
