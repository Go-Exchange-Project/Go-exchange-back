# 21. 시장가 완료 복구 dependency guard 완료 (B)

- **완료일**: 2026-07-30
- **성격**: **독립적인 정합성 버그 수정.** 성능 개선의 부속 작업이 아니다.
- **선행**: [B 설계 스펙](../superpowers/specs/2026-07-29-failed-completion-dependency-guard-design.md),
  [28번](../benchmarks/28-2026-07-29-settlement-observability-gcp.md) 후속 설계 중 발견.

## 왜 필요했나

`SettlementRetryWorker.RunOnce()`는 `retryFailedSettlements()` → `retryFailedCompletions()` 순으로
호출한다. 순서 자체는 맞았지만 **`retryFailedCompletions`가 그 주문의 미해결 failed settlement를
전혀 확인하지 않았다.** 게다가 `retryFailedSettlements`는 비-transient 분류와 `RetryCount` 소진을
자동 재시도 대상에서 걸러내는데, 이 두 경우는 `OPEN`으로 **영구히 남는다** — 그 사이에도 completion
재시도는 그대로 실행됐다.

결과: **미정산 trade 위에서 시장가 완료가 실행**돼 잔여 홀드를 잘못 반환할 수 있었다. 홀드가 실제보다
일찍·많이 풀리므로 초과 지출 위험으로 이어진다. 4차 축 1의 A(런타임 fence)를 먼저 넣어도 이 구멍은
닫히지 않는다 — 정상 경로 처리량은 좋아져도 실패 순간의 `trade(X) 미정산 → terminal(X) 복구 선행`
순서는 그대로 성립하기 때문에, B(이 수정)가 A·C의 안전한 전제로 먼저 필요했다.

## 어떻게 했나

### 계약

시장가 terminal 복구는, 그 terminal보다 앞선 trade 중 같은 주문을 maker 또는 taker로 포함하는
`OPEN` failed settlement가 하나라도 있으면 실행하지 않는다.

```sql
SELECT EXISTS (
    SELECT 1 FROM failed_settlements
    WHERE status = 'OPEN' AND (buy_order_id = $1 OR sell_order_id = $1)
)
```

`HasOpenFailureForOrder(orderID uint) (bool, error)`를 **네 계층**(`FailedSettlementRepository` →
`failedSettlementRepository`(서비스 인터페이스) → `FailedSettlementService` →
`retryFailedSettlementStore`(워커 인터페이스))에 전부 통과시켰다.

**`ListOpenFailures(50)` 결과를 메모리에서 검색하지 않는다** — batch limit(50) 밖의 dependency를
놓쳐 fail-open이 되기 때문이다. repository 테스트에 앞쪽 unrelated `OPEN` 60건을 먼저 깔고 그 뒤에
대상을 넣어도 찾아내는 회귀 테스트로 이를 고정했다.

`engine_sequence`는 조건에 넣지 않았다 — `FailedMarketCompletion`에 그 필드가 없고, **동일 주문의
trade는 해당 주문의 terminal보다 항상 먼저 엔진에서 방출된다**는 불변식이 있어 같은 주문을 참조하는
`OPEN` 실패는 전부 terminal의 선행 dependency이기 때문이다. 스키마 확장(`NextRetryAt`·
`EngineSequence` 추가)도 하지 않았다.

### fail-closed는 phase 단위

- dependency store가 `nil`이거나 조회가 오류면, 오류 로그 1회 후 **이번 `RunOnce()`의 completion
  phase 전체를 중단**한다(뒤에 남은 completion도 이번 사이클에는 실행하지 않는다).
- **차단은 오류가 아니다** — 로그를 남기지 않고 `settlement_completion_blocked_total`(Counter)만
  증가시킨다. 이 카운터는 **차단된 polling 횟수**이지 현재 차단된 unique 주문 수가 아니다(gauge는
  추가하지 않았다).
