# 매칭 엔진 두 quantum 설계 (B-3)

> 상태: 설계 확정. 구현 미착수.
> 선행: B-2 주문 생성 idempotency key (완료, [36번 벤치마크](../../benchmarks/36-2026-08-29-order-idempotency-key.md))

## 0. 왜 필요한가

현재 매칭 엔진 루프([engine.go:154-197](../../../internal/matching/engine.go))에는 서로 반대 방향의
기아(starvation) 두 개가 있다.

**취소 홍수 → 신규 주문 기아.** 루프 첫머리가 대기 중 취소를 논블로킹으로 드레인하면서
`continue`한다. 이 드레인에는 상한이 없다. 취소가 계속 들어오는 동안 `OrderCh`는 단 한 번도
읽히지 않는다.

**대형 sweep → 취소 기아.** `processOrder`는 `Match(order)`를 호출하고, `Match`는 주문이
소진되거나 상대가 없어질 때까지 **book 전체를 한 번에** 훑는다. 5,000개 maker를 쓸어가는
주문 하나가 도는 동안 `CancelCh`는 읽히지 않는다.

둘 다 "느리다"가 아니라 **"제어권이 돌아오지 않는다"**는 문제다. 그래서 해법도 튜닝이 아니라
스케줄러에 제어점(control point)을 만드는 것이다.

## 1. 왜 이 방식인가

세 후보를 검토했다.

| 후보 | 내용 | 판정 |
|---|---|---|
| A | 조각 사이에 취소·ticker만 허용, 후속 주문은 추월 금지 | **채택** |
| B | active sweep을 backpressure 시 park | 기각 — 숨은 active 주문을 무기한 park |
| C | 우선순위 큐 / 별도 취소 goroutine | 보류 — A로 부족하다는 실측 증거가 나오면 후속 |

A를 고른 이유는 **price-time priority를 깨지 않는 유일한 후보**이기 때문이다. 조각 사이에
새 주문을 받으면 나중에 들어온 주문이 진행 중인 sweep을 추월한다. 취소는 추월이 아니라
**철회**이므로 우선순위 계약을 깨지 않는다.

작업 순서도 값 탐색이 아니라 측정 순서로 고정한다.

| # | 단계 | 체크포인트 |
|---|---|---|
| 1 | 행동 변경 없는 계측 + 목적형 하니스 추가, 커밋 | **CP A** |
| 2 | 그 SHA에서 baseline 3회 수집 → SHA·JSON 보존 | **CP A** |
| 3 | 두 quantum 구현 | **CP A** |
| 4 | 같은 하니스로 6조합 3회 탐색 → 상위 2개 5회 확증 → 값 확정 → 전체 로컬 검증·CI | **CP A** |
| 5 | GCP 500 VU 1회 — 값 탐색이 아니라 **최종 회귀 게이트로만** | **CP B** |

1·2가 같은 체크포인트인 이유는 §13에, baseline을 4에서 다시 재지 않는 이유는 §8.4에 있다.

---

## 2. 조각화 단위와 재개 슬롯

### 2.1 분해

`Match`의 네 루프(`matchBuy`/`matchSell`/`matchMarketBuy`/`matchMarketSell`)를 읽은 결과,
**반복 사이에 들고 가는 지역 상태가 하나도 없다.** 매 iteration이 `bestMatchable*`로 상대를
새로 찾고, 진행 상태는 전부 `*Order`(`Amount`/`FilledAmount`/`QuoteAmount`/`FilledQuoteAmount`)와
book에 있다. 그래서 재개에 필요한 것은 **주문 포인터 하나**이고, 새 커서·이터레이터 상태가
필요 없다.

또 `Match`는 프로덕션에서 `processOrder` 한 곳에서만 불리고 나머지 약 30곳은 전부 테스트·벤치다.
그래서 `Match(order)` 시그니처를 그대로 둔다.

```go
// 재개 슬롯: 샤드당 하나. 조각 사이에 살아남는 유일한 sweep 상태.
type activeSweep struct {
	order  *Order
	book   *OrderBook
	trades int // 이 주문이 지금까지 만든 총 체결 수
}

// matchSlice는 최대 budget개의 체결을 만들고 돌아온다.
// done=true는 "더 체결할 수 없다"이고, budget 소진(done=false)과 다르다.
func (me *MatchingEngine) matchSlice(book *OrderBook, order *Order, budget int) (trades int, done bool)

// finishOrder는 마지막 조각에서만 부른다.
// 시장가면 MarketOrderDone 1회, 지정가 잔량이 있으면 book에 등록.
func (me *MatchingEngine) finishOrder(book *OrderBook, order *Order)
```

- `Match(order)` = budget 0(무제한)으로 `matchSlice` 1회 + `finishOrder` → **오늘과 동일**
- 스케줄러 = `maxMatchesPerTurn` budget으로 `matchSlice` 반복, `done`일 때만 `finishOrder`

### 2.2 budget 계약

| 값 | 의미 |
|---|---|
| `budget == 0` | 무제한 sentinel. **public `Match()` 전용.** |
| `budget > 0` | `trades >= budget`일 때만 yield |
| `budget < 0` | 발생 불가 — 설정 시점에 차단 |

루프 검사는 `if budget > 0 && trades >= budget { return trades, false }`이고, 위치는
**루프 조건 뒤**다. 예산 경계에서 정확히 전량 체결된 sweep이 같은 조각에서 `done=true`로
끝나야 빈 조각이 생기지 않는다.

`maxMatchesPerTurn`·`maxConsecutiveCancels`는 설정 시점에 `>= 1`을 검증하고, 0·음수는
**기동 실패**로 처리한다. 조용히 기본값으로 되돌리지 않는다 — 0은 무제한 sentinel과 충돌해
quantum을 통째로 무력화하므로, 조용한 fallback이 가장 나쁜 실패 모드다.

### 2.2.1 strict env 계약

**전용 parser를 새로 쓴다. 기존 `parsePositiveIntEnv`를 재사용하지 않는다.**

기존 파서([config/database.go:68](../../../config/database.go))는 세 지점이 이 요구사항과
정반대다.

```go
func parsePositiveIntEnv(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))   // ① 미설정과 "" 구분 불가
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback                          // ② 오류·0·음수를 조용히 fallback
	}
	return parsed
}
```

| # | 기존 동작 | 이 요구사항 |
|---|---|---|
| ① | `os.Getenv` — 미설정과 빈 문자열이 모두 `""` | `LookupEnv`로 구분. 빈 문자열은 error |
| ② | 파싱 실패·`<= 0`이면 **조용히 fallback** | **error**. 잘못된 값으로 부하를 도는 것을 막는다 |
| ③ | `TrimSpace`로 `" 3"`을 허용 | 공백 불허 |

기존 파서는 `EngineShardsFromEnv`·`OutboxBatchSizeFromEnv` 등 5곳이 쓰고 있다. **그 동작을
바꾸지 않는다** — 이번 변경 범위 밖이고, 그쪽은 조용한 fallback이 의도된 설계다.

```go
// LookupEnv 기반. 미설정과 "빈 문자열로 설정됨"을 구분한다.
func strictPositiveEnv(key string, def int) (int, error)
```

| 입력 | 결과 | 왜 |
|---|---|---|
| **미설정** (`LookupEnv` ok=false) | `def` | 설정하지 않은 것은 "기본값을 쓰겠다"는 명시적 선택이다 |
| **설정됐지만 빈 문자열** (`""`) | **error** | 셸 변수 오타·치환 실패의 전형적 결과다. 조용히 기본값을 쓰면 잘못된 설정으로 부하를 도는 것을 못 잡는다 |
| `" 3"`, `"3 "` | error | 공백 허용 안 함 |
| `"+3"`, `"03"` | error | 10진 정수 정규형만 허용 |
| `"0"`, `"-1"` | error | `>= 1` 위반. 0은 무제한 sentinel과 충돌 |
| 오버플로 | error | — |
| `"3"` | `3` | — |

`os.Getenv`는 미설정과 빈 문자열을 구분하지 못하므로 쓰지 않는다.

**환경변수명은 프로젝트 관례를 따른다** (기존 `GOEXCHANGE_JWT_SECRET`·
`GOEXCHANGE_OUTBOX_BATCH_SIZE` 등 11개와 동일한 접두).

