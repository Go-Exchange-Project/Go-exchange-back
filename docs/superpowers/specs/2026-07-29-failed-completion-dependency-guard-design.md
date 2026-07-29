# B. 시장가 완료 복구의 dependency guard 설계 (정합성 수정)

- **날짜**: 2026-07-29
- **성격**: **독립적인 정합성 버그 수정.** 성능 개선의 부속 작업이 아니다.
- **선행 조사**: [28번](../../benchmarks/28-2026-07-29-settlement-observability-gcp.md) 후속 설계 중
  발견. 4차 축 1의 per-order fence(A)와 cancel durable defer(C)의 **안전한 전제**이기도 하다.

## 왜 필요한가 — 지금 뚫려 있는 구멍

`SettlementRetryWorker.RunOnce()`는 `retryFailedSettlements()` → `retryFailedCompletions()` 순으로
호출한다(settlement_retry_worker.go:69-72). 순서 자체는 맞지만 **`retryFailedCompletions`가 그
주문의 미해결 failed settlement를 확인하지 않는다**. 게다가 `retryFailedSettlements`는

- **비-transient 분류**를 건너뛰고(:87-89),
- **RetryCount 소진**을 건너뛴다(:90-92).

이 둘은 `OPEN`으로 **영구히 남는데**, 그 사이 completion 재시도는 **그대로 실행된다.**

> 결과: **미정산 trade 위에서 시장가 완료가 실행**돼 **잔여 홀드를 잘못 반환**할 수 있다.
> 홀드가 실제보다 일찍·많이 풀리므로 **초과 지출 위험**으로 이어진다. 성능과 무관한 정합성 결함이다.

**A(런타임 fence)를 먼저 넣으면 안 되는 이유**: 정상 경로 처리량은 좋아져도 실패 순간에
`trade(X) 미정산 → terminal(X) 복구 선행 → 잘못된 잔여 홀드 반환`이 그대로 성립한다.
"보통은 빠르지만 실패하면 금융 정합성을 깬다"는 구조는 허용할 수 없다.

## 계약

**시장가 terminal 복구는, 그 terminal보다 앞선 trade 중 같은 주문을 maker 또는 taker로 포함하는
`OPEN` failed settlement가 하나라도 있으면 실행하지 않는다.**

```
시장가 terminal 복구 전에:

EXISTS failed_settlement
WHERE status = 'OPEN'
  AND (buy_order_id = X OR sell_order_id = X)

true:
  terminal durable defer
  retry count 소비 없음
  다음 RunOnce에서 재확인
  blocked counter 증가

false:
  기존 completion 복구 실행

query error:
  fail-closed
  terminal 실행 금지
  상태 변경 없음
  오류 로그
  다음 RunOnce에서 재확인
```

### 인터페이스 계약 — `HasOpenFailureForOrder`

```go
HasOpenFailureForOrder(orderID uint) (bool, error)
```

**다음 네 계층을 모두 통과해야 한다**(이름은 현재 코드와 일치):

- `FailedSettlementRepository`(repository)
- `failedSettlementRepository`(service 인터페이스, failed_settlement_service.go:50)
- `FailedSettlementService`
- `retryFailedSettlementStore`(워커가 보는 인터페이스, settlement_retry_worker.go:26)

SQL 의미는 **DB에서 EXISTS로 판정**한다:

```sql
SELECT EXISTS (
    SELECT 1
    FROM failed_settlements
    WHERE status = 'OPEN'
      AND (buy_order_id = $1 OR sell_order_id = $1)
)
```

**`ListOpenFailures(50)` 결과를 메모리에서 검색하는 방식은 금지한다.** batch limit(50) 밖에 있는
dependency를 놓쳐 **fail-open**이 되기 때문이다. repository 테스트에는 **앞쪽에 unrelated `OPEN`
실패를 50건 이상 넣고 그 뒤에 같은 주문의 실패를 넣어도 `true`**가 나오는 케이스를 포함해 이
회귀를 고정한다.

### fail-closed 흐름 (phase 단위)

```
dependency store == nil
  → completion phase 전체 중단
  → terminal 실행 금지

dependency query error
  → 오류 로그 1회
  → completion phase 전체 중단
  → 이후 completion도 이번 RunOnce에서는 실행하지 않음

dependency exists
  → blocked counter +1
  → 해당 completion만 건너뛰고 다음 completion 검사

dependency absent
  → 기존 completion 복구 실행
```

조회 오류에서 **phase 전체를 중단**하면 정상 completion도 최대 10초(다음 `RunOnce`) 늦어지지만,
**DB 상태를 신뢰할 수 없는 상황에서 일부만 실행하는 것보다 안전**하고 **최대 50회의 동일 오류
로그**도 막는다.

### 차단 상태에서 금지되는 것

- `CompleteMarketOrder`를 **호출하지 않는다**
- `FailedMarketCompletion`을 **성공·실패 처리하지 않는다**
- completion의 **retry count를 소비하지 않는다**
- **주문 상태와 홀드를 변경하지 않는다**
- 다음 polling에서 **다시 dependency를 확인**한다