- 비-transient·retry 소진으로 `OPEN`에 영구히 남은 settlement도 **계속 차단**한다. 홀드가 오래
  잠기는 것은 운영 장애지만, 미정산 trade 위에서 잘못 반환하는 것보다 안전하다는 설계 판단이다.
- 재시도 backoff는 넣지 않았다 — 워커 주기(기본 10초) + 배치 상한만으로 비용이 무시할 수준이고,
  backoff는 정산 복구 직후의 홀드 해제를 늦춰 사용자 피해로 이어진다.

### 차단 시 금지되는 것 (전부 테스트로 고정)

- `CompleteMarketOrder` 호출 안 함
- `FailedMarketCompletion` 성공·실패 처리 안 함
- completion의 retry count 소비 안 함
- 주문 상태·홀드 변경 안 함

## 결과

Task 1~3을 TDD로 순차 구현(RED→GREEN 확인 후 커밋):

| Task | 커밋 | 내용 |
|---|---|---|
| 1 | `3ec09cd` | repository `HasOpenFailureForOrder`(EXISTS) — maker·taker 매칭, batch limit 회귀, RESOLVED 무시 3종 |
| 2 | `e913d8d` | service 계층 위임 + `settlement_completion_blocked_total` 카운터 |
| 3 | `945f94e` | 워커 guard 배선 — 차단·정상·조회 오류·store nil 4경계, phase 중단, 해결 후 재개 |

**계획 코드에서 벗어난 지점 하나**: Task 3의 테이블 테스트가 제안한
`assert.False(t, completions.resolved, "차단 시 성공 처리 금지")`를 모든 케이스에 무조건 적용하면
"dependency 없으면 실행" 케이스(정상적으로 완료·resolve돼야 함)와 모순됐다. 실제 정상 동작을
깨뜨리면서까지 테스트를 맞추는 대신, `assert.Equal(t, tc.wantCompleted, completions.resolved)`로
고쳐 케이스별로 옳게 검증했다.

**계획이 새 fake 이름(`fakeFailedSettlementRepo`)을 제안한 지점**은 같은 패키지에 이미 있던
`fakeFailedSettlementRepository`를 확장해 재사용했다 — 구조적으로 동일한 fake가 중복 생기는 것을
피했다.

### 전체 검증

- `go build ./...`, `go vet ./...` 클린.
- `go test ./... -count=1`(통합 DSN `postgres://...localhost:55432/goexchange_test`) — 전 패키지
  그린, **SKIP 0**.
- `go test ./internal/service/... ./internal/repository/... ./cmd/... -race -count=1` — 그린,
  데이터 레이스 0건.
- **기존 워커 테스트 7종 전부 그린** — 단, completion 전용 3종(`TestRetryWorkerRetriesCompletionAndResolves`,
  `TestRetryWorkerRecordsCompletionFailure`, `TestRetryWorkerSkipsExhaustedCompletionRetryCount`)은
  `FailedSettlements` 필드를 아예 설정하지 않고 있었는데, 새 fail-closed 규칙상 이 필드가 비어 있으면
  phase가 즉시 중단돼 completer가 호출되지 않는다 — 각 테스트에
  `FailedSettlements: &fakeFailedSettlementStore{}`(기본값 `HasOpenFailureForOrder`→`false, nil`)
  **한 줄씩만 추가**해 기존 단언(assert)은 그대로 두고 guard 도입 후에도 정상 흐름이 여전히
  통과함을 확인했다 — 동작 축소가 아니라 가드 추가라는 증거다.

## 범위 밖

- **A. per-order runtime fence**(dispatcher의 파티션 전체 배리어를 주문별 dependency fence로 축소)
- **C. cancel terminal의 durable defer 계약**(현재 `processOrderCancellationEvent`는 실패 시 로그만
  남기고 outbox `PENDING` 유지 → 재기동 리플레이로만 복구)

B가 "복구 경로는 순서를 지킨다"는 전제를 확보했으므로, A·C는 이 전제 위에서 하나의 스펙으로 이어서
설계한다.