- `GOEXCHANGE_MATCHING_MAX_MATCHES_PER_TURN`
- `GOEXCHANGE_MATCHING_MAX_CONSECUTIVE_CANCELS`

### 2.2.2 설정 주입 경로

```go
// config 패키지 — 기존 EngineShardsFromEnv와 같은 자리, 같은 역할
func MatchingQuantumFromEnv() (maxMatchesPerTurn int, maxConsecutiveCancels int, err error)

// matching 패키지
type QuantumConfig struct {
	MaxMatchesPerTurn     int
	MaxConsecutiveCancels int
}
func (c QuantumConfig) Validate() error
func NewShardedEngineWithQuantum(shardCount int, cfg QuantumConfig) (*ShardedEngine, error)
```

`config`가 `matching` 타입을 반환하지 않고 정수 두 개를 반환하는 이유: 새 패키지 의존을 만들지
않기 위해서다. `main`이 두 값을 받아 `QuantumConfig`를 구성한다.

- **`main`은 기동 시 `MatchingQuantumFromEnv()`를 호출하고, 에러면 `log.Fatal`로 기동을
  실패시킨다.** 잘못된 값으로 뜬 서버가 부하를 받는 것보다 안 뜨는 편이 낫다.
- **모든 샤드에 같은 값이 주입된다.** 기존 `SetMatchLatencyObserver`가 전 샤드를 순회하는
  패턴을 그대로 따른다. 샤드별 차등은 근거가 없다(§14 R8).
- **기존 무인자 생성자 `NewMatchingEngine()`·`NewShardedEngine(n)`은 테스트용 기본값을
  유지한다.** 약 30곳의 테스트·벤치 호출부를 건드리지 않기 위해서다. 프로덕션 `main`만
  검증된 설정을 주입하는 경로로 정리한다.
- `NewShardedEngine`은 `shardCount < 1`을 조용히 1로 클램프한다. 이 동작은 **바꾸지 않는다** —
  이번 변경 범위 밖이고, quantum 값과 달리 0이 sentinel과 충돌하지도 않는다.
- env 이름 상수는 기존 `EnvGOExchange*` 상수 블록에 나란히 추가한다.

### 2.3 슬롯 진입 가드

`Match` 앞머리의 조기 반환을 슬롯 생성 전으로 옮긴다. 의미가 달라지면 안 되는 지점이다.

| 경로 | slot | `MatchLatencyObserver` | dirty | terminal event / book |
|---|---|---|---|---|
| `order == nil` | 없음 | **호출 안 함** | 없음 | 없음 |
| LIMIT `Amount <= 0` | 없음 | **1회** | **없음** | 없음 |
| MARKET SELL `Amount <= 0` | 없음 | **1회** | **없음** | `MarketOrderDone` 없음 |
| 알 수 없는 Side | 없음 | **1회** | **없음** | 없음 |
| MARKET BUY | 생성 | 마지막 조각 1회 | 규칙대로 | 규칙대로 |
| 정상 주문 | 생성 | 마지막 조각 1회 | 규칙대로 | 규칙대로 |

`finishOrder`에 도달하지 않는 즉시 완료 주문도 observer를 정확히 1회 호출한다. 현재
`processOrder`가 그렇게 동작하므로, 이 표는 기존 동작의 보존이다.

### 2.4 dirty·observer 규칙 — 의도된 동작 변경

새 규칙: **book이 실제로 바뀐 조각에서만 `markDirty`.**

| 시점 | 동작 |
|---|---|
| 조각에서 `trades >= 1` | 조각 반환 전 `markDirty(symbol)` |
| `finishOrder`가 지정가 잔량을 `AddOrder` | `markDirty(symbol)` |
| 그 외 | dirty 아님 |
| `MatchLatencyObserver` | `finishOrder`와 같은 조각에서만 1회 |

**조각마다 dirty가 필요한 이유:** 현재 `processOrder`는 `Match` **완료 후** `markDirty`를
부른다. 조각화만 하고 이 규칙을 안 넣으면, sweep 중간에 도는 ticker가 그 심볼을 dirty로 보지
못해 **캐시와 WebSocket 스냅샷이 sweep 내내 멈춘다.**

**의도된 변경:** 현재는 `Match` 뒤 무조건 `markDirty`이므로 no-op·잘못된 side·0수량 주문과
**체결 없는 시장가**도 dirty를 만들고, 티커가 동일한 스냅샷을 재생성·브로드캐스트한다.
새 규칙에서는 그 낭비가 사라진다. book이 바뀌는 다른 경로(`processCancel`의 `Removed`)는
자체적으로 dirty를 찍으므로 누락이 생기지 않는다.

### 2.5 계측 의미 이동 — 전후 비교 전에 고정

`MatchLatencyObserver`의 정의(`EnqueuedAt` → 처리 완료)는 그대로지만, 값에 yield 대기가
포함되기 시작한다. **대형 sweep의 match latency는 조각화 후 반드시 증가한다.** 이것은 회귀가
아니라 설계된 교환(취소 기아 제거의 대가)이다. baseline 문서에 "sweep 크기별로 나눠 비교하고
전체 평균으로 비교하지 않는다"를 명시한다. 이 문장을 미리 적어두지 않으면 튜닝 단계에서
이 지표를 되돌리려는 압력이 생긴다.

### 2.6 취소가 조각 사이에 끼어들 때

새 케이스가 생기지 않는다는 것이 요점이다.

- **resting maker 취소** → book에서 제거되므로 이후 조각이 건드릴 수 없다. 부분 체결 후
  취소면 `trade(부분) → cancelled(잔량)` 순서인데, 기존의 "부분 체결된 주문 취소"와 같은 경로다.
- **active sweep 주문 자신의 취소** → 그 주문은 book에 없으므로 not-found. §5의 B′ 경로.
- **새 주문 추월** → `OrderCh`를 안 읽으므로 불가능하다. 조각화가 price-time priority를 깨지
  않는 이유가 이것이다.

### 2.7 제외한 것

`activeSweep`에 커서·레벨 포인터를 캐싱하지 않는다. 매 조각이 `bestMatchable*`로 상대를 다시
찾는 O(log n) 재탐색을 하지만, **그것이 조각 사이의 취소를 반영하는 유일하게 안전한 방법**이다.
캐싱하면 취소로 사라진 레벨을 가리킬 수 있다. 재탐색 비용이 실측에서 문제가 되면 그때 다룬다.

---

## 3. 스케줄러 상태 기계

### 3.1 지속 상태

```go
activeSweep          *activeSweep        // nil = idle
shuttingDown         bool
cancelsSinceProgress int                 // progress 이벤트에서만 reset
pendingCancel        *CancelOrderCommand // latch됨, 아직 미처리
pendingOrder         *Order              // latch됨, 아직 미admit
tickerDue            bool                // latch됨, 아직 미flush
```

**blocking select는 작업을 직접 실행하지 않는다.** latch만 하고 turn을 끝낸다. latch된 작업은
**다음 계측 turn에서** 처리된다. 이렇게 하면 모든 실제 작업이 계측 구간 안에서 일어나고,
`turn_duration`에 블로킹 대기 시간이 섞이지 않는다.

### 3.2 turn

