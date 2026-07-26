# 3차 ② 처리량 바인딩 링크 진단 runbook (진단 전용)

> **For agentic workers:** 이건 코드 TDD가 아니라 **진단 측정 runbook**이다. hands-on 세션에서
> Phase 0→4를 순서대로, 체크박스(`- [ ]`)로 진행하며 실행한다. **애플리케이션 코드 무수정.**

**Goal:** 현재 시스템(① 게이트 포함)의 실제 안정 상태에서 엔진→ExecutionCh→OutboxWriter→정산
큐→정산 워커 체인의 **최초 바인딩 링크를 확정**하고 진단 문서를 낸다. 수정은 하지 않는다.

**Architecture:** 앱 무수정. 단일 심볼 BTC crossing 주문으로 **ExecutionCh high-watermark(~768)
지속 + engine_gate 셰딩** 안정 상태를 만들고, 기존 게이지 + 반복 goroutine 프로파일 + CPU pprof로
정산 큐 깊이를 결정 신호로 삼아 판별한다. 로컬 우선, 신호 흐리면 GCP 승격.

**Tech Stack:** Go(net/http/pprof :6060), Prometheus 게이지, k6, curl, docker compose.

**스펙 문서:** `docs/superpowers/specs/2026-07-26-throughput-binding-link-diagnosis-design.md`

## Global Constraints

- **애플리케이션 코드 무수정.** env/구성/드라이버(측정 도구)만 허용. 특정 수정 설계 금지(다음 스펙).
- **① 게이트를 끄지 않는다.** 재현 목표는 채널 100% 포화가 아니라 **ExecutionCh high-watermark 지속
  + `orders_admission_rejected_total{stage="engine_gate"}` 증가**(현재 시스템의 실제 안정 상태).
- **결정 신호 = `settlement_worker_queue_length` 깊이**(단일 심볼이라 한 worker 인덱스에 집중).
- goroutine 프로파일은 **반복 수집**(순간 스냅샷). 단일 스택으로 단정 금지 — 지속성이 증거.
- GCP 사용 시 `DEV_TOOLS_TOKEN`·외부 IP 트랜스크립트 노출 금지, 측정 후 VM stop.
- 정확한 게이지: `settlement_worker_queue_length{worker}`, `matching_engine_channel_length{channel="execution"|"order"}`,
  처리율 = `order_settlement_duration_seconds_count` 증가율(runbook 고정 출처).

---

### Phase 0: Preflight (부하 시작 전 — 실패 시 진행 금지)

- [ ] **Step 1: ① 게이트 코드 확인** — `git log --oneline | Select-String "3차 ①"` 로 `1ff8d32`(하류
  포화 시 신규 주문 억제)가 HEAD 조상인지 확인. engine.go:30/163/341에 게이트 존재 확인.
- [ ] **Step 2: backend 기동(pprof on)** — 로컬 docker compose(stress)로 backend+DB 기동하되
  **`GOEXCHANGE_ENABLE_PPROF=true`** 오버라이드(compose 기본 false, docker-compose.stress.yml:24).
  예: `$env:GOEXCHANGE_ENABLE_PPROF="true"` 후 `docker compose -f docker-compose.stress.yml up -d --build backend`
  (또는 로컬 `go run ./cmd`에 env 주입). DB는 로컬 Postgres.
- [ ] **Step 3: pprof·metrics 도달 확인** — `curl http://127.0.0.1:6060/debug/pprof/` 200 확인 +
  `curl http://127.0.0.1:8080/metrics | Select-String "settlement_worker_queue_length|matching_engine_channel_length"`
  로 게이지 노출 확인. **하나라도 실패면 부하 시작 금지**(원인 해결 후 재시도).

---

### Phase 1: 로컬 재현 (안정 상태 도달)

- [ ] **Step 1: crossing 드라이버 준비** — 단일 심볼 BTC에 **crossing 주문**(매수가 매도를 넘겨
  즉시 체결)을 고속 주입하는 k6. 기존 `_workspace/loadtest/order-spike-availability.js`의
  `takerCrossingLimitFlow` 비중을 높인 변형 또는 전용 소형 드라이버(측정 도구, 앱 수정 아님).
  체결을 최대화해 `ExecutionCh`·정산 부하를 만든다.
- [ ] **Step 2: 안정 상태 도달** — VU를 올리며 다음을 동시 만족하는 **지속(수십 초) 안정 상태**를 만든다:
  - `matching_engine_channel_length{channel="execution"}`가 high-watermark(~768=1024×0.75) 부근 지속.
  - `orders_admission_rejected_total{stage="engine_gate"}` 지속 증가(셰딩 발생 = 재현 성립).
  - 채널 100% 포화는 목표 아님(①이 그 전에 셰딩).
