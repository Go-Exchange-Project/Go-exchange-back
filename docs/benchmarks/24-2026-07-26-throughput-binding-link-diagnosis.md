# 24번째 측정 (2026-07-26): 처리량 바인딩 링크 진단 (3차 ②-진단)

## 왜 이 측정을 했는지

23번(⑤)이 "DB idle 75~86%인데 hold(5,000)에서 93.1% 셰딩"을 실측해, 병목이
DB에서 엔진/`ExecutionCh` 다운스트림으로 옮겨간 **직렬화**임을 밝혔다. 그러나
체인의 어느 링크가 실제로 묶는지는 pprof·게이지 없이는 미확정이었다. 이번
사이클은 **진단 전용** — 애플리케이션 코드를 한 줄도 고치지 않고, 기존
계측(`/debug/pprof`, Prometheus 게이지)만으로 최초 바인딩 링크를 확정한다.
수정 설계는 범위 밖(별도 스펙 "②-수정"으로 예고).

## 방법

- **재현 목표**: 채널 100% 포화가 아니라, 3차 ①(하류 인지 게이트)이 반영된
  **현재 시스템의 실제 안정 상태** — `ExecutionCh` high-watermark(1024×0.75=768)
  지속 도달 + `orders_admission_rejected_total{stage="engine_gate"}` 지속 증가.
  ① 게이트는 끄지 않았다.
- **드라이버**: 전용 소형 k6 스크립트 `_workspace/loadtest/crossing-flood.js`
  신규 작성(측정 도구, 앱 코드 아님) — 단일 심볼 BTC에 상대편을 항상 넘는
  가격(±50틱)의 지정가 주문만 연속 주입해 체결을 극대화. `constant-vus`
  executor, VU 1000, `SLEEP_MS=0`(무대기).
- **환경**: 로컬(Windows, PowerShell) — 로컬 backend(`go run ./cmd`,
  `GOEXCHANGE_ENABLE_PPROF=true`) + 로컬 Docker Postgres
  (`goexchange-postgres-test`). GCP 미사용(아래 "환경 판정" 참고).
- **신호 출처**: `settlement_worker_queue_length{worker}`(결정 신호),
  `matching_engine_channel_length{channel="execution"}`,
  `orders_admission_rejected_total{stage="engine_gate"}`, 반복
  `/debug/pprof/goroutine?debug=2` 덤프, `/debug/pprof/profile?seconds=30`
  CPU 프로파일, `docker stats`(Postgres CPU).
- **처리율 출처 함정**: 계획서가 지정한 고정 출처
  `order_settlement_duration_seconds_count`는 **실측 중 죽은 메트릭으로
  드러났다** — `.Observe()` 호출부(`cmd/main.go:688`, `processTradeSettlement`
  내부)는 부트스트랩 리플레이 경로에서만 실행되고, 실제 라이브 정산 워커는
  그룹커밋 배치 경로(`collectTradeBatch` → `SettleTradeBatch`,
  `cmd/main.go:192,567`)를 타는데 이 경로는 `order_settlement_duration_seconds`를
  전혀 건드리지 않는다(런 종료까지 `_count=0` 확인). 계획서가 명시적으로
  예비한 대안인 **`trades` 테이블 row 카운트 T0/T1 델타**로 대체했다.

## 재현 결과 — 안정 상태 도달 확인

5회의 독립된 부하 런(최초 90초 런 + CPU 프로파일용 60초 런 + 재현 검증용
3회 추가 런)에서 **동일한 패턴이 예외 없이 반복**됐다:

| 항목 | 관측값 |
|---|---|
| `matching_engine_channel_length{channel="execution"}` | high-watermark(768) 도달 후 지속(최초 런), 재차 확인 |
| `orders_admission_rejected_total{stage="engine_gate"}` | 지속 증가: 16,103 → 68,465 → 100,481(단일 90초 런 내 3개 시점) |
| `settlement_worker_queue_length{worker="8"}` | **256(cap) 고정 포화** — 5회 런 모두 동일 |
| `settlement_worker_queue_length{worker="0..7,9"}` | **항상 0** — 5회 런 모두 동일 |

BTC 심볼 해시가 항상 worker 인덱스 8로 라우팅되고(`cmd/main.go:424-428`
`forwardToSettlementQueue`), 나머지 9개 워커가 완전히 유휴인 채 이 하나만
cap까지 찬다 — 단일 심볼 부하에서 예상된 그림이지만, **셰딩이 발생하는
정확한 순간에 이 워커가 실제로 바인딩 지점인지**는 이 게이지만으로는
"강한 정황"이지 확정 증거가 아니라서, 아래 반복 goroutine 덤프로 교차
확인했다.

## 판별표 대입 — 최초 바인딩 링크 확정

스펙의 3행 판별표:

| 관측 | 판정 |
|---|---|
| 정산 큐 256 지속 포화 + ExecutionCh high-watermark 지속 + 셰딩 | **정산 워커 바인딩** |
| 정산 큐 여유 + ExecutionCh high-watermark 지속 + 셰딩 | OutboxWriter 계층 후보 |
| 두 큐 모두 여유 + 셰딩 없음 | 엔진 매칭 또는 부하 발생기 |

실측은 **1행과 정확히 일치**: `settlement_worker_queue_length{worker="8"}=256`
(cap) 지속 포화 + `execution` 채널 high-watermark 지속 + `engine_gate` 셰딩
지속 증가. → **최초 바인딩 링크 = 정산 워커(심볼당 1개)**.

