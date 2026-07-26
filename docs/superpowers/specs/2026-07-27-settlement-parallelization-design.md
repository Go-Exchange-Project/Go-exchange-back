# 3차 ②-수정 정산 병렬화 설계 (순서 보존 브로드캐스트)

- **날짜**: 2026-07-27
- **상태**: 설계 검토 중
- **로드맵**: [3차 리팩토링 ②-수정](../../refactor/README.md)
- **근거**: [24번 진단](../../benchmarks/24-2026-07-26-throughput-binding-link-diagnosis.md)이
  **최초 바인딩 링크 = 정산 워커(심볼당 1개)** 를 실측 확정(5회 런 모두
  `settlement_worker_queue_length{worker="8"}=256` 고정 포화, 반복 goroutine 덤프가 매번 활성 워커
  1개의 `IO wait`/pgx Exec 지속, Postgres CPU 0~3.66%·backend 2/16코어로 자원 경쟁 배제).
  단일 심볼의 정산이 **DB 왕복마다 직렬화**되는 것이 처리량 천장이다.

## 무엇을 바꾸는가

**정산 DB 작업은 병렬로, 외부로 나가는 브로드캐스트는 순서대로.** **정산 도메인 로직**
(`SettleTradeBatch`·멱등·폴백 의미론)은 그대로 두되, **orchestration helper는 바뀐다** — 현재
`settleTradeBatchWithFallback`(main.go:551)은 정산과 브로드캐스트를 **한 함수 안에서** 수행하고
반환값이 없어, worker가 "정산만 하고 결과를 coordinator에 넘기는" 구조가 불가능하다. 따라서:
- **정산 결과를 반환하는 helper**(정산+폴백 수행 → 방출할 trade들 + 실패 정보 반환, 브로드캐스트 안 함)
- **순서 커밋 단계의 broadcast helper**(coordinator가 `batchSeq` 순서로 호출)
로 분리한다. 도메인 로직(트랜잭션·멱등·폴백 판단)은 이동만 하고 의미는 불변.

```
단일 dispatcher
  batch #1 ─→ worker
  batch #2 ─→ worker
  batch #3 ─→ worker
완료 순서: #2, #3, #1
정산 커밋: 병렬 완료 그대로 허용
브로드캐스트: #1 → #2 → #3 (디스패치 순서로 직렬 방출)
```

### 왜 브로드캐스트 순서를 보존하는가 (A 기각)

"클라이언트가 시각·`engine_sequence`로 정렬한다"는 가정은 **현재 코드와 다르다**:
- 프런트는 수신 순서대로 앞에 삽입한다 — `Index.tsx:294` `setTrades((prev) => [trade, ...prev])`.
  정렬 없음.
- `trades` 배열도 전달 순서대로 처리 — `Index.tsx:317-320`.
- 서버도 순서 보존을 **의도된 속성**으로 명시 — `broadcastSettledTrades` 주석(main.go:710,
  "등장 순서·배치 내 순서 보존").

따라서 순서를 흩뜨리면 최근 체결 목록이 실제 엔진 순서와 다르게 표시된다 — 문서화로 끝나는 내부
변화가 아니라 **사용자 가시 동작의 회귀**다. 순서 보존은 요청 안 한 유연성이 아니라 **회귀 방지**다.

## 설계

### 토폴로지 — 파티션 dispatcher 유지 + 전역 공용 worker pool

기존 `settlementQueues`는 **심볼별이 아니라 해시 파티션 큐**(여러 심볼이 한 큐를 공유할 수 있다).
이를 유지하고 **정산 worker만 전역 공유**한다:

```
hashed partition queues
  dispatcher 0 ─┐
  dispatcher 1 ─┼─→ global settlement worker pool (N)
  ...           │
  dispatcher P-1┘
```

- 각 **dispatcher가 자기 파티션의** 순서·종결 배리어·broadcast reorder 상태를 **소유**한다.
- **전체 DB 동시성은 정확히 N**(파티션 수 × N이 아니다).
- 기존 심볼 해시 라우팅·파티션 내 FIFO는 그대로 — 다중 심볼이 같은 파티션에 배정돼도 기존보다
  약화되지 않는다.
