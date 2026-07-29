# A+C. per-order 런타임 fence와 terminal durable defer 설계

- **날짜**: 2026-07-30
- **성격**: 4차 리팩토링 **축 1(안전한 지속 처리율 증대)**의 본 수정. 축 2(매칭 quantum)와 **독립 판정**.
- **선행**: [28번 GCP 판정](../../benchmarks/28-2026-07-29-settlement-observability-gcp.md) →
  **partition-wide fence가 지배적 병목**. 정합성 전제인 [B(dependency guard)](2026-07-29-failed-completion-dependency-guard-design.md)는 완료.
- **두 축은 한 사이클**: A는 terminal을 "기다리지 않게" 만들고, C는 "기다릴 수 없을 때 안전하게 미루는" 수단이다.
  C 없이 A만 넣으면 실패 순간에 terminal이 갈 곳이 없다.

---

## 왜 필요한가 — 28번이 지목한 것

현재 dispatcher는 종결 이벤트(`MarketOrderDone`·`OrderCancelled`)를 만나면
**파티션 전체를 정지**시키고 in-flight를 0까지 드레인한 뒤 단건 실행한다
([settlement_pipeline.go:102-114](../../../cmd/settlement_pipeline.go)).

28번 실측:

| 지표 | 값 | 의미 |
|---|---|---|
| `market_done` 배리어 진입 시 in-flight | **항상 정확히 1.000** | 배치가 커지기 전에 매번 끊긴다 |
| 배리어 대기 duty | **52.3%** | dispatcher 시간의 절반이 대기 |
| 배리어 대기 ≈ attempt ≈ job 실행 | 13.48 / 13.33 / 13.45 ms | 대기 = 앞 배치 1건의 실행시간 그대로 |
| worker busy | **13.1%** (N=4 → **≈0.52 worker-equivalent**) | 병렬화가 사실상 작동하지 않음 |
| dispatch 대기 | 9 µs | 채널·worker 공급은 병목이 아니다 |

[27번 재분석](../../benchmarks/27-2026-07-28-settlement-binding-reanalysis.md)에서 terminal 이벤트가
**전체 outbox 이벤트의 31.9%**(91,097건 중 29,041건)임이 확인됐다. ②-수정 당시의
"terminal은 드물다"는 전제가 실측으로 반증됐고, 그 결과 배치가 32 대신 **평균 2.82**로 파편화된다.

> **배리어의 범위가 과하다.** 정합성이 요구하는 것은
> "**같은 주문**의 선행 trade가 끝난 뒤 그 주문의 terminal이 실행될 것"이지,
> "**파티션의 모든** trade가 끝날 것"이 아니다.

---

## 계약 요약

```
A. terminal은 자기 주문의 outstanding trade batch가 모두 retire될 때까지만 기다린다.
   무관한 주문의 배치는 계속 dispatch되고 계속 실행된다.

C. 기다린 끝에도 실행할 수 없으면(dependency가 실패로 종결됐거나 실행이 실패하면)
   terminal을 내구적으로 defer하고 온라인 retry worker에 인계한다.
   재시작은 복구 수단이 아니다.
```

**보장하는 것**

- 같은 주문에 대해 **trade 정산 → terminal 실행** 순서
- terminal은 **실행되거나, 내구 기록으로 인계되거나, outbox PENDING으로 남거나** 셋 중 하나 —
  **조용히 사라지지 않는다**
- 무관한 주문의 이벤트는 terminal 대기 중에도 **계속 진행**
- 홀드는 **늦게 풀릴 수는 있어도 일찍 풀리지 않는다**

**보장하지 않는 것**

- 파티션 전체의 엔진 시퀀스 순서 (이미 보장 대상이 아니다 — REST는 엔진 시퀀스 prefix를 보장하지 않는다)
- terminal의 **지연 상한** (DB per-call timeout 계약이 없다 — 백로그)
- outbox corruption이 발생한 뒤의 **자동** 복구 (fail-fast + 수동 복구)

---

# A. per-order 런타임 fence

## A-1. 토폴로지

**terminal도 기존 공용 worker pool에서 job으로 실행한다.** dispatcher는 순서·dependency·dispatch만 소유한다.

- terminal job은 **별도 job kind/ID**를 쓰며 trade의 `batchSeq`·reorder pending map을
  **소비하지 않는다**.
- 근거: terminal은 **WS 메시지를 만들지 않는다** —
  `processMarketOrderDone`([main.go:487](../../../cmd/main.go))과
  `processOrderCancellationEvent`([main.go:490](../../../cmd/main.go))는 `broadcast`를 **받지 않는다**.
  따라서 지연된 terminal이 reorder coordinator를 막을 수 없다.
