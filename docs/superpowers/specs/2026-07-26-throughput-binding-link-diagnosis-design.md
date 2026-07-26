# 3차 ② 처리량 바인딩 링크 진단 설계 (진단 전용)

- **날짜**: 2026-07-26
- **상태**: 설계 검토 중
- **로드맵**: [3차 리팩토링 ②](../../refactor/README.md) — BTC 단일 심볼 처리량. **이 사이클은 진단 전용.**
- **근거**: 23번(⑤)이 "DB idle 75~86%인데 hold(5,000)에서 93% 셰딩"을 실측 — 병목이 DB에서
  엔진/`ExecutionCh` 다운스트림으로 옮겨간 **직렬화**임을 밝혔으나, 체인의 어느 링크가 묶는지는
  pprof가 없어 미확정. **안 재고 안 고친다** — 최초 바인딩 링크를 확정하고 진단 문서를 낸다.

## 이 사이클의 경계

- **진단 전용.** 애플리케이션 코드 무수정. 기존 계측(`/debug/pprof` :6060, 채널 게이지, 정산 큐
  게이지)만 사용.
- 산출물 = **최초 바인딩 링크 확정 + 진단 문서**. **수정 설계는 진단 결과 확정 후 별도 스펙.**
  지금 워터마크식 정산 병렬화 등 특정 수정을 확정하지 않는다(엉뚱한 병목에 정성 들이는 것 방지).

## 진단이 답할 질문 — 최초 바인딩 링크는?

체인: **엔진 매칭 → `ExecutionCh`(1024) → OutboxWriter(단일 소비자) → 정산 큐(256, 심볼 해시)
→ 정산 워커(심볼당 1개, `main.go:426`) → DB.** 직렬 체인이라 가장 느린 링크가 먼저 backup되며
역류한다. **결정적 판별 신호 = 정산 큐 깊이**(게이지 이미 존재, `RegisterSettlementWorkerQueueGauges`
main.go:170).

**중요 — ① 게이트가 관측 상태를 바꾼다**: 3차 ①이 이미 반영돼(engine.go:30/163/341), 정상 작동일수록
`ExecutionCh`는 **1024까지 안 차고 ~768(high-watermark) 부근에서 engine_gate 셰딩이 시작**된다.
따라서 재현 조건은 "채널 100% 포화"가 아니라 **"ExecutionCh가 high-watermark에 지속 도달 +
engine_gate 셰딩 발생"**이다. **① 게이트를 끄고 과거 포화를 재현하지 않는다** — 이번 진단은
**현재 시스템의 실제 안정 상태**에서 최초 제한 링크를 찾는 것이 목적이다.

| 로드 중 관측 | 최초 바인딩 링크 | (후속) 방향 |
|---|---|---|
| **정산 큐 256 지속 포화** + ExecutionCh high-watermark 지속 + 셰딩 | **정산 워커**(심볼당 1개) | 정산 병렬화(체결 병렬풀 + 종결 이벤트 워터마크 순서 보존) |
| **정산 큐 여유** + ExecutionCh high-watermark 지속 + 셰딩 | **OutboxWriter 계층 후보** | goroutine 스택 + outbox flush 히스토그램으로 **세분화**(outbox batch INSERT / OutboxWriter 직렬화 / sharded execution merge 채널 / 정산 큐 forward 직전 경로) |
| **두 큐 모두 여유 + 셰딩 없음** + 처리량 캡 | **엔진 매칭 또는 부하 발생기** | 조사(엔진 분할은 단일 심볼 난제; 드라이버 한계도 배제) |

강한 사전 가설(코드 근거)은 "정산 워커"지만, **가설이지 결론이 아니다** — 정산 큐 게이지가 갈라준다.

## 진단 방법 (측정, 앱 무수정)

0. **Preflight(부하 시작 전 필수)**: pprof 서버는 `GOEXCHANGE_ENABLE_PPROF=true`일 때만 뜬다
   (main.go:42; `docker-compose.stress.yml`은 기본 false, line 24). ① `GOEXCHANGE_ENABLE_PPROF=true`
   확인 → ② `curl http://127.0.0.1:6060/debug/pprof/` 성공 확인 → **실패 시 부하 시작 금지**.
   (env/구성 변경이라 "앱 무수정" 경계와 충돌 없음.)