- **파티션 수 P**는 기존 `GOEXCHANGE_SETTLEMENT_WORKERS`가 계속 의미한다(**의미 변경 없음**).
  동시 정산 수 N은 **신규 `GOEXCHANGE_SETTLEMENT_CONCURRENCY`** 로 분리한다(아래 "병렬도 설정").

### dispatcher 상태기계 (교착 없는 event loop)

dispatcher가 job 送信과 completion 수신을 **블로킹 순차**로 하면 교착한다:
`dispatcher → 가득 찬 jobs 채널 send 대기` × `worker → 가득 찬 completion 채널 send 대기`
→ worker가 jobs를 소비 못 하고 dispatcher는 completion을 못 받는다.

따라서 dispatcher는 **항상 select로 multiplex하는 명시적 event loop**다:

```go
for {
    select {
    case jobs <- nextBatch:      // 배치 디스패치(있을 때만 활성 — 없으면 nil 채널)
    case res := <-completions:   // in-flight 감소 + reorder 버퍼 + 순서대로 방출
    case ev, ok := <-partitionQueue: // 입력 수집(배리어 중엔 nil 채널로 비활성)
    }
}
```

**규칙(불변식)**:
- dispatcher는 **jobs send와 completion receive를 절대 분리해 블로킹하지 않는다**(항상 같은 select).
- **종결 배리어 중에는 새 job을 dispatch하지 않고 completion만 수신**한다(입력·jobs 케이스를 nil로
  비활성화) → in-flight 단조 감소 → 유한 시간에 0.
- **채널 용량**: `jobs`는 전역 공용(용량 = N, worker 수와 동일하게 두어 대기 job을 최소화),
  `completions`는 **파티션별**이며 용량 = 해당 파티션의 최대 in-flight(= N) → **worker의 completion
  送信이 영구 블로킹하지 않는다**(각 in-flight 배치는 자기 슬롯을 이미 확보한 상태로만 디스패치된다).
- **shutdown 순서**: 입력 close → 남은 배치 dispatch → completion drain(순서대로 방출) →
  worker 종료 → 기존 도미노 유지.

- **worker N개**: `settleTradeBatchWithFallback`의 **정산 부분만** 수행(브로드캐스트 안 함) →
  결과(`batchSeq` + 방출할 trade들 + 실패 정보)를 자기 파티션 `completions`로 반환.
- **coordinator 역할은 dispatcher와 같은 goroutine**(위 event loop): completion을 작은 **pending
  map**에 저장하고 `nextBroadcastSeq`부터 **연속으로만** 방출.

**추적 대상이 batch 단위**라 상태가 작다. trade별 low-watermark·gap 추적은 **불필요**.

### 종결 이벤트 배리어 — 어디까지 기다리는가 (명시)

비-trade 이벤트(`MarketOrderDone`/`OrderCancelled`)를 만나면 dispatcher는 **앞선 모든 배치가
① DB 정산 커밋 + ② outbox 처리 확정 + ③ 순서대로 브로드캐스트 방출(enqueue)** 까지 마친 뒤
종결 이벤트를 단건 처리한다.

- **정합성 최소 조건은 ①(+②)** — 종결 이벤트가 그 주문의 체결들보다 먼저 처리되면 잔여 홀드 반환이
  틀린다. 현재 `collectTradeBatch`의 boundary 의미론과 동일한 보장.
- **③까지 포함하는 이유**: 외부 관측 순서까지 유지되어 가장 이해하기 쉽다(종결 이벤트의
  브로드캐스트가 앞선 체결보다 먼저 나가지 않는다). 비용은 배리어 시점에만 발생(종결 이벤트는 드묾).
- **기아 없음**: 배리어 진입 후 dispatcher는 **새 배치를 던지지 않으므로** in-flight는 단조 감소해
  반드시 0에 도달한다(유한 대기).

### 동시성 원시 — WaitGroup 대신 completion 채널 + in-flight 카운터