- `runTerminal`의 dispatcher 인라인 실행([settlement_pipeline.go:94-99](../../../cmd/settlement_pipeline.go))은 제거한다.

## A-2. outstanding job — 용어와 불변식

> **outstanding job** = worker에게 **송신됐지만** dispatcher가 completion을 받아 **retire하지 않은** job.
> 채널 대기·실행 중·completion 대기를 **모두 포함**한다.

```
maxOutstanding      = 2 * concurrency
cap(completions)    = maxOutstanding
각 job은 completion을 정확히 1회 송신
outstanding < maxOutstanding 일 때만 새 job dispatch
```

**네 조건이 함께 있어야** "worker의 completion 송신은 영구 블로킹하지 않는다"가 성립한다.
현재 `completions`는 `cap = concurrency`이므로([settlement_pipeline.go:66](../../../cmd/settlement_pipeline.go))
상한을 `2N`으로 올리면서 **반드시 같이 올린다**. 두 값은 **같은 상수**로 묶어 드리프트를 막는다.

**`2N`을 고른 이유**: 기존 채널 구조(`jobs` cap = `concurrency`, worker N)와 정확히 대응한다 —
실행 중 최대 N + 채널 대기 최대 N. `N`으로 두면 모든 job이 실행 중일 때 채널이 비어,
하나가 끝날 때마다 worker가 dispatcher의 다음 송신을 기다린다.

**`2N`의 처리량 이득은 미측정이다.** 28번에서 in-flight는 1을 넘지 않았다 — 병목은 상한이 아니라
fence였다. fence 제거 후 재측정에서 worker starvation과 dispatcher 부하를 함께 확인하며,
**"CPU 여유가 영구히 충분하다"고 주장하지 않는다.**

## A-3. 등록 시점 — 송신 성공 시점

```go
case jobs <- readyJob:
    dispatched[jobID] = touchedOrderIDs
    outstanding++
    // touched order별 inFlight count 증가
    readyJob = nil
```

Go `select`의 send case가 **선택된 뒤 case body에서** 등록한다. worker가 아무리 빨리 끝내도
completion은 채널에 들어갈 뿐이고, dispatcher는 case body를 끝내기 전에 completion을 처리할 수 없다 —
**등록보다 completion 처리가 앞서는 경쟁은 없다.**

명시적으로 고정할 것:

- send case가 **선택되지 않으면 등록하지 않는다**
- stop/context case가 선택되면 `readyJob`의 dependency 상태가 **남지 않는다**
- worker는 dependency map을 **읽지도 변경하지도 않는다**
- completion에 해당하는 `dispatched[jobID]`가 없으면 **내부 불변식 위반**
- completion 처리 후 `dispatched[jobID]` 삭제 및 `outstanding--`
- touched order count가 0이면 `inFlight` entry **삭제**

## A-4. dependency 집합은 dispatcher가 계산한다

**성능 선택이 아니라 정합성 요구다** — terminal이 도착한 시점에 그 주문의 집합이 이미 알려져 있어야 한다.

- `collectTradeBatch`([main.go:528](../../../cmd/main.go)) **직후** dispatcher가 `touchedOrderIDs`를 계산
- maker·taker ID를 **중복 제거**하고 **배치당 1회**만 카운트(`inFlight[X] += 1`, 완료 시 `-= 1`)
- dispatcher가 `orderID → outstanding batch count`의 **단독 소유자**
- completion은 **batch ID와 결과만** 돌려준다
- **dispatch되지 않은 job은 카운트하지 않는다**

## A-5. retire와 dependency 충족은 다른 일이다

| | 성공 completion | 실패 completion |
|---|---|---|
| **retire** (`outstanding--`, count 감소, 0이면 삭제) | 수행 | **똑같이 수행** |
| **dependency 충족** (terminal 실행 가능) | ⭕ | **❌ 아니다** |

실패에도 retire하지 않으면 슬롯이 새고 `inFlight[X]`가 0에 도달하지 못해
**waiting terminal이 영구 잔류**한다 — 실패 하나로 상한이 영구 포화된다.
반대로 retire를 "dependency 완료"로 읽으면 **`dependency failed ≠ completed`** 계약이 깨진다.

> **count 0은 "terminal 실행 가능"이 아니라 "terminal dispatch 가능"이다.**

**dependency 충족의 권위는 DB다.** terminal job이 **실행 시점에** B의 guard를 호출한다:

```
inFlight[X] == 0 → terminal job dispatch (waiting 상태에서 제거)
  [worker] HasOpenFailureForOrder(X)
     없음      → terminal 실행
     있음      → C의 durable defer (성공·완료로 간주하지 않는다)
     조회 오류 → fail-closed: 실행 금지, C의 defer 시도
```

**happens-before**(조건부): 실패한 배치는 **자기 job 안에서** `failed_settlements` 기록을 커밋한 뒤
completion을 보내고, dispatcher는 그 completion을 받아야 retire·dispatch한다.
**단, 기록 자체가 실패하면 성립하지 않는다** → A-6.

**메모리 실패 플래그를 dependency 판정에 쓰지 않는다.** 복구 워커는 DB를 보고 판단하므로
두 경로가 서로 다른 진실을 볼 수 있다. completion의 성공/실패는 **메트릭·로그 용도**이며,
**예외는 `undurableOrderIDs` 하나뿐**이다(A-6).

## A-6. undurable quarantine — happens-before가 깨지는 유일한 구멍

**현재 코드의 실제 경로:**

```
trade 정산 실패
→ failed_settlements.RecordFailure 도 실패          main.go:702-705
→ processTradeSettlement returns handled=false
→ processSingleOutboxEvent 가 handled 를 버린다      main.go:509-513
→ settleTradeBatchWithFallback 은 반환값이 없다      main.go:553
→ worker 는 무조건 success completion 을 송신        settlement_pipeline.go:43-47
```

이 상태에서 terminal이 DB guard를 조회하면 `OPEN` row가 없어 **dependency 없음으로 오판**한다.

**계약 — 메모리 quarantine + outbox PENDING backstop**

```
undurableOrderIDs = processSingleOutboxEvent 가 handled == false 를 반환한 trade 의
                    BuyOrderID · SellOrderID
```

`handled == false` **하나만** 기준으로 삼으면 `failureRecorder == nil`([main.go:699](../../../cmd/main.go))도
자동 포함되고, **`MarkProcessed` 실패는 자동 제외**된다([main.go:518-521](../../../cmd/main.go) —
정산은 커밋됐고 마킹만 실패했으므로 dependency는 충족이다).

**배선**(A의 필수 선행): `processSingleOutboxEvent` → `handled bool` 반환 /
`settleTradeBatchWithFallback` → `[]uint` 반환 / `settleBatch func(batch, collect) []uint` /
`settlementResult`에 `undurableOrderIDs []uint` 추가. **worker는 판정하지 않고 전달만 한다.**

**dispatcher의 단계 내 순서를 고정한다:**

```
case res := <-completions:
    1) unsafeOrders 에 res.undurableOrderIDs 표시     ← 반드시 먼저
    2) outstanding--, touched count 감소, 0이면 entry 삭제
    3) ready 가 된 waiting terminal 평가
         unsafeOrders 에 있으면 → dispatch 금지, outbox PENDING 유지,
                                   waiting 에서 제거, unsafeOrders entry 삭제, 카운터++
         없으면                → terminal job dispatch
    4) pending[res.seq] = res; flushBroadcasts()
```

**quarantine이 terminal을 놓칠 수 없는 이유**: 그 배치가 X를 touch했으므로 `inFlight[X] ≥ 1`이고,
terminal은 `inFlight[X] == 0`에서만 dispatch된다 — **문제의 completion보다 terminal이 먼저 나갈 수 없다.**
A의 fence가 quarantine의 건전성을 보장한다.

**메모리 상태의 지위**: DB와 경쟁하는 **제2의 성공 권위가 아니다.** DB에 권위 있는 기록을 만들지
못했을 때의 **보수적 fail-closed overlay**다. 프로세스가 죽어 메모리가 사라져도 outbox에
trade·terminal이 **모두 PENDING**이므로 boot 경로가 받는다.

**알려진 한계**: terminal이 오지 않는 주문(부분 체결 후 잔존)의 `unsafeOrders` entry는 재시작까지
잔류한다. 생성 조건이 **이중 DB 실패**라 드물지만 **상한을 주장하지 않는다** —
gauge로 관측하고, 증가가 실측되면 그때 eviction을 설계한다.

## A-7. shutdown 계약

**graceful** — 이미 partition input에서 꺼낸 이벤트를 버리지 않는다.