- [ ] **Step 3: 로컬 유효성 판정 / GCP 승격 결정** — 다음이면 **즉시 GCP 승격**(미루지 않음):
  - high-watermark·셰딩이 **또렷이 지속되지 않음**(신호 흐림), 또는
  - **로컬 자원 경쟁 오염 징후**(로컬 Postgres CPU 포화 / 로컬 CPU 경합으로 "정산 워커 직렬화"가
    아니라 "로컬 DB 한계"로 왜곡 — top으로 DB CPU가 이미 포화면 로컬 신호 신뢰 불가).
  - 승격 시 GCP는 23번과 동일(서버·DB e2-highcpu-4, load-gen e2-standard-8), preflight(Phase 0) 재수행.

---

### Phase 2: 샘플 수집 (안정 상태에서)

- [ ] **Step 1: 게이지 시계열** — 15초 간격으로 여러 번:
  `curl -s http://127.0.0.1:8080/metrics | Select-String "settlement_worker_queue_length|matching_engine_channel_length|orders_admission_rejected_total"`
  → 각 시점 값 기록(특히 **정산 큐 깊이가 256에 지속 근접하는지**).
- [ ] **Step 2: 처리율(trades/s)** — `order_settlement_duration_seconds_count`를 T0/T1(예: 30초 간격)
  두 번 읽어 증가량/시간으로 정산 처리율 산출(runbook 고정 출처).
- [ ] **Step 3: 반복 goroutine 프로파일** — `/debug/pprof/goroutine?debug=2`를 **여러 번** 저장:
  baseline 1회 + hold 안정 상태 **10~15초 간격 ≥3회** + high-watermark/셰딩 직후 1회 + recovery 1회.
  `curl -s "http://127.0.0.1:6060/debug/pprof/goroutine?debug=2" > _workspace/diag-24/goroutine-<라벨>.txt`
  각 덤프에서 **엔진이 `chansend`(ExecutionCh)·OutboxWriter가 정산 큐 send·정산 워커가 DB syscall에
  각각 몇 개 막혀 있는지** 집계. 판정 증거 = **반복 덤프에서 같은 블로킹 위치 지속**.
- [ ] **Step 4: CPU pprof 30초** — `curl -s "http://127.0.0.1:6060/debug/pprof/profile?seconds=30" >
  _workspace/diag-24/cpu.pb.gz` → `go tool pprof -top cpu.pb.gz`(DB idle이면 CPU 적을 것 — 직렬화 방증).
- [ ] **Step 5: 서버·DB CPU** — top으로 backend·postgres CPU/idle 기록(로컬이면 자원 경쟁 판정에도 사용).

---

### Phase 3: 판정 (판별표 매칭)

- [ ] **Step 1: 최초 바인딩 링크 확정** — 스펙 판별표에 대입:
  - **정산 큐 256 지속 포화 + ExecutionCh high-watermark 지속 + 셰딩** → **정산 워커 바인딩**.
  - **정산 큐 여유 + ExecutionCh high-watermark 지속 + 셰딩** → **OutboxWriter 계층 후보** →
    goroutine 스택 + outbox flush 히스토그램(`trade_outbox_flush_*`)으로 세분화(outbox INSERT /
    OutboxWriter 직렬화 / sharded merge 채널 / forward 직전).
  - **두 큐 여유 + 셰딩 없음 + 처리량 캡** → **엔진 매칭 또는 부하 발생기** 조사(드라이버 한계 배제).
  - 반복 goroutine 덤프의 지속 블로킹 위치가 표 판정과 **일치하는지 교차 확인**.
- [ ] **Step 2: 신뢰도·환경 판정** — 로컬 신호가 흐리거나 오염이면 Phase 1 Step 3대로 GCP 재측정 결과로 판정.

---

### Phase 4: 진단 문서 + README

- [ ] **Step 1: 진단 문서** — `docs/benchmarks/24-<측정일>-throughput-binding-link-diagnosis.md`:
  왜(⑤의 직렬화 미확정) / 방법(① 게이트 유지, high-watermark+셰딩 재현, 게이지·반복 goroutine·CPU) /
  **최초 바인딩 링크 확정 + 판별표 어느 행** / 반복 덤프·게이지 증거 / **실제 사용 환경(로컬/GCP)과
  승격 여부·이유** / **다음 수정 방향 결정(별도 스펙 예고)**. 수치는 측정값 그대로.
- [ ] **Step 2: README** — 3차 표 ②-진단 🔨→✅ + 진단 문서 링크. 확정된 수정 방향을 다음 조각(②-수정)으로 예고.
- [ ] **Step 3: Commit + 푸시** — 문서 커밋(commit-message 스킬, 한글). Go 무변경이라 CI 회귀 없음.
- [ ] **Step 4: 정리** — GCP 썼으면 VM stop, 로컬이면 compose down.

---

## 다음 (범위 밖)

**②-수정**(진단 결과 확정 후 별도 스펙): 정산 워커 바인딩이면 워터마크식 정산 병렬화(체결 병렬풀 +
종결 이벤트 순서 보존), OutboxWriter 계층이면 세분화된 지점의 병렬화/최적화, 엔진이면 분할 검토.