```
turn:
  ── turn 계측 시작 ─────────────────────────────────────────
  0. hadActive := (activeSweep != nil)   // turn 시작 시점의 상태를 고정한다

  1. cancel phase
       for cancelsSinceProgress < maxConsecutiveCancels:
           cmd := takeCancel()          // pendingCancel 우선, 없으면 CancelCh 논블로킹
           if cmd == nil: break
           processCancel(cmd)
           cancelsSinceProgress++

  2. ticker phase
       if tickerDue || 논블로킹 recv ticker.C:
           tickerDue = false
           flushSnapshots()

  3. slice phase
       if activeSweep != nil:
           trades, done := matchSlice(sweep.book, sweep.order, maxMatchesPerTurn)
           sweep.trades += trades
           if trades >= 1:  markDirty(symbol)
           if done:         finishOrder(...); observer 1회; activeSweep = nil
           else:            yields++
           progress(P-a)                                    // reset

  4. stop latch
       if !shuttingDown: 논블로킹 recv stopCh → shuttingDown = true

  5. admission phase — hadActive면 건너뛴다
       if !hadActive:                                       // ← turn 시작 상태로 판단한다
           if pendingOrder != nil:                          // 이미 접수된 작업 — 게이트 없음
               admit(pendingOrder); pendingOrder = nil
               progress(P-b)
           else if !shuttingDown && emitBackpressured():
               progress(P-d)                                // 신규 유입만 억제
           else:
               order := 논블로킹 recv OrderCh
               if order != nil: admit(order); progress(P-b)
               else:            progress(P-c)               // 기아 대상 부재
  ── turn 계측 종료 (turn_duration_seconds) ─────────────────

  6. if activeSweep != nil || pendingCancel != nil || pendingOrder != nil || tickerDue:
         continue

  7. if shuttingDown:
         if latchOneNonBlocking(): continue   // CancelCh 또는 OrderCh에서 1건 latch
         // activeSweep == nil이고 두 채널 논블로킹 recv가 모두 비었다 → drain 완료
         flushSnapshots(); close(ExecutionCh); close(SnapshotCh); return

  8. blocking select — 실행하지 않고 latch만 한다
         case cmd := <-CancelCh : pendingCancel = &cmd
         case ord := <-orderCh  : pendingOrder = ord   // emitBackpressured()면 orderCh = nil
         case <-ticker.C        : tickerDue = true
         case <-stopCh          : shuttingDown = true
     continue
```

### 3.3 progress 이벤트와 wedge 방지

`cancelsSinceProgress`는 phase-local 변수가 아니라 **엔진 지속 상태**이고, 아래 네 이벤트에서만
0으로 reset된다.

| 이벤트 | 조건 | 왜 progress인가 |
|---|---|---|
| P-a | 조각을 1회 실행 | 진행 중 작업이 전진했다 |
| P-b | 주문 1건 admit | 신규 주문이 전진했다 |
| P-c | `activeSweep == nil`이고 admit할 주문 없음 | 굶을 대상이 없다 |
| P-d | `activeSweep == nil`이고 `emitBackpressured()` | 유입 억제는 의도된 동작이다 |

**불변식: 모든 turn은 P-a~P-d 중 정확히 하나를 발생시킨다.**

이 "정확히 하나"를 지키려면 **5단계가 3단계의 결과가 아니라 turn 시작 시점의 상태를 보고
판단해야 한다.** 0단계에서 `hadActive`를 고정하는 이유가 이것이다. `activeSweep != nil`을
5단계에서 다시 읽으면 이런 turn이 생긴다.

```
turn 시작: activeSweep != nil
3단계    : 마지막 조각이 끝나 activeSweep = nil, P-a 발생
5단계    : activeSweep == nil을 보고 admission 실행 → P-b/P-c/P-d 추가 발생
결과     : 한 turn에 progress 두 개
```

기능적으로는 counter가 0으로 두 번 리셋될 뿐이라 당장 눈에 띄지 않는다. 그러나 불변식이
"정확히 하나"가 아니게 되면 **SCH-5가 검증할 대상이 사라지고**, 이후 누군가 5단계를 조건부로
바꿀 때 wedge를 막아주던 근거가 없어진다. 또 sweep이 끝난 turn만 취소 상한을 두 배로 얻는
비대칭도 생긴다.

`hadActive`가 true인 turn은 admission을 건너뛴다. 그 sweep이 끝난 주문의 다음 작업은 **다음
turn의 5단계**에서 받는다. 한 turn 늦어지지만, 그 turn은 어차피 조각을 실행했으므로 진행이
멈추지 않는다.

3단계에서 슬롯이 있으면 P-a, 없으면 5단계가 P-b/P-c/P-d 중 하나를 반드시 발생시킨다. 이
불변식이 깨지면 counter가 상한에 고정되어 취소를 영원히 처리하지 못하는 **wedge**가 생긴다.
그래서 이것을 테스트로 고정한다 (SCH-5).

P-d를 progress로 두는 것이 옳은 이유: `emitBackpressured()`는 원래 **하류 포화 시 신규 주문
유입을 억제해 취소 emit 헤드룸을 확보**하는 장치다. 그 상태에서 취소를 상한 없이 처리하는 것은
버그가 아니라 원래 의도다. 굶는 주체가 없으므로 counter를 유지할 이유도 없다.

### 3.3.1 latch된 주문은 backpressure 게이트를 통과하지 않는다

5단계에서 `pendingOrder` 처리가 `emitBackpressured()` 검사보다 **먼저** 와야 한다. 순서가
뒤집히면 다음 시퀀스에서 무기한 park가 생긴다.

```
turn N   : 8단계 blocking select가 OrderCh를 읽어 pendingOrder에 latch
           (이 시점에는 게이트를 통과했다)
turn N+1 : 1단계 cancel phase가 cancellation을 emit해 ExecutionCh를 watermark 위로 올림
           5단계가 게이트를 먼저 보면 → P-d, pendingOrder는 그대로 남음
           6단계가 pendingOrder != nil을 보고 continue
turn N+2…: 같은 상태 반복 → busy loop, 하류가 안 풀리면 무기한 park
```

원칙은 §3.5의 shutdown drain과 같다. **`emitBackpressured()`는 `OrderCh`에서의 신규 유입만
억제하는 장치이고, 이미 엔진 안으로 들어온 작업에는 적용되지 않는다.** 이 구분을 놓치면
"숨은 active 주문을 무기한 park한다"는 이유로 기각했던 후보 B의 실패 모드가 latch 경로로
되살아난다.

부수 효과로 6단계의 `pendingOrder != nil` 조건은 5단계가 항상 소비하므로 정상 경로에서는
참이 되지 않는다. 그래도 남겨둔다 — 이 조건이 참인 채로 turn이 끝나면 그것이 곧 이 결함의
신호이므로, 조건을 지우면 회귀가 조용히 통과한다.

### 3.4 진행성 계약

**계약을 enqueue 기준으로 쓰지 않는다.** Go 채널의 ready 관측은 enqueue와 동시가 아니다.
1단계 도중 enqueue된 주문은 다음 turn의 5단계까지 보이지 않을 수 있다. 따라서:

> ❌ "주문이 enqueue된 시점부터 최대 `maxConsecutiveCancels`개의 취소만 처리된다"
>
> ✅ **control point(5단계)에서 `OrderCh`가 ready로 관측된 이후, 그 주문이 admit되기 전까지
> 처리되는 취소는 최대 `maxConsecutiveCancels`개다.**

같은 방식으로 좁힌 상한 표:

| 구간 | 스케줄러가 보장하는 상한 |
|---|---|
| cancel phase | 최대 `maxConsecutiveCancels`개의 취소 처리 + cancellation emit |
| sweep slice | 최대 `maxMatchesPerTurn`개의 trade emit |
| 시장가 terminal slice | 위에 더해 `MarketOrderDone` emit 최대 1회 추가 |
| ticker | snapshot 비용 **상한 없음** |
| 각 emit | wall-clock **상한 없음** (`ExecutionCh` send에 timeout 없음) |

스케줄러가 세는 것은 **emit 시도 횟수**뿐이다. 하류가 멈추면 조각 하나도 끝나지 않을 수 있고,
그 구간에서 quantum은 아무것도 보장하지 않는다. 이것은 설계상 수용한 한계이며(§14 R1),
해결은 범위 밖이다.

### 3.5 shutdown 계약

> shutdown drain의 `OrderCh`는 이미 접수된 작업이므로 `emitBackpressured()`로 차단하지 않는다.
> `activeSweep == nil`이고 `OrderCh`·`CancelCh`의 non-blocking receive가 모두 비었을 때 drain
> 완료로 판정한다. 이는 HTTP·hold coordinator·cancel worker가 먼저 종료돼 새 producer가 없다는
> lifecycle을 전제로 하며, 채널 `len()`으로 판정하지 않는다.

근거는 [main.go:395-419](../../../cmd/main.go)와
[cancel_command_lifecycle.go:64-71](../../../cmd/cancel_command_lifecycle.go)이다. 종료 체인은
HTTP `Shutdown` → `holdCoordinator.Shutdown()` → `stopCancelWorkerThenEngine(...)` → `me.Stop()`
순이고, 후자의 루프는 timeout이 나도 `break`하지 않고 다시 기다린다. 따라서 `me.Stop()` 시점의
"새 producer 없음"은 best-effort가 아니라 무조건이다. 또한 outbox writer는 엔진 `Done()`
**이후에** 기다려지므로 drain 내내 살아 있고, `ExecutionCh` 블로킹 send가 해소된다.