```
stop 수신
→ partition input 비활성화 (새 outbox 이벤트를 더 읽지 않음)
→ 이미 만들어진 readyJob 은 계속 dispatch
→ outstanding completion 계속 수신
→ dependency 가 0이 된 waiting terminal 을 terminal job 으로 dispatch
→ readyJob = 0 && outstanding = 0 && waitingTerminal = 0 이면 dispatcher 종료
→ jobs 채널 종료 → worker 종료 확인
```

즉 **이미 소비한 terminal은 dependency 해제 후 실행하거나 durable defer까지 완료**한다.

**강제 종료**(deadline·context cancel) — graceful drain을 끝까지 보장할 수 없다.

- 아직 dispatch되지 않은 이벤트는 dependency map에 **등록하지 않는다**
- 원본 outbox는 **PENDING 유지**
- 처리 중 트랜잭션은 **DB 원자성**에 맡긴다
- 완료되지 않은 failure record는 **부팅 replay backstop** 대상
- 메모리 상태를 **억지로 "완료" 처리하지 않는다**
- 종료 로그에 미완료 outstanding·waiting terminal **수**를 남기되 **주문 ID는 노출하지 않는다**

**채널 소유권**: `completions`를 worker보다 먼저 닫으면 panic이 가능하므로 dispatcher가 임의로 닫지
않고 worker 종료 순서에 맞춰 소유권을 명시한다.

## A-8. waiting terminal의 상한

`2N`은 **dispatched job만** 제한하므로 waiting terminal은 별도 상태다. 정상 불변식 아래에서는:

```
최대 touched orders ≤ maxOutstanding × maxBatchSize × 2 = 2N × 32 × 2
```

terminal이 durable defer로 전환되면 **메모리 waiting 상태에서 즉시 제거**한다
(정확히는 **dispatch 시점에 제거**되고 defer 판단은 worker에서 일어나므로, dispatcher는 defer 상태를
들고 있지 않다).

**동일 주문에 terminal이 둘 이상 나타나는 것은 엔진 불변식 위반**으로 처리하고 조용히 덮어쓰지 않는다
(오류 로그 + 카운터). 단, **저장소가 이를 탐지하지는 않는다** → C-4.

---

# C. terminal durable defer

## C-1. durable handoff — 기존 outbox 의미에 맞춘다

현재 코드에서 outbox `PROCESSED`는 두 가지를 의미한다:

1. 도메인 작업 성공이 내구적으로 커밋됨
2. **도메인 작업 실패가 `failed_*` 테이블에 내구적으로 인계됨**

`failed_settlements`([main.go:706](../../../cmd/main.go))와
`failed_market_completions`([main.go:628](../../../cmd/main.go))가 **이미 2번 계약을 쓴다** —
기록이 성공하면 `handled=true`를 반환하고 호출자가 원본 outbox를 마킹한다.

```
terminal 실행 실패 또는 dependency 차단
  → failure record 커밋
  → Process=true → 원본 outbox PROCESSED (durable handoff 확정)
  → 라이브/replay 계속
  ⋯ 온라인 retry worker: dependency 확인 → terminal 실행 → record RESOLVED

failure record 커밋 자체가 실패
  → Process=false → outbox PENDING 유지 → Undurable → boot 경로는 즉시 중단
```

**이전에 검토한 3단계(취소 커밋 → `MarkProcessed` → `Resolve`)는 폐기한다.** 유지하면
온라인 worker에 outbox marker 의존을 새로 주입해야 하는데
`SettlementRetryWorker`에는 현재 outbox 의존이 **전혀 없고**
([settlement_retry_worker.go:43-51](../../../internal/service/settlement_retry_worker.go)),
cancel만 outbox를 만지는 **비대칭**이 생긴다.

## C-2. `failed_order_cancellations` — durable retry index

**outbox가 진실의 원본이고, 이 테이블은 온라인 복구를 위한 retry index다.**

`matching.OrderCancelled`([engine.go:112-117](../../../internal/matching/engine.go))는
`OrderID` / `CoinSymbol` / `Side` / `EngineEventID` 네 필드뿐이므로 전부 보존한다.

| 컬럼 | 용도 |
|---|---|
| `OrderID` | **uniqueIndex** — 멱등 키 |
| `OutboxEventID` | provenance(원본 추적·감사·1:1 연결). **복구 시 마킹 키가 아니다** |
| `CoinSymbol` · `Side` · `EngineEventID` | `OrderCancelled` 재구성 |
| `ErrorMessage` · `Status` · `RetryCount` · `OccurredAt` · `Resolution` · `ResolvedAt` | `failed_market_completions`와 동형 |