1. **재현**: 단일 심볼 BTC에 **crossing 주문**을 고속 주입해 체결을 대량 유발 → **ExecutionCh가
   high-watermark(~768)에 지속 도달 + engine_gate 셰딩이 발생하는 안정 상태**를 만든다(채널 100%
   포화가 목표가 아님 — ①이 그 전에 셰딩하며, 게이트를 끄지 않는다). 드라이버는 기존
   `order-spike-availability.js`의 crossing 흐름을 높인 변형 또는 전용 소형 드라이버(측정 도구이지 앱 수정 아님).
2. **샘플(안정 상태)**:
   - `matching_engine_channel_length{channel="execution"|"order"}` — 이미 존재.
   - **정산 큐 깊이** `settlement_worker_queue_*` — 이미 존재(핵심 신호).
   - `orders_admission_rejected_total{stage="engine_gate"}` 증가 — 셰딩 발생 확인(재현 성립 판정).
   - **정산 처리율(trades/s)** — 출처 고정: `order_settlement_duration_seconds_count` 증가율 **또는**
     trade 테이블 row 증가량 중 runbook에서 하나로 고정.
   - **CPU pprof 30초**(`/debug/pprof/profile`) — CPU 소재(DB idle이면 적을 것).
   - **goroutine 프로파일 — 반복 수집**(`/debug/pprof/goroutine?debug=2`는 순간 스냅샷이지 누적
     block profile 아님): baseline 1회 + hold 안정 상태 10~15초 간격 **≥3회** + high-watermark/셰딩
     직후 1회 + recovery 1회. **판정 증거 = 반복 덤프에서 같은 블로킹 위치가 지속됐는지**(단일
     스택으로 단정 금지). 엔진이 `chansend`(ExecutionCh)에, OutboxWriter가 정산 큐 send에, 정산
     워커가 DB syscall에 몇 개나 막혀 있는지. **앱 무수정으로 가능**.
   - 서버·DB CPU(top).
3. **block profile 주의**: `runtime.SetBlockProfileRate`가 앱에 설정돼 있지 않아 **block profile은
   앱 무수정으론 불가**. 대신 **goroutine 프로파일**(블록된 goroutine 현재 스택)이 같은 질문
   ("어디서 막혀 있나")에 답하므로 그것으로 대체한다. block profile을 굳이 원하면 rate 설정(1줄
   진단 토글)은 실행 시 판단 — 그러나 goroutine 덤프로 충분.

## 재현 위치 — 로컬 우선, 조건부 GCP 승격

이건 스케일이 아니라 직렬화 문제라 로컬 재현 가능성이 높다. **로컬 우선.** 단, 다음이면 **즉시
GCP 측정으로 승격**한다:
- 로컬에서 **구조적 포화 신호가 명확하지 않음**(정산 큐/ExecutionCh가 깔끔히 포화되지 않거나
  판별 표의 어느 행에도 또렷이 안 들어맞음).
- **로컬 자원 경쟁이 개입**(로컬 단일 Postgres·CPU 경합이 정산 워커 신호를 오염 — 예: 로컬 DB가
  포화돼 "정산 워커 직렬화"가 아니라 "로컬 DB 한계"로 왜곡).

즉 로컬은 **빠른 1차 확인**이고, 신호가 흐리면 미루지 않고 GCP(e2-highcpu-4, 23번과 동일 환경)로
올려 깨끗한 신호를 얻는다.

## 산출물 (outcome)

- `docs/benchmarks/24-<측정일>-throughput-binding-link-diagnosis.md` — 재현 방법 · 게이지/프로파일
  증거 · **최초 바인딩 링크 확정** · 판별 표의 어느 행인지 · **다음 수정 방향 결정**(별도 스펙 예고).
- 로컬/GCP 중 실제 사용한 환경과 승격 여부·이유를 명기.

## 검토한 대안

- **바로 수정(워터마크 정산 병렬화) 설계**: 바인딩 링크가 OutboxWriter나 엔진이면 헛수고. 22번이
  "병렬 writer"를 측정으로 기각한 전례. 기각 — 진단 선행.
- **block profile 위해 앱에 SetBlockProfileRate 추가**: "앱 무수정" 경계 위반이고, goroutine
  프로파일로 충분. 기각(원하면 실행 시 판단).

## 범위 밖 / 후속

- **표적 수정 설계·구현**(진단 결과 확정 후 별도 스펙): 정산 병렬화(워터마크) 또는 OutboxWriter
  병렬화 또는 엔진 분할 중 진단이 지목한 것.
- 다중 심볼(단일 심볼 BTC 집중), 히스테리시스, ① 후속(주문당 fan-out 메트릭 등).