**drain 중 새 producer는 지원하지 않는다.** 이 전제가 깨지면 완료 판정이 성립하지 않는다.

`drainPendingWork()`는 삭제하고 7단계로 대체한다. active slot을 모르고 quantum도 걸지 않으므로
재사용할 수 없다.

### 3.6 스케줄러 테스트

| # | 테스트 | 기대 |
|---|---|---|
| SCH-1 | 취소 홍수 + 대기 주문 | 연속 주문 admit 사이의 취소 처리 수 ≤ `maxConsecutiveCancels` |
| SCH-2 | 5,000 maker sweep 중 취소 도착 | 취소가 `maxMatchesPerTurn` 체결 이내에 처리됨 |
| SCH-3 | active sweep 중 신규 주문 도착 | sweep 완료 전에는 admit되지 않음 (추월 없음) |
| SCH-4 | blocking select latch | latch된 작업이 다음 turn에서 처리됨. `turn_duration`에 블로킹 대기 미포함 |
| SCH-5 | **progress 불변식** | 임의 입력 조합에서 모든 turn이 P-a~P-d 중 정확히 1개 발생. `cancelsSinceProgress`가 두 turn 연속 상한에 머무르지 않음 |
| SCH-6 | `emitBackpressured()` 중 취소 홍수 | 취소가 상한 없이 처리됨(P-d), 신규 주문은 억제됨 |
| SCH-7 | **latch된 주문 + cancel phase가 만든 backpressure** | latch된 주문이 같은 turn에 admit됨. park·busy loop 없음 |

**SCH-7의 판별 계약 (§3.3.1 변이를 잡는 테스트).** 결함이 hang이 아니라 **유실과 busy loop**로
나타나므로, 단언도 그 형태여야 한다.

| 구성 | 값 |
|---|---|
| `ExecutionCh` | 작은 용량(16), **소비자 정지**. watermark 바로 아래(11건)까지 미리 채움 |
| latch 대상 | **어떤 것과도 체결되지 않는** 지정가 주문 1건 (emit 0건 — 그래야 올바른 구현도 send에서 막히지 않는다) |
| 트리거 | resting 주문 취소를 여러 건 투입해 cancellation emit으로 watermark를 **넘긴다** |
| 단언 1 | 그 지정가 주문이 deadline(예: 2s) 안에 book에 등록된다 |
| 단언 2 | latch 시점부터 admit까지 관측된 `Turn` 횟수가 상한(예: 100) 미만이다 |

단언 1이 유실을, 단언 2가 busy loop를 잡는다. 게이트 검사를 `pendingOrder` 처리보다 앞으로
되돌리는 변이를 넣으면 **두 단언이 모두 실패**해야 한다. 체결 0건 주문을 쓰는 이유가 중요하다 —
체결이 나는 주문을 쓰면 `ExecutionCh`가 가득 찬 상태에서 올바른 구현도 blocking send에 걸려
정상과 결함을 구분할 수 없다(§10 D-1과 같은 이유).

---

## 4. 계측

### 4.1 노출 방식

`internal/matching`은 지금도 prometheus를 import하지 않고 `MatchLatencyObserver` 콜백으로만
노출한다. 그 규칙을 지켜 옵셔널 func 필드 구조체 하나를 추가한다(nil = 비활성).
기존 `MatchLatencyObserver`는 **손대지 않는다.**

```go
type EngineObservers struct {
	Turn         func(d time.Duration)                     // 1~5단계 소요 (블로킹 대기 제외)
	Slice        func(trades int, emitBlock time.Duration)
	OrderAdmitted func(queueWait time.Duration)            // admit 시각 − Order.EnqueuedAt
	OrderDone    func(trades int)                          // finishOrder 시점의 sweep.trades
	Cancel       func(queueWait time.Duration)
	EmitBlock    func(kind EmitKind, d time.Duration)      // trade | done | cancelled
	Yield        func()
}
```

`OrderAdmitted`와 `OrderDone`을 분리하는 이유: 큐 대기는 **admit 시점**에 확정되고 체결 수는
**완료 시점**에 확정된다. 하나로 묶으면 sweep이 진행되는 내내 queueWait 관측이 지연되어,
sweep 중 큐 상태를 볼 수 없다. 즉시 완료 경로(§2.3)도 `OrderAdmitted`만 발생하고
`OrderDone`은 발생하지 않는다 — 슬롯이 만들어지지 않았으므로 셀 체결이 없고, 여기서
`OrderDone(0)`을 내면 `executions_per_order` 분포가 0으로 오염된다.

**호출 순서 계약 — `Slice`는 `finishOrder` 뒤에 부른다.** `emitBlock` 인자는 그 조각이
`ExecutionCh`에서 블로킹된 **총** 시간이어야 한다. 마지막 조각은 `finishOrder`가
`MarketOrderDone`(시장가)을 emit하므로, `Slice`를 `finishOrder`보다 먼저 부르면 그 블로킹
시간이 `emit_block_per_slice`에서 통째로 빠진다. 하필 그 emit이 하류 포화 시 가장 오래
막히는 지점이므로, 순서가 뒤집히면 측정이 실패를 가장 잘 보여줘야 할 구간에서 침묵한다.

**계측 커밋에서도 `Slice`를 호출해야 한다.** 조각화 이전에는 "조각 = 주문 전체"이므로 `Slice(총 체결 수,
그 주문의 emit 블로킹 누적)`으로 1회 부른다. 계측 커밋이 `Slice`를 안 부르면
`matches_per_slice`·`emit_block_per_slice`의 baseline이 비어 탐색 단계에서 전후 비교가 불가능해진다.

`CancelOrderCommand`에 `EnqueuedAt time.Time`을 추가하고
[engine.go:350 `CancelOrder`](../../../internal/matching/engine.go) 한 곳에서 채운다. 제로값이면
관측을 건너뛴다 — 테스트가 구성한 command가 가짜 지연을 만들지 않게 하기 위해서다.

### 4.2 지표와 bucket

| 지표 | 타입 | 라벨 | bucket |
|---|---|---|---|
| `matching_engine_executions_per_order` | Histogram | — | 1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 4096 |
| `matching_engine_matches_per_slice` | Histogram | — | 1, 2, 4, 8, 16, 32, 64, 128, 256 |
| `matching_engine_turn_duration_seconds` | Histogram | — | `ExponentialBuckets(1e-5, 4, 8)` → 10µs … 164ms |
| `matching_engine_order_queue_wait_seconds` | Histogram | — | `ExponentialBuckets(1e-4, 4, 9)` → 100µs … 6.5s |
| `matching_engine_cancel_queue_wait_seconds` | Histogram | — | `ExponentialBuckets(1e-4, 4, 9)` |
| `matching_engine_quantum_yields_total` | Counter | — | — |
| `matching_engine_emit_block_seconds` | Histogram | `event` | `ExponentialBuckets(1e-6, 5, 9)` → 1µs … 3.9s |
| `matching_engine_emit_block_per_slice_seconds` | Histogram | — | `ExponentialBuckets(1e-6, 5, 9)` |

`matches_per_slice`의 상단 bucket이 `maxMatchesPerTurn` 후보 최대값(128)보다 한 칸 큰 것은
의도적이다. 상한을 넘는 관측이 나오면 그 자체가 버그 신호이므로 bucket에 잡혀야 한다.

이미 있는 `matching_engine_channel_length`·`..._shard_order_channel_length`는 재사용한다.

### 4.3 계측 오버헤드를 먼저 격리한다

계측 커밋은 **행동 변경이 0**이어야 한다. 계측만 넣은 상태에서 기존 `engine_bench_test.go`를 전후
비교하고 **계측 자체의 오버헤드를 먼저 기록**한다. 이 숫자를 따로 잡아두지 않으면 4단계에서
quantum 비용과 계측 비용이 섞여 구분이 불가능해진다.

---

## 5. 취소 의미 변화 (B′)

