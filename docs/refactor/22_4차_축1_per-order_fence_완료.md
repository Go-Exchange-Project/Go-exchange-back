# 22. 4차 축 1 — per-order 런타임 fence + terminal durable defer 완료 (A+C)

- **완료일**: 2026-07-30
- **성격**: 정산 dispatcher의 **파티션 전체 배리어를 주문 단위 fence로 축소**(A) +
  취소 terminal에도 시장가 완료와 동형의 **내구 defer 계약**을 갖춘다(C).
- **선행**: [28번 GCP 관측성 판정](../benchmarks/28-2026-07-29-settlement-observability-gcp.md)
  (파티션 전체 fence가 지배 원인), [B(시장가 완료 dependency guard) 완료](21_시장가완료_dependency_guard_완료.md),
  [A+C 설계 스펙](../superpowers/specs/2026-07-30-per-order-fence-and-terminal-durable-defer-design.md),
  [구현 계획](../superpowers/plans/2026-07-30-per-order-fence-and-terminal-durable-defer.md).

## 왜 필요했나

28번이 GCP 스케일에서 27번의 정산 큐 포화(`worker="8"`=256 cap)를 재현하고 지배 원인을
확정했다: hold 구간 `market_done` 배리어 wait duty **52.3%**, 배리어 진입 시 in-flight는
**항상 1**. 종결 이벤트(취소·시장가완료)가 도착하면 **그 이벤트와 무관한 주문의 배치까지**
파티션 전체가 멈춰 새 배치를 만들지도 디스패치하지도 못했다.

B가 "복구 경로는 순서를 지킨다"는 정합성 전제(미정산 trade 위에서 terminal이 실행되면 안
된다)를 먼저 닫았으므로, 이제 그 전제를 지키면서 배리어를 좁힐 차례였다. 그런데 배리어를
주문 단위로 좁히려면 **취소 terminal도 시장가 완료와 같은 급의 내구 defer 계약**을 갖춰야
한다 — 아니면 A만 넣었을 때 취소가 실행 실패 시 로그만 남기고 outbox `PENDING`으로만 남는
비대칭이 생겨, 온라인 복구 없이 재부팅 리플레이에만 의존하게 된다. 그래서 A(fence)와
C(취소 durable defer)를 하나의 스펙으로 묶었다.

## 어떻게 했나

### A. per-order 런타임 fence

- `dependencyTracker`(Task 6, `cmd/settlement_dependency.go`)가 **주문ID → 아직 retire되지
  않은 배치 수**(`inFlight`)를 추적하는 순수 상태 기계다. dispatcher가 단독으로 소유하고
  worker는 이 상태를 읽지도 쓰지도 않는다.
- dispatcher(Task 7)는 job에 `kind`(trade/terminal)와 dispatcher 고유 `id`를 부여한다.
  terminal도 더 이상 dispatcher가 인라인으로 실행하지 않고 worker pool의 job으로 편입된다.
- terminal은 **자기 주문을 건드린 배치가 모두 retire될 때까지만** `waiting`에서 대기한다.
  무관한 주문의 배치는 그동안 계속 dispatch된다(28번이 실측한 문제의 직접 해소).
- **내구 기록 자체가 실패한 주문**(`undurableOrderIDs`)은 `unsafeOrders`에 quarantine돼,
  그 주문의 terminal이 ready가 돼도 dispatch하지 않고 조용히 버린다 — outbox는 `PENDING`으로
  남아 다음 부팅 리플레이가 처리한다. quarantine 표시는 retire 안에서 count 감소보다
  **먼저** 일어난다(순서가 바뀌면 terminal이 quarantine을 못 보고 ready로 오판할 수 있다).
- `maxOutstanding = 2 * concurrency`이고 `completions` 채널 용량도 같은 값이다 — 두 값이
  갈라지면 worker의 완료 송신이 영구 블로킹할 수 있다.
- terminal은 브로드캐스트 시퀀스(`seq`)를 소비하지 않는다 — WS 메시지를 만들지 않으므로
  reorder coordinator와 무관하다.
- 주문당 terminal 1개는 엔진 불변식이다. 중복 도착은 로그만 남기고 조용히 무시한다(첫
  번째를 덮어쓰지 않는다) — 저장소 수준 탐지는 범위 밖으로 명시적으로 남겼다(아래 참고).

### C. 취소 terminal의 durable defer

- `FailedOrderCancellation`(Task 4)이 `FailedMarketCompletion`과 동형으로 신설됐다:
  `RecordFailure`(실행이 실제로 실패, `ON CONFLICT DO UPDATE`+`retry_count+1`)와
  `EnsureDeferred`(dependency 차단으로 실행을 시도조차 안 함, `ON CONFLICT DO NOTHING`,
  `retry_count`는 0에서 시작)를 명확히 구분한다. `EnsureDeferred`는 기존 행(특히
  `RESOLVED`)을 절대 되살리지 않는다.