`ProcessOrderCancellation`([order_service.go:336](../../../internal/service/order_service.go))은 단일
트랜잭션이고 `isCancellableOrderStatus`가 no-op 멱등성을 주므로 at-least-once 재시도가 안전하다.

## C-3. `RecordFailure` vs `EnsureDeferred`

**두 의미를 반드시 분리한다.** 현재 저장소는 충돌 때마다 `retry_count + 1`을 한다
([failed_market_completion_repository.go:29-39](../../../internal/repository/failed_market_completion_repository.go),
[failed_settlement_repository.go:35-45](../../../internal/repository/failed_settlement_repository.go)).
차단에 `RecordFailure`를 쓰면 **한 번도 시도하지 않은 terminal의 retry budget이 replay마다 소모**된다.

```
RecordFailure(..., sourceOutboxID, executionErr)
  실행을 시도했고 실패. 신규 1, 기존 행이면 retry_count + 1.

EnsureDeferred(..., sourceOutboxID, reason)
  실행하지 않음. 없으면 retry_count = 0 으로 생성, 있으면 DO NOTHING 후 기존 행 반환.
  retry_count 증가 없음. replay 에도 멱등.
```

**`EnsureDeferred`는 `ON CONFLICT DO NOTHING` 의미론이다** — 기존 행의 `status`·`resolved_at`을
건드리지 않는다. 특히 **`RESOLVED`를 `OPEN`으로 되돌리지 않는다**(이미 실행된 terminal을 재실행
대상으로 되살리게 된다). GORM `DoNothing`은 기존 행을 돌려주지 않으므로 뒤이은 조회 1회가 필요하다
([wallet_reporsitory.go:157-160](../../../internal/repository/wallet_reporsitory.go)에 선례).

**`retry_count = 0` 마이그레이션 (필수)**

워커는 실행 전에 `failure.RetryCount >= MaxRetryCount`를 검사한다
([settlement_retry_worker.go:134](../../../internal/service/settlement_retry_worker.go)).
deferred record가 1에서 시작하면 실제 시도가 **4회로 줄어든다** —
"dependency 차단은 retry budget을 소비하지 않는다"는 계약이 **저장소 계층에서 깨진다**.
문서에 한계로 적는 것으로는 계약을 지킬 수 없다.

```
failed_market_completions:
  DB default   1 → 0
  CHECK        retry_count > 0 → retry_count >= 0
failed_order_cancellations: 처음부터 같은 계약으로 생성
```

- 기존 데이터는 모두 1 이상이므로 **데이터 변환이 필요 없다** — 의미만 확장된다.
- **GORM `default:1` 태그를 반드시 함께 0으로 바꾼다**
  ([failed_market_completion.go:21](../../../internal/model/failed_market_completion.go)).
  남겨두면 Go에서 `RetryCount: 0`이 zero value로 INSERT에서 생략돼 DB default가 먹는다.
  통합 테스트도 모델 기준 `AutoMigrate`를 돌리므로
  ([testdb/integration.go:31](../../../internal/testdb/integration.go)) 양쪽이 맞아야 재현된다.
- `AutoMigrate`는 기존 CHECK를 갱신하지 않는다 —
  [`migrations/004_order_cancelled_event.sql`](../../../migrations/004_order_cancelled_event.sql)의
  멱등 패턴(fresh DB/기존 DB 양쪽 처리)을 그대로 따른다.
- **Down은 no-op**으로 두고 사유를 주석에 남긴다 — `retry_count = 0` 행이 있으면 CHECK를 다시
  좁힐 수 없다([001_constraints.sql:341-342](../../../migrations/001_constraints.sql)의 자체 방침).
- **`failed_settlements`는 건드리지 않는다.** trade 정산에는 dependency 차단 개념이 없다.

## C-4. unique 제약의 정확한 의미

> `OrderID` unique는 **주문당 failure record를 하나로 수렴**시킨다. replay·재시도는 멱등적으로 같은
> 행을 재사용한다. 현재 리포지토리가 `ON CONFLICT DO UPDATE`를 쓰므로 **서로 다른 terminal 이벤트를
> invariant violation으로 탐지하지 않는다.**

주문당 terminal 1개는 **엔진 불변식과 기존 테스트**에 의존하며, 저장소 수준 탐지는 범위 밖이다.

## C-5. 기록 실패 자체의 처리

- **terminal worker job 안에서** bounded transient-only backoff (dispatcher가 아니다 — dispatcher를
  막지 않는다)