active sweep 중인 주문은 book에 없으므로 엔진 취소는 not-found를 반환한다. 그런데
**not-found가 즉시 NOOP이 아니다.**
[cancel_command_worker.go:288-310](../../../internal/service/cancel_command_worker.go)의
`applyNotFound`는 DB 주문이 아직 open이면 `scheduleRetry`하고, terminal일 때만 `MarkNoop`한다.
사용자도 not-found를 받지 않는다 — HTTP는 이미 `202 Accepted`를 반환했다.

```
취소 도착 → 엔진 not-found
          → DB 주문 open → command PENDING 유지, 지수 백오프 재시도
          → sweep 완주
             ├ 지정가 잔량이 book에 등록됨 → 다음 retry가 제거, 취소 이벤트 1회
             └ 전량 체결 / 시장가 종결 → DB terminal → MarkNoop, ORDER_RELEASE 없음
```

**기존 durable retry 상태 기계를 그대로 재사용한다. 새 복구 경로를 만들지 않는다.**

백오프는 `nextBackoff` 100ms → 5s ×2 지수다. **이것은 재시도 빈도의 상한일 뿐이고, 시도
횟수와 누적 비용에는 상한이 없다.** sweep이 길어질수록 그 취소의 `RecordAttempt` DB write는
계속 쌓인다(5s 간격으로 수렴). 유계인 것은 초당 재시도 수이지 총량이 아니다.

**대가를 명시한다:** 잔량이 book에 올라간 뒤 worker의 다음 retry 전까지 **추가 체결될 수 있다.**
그리고 조각 사이 취소로 maker가 체결을 회피할 수 있다. 둘 다 의도한 교환이다.

제약: **active taker 취소는 선점하지 않으며, 기존 durable retry가 sweep 이후 결말을 낸다.**

| # | 테스트 | 기대 |
|---|---|---|
| B-1 | active 지정가 sweep 취소 | not-found → PENDING 재시도 → 잔량 book 등록 → 제거·취소 이벤트 정확히 1회 |
| B-2 | active sweep 전량 체결 | not-found → DB terminal → `MarkNoop`, `ORDER_RELEASE` 없음 |
| B-3 | active market 취소 | PENDING 재시도 → 정상 완료 또는 finalizer 종결 뒤 NOOP |
| B-4 | 조각 사이 maker 취소 | maker가 이후 조각에서 체결되지 않음 (의도된 체결 회피) |
| B-5 | 부분 체결된 maker를 조각 사이에 취소 | `trade(부분)` → `cancelled(잔량)` 순서, 홀드 정확히 정산 |

---

## 6. 시장가와 복구 (A′)

- **LIMIT·MARKET 모두** `maxMatchesPerTurn`을 적용한다.
- 재개 슬롯은 심볼별이 아니라 기존 직렬화 단위인 **`MatchingEngine` 샤드당 하나**다.
- 재개 작업을 `OrderCh`에 **다시 넣지 않는다.** 새 주문 추월을 막기 위해서다.
- `MarketOrderDone`은 **마지막 조각에서만 정확히 한 번** 발행한다.
- active market 취소는 B′대로 PENDING 재시도하고, 정상 완료나 재기동 finalizer 종결 뒤 NOOP.
- **크래시 후 sweep을 재개하지 않는다.** 커밋된 체결까지만 인정하고 잔량은 finalizer가 종결한다.
- 시장가는 bootstrap에 들어가지 않는다.
  [`FindOpenOrdersForBootstrap`](../../../internal/repository/order_repository.go)은 LIMIT만
  조회하고, 시장가는 outbox replay 뒤 `StaleMarketOrderFinalizer`가 먼저 종결한다.
- **크래시 노출 시간이 늘어난다는 점을 명시한다.** 다만 새로운 복구 상태 기계는 필요 없다.
- `ExecutionCh`가 막히면 조각 하나도 끝나지 않을 수 있으므로 wall-clock 진행성은 보장하지 않는다.

| # | 테스트 | 기대 |
|---|---|---|
| A-1 | MARKET BUY/SELL을 `quantum=1`로, 개입 없이 | 기존과 동일한 체결열, `done` 1회 |
| A-2 | 조각 사이 크래시, trade outbox 커밋 전 | 해당 체결 소멸, 홀드 정확히 복구 |
| A-3 | 조각 사이 크래시, 일부 trade outbox 커밋 후 | replay 후 partial 기준으로 finalizer가 잔량 정확히 해제 |
| A-4 | 재기동 | sweep 재개 없음. 지정가만 bootstrap, 시장가는 finalizer가 종결 |
| A-5 | graceful shutdown 중 active market sweep | sweep 완주 + `MarketOrderDone` 발행 후 엔진 종료 |

---

## 7. 섹션 2·3 경계 테스트

| # | 테스트 | 기대 |
|---|---|---|
| S2-1 | `budget=1`, 첫 체결로 전량 체결 | 첫 slice `done=true`, `trades=1`, 빈 slice 없음 |
| S2-2 | `budget=1`, 마지막 maker 소진 + taker 잔량 존재 | slice1 `done=false`, slice2 `done=true` |
| S2-3 | 시장가 sweep을 여러 조각으로 | `MarketOrderDone` 정확히 1회, `finishOrder` 1회 |
| S2-4 | public `Match()` (budget=0) 회귀 | 체결·이벤트 순서가 기존과 **의미적으로 동일** |
| S2-5 | `strictPositiveEnv` 계약표(§2.2.1) 전 행 | **미설정 → default**, 빈 문자열·`" 3"`·`"+3"`·`"0"`·`"-1"`·오버플로 → **error**. 기본값 fallback 없음 |
| S2-7 | `LoadQuantumConfig()` 실패 시 `main` 기동 | 기동 실패. 기본값으로 뜨지 않음 |
| S2-8 | `NewShardedEngineWithQuantum(4, cfg)` | 4개 샤드 전부에 동일한 두 값이 주입됨 |
| S2-6 | 즉시 완료 4경로 | observer 1회(nil 제외 3경로), book 무변화, terminal event 없음, **dirty 없음** |

**S2-4의 "의미적으로 동일"의 정의:** 이벤트 종류·순서, 가격, 수량, 매수/매도 주문 ID의 시퀀스가
동일하다. **동적 필드는 비교에서 제외한다** — trade ID, 생성 시각, `tradeSeq`, engine ID는
실행마다 달라지므로 바이트 비교는 성립하지 않는다. 비교 대상 필드를 테스트에 명시적으로
열거하고, 새 필드가 추가되면 컴파일이 깨지도록 구조체 리터럴로 비교한다.

---

## 8. 목적형 로컬 하니스

### 8.1 왜 Go 테스트인가

k6는 HTTP·DB를 함께 태우므로 엔진 스케줄러 신호가 묻힌다. 여기서 필요한 것은 엔진 goroutine
하나의 지연 분포다. 그래서 `internal/matching/quantum_harness_test.go`로 구현한다.

**하니스는 quantum 구현이 아니라 계측 커밋의 산출물이다.** baseline은 행동이
바뀌기 전 코드에서, 그리고 나중에 후보를 잴 때와 **같은 하니스 코드로** 나와야 한다. 하니스를
quantum 구현 이후에 쓰면 baseline과 후보 측정이 서로 다른 하니스 버전에서 나오게 되고, 전후 차이에
하니스 변경분이 섞인다.

**opt-in 빌드 태그로 격리한다.** 파일 머리에 `//go:build quantumharness`를 두고
`go test -tags quantumharness`로만 실행한다. 타이밍 진단은 CI 러너의 부하에 좌우되어
플레이키하므로, **일반 `go test`와 CI에서는 실행되지 않아야 한다.**

공통 골격: 실제 `MatchingEngine`을 `Start()`하고, `ExecutionCh` 소비자를 속도 조절 가능한
goroutine으로 붙이고, 관측 콜백으로 샘플을 수집한 뒤 분위수를 낸다.

### 8.2 시나리오

**시나리오는 다섯 개뿐이다.** 각각이 판정표의 특정 기준 하나를 먹인다. 그 대응이 없는
시나리오는 측정 비용만 늘린다.