- `FailedMarketCompletion`도 같은 구분을 갖도록 확장했다(Task 3): `RetryCount`의 GORM
  default·CHECK 제약을 `1`/`>0`에서 `0`/`>=0`으로 낮추고 `migrations/005_terminal_durable_defer.sql`로
  실제 DB에 반영했다. **차단은 retry budget을 소비하지 않는다**는 계약이 이제 저장소
  계층에서도 지켜진다(아래 "대화 중 정정된 사항" 참고).
- `SettlementRetryWorker`에 3번째 phase(`retryFailedCancellations`)가 추가됐다 —
  `retryFailedCompletions`와 동일한 구조(같은 fail-closed dependency guard: `FailedSettlements`가
  `nil`이면 phase 자체를 건너뜀)로 온라인 복구를 이어받는다.
- `processMarketOrderDone`/`processOrderCancellationEvent`(Task 5)가 **같은 순서**(guard →
  실행 → 실패 시 `RecordFailure`, 차단 시 `EnsureDeferred`)로 통일됐다 — live 경로와 replay
  경로가 같은 guard·같은 defer 경로를 탄다.
- 기록 자체가 실패하면(`RecordFailure`/`EnsureDeferred` 호출도 실패) `handled=false`를
  반환해 outbox를 `PENDING`으로 남긴다 — 조용히 유실하지 않는다.

### OutboxReplayer fail-closed 전환 (Task 2)

부팅 리플레이가 `Process()==false`(비내구적 처리)나 payload corruption을 만나면, 예전엔
조용히 계속 진행(또는 마킹 후 진행)했지만 이제는 **즉시 중단**한다. `transactionalOutboxID`
(정산 트랜잭션 안에서 마킹할 ID, live=행ID/replay=0)와 `sourceOutboxID`(실패 기록의
provenance, 항상 실제 행 ID)를 분리해 두 의미가 섞이지 않게 했다. 부팅 실패 시 사람이
개입해야 하는 절차는 [corrupted-outbox-recovery runbook](../runbooks/corrupted-outbox-recovery.md)에
정리했다.

## 대화 중 정정된 사항

설계 단계에서 초기 가정이 틀렸음을 발견하고 스펙에 반영한 것들:

- **happens-before는 조건부로만 성립한다.** "실패한 배치는 자기 job 안에서
  `failed_settlements` 기록을 커밋한 뒤 completion을 보내고, dispatcher는 그 completion을
  받아야 retire·dispatch한다"는 순서는 **기록 자체가 실패하지 않는 한**에만 성립한다.
  기록마저 실패하는 유일한 구멍이 `undurableOrderIDs`/quarantine 메커니즘이다.
- **`OrderID` unique 제약은 중복 terminal을 탐지하지 않는다.** repository가
  `ON CONFLICT DO UPDATE`를 쓰므로, 서로 다른 terminal 이벤트가 같은 주문에 대해 도착해도
  저장소 계층에서 "이건 두 번째 도착"이라고 구분하지 못한다(그냥 같은 행을 업데이트할 뿐).
  주문당 terminal 1개는 **엔진 불변식과 기존 테스트**에 의존하며, 저장소 수준 탐지는
  범위 밖으로 명시적으로 남겼다 — dispatcher의 `waiting` 중복 검사(메모리, 프로세스
  생존 중에만 유효)가 유일한 방어선이다.
- **defer record가 `retry_count=1`에서 시작하면 실제 시도 기회가 줄어든다.** 예를 들어
  `MaxRetryCount=5`일 때, 차단으로 생성된 기록이 1에서 시작하면 실제 시도는 4회로
  줄어든다 — "dependency 차단은 retry budget을 소비하지 않는다"는 계약이 저장소 계층에서
  깨진다. 문서에 한계로 적는 것으로는 계약을 지킬 수 없어, Task 3/4에서 `retry_count=0`
  시작을 실제로 구현했다.

## 결과

Task 1~7을 TDD로 순차 구현(RED→GREEN 확인 후 커밋):

| Task | 커밋 | 내용 |
|---|---|---|
| 1 | `2183db4` | undurable outcome 전파(`settlementResult.undurableOrderIDs`) — 스펙·계획 문서와 같은 커밋에 섞인 점은 알려진 편차(아래 참고) |
| 2 | `828ab5e` | `OutboxReplayer` fail-closed 전환 + `transactionalOutboxID`/`sourceOutboxID` 의미 분리 |
| 3 | `6ef35d6` | `FailedMarketCompletion.EnsureDeferred` + `retry_count=0` 시작(migration 005) |
| 4 | `63a1c0c` | `FailedOrderCancellation` 신설 + 취소 durable retry subsystem(워커 3번째 phase) |
| 5 | `48eddce` | live 경로에 dependency guard + terminal durable defer 배선(공통 정책) |
| 6 | `4710c12` | `dependencyTracker` 순수 상태 기계(select 루프와 분리, 단위 테스트로 전이 고정) |
| 7 | `2cd2ba5` | dispatcher를 파티션 배리어에서 주문 단위 fence로 전환(핵심), 배리어 지표 제거 + 4종 신규 지표 |