`WaitGroup`도 "dispatcher만 `Add()` / 배리어 진입 후 `Add()` 없음 / worker가 `Done()` /
dispatcher가 `Wait()`" 규칙이면 안전하지만(등록만 dispatcher 기준 직렬이고 완료는 병렬 goroutine에서
일어난다 — 이 점을 정확히 해둔다), **shutdown·실패 결과 수집·브로드캐스트 순서까지** 함께 다뤄야
하므로 **completion 채널 + in-flight 카운터**가 더 자연스럽고 검증하기 쉽다. 배리어·종료 모두
"in-flight가 0이 될 때까지 completion을 수신·방출"이라는 **하나의 루프**로 표현된다.

### 정합성 근거 — 그리고 순서 독립성의 **실제 범위**(실측)

- **잔고·수량**: 홀드가 자금을 미리 예약하므로 정산은 예약분 이동 — **덧셈이라 순서 무관**.
  멱등 키·폴백 무변경.
- **`AvgBuyPrice`는 순서 독립이 아니다(실측 확인)**: `balance.go:172-173`은 매 체결마다
  `(avg*qty + cost) / newQty`로 **나눗셈**을 수행하고 `shopspring/decimal.Div`는 유한 정밀도
  (`DivisionPrecision=16`) 반올림을 한다. 재현 실험(`_workspace/avgprice_exp/main.go`,
  `go run ./_workspace/avgprice_exp`) 결과:
  - 수량(qty)은 **모든 케이스에서 정순=역순 동일**.
  - 평균가는 케이스에 따라 **다름** — 기존 보유분 + 3건 케이스에서 정순 `50000011` vs 역순
    `50000010.9999999999999999`, **차이 1e-16**.
  → 따라서 "최종 잔고·원장·주문 상태가 전부 동일"이라는 단정은 **틀렸다**. 정확한 주장은
  **"잔고·수량·주문 상태는 순서 무관, `AvgBuyPrice`는 마지막 자리 수준의 순서 의존이 있을 수 있다"**.
- **원장 행은 byte-for-byte 동일하지 않다**: 원장은 `AvailableBalanceAfter`/`LockedBalanceAfter`
  (커밋 시점의 중간 잔액)를 기록하므로, 최종 합계가 같아도 **행별 중간 잔액은 커밋 순서에 따라
  달라진다**. 등가성 테스트는 이를 기대해서는 안 된다(아래 검증 계획 참조).
- **비협상 정합성 5검사에는 영향 없음(확인)**: `reconciliation_worker.go`는 원장-지갑 일치·자산
  총량 보존·시장가 잔존만 검사하며 **`avg_buy_price`를 검사하지 않는다**. 1e-16 드리프트는 이
  불변식들을 깨지 않는다(자산 총량은 수량 기준, 원장-지갑은 잔액 기준).

**미결 결정(사용자 판단 필요)**: 위 `AvgBuyPrice` 순서 의존(1e-16)을 **허용**할지, 아니면
**동일 지갑 충돌 배치를 병렬 실행하지 않는 conflict-aware scheduling**으로 제거할지.
(누적원가/누적수량 방식으로 바꾸는 것은 이번 범위를 크게 넘으므로 비추천.)
- **데드락 안전(검증 완료)**: `SettleTradeBatch`는 **주문 락도 지갑 락도 전역 오름차순**
  (settlement_batch.go:107 `LockByIDs(sortedUintKeys(orderIDSet))`, :188
  `walletRepo.LockByIDs(sortedUintKeys(walletIDSet))`)이고 모든 트랜잭션이 **주문→지갑** 같은 단계
  순서를 따른다 → 동시 실행에도 순환 대기가 없다. (`BatchUpdateBalances`는 락 확보 후라 무관.)
- **동일 주문 경합**: 같은 주문의 부분 체결이 두 배치에 걸리면 행 락으로 직렬화된다 — 정합성 안전,
  **병렬도만 감소**. 단일 심볼 집중에서는 자주 발생할 수 있어 **실효 병렬도가 기대보다 낮을 수 있다**
  (정직한 한계, 재측정이 판정).

### 병렬도 설정 — 기존 env 의미를 바꾸지 않는다

- **`GOEXCHANGE_SETTLEMENT_WORKERS`는 지금 의미(파티션 수 P) 그대로 둔다.** 같은 이름을 "동시 정산
  수"로 재해석하면 운영자가 아는 의미가 바뀌어 위험하다.