| 시나리오 | 구성 | 1차 지표 | 먹이는 기준 |
|---|---|---|---|
| **H0 무취소 control** | H1과 주문 수·producer·consumer가 동일하고 **취소율만 0** | `order_queue_wait` p99 | C2의 기준값 |
| **H1 cancel flood** | 대기 주문 N개 + 취소 C건 유입 | `order_queue_wait` p99 | C2 |
| **H2-1 작은 sweep control** | maker 1건 소진 taker, sweep 중 취소 1건 | `cancel_queue_wait` p99 | C1의 기준값 |
| **H2-5000 큰 sweep** | maker 5,000건 소진 taker, **첫 trade 관측 뒤** 취소 1건(§8.6) | `cancel_queue_wait` p99 | C1 |
| **H4 snapshot freshness** | H2-5000과 같은 부하에서 `SnapshotCh` 수신 간격 | 최대 간격 | C4 |

**제외한 것.** 이전 초안의 H3(backpressure)과 H5(혼합)는 뺐다. 둘 다 판정표의 어떤 기준도
먹이지 않고 관찰용이었다. `emit_block_seconds`는 실행 중 항상 수집되므로 전용 시나리오 없이도
보인다. 혼합 부하는 GCP 500 VU 회귀 게이트가 대신한다.

**H0을 별도 시나리오로 세우는 이유.** C2의 상한은 "취소가 없을 때의 주문 지연"을 기준으로
계산된다. 그 control을 H1과 다른 구성에서 재면 비교가 성립하지 않는다 — 상한이 실제보다
느슨하거나 촘촘해지고, 그 차이가 어디서 왔는지 사후에 분리할 수 없다. **H0은 H1에서 취소율만
0으로 바꾼 것이어야 하며, 다른 파라미터를 공유해야 한다.**

**H2-1이 필요한 이유도 같다.** C1의 상한은 "sweep이 작을 때의 취소 지연"에서 나온다.

### 8.3 watchdog·censored·measurement_invalid

**두 실패를 합치지 않는다.** 원인이 다르고 대응도 다르다.

| 구분 | 조건 | 처리 |
|---|---|---|
| **censored** | watchdog 제한 시간 초과 | 표본을 버리지 않고 개수를 보고. 후보는 C5로 탈락 |
| **measurement_invalid** | 작업량·필드·시간값 **계약 위반** | 그 run-set 전체를 즉시 실패시킨다 |

`measurement_invalid` 판정 조건(H2-5000):

- 실제 체결 수 != 5,000
- taker 잔량 != 0
- 관측 yield != `ExpectedYields(5000, maxMatchesPerTurn)`

작업량이 흔들리면 그 회차로는 어떤 것도 주장할 수 없다. 무작위 UserID로 자기 주문이
1~6건 제외되던 결함이 실제로 그 상태였다(§8.4).

baseline의 censored는 "고치려는 문제의 크기"이므로 보존·보고한다. `measurement_invalid`는
그런 성격이 아니다 — 측정 자체가 성립하지 않았다는 뜻이므로 집계 전에 실패시킨다.

### 8.4 로컬 wall-clock C3 종료 — `LOCAL_NOT_MEASURABLE`

**측정 도구가 C3의 ±5% 차이를 구분하지 못한다는 것이 세 번의 서로 다른 방식으로 확인됐다.**
로컬 시간 기반 C3를 여기서 종료한다.

| 방식 | 표본 | 최대 편차 | 기준 |
|---|---|---|---|
| 단일 sweep (5회 중앙값) | 20회 조합 15,504개 | **±9~11%** | 2.5% |
| 묶음 64 sweep (5쌍 비율) | 10 묶음 | **32.55%** | 2.5% |
| 묶음 128 sweep (5쌍 비율) | 10 묶음 | **13.34%** | 2.5% |

단일 sweep 진단(작업량을 5,000으로 완전히 고정한 20회)의 조합 중앙값 분포:

| k | 조합 수 | p05 | p50 | p95 | lower_error | upper_error |
|---|---|---|---|---|---|---|
| 3 | 1,140 | 4.020ms | 4.505ms | 5.634ms | 10.76% | 25.07% |
| 5 | 15,504 | 4.099ms | 4.505ms | 5.000ms | 9.00% | 11.01% |

**제거한 결함과 남은 문제를 구분한다.**

- 제거됨: 작업량 불일치(무작위 UserID로 자기 주문이 1~6건 제외되던 것), `0ns` 기록
- 남음: **실행 간 시간 변동**

**변동의 근본 원인은 미확정이다.** 메모리 정리·운영체제 스케줄링 등 후보를 분리하지 못했다.
"노트북 드리프트"로 단정하지 않는다.

**따라서 로컬 처리량 회귀는 판정하지 못했다.** 이 문서 어디에도 "C3를 통과했다"고 쓰지 않는다.

이전 `baseline`/`explore`/`confirm`/`precision` 산출물은 **파일만 보존**하고 어떤 선택에도
쓰지 않는다. `_workspace/quantum/precision/`은 실패 증거로 남긴다.

**이전에 보고된 "취소 지연 5.11ms → 0s"는 정확한 성능 비교값으로 사용하지 않는다.**
그 자료는 위 정밀도 문제를 안고 있다. "취소가 조각 사이에서 처리될 수 있다"는 **의미적
성질**만 스케줄러 테스트로 주장한다.

### 8.5 대체 계약 — 결정적 계수

시간 대신 **시계와 무관하게 정확히 셀 수 있는 값**으로 조각화의 구조적 비용을 고정한다.

```
slices         = ceil(trades / maxMatchesPerTurn)   (budget <= 0이면 1)
expectedYields = slices - 1
```

5,000 체결 기준 격자 값:

| 설정 | slices | yields | yields/trades |
|---|---|---|---|
| m16 | 313 | 312 | 6.24% |
| m64 | 79 | 78 | 1.56% |
| **m128** | **40** | **39** | **0.78%** |

**이 값은 처리량 손실률이 아니다.** 다음 두 가지만 뜻한다.

1. sweep 하나를 몇 조각으로 나눴는가
2. 그 때문에 스케줄러 제어점으로 몇 번 더 돌아왔는가

focused 엔진 테스트에서 **관측 yield가 이 계산값과 정확히 같은지** 확인한다
(`TestSelectedConfigYieldsMatchExpectation`).

취소 쪽 결정적 계약은 기존 그대로다 — `maxConsecutiveCancels=8`이면 progress 사이 최대 8건,
32면 최대 32건(`TestCancelBoundIsDeterministicPerConfig`).

### 8.6 C1 검증용 H2-5000 시나리오

**순서가 계약이다.** taker 완료를 먼저 기다린 뒤 취소하면 그것은 sweep 중 취소가 아니다.

```
taker 제출
→ 첫 trade 관측          (sweep이 실제로 진행 중임을 확인)
→ victim 취소 제출
→ 취소 완료와 taker 완료를 모두 대기
```

maker·taker·victim은 서로 다른 고정 UserID를 쓰고, 실제 `trades == 5000`과 `잔량 == 0`을
단언한다.

측정값이 시계 해상도(이 머신 ~645µs) 이하로 나오면 **"0초"라고 단정하지 않고 "측정 해상도
이하"로 기록한다.**

### 8.7 산출물

`_workspace/quantum/`의 기존 디렉터리는 전부 보존한다. 새 선택에는 쓰지 않는다.

---

## 9. 후보 선택과 판정표

### 9.1 선택 규칙 — 시간 측정값을 순위에 넣지 않는다

사전 등록된 순위를 그대로 유지한다.

1. `maxMatchesPerTurn`이 **큰** 값 우선
2. 같으면 `maxConsecutiveCancels`가 **작은** 값 우선

**시간 측정값은 순위에 넣지 않는다.** 로컬 wall-clock C3가 판정 불가이므로 순위를 바꿀 근거가
없다.

따라서 격자 `{16, 64, 128} × {8, 32}`의 결정적 1순위는 **`m128-c8`** 이다.

**"가장 빠른 값"이라는 뜻이 아니다.** 격자 중

- 추가 yield가 가장 적고 (5,000 체결당 39회, 0.78%)
- progress 전 연속 취소 상한이 가장 작다 (8건)

는 뜻이다. 실제 처리량은 이 순위로 증명되지 않는다.

### 9.2 판정표