- non-transient → 즉시 강등
- 최종 실패 시 **오류 로그 1회 + `settlement_terminal_defer_record_failed_total{kind}` 1회**
  (cancel·market done 공통 — 두 경로 모두 기록이 실패할 수 있다)
- outbox는 **PENDING 유지**
- 이는 **온라인 복구의 강등이지 정합성 손실이 아니다**
- **wall-clock 상한은 주장하지 않는다** — backoff sleep은 유한하지만 DB 호출 시간이 무한할 수 있고,
  현재 `statement_timeout`·`lock_timeout`·pool wait 상한이 **없다**
  ([config/database.go:133](../../../config/database.go)은 `connect_timeout`만 붙인다). → 백로그

## C-6. crash 복구

| crash 지점 | 다음 동작 |
|---|---|
| failure record 커밋 전 | outbox PENDING → 다음 부팅 replay가 재시도(지속 실패 시 fail-fast) |
| record 커밋 후 · `MarkProcessed` 전 | 다음 부팅에서 기존 OPEN record를 `OrderID`로 확인·재사용 후 마킹 |
| `MarkProcessed` 후 · terminal 복구 전 | 온라인 worker가 OPEN failure를 복구 |
| terminal 성공 후 · resolve 전 | 다음 polling에서 멱등 no-op 후 resolve |
| resolve 후 | 완료 |

**온라인 복구 의존**: handoff 이후 복구는 전적으로 `SettlementRetryWorker`에 달려 있다
(boot replay가 다시 보지 않는다). `failed_market_completions`의 기존 자세와 동일하며,
**worker 미기동은 곧 복구 정지**를 뜻한다.

---

# boot 경로 — OutboxReplayer 완전 fail-closed

## 왜 필요한가

`FindPendingAfter`가 `Order("id ASC")`이지만
([trade_outbox_repository.go:34](../../../internal/repository/trade_outbox_repository.go))
**순서대로 방문할 뿐 앞 이벤트 실패에서 멈추지 않는다**:

- `Process=false` → `continue`([outbox_replayer.go:74](../../../internal/service/outbox_replayer.go))
- corrupted → **`MarkProcessed`로 격리** 후 `continue`([outbox_replayer.go:65](../../../internal/service/outbox_replayer.go))