### 계획에서 벗어난 지점 (전부 보고, 이유 포함)

- **Task 1 커밋 경계**: 스테이징 순서 실수로 Task 1의 코드가 스펙·계획 문서 커밋(`2183db4`)에
  섞였다. 이미 푸시 전이었지만 "항상 새 커밋" 원칙에 따라 rebase로 바로잡지 않고 그대로
  두고 여기 기록한다.
- **테스트 헬퍼 이름 충돌 3회**(Task 1, 6, 7): 계획 코드가 `tradeOutboxEvent(outboxID,
  buyOrderID, sellOrderID)` 3-인자 형태를 새로 쓰려 했지만, 같은 패키지에 이미
  `tradeOutboxEvent(outboxID, engineSequence)`(2-인자, 주문ID 미설정)가 있어 충돌했다.
  Task 1에서 만든 `tradeOutboxEventForOrders`를 Task 6·7에서도 재사용해 중복 헬퍼를
  피했다.
- **사전 존재 테스트 위생 버그 수정**(Task 2): `order_cancellation_integration_test.go`의
  한 테스트가 실제 `OutboxWriter`로 커밋한 `ORDER_CANCELLED` outbox 행을 PROCESSED로
  마킹하지 않고 끝나, 공유 테스트 DB에 PENDING 행이 영구히 남아 있었다. 옛 관대한
  리플레이어는 이를 조용히 허용했지만 새 fail-closed 계약 하에서는 이후 통합 테스트를
  실패시켜, `t.Cleanup`으로 직접 정리하도록 고쳤다.
- **`cmd/main.go` 배선 추가**(Task 4, 계획 파일 목록에 없었음): `SettlementRetryWorker`에
  `CancelProcessor`/`FailedCancellations` 필드를 추가했는데, `main()`에서 실제 구현체를
  채워 넣지 않으면 새 취소 재시도 phase가 프로덕션에서 항상 조용히 no-op이 되므로 함께
  배선했다.
- **호출 체인 시그니처 확장**(Task 5, 계획엔 최상위 함수만 명시): `processExecutionEvent`
  에 `guard`·`cancelDeferStore`를 추가하려면 그 앞단의 `processSingleOutboxEvent`·
  `settleTradeBatchWithFallback`도 같은 파라미터를 릴레이해야 컴파일이 성립해 기계적으로
  확장했다. `processMarketOrderDone`은 사양대로 `sourceOutboxID`를 받지 않는다 —
  `marketCompletionFailureRecorder`의 시그니처가 애초에 그 값을 받지 않고(Task 3에서
  확정), 설계 문서도 "저장하지 않고 provenance로만 쓴다"고 명시했다.
- **낡은 테스트 2개 삭제**(Task 7): `TestPartitionDispatcherProcessesTerminalEventAfterPrecedingBatches`
  와 `TestDispatcherRecordsBarrierMetricsPerTerminalType`은 새 계약 하에서 사실이 아닌
  것(배리어가 파티션 전체를 막는다는 전제)을 단언하므로 삭제했다. 전자는 새
  `TestDispatcherHoldsTerminalUntilSameOrderBatchesRetire`(같은 주문만 막는다는 수정된
  계약으로)가, 후자는 배리어 지표 폐기와 함께 대체 지표 테스트로 흡수했다.

### 전체 검증

- `go build ./...`, `go vet ./...` 클린.
- `go test ./... -count=1 -race` — 전 패키지 그린.
- `go test ./internal/repository/... ./internal/service/...`(통합 DSN, `docker-compose.test.yml`
  로 완전히 초기화한 DB) — **연속 2회 실행**해 PENDING 잔재 없이 그린임을 확인.
- 마이그레이션(`005_terminal_durable_defer.sql`)을 실제 로컬 postgres에 적용해
  `\d failed_order_cancellations`로 CHECK 제약·default 값을 직접 확인.
- `git grep -n "SettlementBarrier" -- '*.go'` → 결과 없음(전부 제거 확인).
- `git grep -n "OrderSettlementDuration"` → `cmd/main.go`의 기존 호출 1곳(주석 "기존 유지")과
  `internal/metrics/metrics.go`의 정의만 — **의미·라벨 불변** 확인.

## 범위 밖

- **durable dependency index 신설**(주문↔선행 outbox 관계를 DB에 저장) — 스키마·replayer·claim
  의미까지 확대. 실측 없이 과하다.
- **`unsafeOrders` eviction 정책** — gauge(`settlement_quarantined_orders`)로 먼저 관측.
- **저장소 수준의 중복 terminal 탐지** — 엔진 불변식·기존 테스트에 의존.
- **`settlement_job_execution_seconds`의 `fallback`/`failed` 라벨 분리**.
- **`failed_settlements`의 `retry_count` 제약 변경**(이번 축은 market_completion/
  order_cancellation 두 테이블만 다룬다).
- **29번 GCP 재측정 실행** — [runbook](../superpowers/plans/2026-07-30-per-order-fence-gcp-remeasurement.md)
  만 작성했고, 실행은 별도 측정 세션에서 수행한다.