| # | 기준 | 상태 | 근거 |
|---|---|---|---|
| C1 | sweep 중 취소 지연 ≤ 300ms | **통과** | 선택값 focused 측정(§8.6 순서) |
| C2 | cancel flood 중 주문 지연 ≤ 300ms | **통과** | 선택값 focused 측정 |
| **C3** | 처리량 보존 ±5% | **`LOCAL_NOT_MEASURABLE`** | §8.4. 최종 통합 GCP까지 보류 |
| C4 | 스냅샷 최대 간격 ≤ 300ms | **통과** | 선택값 focused 측정 |
| C5 | censored 부재 | **통과** | 선택값 측정에서 표본 누락 0 |
| C6 | 의미 교차 검증 | **통과** | B′ 의미 테스트 + 스케줄러 계약 |
| C7 | yield 비용 | **관찰** | 결정적 계수로 대체(§8.5) |

C1·C2·C4는 **후보 간 미세 비교가 아니라 안전 상한 확인**이다. 로컬 시간으로 후보를 가르는
시도는 종료됐으므로, 선택값 하나만 focused로 확인한다.

선택값이 하나라도 실패하면 다음 후보로 자동 이동하지 않고 **중단·보고**한다.

### 9.3 최종 판정 문구

- 취소 기아 방지 구조: **검증됨**
- `m128-c8` 결정적 스케줄링 계약: **검증됨** (yield 39, 취소 상한 8)
- 로컬 ±5% 처리량 보존: **판정 불가**
- 실제 처리량·500 VU 회귀: **최종 통합 GCP까지 보류**
- 이전 "5.11ms → 0s": **정확한 성능 비교값으로 사용하지 않음**
- GCP: **지금 실행하지 않음**

---

## 10. shutdown 테스트

| # | 테스트 | 기대 |
|---|---|---|
| D-1 | `ExecutionCh`를 high-watermark로 채우고 주문·취소를 큐에 남긴 뒤 `Stop()`. **소비자가 뒤이어 watermark를 해소한다** | 큐에 있던 주문과 취소가 **둘 다 처리**되고, 그 뒤 `ExecutionCh`·`SnapshotCh`가 닫힌다 |
| D-2 | 진행 중 지정가 sweep 도중 `Stop()` | sweep 완주 후 종료. 선점 없음 |
| D-3 | 진행 중 시장가 sweep 도중 `Stop()` | sweep 완주 + `MarketOrderDone` 발행 후 종료 (A-5와 동일 계약) |
| D-4 | **`Stop()` 전에 이미 접수된** 취소가 drain 중 처리됨 | 같은 quantum 스케줄러로 처리되고 취소 이벤트 발행 |

**소비자를 반드시 포함해야 하는 이유:** `ExecutionCh`가 가득 찬 채 소비자가 없으면 **올바른
구현도 블로킹 send에서 멈춘다.** 그 상태로는 정상과 버그를 구분할 수 없다. 소비자가 watermark를
해소해야 테스트가 실제 계약을 검증한다.

**D-1이 게이트 오적용을 잡는 방식은 deadlock이 아니라 주문 유실이다.** 7단계에
`emitBackpressured()` 게이트가 잘못 걸리면, `OrderCh` 수신이 "비었음"으로 관측되는 것이 아니라
**건너뛰어진다.** 그러면 엔진이 `OrderCh`에 주문을 남긴 채 drain 완료로 판정하고 채널을 닫는다.
D-1의 "큐에 있던 주문이 처리됐다" 단언이 그 시점에 실패한다.

**drain 중 새 producer는 지원하지 않는다.** D-4를 "drain 중 새로 도착한 취소"로 쓰면 §3.5의
lifecycle 전제와 모순되므로 그렇게 쓰지 않는다.

---

## 11. GCP 500 VU 회귀 게이트

### 11.1 범위

**용도는 회귀 확인 1회뿐이다. 값 탐색이 아니다.** 값은 §9에서 이미 확정된 상태로 들어간다.

- 조건: 기존 4 VM, 기존 토폴로지·machine type, ramp 30s + hold 10m, 500 VU **1회**
- 신규 VM·방화벽·IAM·machine type 변경 금지
- 750 VU, 탐색 실행, 결과 확인용 추가 반복 금지
- 실행이 무효가 되더라도 자동 재실행하지 않고 VM을 정지한 뒤 보고
- **이 실행에는 사용자의 별도 유료 승인이 필요하다.** §9까지의 승인은 이 실행을 포함하지 않는다.

### 11.2 preflight (전부 통과해야 부하 시작)

| 항목 | 기준 |
|---|---|
| concurrency 값 | 3곳 일치 (8) |
| dev token | 인증 200 + 구토큰 403. **값·hash·fingerprint를 출력하지 않는다** |
| load-gen linger | `enable-linger` 완료 |
| SSH 경로 | server·db는 IAP, load-gen은 집 IP 직접 SSH |
| 기준 SHA | 계획된 커밋과 일치 |
| **quantum 값 적용 증거** | 아래 세 곳이 **모두** §9에서 선택된 값과 일치 |

**quantum 값 적용 증거를 preflight 항목으로 둔다.** 값이 실제로 서버에 적용됐는지 확인하지
않으면, 회귀 게이트가 검증하는 것이 무엇인지 알 수 없다. 기본값으로 뜬 서버를 측정하고 선택된
값을 측정했다고 기록하는 것이 가장 흔한 실패 방식이다. 세 곳을 본다.

선택된 두 값은 **측정용 compose override에 명시적으로 넣는다. 미설정 기본값에 의존하지 않는다** —
기본값에 기대면 코드가 바뀌었을 때 측정이 조용히 다른 값으로 돈다.

1. `docker compose config` 출력의 `GOEXCHANGE_MATCHING_MAX_*`
2. 실행 중 컨테이너의 실제 환경 (`docker compose exec ... env | grep MATCHING`)
3. 서버 기동 로그의 `matching engine sharded: ... maxMatchesPerTurn=N maxConsecutiveCancels=M`

셋 중 하나라도 어긋나면 **preflight 실패**다.

**preflight 실패 = 부하 실행 금지.**

### 11.3 통과 기준 — 36번의 정확한 값

정합성·가용성은 **36번과 동일한 0/100% 기준**을 그대로 쓴다. 허용폭을 새로 만들지 않는다.

**k6 측 (load-gen A·B 각각)**

| 항목 | 기준 |
|---|---|
| 주문 응답 가용성 (hold) | **100.00%**, fail **0** |
| 주문 업무 성공 (hold) | **100.00%**, fail **0** |
| 1초 계약 초과 | **0건** |
| HTTP 실패 | **0** |
| k6 checks 실패 | **0** |
| 취소 성공률 | **100.00%** |
| 멱등성 계약 위반 (400·409) | **0** |
| 202 PENDING 응답 | **0** |

**서버·DB 측**

| 항목 | 기준 |
|---|---|
| `failed_settlements` | **0** |
| `failed_market_completions` | **0** |
| `failed_order_cancellations` | **0** |
| `reconciliation_violations` | **0** |
| 주문 수 = 멱등성 키 수 = `ORDER_HOLD` 건수 = k6 iteration 합계 | **완전 일치** |
| outcome 분포 | `ACCEPTED` 100%, PENDING·REJECTED·UNKNOWN **0건** |
| 키 1건이 여러 주문을 가리킴 | **0** |
| 주문 1건을 여러 키가 가리킴 | **0** |
| 주문 1건에 hold 2건 이상 | **0** |
| 키 없는 주문 | **0** |
| stale PENDING (임계 5분 초과) | **0** |
| `POST /orders` 상태 분포 | 200만 (400·409·503·202 **0건**) |

**이번 변경 고유 항목**

| 항목 | 기준 |
|---|---|
| 신규 지표 8종 — metric family 존재 | 8종 전부 `/metrics`에 노출 |
| 신규 지표 — 표본 수 | `turn_duration`·`order_queue_wait`·`executions_per_order`·`matches_per_slice`·`emit_block{event="trade"}`·**`emit_block_per_slice_seconds`** 의 `_count` > 0 |
| `emit_block_per_slice_seconds` | 주문 slice가 존재하는 한 **`_count` 0은 배선 실패다.** `_sum`이 0인 것은 허용한다 — 하류가 한 번도 막히지 않았을 수 있다 |
| 신규 지표 — **0이어도 정상인 것** | `quantum_yields_total`, `cancel_queue_wait_count`, `emit_block{event="done"}`의 0은 **실패가 아니다**. 워크로드에 시장가·취소·yield가 없었을 수 있으므로, 0이면 그 사실을 워크로드 구성으로 설명해 보고서에 적는다 |
| p95 | **참고값으로만 기록.** quantum 효과로 귀속하지 않는다 (사전 등록된 정량 게이트가 없다 — 36번 §8과 동일한 이유) |