**비-transient 또는 retry 소진으로 `OPEN`에 남은 settlement도 계속 차단한다.** 홀드가 오래 잠기는
것은 운영 장애지만, **미정산 trade 위에서 잘못 반환하는 것보다 안전하다.** 이 경우는 별도
alert·수동 복구 대상이다.

### 조회 오류는 fail-closed

**dependency 존재 여부를 확인하지 못하면 terminal을 실행하지 않는다.** 조회가 DB 오류로 실패했을 때
"OPEN이 없음"으로 해석하면 **가장 위험한 fail-open 회귀**가 된다. 정상적인 dependency 차단과 달리
이는 **실제 인프라 오류이므로 오류 로그를 남긴다**(차단 자체는 정상 동작이라 로그를 남기지 않는다).

### `engine_sequence`를 조건에 넣지 않는 근거

`FailedMarketCompletion`에는 `EngineSequence` 필드가 **없다**(`OrderID`·수량·상태·retry만). 이번
수정에 스키마 확장을 끌어들이지 않는다. 대신 다음 불변식이 sequence 조건을 불필요하게 만든다:

> **동일 주문의 trade는 해당 주문의 terminal보다 항상 먼저 엔진에서 방출되므로, 같은 주문을
> 참조하는 `OPEN` failed settlement는 모두 terminal의 선행 dependency다.**

### 재시도 스케줄

`NextRetryAt` 같은 컬럼은 **만들지 않는다**(현재 어느 모델에도 없다). 차단 시 **다음 `RunOnce()`
에서 즉시 재확인**한다. 근거: 워커 주기 10초 + `settlementRetryBatchLimit` 상한이라 비용이 무시할
수준이고, **backoff는 settlement 복구 직후의 홀드 해제를 늦춘다**(잠긴 홀드 = 사용자 피해).

## 관측

**`settlement_completion_blocked_total`** (Counter). **차단된 polling 횟수**이지 현재 차단된 주문
수가 아니다.

```
RunOnce에서 OPEN dependency 때문에 completion 실행을 건너뛸 때마다 +1
```

- `rate()`/`increase()`로 **지속 차단을 탐지**한다.
- **unique blocked order 수로 해석하지 않는다.** 현재 OPEN 주문 수의 원본은 DB다.
- 라벨은 필요 없으면 넣지 않는다. 넣더라도 `reason="open_failed_settlement"` 같은 **고정값만**.
- **현재 차단 수 gauge는 넣지 않는다** — worker batch limit 때문에 전체 DB 상태를 정확히 반영하기
  어렵고, 정확한 값에는 별도 count 쿼리가 필요하다.

## 재현 테스트 (9종)

1. **`BuyOrderID`가 같은** `OPEN` settlement가 있으면 completion 미실행
2. **`SellOrderID`가 같은** `OPEN` settlement가 있어도 미실행
3. **unrelated 주문**의 `OPEN` settlement는 completion을 막지 않음
4. **해결된(RESOLVED)** settlement는 completion을 막지 않음
5. **비-transient·retry 소진**으로 `OPEN`에 남은 settlement도 **계속 차단**
6. dependency 차단 중 **completion retry count가 증가하지 않음**
7. failed settlement 해결 후 **다음 `RunOnce()`에서 completion 실행**
8. 여러 dependency 중 **하나라도 `OPEN`이면 계속 차단**
9. **dependency 조회가 오류를 반환하면** terminal을 실행하지 않고 **completion 상태와 retry count를
   그대로 유지**(fail-closed 회귀 방지 — 이 테스트가 없으면 가장 위험한 fail-open이 남는다)

**fail-closed 경계 2종 추가:**

10. **dependency store가 nil이면** completer **미호출**(phase 전체 중단)
11. **첫 dependency 조회가 실패하면 뒤에 있는 completion도 이번 사이클에는 미실행**(phase 중단)

**batch limit 회귀 고정(repository 단위):**

12. 앞쪽에 **unrelated `OPEN` 실패 50건 이상**을 넣고 그 뒤에 같은 주문의 실패를 넣어도
    `HasOpenFailureForOrder` = `true`

**테스트 개수를 고정할 필요는 없다** — 위 항목들을 하나의 테이블 테스트로 묶어도 된다. 핵심은
**각 경계가 실제로 고정되는 것**이다. 다만 repository 단위 테스트만이 아니라 **`RunOnce()` 통합
흐름**으로 **"trade 복구 전 terminal 미실행 → trade 복구 후 terminal 실행"**을 고정한다(7번).

## 범위 밖

- **A. per-order runtime fence**(dispatcher의 파티션 전체 배리어 축소)
- **C. cancel terminal의 durable defer 계약**(현재 `processOrderCancellationEvent`는 실패 시 로그만
  남기고 outbox `PENDING` 유지 → 재기동 리플레이로만 복구, 관측 가능한 OPEN 레코드 없음)
- 스키마 확장(`NextRetryAt`·`EngineSequence` 추가), 현재 차단 수 gauge, 정산 동작·순서 변경