[:69-71](../../../internal/service/outbox_replayer.go)의 근거("정산은 멱등·가환이라 뒤 이벤트를 계속
처리해도 안전")는 **trade↔trade에만 맞고 trade→terminal에는 적용되지 않는다.**
그리고 corrupted 격리는 단순한 skip이 아니라 **처리되지 않은 금융 이벤트를 PROCESSED로 영구 선언**하는
파괴적 행위다.

또한 `Process`는 `processExecutionEvent`를 그대로 호출하므로
([main.go:135-138](../../../cmd/main.go)) **replayer의 terminal 처리에 dependency guard가 없다.**
B는 `retryFailedCompletions()`만 보호한다.

## 계약

**중단 2조건** — 현재 행을 **PENDING으로 유지**하고 `Replay()`가 error를 반환한다.
[main.go:141-143](../../../cmd/main.go)이 이미 `log.Fatal`이라 **fail-fast는 자동**이며,
**라이브 파이프라인은 개시되지 않는다**(replayer가 boot-only이므로, 중단 후 라이브를 띄우면
PENDING 백로그가 영영 처리되지 않고 새 terminal이 미정산 trade 위에서 실행될 수 있다).

1. `Process == false`(내구 확정 실패) → `Undurable`
2. corrupted(역직렬화 불가) → **마킹하지 않고** `Corrupted`

**중단하지 않는 경우**: `MarkProcessed` 실패([outbox_replayer.go:76-81](../../../internal/service/outbox_replayer.go)).
정산이 이미 커밋돼 dependency가 충족됐고 다음 부팅이 멱등 재처리한다 —
A-6에서 마킹 실패를 quarantine에서 제외한 것과 **같은 기준**이다.

**결과 4분류**

| 분류 | 상태 | replay |
|---|---|---|
| `Undurable` | PENDING 유지 | **즉시 중단** |
| `Corrupted` | PENDING 유지, 마킹 안 함 | **즉시 중단** |
| `Deferred` | 도메인 확정, `MarkProcessed`만 실패 | 계속 |
| durable terminal defer 성공 | `Process=true` | 계속(정상 마킹) |

**replayer terminal guard**: replayer의 terminal 처리도 **라이브와 같은 guard**
(`HasOpenFailureForOrder`)를 통과한다. 차단된 terminal은 **C의 durable defer로 전환**하고
(cancel → `failed_order_cancellations`, market done → `failed_market_completions`) **replay는 계속한다.**
PENDING 유지로 다음 부팅에 미루지 않는다 — boot-only replayer에서 그것은 **재시작을 복구 수단으로 삼는 것**이다.

**DB 장애 중 부팅 실패는 계약이다**: `Process=false`는 정산·기록 동시 실패(DB 이상)를 뜻하므로
DB 회복 전까지 서버가 뜨지 않는 것이 정상이며, crash loop는 버그가 아니다. runbook에 명시한다.

**corrupted 수동 복구**: 자동 격리를 제거한 자리는 **사람의 결정**이 대체한다. runbook에 행 검토·처리
절차를 문서화한다. **새 스키마·엔드포인트는 만들지 않는다.**

## ID 의미 분리

```
transactionalOutboxID  정산 트랜잭션 안에서 PROCESSED 마킹할 ID.  live: row ID / replay: 0
sourceOutboxID         원본 이벤트 provenance.                    live: row ID / replay: row ID

processExecutionEvent(event, transactionalOutboxID, sourceOutboxID, ...)
OutboxReplayer.Process func(sourceOutboxID uint64, event matching.ExecutionEvent) bool
```

`transactionalOutboxID`를 replay에서 `row.ID`로 바꾸면 trade 경로의 in-tx 마킹이 되살아나
`markedInTx` 의미가 바뀐다([main.go:133-134](../../../cmd/main.go)의 의도). **반드시 별개 인자다.**

`failed_market_completions`에는 `OutboxEventID` 컬럼이 없으므로 market done 경로의 `sourceOutboxID`는
**저장하지 않고 호출 provenance로만** 쓴다. 이번 범위에서 시장가 테이블은 확장하지 않는다.

---

# 관측

배리어가 사라지므로 `settlement_barrier_*` 계열은 **더 이상 방출되지 않는다.**
**기존 시계열을 재활용하지 않는다**(의미가 달라진 값을 같은 이름으로 잇지 않는다). 대체 지표:

| 지표 | 타입 | 의미 |
|---|---|---|
| `settlement_terminal_wait_seconds{kind}` | Histogram | terminal 도착 → dispatch. **배리어 대기의 대체** |
| `settlement_outstanding_jobs{partition}` | **Gauge** | 현재 outstanding. dispatch 성공 시 +1, retire 시 −1 |
| `settlement_terminal_deferred_total{kind,reason}` | Counter | `reason=dependency_open` / `quarantine` |
| `settlement_dependency_record_failed_total` | Counter | **trade**의 실패 기록 자체가 실패(= quarantine 등록) |
| `settlement_terminal_defer_record_failed_total{kind}` | Counter | **terminal**의 defer 기록 최종 실패(온라인 복구 강등) |
| `settlement_quarantined_orders` | Gauge | 현재 `unsafeOrders` 크기. **무한 증가 감시용** |

`kind` = `cancel` \| `market_done`. `settlement_completion_blocked_total`(B)은 그대로 둔다.

**`settlement_outstanding_jobs`는 Gauge여야 한다.** 판정 기준이 "`2N`에 **상시** 붙어 있는가"이므로
dispatch 순간의 분포(Histogram)로는 **얼마나 오래 유지됐는지**를 알 수 없다. 순간 분포가 따로 필요해지면
`settlement_outstanding_jobs_at_dispatch`라는 **별도 이름**으로 추가하고, "상시" 판정에는 쓰지 않는다.

**defer 기록 실패는 kind로 일반화한다.** `EnsureDeferred`/`RecordFailure`는 cancel뿐 아니라
market done 경로에서도 실패할 수 있다. trade의 실패 기록 실패(`settlement_dependency_record_failed_total`)와는
**의미가 다르므로 지표를 합치지 않는다** — 전자는 quarantine을 유발하고 후자는 온라인 복구만 강등한다.

---

# 검증 계획

## 단위·통합

**A — dispatcher**

1. 무관한 주문의 terminal은 **다른 배치를 막지 않는다**(핵심 회귀 — 현재는 막는다)
2. 같은 주문의 terminal은 그 주문의 outstanding batch가 **모두 retire될 때까지 실행되지 않는다**
3. 실패 completion도 **retire된다**(슬롯·count 누수 없음)
4. 실패 completion 후 terminal은 **DB guard로 판정**된다(메모리 플래그 아님)
5. `outstanding`은 **`2N`을 넘지 않는다**
6. send case 미선택 시 **등록되지 않는다**
7. 배치당 touched order는 **중복 제거 후 1회만** 카운트(maker=taker 자기거래 포함)
8. graceful shutdown: 소비한 terminal이 **실행되거나 durable defer까지 완료**된다
9. 강제 종료: 등록 상태가 **남지 않고** outbox PENDING이 유지된다

**A-6 — quarantine**

10. `handled=false` → 해당 주문 quarantine, terminal **미dispatch**, outbox PENDING
11. `MarkProcessed` 실패 → **quarantine되지 않는다**(정산은 커밋됨)
12. quarantine된 주문의 terminal 소비 후 **entry 삭제**
13. quarantine 중에도 **무관한 이벤트는 계속 처리**

**C — 저장소**

14. `EnsureDeferred` 최초 생성은 **retry count 0**
15. 같은 주문에 반복 호출해도 **0**
16. `RESOLVED` 행에 호출해도 **상태·retry count 불변**
17. `RecordFailure` 최초 생성은 **1**
18. deferred 행에서 실제 실행이 실패하면 **0→1**
19. 기존 실제 실패 행에서 재실패하면 **1→2**
20. retry count 0인 deferred 행이 **`MaxRetryCount`만큼 실제 시도 기회**를 얻는다
21. 마이그레이션 후 **기존 1 이상 레코드가 그대로 유지**된다

**C — 온라인 retry worker** (저장소만으로는 복구가 작동함을 증명하지 못한다)

22. cancel retry가 **OPEN trade dependency에서 실행되지 않는다**
23. dependency 차단 중 cancel의 **retry count가 0으로 유지**된다
24. dependency 해결 후 **cancel이 실행되고 failure record가 RESOLVED**된다
25. cancel 실행이 실패하면 **`RecordFailure`로 count가 증가**한다(차단과 구분)
26. **cancel·market done 양쪽** dependency 조회 오류가 모두 **fail-closed**
27. terminal defer 기록 최종 실패 시 **`{kind}`별 counter 증가**
28. worker의 dependency store가 없으면 **terminal phase 전체 fail-closed**

**boot**

29. `Process=false` → 현재 행 PENDING, **뒤 이벤트 미처리**, `Replay()` error
30. corrupted → **`MarkProcessed` 미호출**, 뒤 이벤트 미처리, `Replay()` error
31. `MarkProcessed` 실패 → **계속 진행**, `Deferred` 증가
32. replay terminal이 OPEN dependency를 만나면 **durable defer 후 계속**
33. replay trade settler는 `transactionalOutboxID=0`, replay terminal failure record는
    실제 `sourceOutboxID=row.ID`, **live 경로는 두 ID 동일** — 한 테스트에서 동시에 고정

## 실측 (29번)

**26번과 동일 조건 same-session A/B**(e2-highcpu-4 server+DB, e2-standard-8 load-gen 수평 증설,
`LOAD_START_AT_MS` 배리어, `--summary-export`).

| 판정 | 기준 |
|---|---|
| **주 가설** | `settlement_terminal_wait_seconds` p50이 **배리어 대기 13.48ms 대비 유의하게 감소** |
| **처리량** | worker busy가 **13.1%에서 상승**, 배치 크기가 **2.82에서 상승** |
| **정합성(비협상)** | 무결성 검사 전항목 통과 — 이게 깨지면 다른 수치는 읽지 않는다 |
| **회귀 없음** | `sli_cancel_success` 100%(26번) 유지, 가용성 하드 보장 유지 |
| **부작용 감시** | Gauge `settlement_outstanding_jobs{partition}`가 `2N`에 **상시** 붙어 있으면 dispatcher가 새 병목 |

**해석 전 무결성 검사를 먼저 통과시킨다.** 통과 전 성능 수치는 읽지 않는다.

---

# 범위 밖

- **축 2 (매칭 quantum)** — 독립 판정
- **terminal-outbox 원자성** (terminal 커밋과 outbox 마킹을 한 트랜잭션으로) — cancel·market done 공통 사이클
- **DB per-call timeout 계약** (`statement_timeout`·`lock_timeout`·pool wait·context) — C-5의 상한 부재 원인
- **durable dependency index 신설**(주문↔선행 outbox 관계를 DB에 저장) — 스키마·replayer·claim 의미까지 확대. 실측 없이 과하다
- **`unsafeOrders` eviction 정책** — gauge로 먼저 관측
- 저장소 수준의 **중복 terminal 탐지**
- `settlement_job_execution_seconds`의 fallback/failed 라벨 분리
- `failed_settlements`의 `retry_count` 제약 변경