### 11.4 산출물 시크릿 게이트 — 필수 중단 조건

[runbook §7.5](../../gcp-stress-test-runbook.md)의 7단계를 **그대로** 따른다. 어느 단계든
실패하면 **작업 중단**이며, 스스로 완화하거나 우회하지 않는다.

- summary 원본은 원격 VM 또는 OneDrive 밖 임시 경로에만 생성
- `setup_data`를 redaction metadata로 치환
- metrics가 정리 전후 동일한지 검증 → 불일치 시 **중단**
- JWT·`"token"`·Bearer·Authorization·`GOEXCHANGE_JWT_SECRET` 패턴 스캔
- **히트가 하나라도 있으면 packaging·복사 중단**
- 통과한 파일만 tgz + checksum
- 그 이후에만 `_workspace/`·`_artifacts/`로 복사

스캔 보고는 **파일별 hit 개수와 종류만** 남긴다. 값·해시·fingerprint를 출력하지 않는다.
발견해도 즉시 수정·삭제하지 않고 먼저 보고한다. 기존 checksum과 파일은 검사 단계에서
변경하지 않는다.

### 11.5 종료

VM 4대를 정지하고, **`gcloud`로 4대가 TERMINATED로 조회된 결과**로 종료를 판정한다. 정지 명령을
실행했다는 사실은 판정 근거가 아니다. VM·디스크를 삭제하지 않는다.

결과는 `docs/benchmarks/37-YYYY-MM-DD-matching-quantum.md`에 기록한다.

---

## 12. NOT in scope

- 625 VU 해석, 잔여 growth 분석, 750 VU 재확인 — 정확성 단계 종료까지 보류
- `ExecutionCh` send timeout·bounded send — 진행성의 실제 한계지만 별건
- `OrderCh` 재삽입, 우선순위 큐, 심볼별 슬롯, 샤드 재배치
- 크래시 후 sweep 재개 — A′에서 명시적 제외
- `orderIntakeHighWatermarkRatio` 히스테리시스·env화 (④ 항목)
- 스냅샷 depth·interval 튜닝
- B 잔여 4·5·6 (경로별 DB timeout, readiness, shutdown drain)
- 기존 E2E 문구 실패 1건 (`ETH 주문 가능` vs `ETH available`) — 별도 부채 유지
- `docs/benchmarks/README.md` 19~35번 목록 공백

---

## 13. 구현 체크포인트

**체크포인트는 두 개다. P1만 작업을 중단한다. P2는 구현 중 바로 정리한다.**

| CP | 범위 | 승인 |
|---|---|---|
| **A. 로컬 완료** | 계측·하니스·baseline → quantum 구현 → 로컬 탐색·값 선택 → 전체 로컬 검증·CI | **중간 승인 없이 연속 진행.** 측정 무효화·정확성 훼손 P1에서만 즉시 중단 |
| **B. GCP 완료** | 500 VU 1회 → VM 정지·TERMINATED 조회 → 분석 → 보고서·문서 | **별도 유료 승인 후에만 시작** |

CP A 안에서도 **계측·하니스 커밋과 quantum 구현 커밋은 분리한다.** 같은 커밋에 계측과 행동
변경이 섞이면 이후 모든 전후 비교의 기준선이 "이미 quantum이 들어간 상태"가 된다. baseline은
**행동이 바뀌기 전 SHA에서** 나와야 한다. 하니스도 그 커밋에 함께 들어가야 baseline과 후보
측정이 같은 하니스 코드에서 나온다(§8.1).

**탐색 단계는 baseline을 다시 재지 않는다.** 보존한 JSON을 읽어 판정식의 기준값으로 쓴다.
재수집하면 그것은 이미 quantum이 들어간 코드에서 나온 값이므로 baseline이 아니다.

### 13.1 테스트 실행 정책

- 작업 중에는 **focused test만** 실행한다. 해당 코드를 작성할 때만이다.
- 로컬 구현이 **전부 끝난 뒤 정확히 한 번**:

```
go test -count=1 ./...
go test -count=1 -race ./internal/matching ./internal/service
go vet ./...
go build -trimpath ./cmd
```

- `-tags quantumharness`는 baseline 수집과 후보 측정에서만 실행한다. 일반 `go test`와 CI에
  포함하지 않는다.
- **mutation 검증은 하지 않는다.** 파일을 변이했다 복구하는 절차 자체가 과거에 미커밋 작업을
  날린 사고의 원인이었고, 여기서 얻는 확신에 비해 비용과 위험이 크다. 대신 각 테스트가 계약을
  **직접 단언**하도록 쓴다.
- 커밋 전 `commit-message` 스킬을 거치며, 기존 커밋 amend/rebase/force-push는 하지 않는다.

### 13.2 커밋 단위

1. 축소된 설계·계획 문서
2. 계측·하니스·baseline
3. quantum 구현·선택값·로컬 검증
4. GCP 보고서·최종 문서

### 13.3 중단 조건

- 정확성 계약을 지킬 수 없는 **구조적 P1**
- **baseline 또는 후보 run-set이 유효하지 않음** (§9.4 V1~V5)
- **통과 후보 0개** — 임계값을 완화하거나 격자를 자동 확대하지 않는다
- 전체 로컬 검증 또는 **CI 실패**
- **GCP 유료 승인 필요 시점**
- **runbook §7.5 시크릿 게이트 실패**

중단 시: 안전 종료 가능한 프로세스·VM을 종료하고, 코드·산출물·원본 로그를 보존하며,
임의 rollback/reset/force-push를 하지 않고, 정확한 차단 지점과 재개 명령을 보고한다.

---
## 14. 열린 위험

| # | 항목 | 유형 | 처리 |
|---|---|---|---|
| R1 | `ExecutionCh` 블로킹 send에 timeout이 없어 wall-clock 진행성은 quantum으로 보장 불가 | **accepted limitation** | §3.4에 명시. `emit_block_seconds`로 계측만. 해결은 §12로 분리 |
| R2 | §9에서 C1~C6 통과 조합이 0개 | **조건부 P1 (중단)** | 임계값 완화 금지. 보고 후 중단 |
| R3 | `MatchLatencyObserver` 값 상승 | **control** | §2.5 고지문 + sweep 크기별 분리 비교. 회귀로 판정하지 않는다 |
| R5 | 계측 오버헤드가 quantum 효과와 섞임 | **control** | 계측 커밋과 quantum 커밋 분리 + §4.3 오버헤드 선기록 |
| R6 | no-op 주문의 dirty 제거로 스냅샷 브로드캐스트 빈도 감소 | P2 | 의도된 변경. S2-6으로 고정 |
| R7 | 조각마다 `bestMatchable*` 재탐색 O(log n) 비용 | P2 | 캐싱은 조각 사이 취소를 놓치므로 불채택. 실측에서 문제가 되면 후속 |
| R8 | `maxConsecutiveCancels` 기본값이 샤드마다 동일 | P2 | 심볼별 차등은 근거 없음. 단일 값 유지 |
| R9 | 37번의 p95를 quantum에 귀속하고 싶어지는 압력 | P2 | §11.3 마지막 행으로 사전 차단 |
| R10 | active sweep 취소의 `RecordAttempt` DB write 누적량에 상한이 없음 (§5) | P2 | 5s 간격으로 수렴하므로 sweep 길이에 선형. CP2의 B-1에서 sweep당 재시도 횟수를 기록만 한다 |

R4(취소 재시도 횟수 상한이 sweep보다 짧을 가능성)는 **삭제했다.**
`applyNotFound`/`scheduleRetry`/`nextBackoff` 경로에 최대 시도 횟수 상한이 없음을 코드로
확인했다. 재시도는 백오프만 늘어나며 command를 조기 종결시키지 않는다.

**사용자 선택이 필요한 열린 P1은 없다.** R2는 발생 시 중단 후 보고하는 조건이고,
§11 진입에 필요한 유료 승인은 CP B 시작 시점에 별도로 요청한다.