### 교차 확인 1 — 반복 goroutine 덤프 (지속성 증거)

`_workspace/diag-24/`에 baseline 1 + hold 반복(간격 10~38초) 5회 +
셰딩-직후 1 + recovery 2회, 총 9개 스냅샷을 저장. 서로 다른(독립된) 부하
런에서 캡처한 `hold1`·`hold2`·`hold5-mid` 세 덤프 모두에서 **정확히 1개의
활성 goroutine**이 동일한 스택으로 잡혔다:

```
goroutine 114 [IO wait]:
...
internal/repository.(*OrderRepository).BatchUpdateExecutions
  order_repository.go:177
internal/service.(*SettlementService).SettleTradeBatch.func1
  settlement_batch.go:299
gorm.io/gorm.(*DB).Transaction
internal/service.(*SettlementService).SettleTradeBatch
  settlement_batch.go:48
main.settleTradeBatchWithFallback
  cmd/main.go:567
```

즉 **하나뿐인 활성 정산 워커 goroutine이 DB 왕복(pgx Exec)을 순차적으로
반복하며 `IO wait` 상태로 잡힌다** — 서로 다른 런, 서로 다른 시각에 캡처한
덤프에서 동일 위치가 반복됐다(단일 스택으로 단정하지 않고 지속성으로 판단).
`settlement_batch_size` 히스토그램도 관측 내내 평균 32(=`settlementBatchMaxSize`
상한)에 붙어 있어, 워커가 매 배치 수집 주기를 상한까지 꽉 채우고 있음을
뒷받침한다.

### 교차 확인 2 — 로컬 자원 경쟁 배제 (CPU)

| 대상 | 측정값 | 해석 |
|---|---|---|
| Postgres(Docker) CPU (`docker stats`) | **0.00% ~ 3.66%**(부하 중 3회 샘플) | 사실상 유휴 — DB가 병목이 아님(23번 결론과 일치) |
| Backend 프로세스 CPU (`process_cpu_seconds_total` 델타, 10초 창) | **196.4%**(GOMAXPROCS=16 중 약 2코어) | CPU 포화 아님 — 연산량이 아니라 직렬 대기가 병목 |
| `/debug/pprof/profile?seconds=30` 상위 함수 | `runtime.cgocall` 46.6%(flat) | Windows 로컬 환경 고유의 syscall 경유 경로로 추정(리눅스 배포 환경과 다를 수 있음) — 참고 정보로만 기록, 결정 신호로 쓰지 않음 |

DB도 backend도 CPU로 막힌 게 아니다 — **로컬 자원 경쟁으로 신호가 오염됐을
가능성은 배제**된다(계획서가 명시한 필수 배제 조건 충족).

### 처리율 (참고 수치, trades 테이블 델타 대체 출처)

`SELECT count(*) FROM trades` T0=16,267 → T1=29,579 (Δ≈38초) ≈ **약 350
trades/s** (사용된 심볼당 단일 정산 워커의 실측 처리 한계 근사치. 반올림 없이
원본 그대로 기록 — 정밀한 시간 간격 로그는 남기지 않아 ±소수 초 오차 있음을
명시).

### OutboxWriter 배제 근거

`trade_outbox_flush_seconds`(outbox 배치 INSERT 커밋 시간) 히스토그램은
전체 관측 구간에서 대다수(약 95~98%)가 25ms 미만이었고, `trade_outbox_write_errors_total=0`.
즉 OutboxWriter 계층 자체는 정산 큐로 넘기기 전 단계에서 빠르게 처리되고
있어 2행(OutboxWriter 계층 후보)에 해당하지 않는다.

## 환경 판정 — 로컬 유지, GCP 미승격

계획서의 GCP 즉시 승격 조건(① 신호가 흐림, ② 로컬 자원 경쟁으로 신호 오염)
중 어느 것도 해당하지 않았다:

- 신호는 5회 독립 런 전부에서 동일하게 재현됐다(worker 8=256 고정, 나머지=0) —
  흐리지 않다.
- Postgres CPU 0~3.66%, backend CPU 약 2/16코어 — 로컬 자원 경쟁 없음(위
  교차 확인 2).

**따라서 이번 진단은 로컬 측정으로 충분하다고 판단해 GCP로 승격하지
않았다.** DEV_TOOLS_TOKEN·외부 IP는 애초에 로컬 전용이라 노출 이슈 없음.

## 다음 수정 방향 (별도 스펙 예고 — 이번 사이클 범위 밖)

확정된 바인딩 링크(정산 워커, 심볼당 1개)에 따라 스펙이 예고한 후속은
**정산 병렬화**: 심볼당 체결을 병렬 풀로 처리하되, 종결 이벤트(트레이드
확정) 순서 보존을 워터마크 방식으로 유지하는 접근. 이번 진단에서는 특정
수정 설계를 확정하지 않는다(스펙의 명시적 제약) — 다음 사이클 "②-수정"에서
별도 스펙으로 다룬다.

## 진단 도구

- k6 드라이버(신규, 측정 전용): [`_workspace/loadtest/crossing-flood.js`](../../_workspace/loadtest/crossing-flood.js)
- goroutine 덤프·게이지 스냅샷: `_workspace/diag-24/`(용량 문제로 리포 미커밋,
  로컬 보관 — 위 표·발췌가 원본 수치 그대로 인용)