- **신규 `GOEXCHANGE_SETTLEMENT_CONCURRENCY`(=N)** 로 전역 worker pool 크기를 정의한다.
  **N=1이면 현재 동작과 동등**(롤백 안전판·등가성 기준선).
- **상한 근거**: 정산 병렬 트랜잭션은 주문 홀드·아웃박스·리컨실리에이션과 **같은 DB 풀**을 공유한다
  (`GOEXCHANGE_DB_MAX_OPEN_CONNS`, 기본 **25**). 실효 병렬도는 `min(N, DB 풀 여유)`이므로 풀 고갈로
  주문 경로가 굶지 않도록 **보수적 기본값(4)** 에서 시작한다(기본 10을 곧바로 쓰지 않는다).
- **실증 스윕**: 재측정에서 **1/2/4/8**.

## 검증 계획

1. **등가성(정합성 핵심) — 무엇이 같아야 하는지 정확히**: 같은 이벤트 열을 병렬 N(예: 4)과 직렬
   1(`CONCURRENCY=1`)로 처리했을 때
   - **동일해야 함**: 지갑 `available/locked/quantity/krw` 최종값, 주문 `FilledAmount`·상태,
     원장 행 **개수와 delta 합계**, 자산 총량 보존, `failed_settlements`/`failed_market_completions` 0.
   - **동일할 필요 없음(명시적 비-기대)**: 원장 행별 `AvailableBalanceAfter`/`LockedBalanceAfter`
     (커밋 순서 의존), `AvgBuyPrice`의 최종 자리(위 실측대로 1e-16 수준 차이 가능 —
     단, "미결 결정"에서 conflict-aware를 택하면 이것도 동일해야 함).
   - 원장 행 집합을 **byte-for-byte 비교하지 않는다**.
2. **브로드캐스트 순서 보존(회귀 방지)**: 워커 완료를 의도적으로 뒤섞어도(#2·#3 먼저 완료) 방출은
   `#1→#2→#3` 순서임을 결정론적으로 단언.
3. **종결 이벤트 배리어**: 체결 다수 + 종결 이벤트 혼합 시 종결이 **앞선 모든 배치의 커밋·브로드캐스트
   뒤에** 처리됨을 단언. 배리어 대기가 유한함(기아 없음).
4. **동일 주문 동시 배치**: 같은 주문의 부분 체결이 두 배치에 걸려도 정합성 유지(락 직렬화).
5. **폴백·멱등 회귀**: 배치 실패 시 단건 폴백, 중복 이벤트 멱등 — 기존 테스트 무수정 그린.
6. **종료 드레인**: 큐 close 시 in-flight 배치를 모두 처리·방출한 뒤 종료(기존 도미노 유지),
   유실 0. `-race` 클린.
7. **처리량 수치는 이 스펙에서 주장하지 않는다** — 실증은 **24번 재실행**(같은 진단 하니스로
   1/2/4/8 스윕, `settlement_worker_queue_length` 포화 해소 여부 판정).

## 검토한 대안

- **A) 브로드캐스트 순서 뒤섞임 허용**: 프런트가 수신 순서로 삽입하므로(`Index.tsx:294`) 최근 체결
  목록이 엔진 순서와 달라지는 **사용자 가시 회귀**. 기각.
- **완료 배치 전체를 재정렬 후 일괄 처리**: 정산 커밋까지 순서를 묶어 병렬 이득을 상쇄. 기각 —
  **브로드캐스트만** 순서 커밋하는 최소 형태를 택한다.
- **trade별 low-watermark(gap 추적)**: batch 단위 재정렬로 충분. 기각(불필요한 복잡도).

## 범위 밖 / 후속

- 정산 로직(`SettleTradeBatch`) 자체 변경, 심볼 파티션 수와 병렬도의 env 분리(측정 후 조건부).
- **`order_settlement_duration_seconds_count`가 라이브 배치 정산 경로를 관측하지 못하는 문제**
  (24번이 발견한 죽은 메트릭) — 별도 관측성 후속.
- 처리량 실증(24번 재실행), 엔진 분할, 다중 심볼.
