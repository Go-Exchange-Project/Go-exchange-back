# 매칭 엔진 방출 용량 예약 실행 계획

> **For agentic workers:** Steps use checkbox (`- [ ]`) syntax. Task 1~5는 RED(실패 테스트) → 구현 → GREEN 순서다. Task 7·8은 CP A 구현에 대한 **통합 인수 테스트**라 처음부터 GREEN일 수 있다 — 실패하면 CP A 결함으로 보고 멈춘다.

**Goal:** 하류(outbox·정산) 정지가 매칭 엔진 goroutine을 `ExecutionCh` send에 가두지 않게 한다. 방출할 자리가 없으면 작업을 시작하지 않고 park하며, 그동안 취소·stop에 응답한다.

**Architecture:** 엔진은 샤드 `ExecutionCh`의 유일한 writer라는 사실로, 작업 시작 전에 칸을 확인한다. 조각은 `maxMatchesPerTurn+1+maxConsecutiveCancels`칸, 취소는 1칸. 칸이 없으면 6단계에서 park select(capacity 신호·취소·ticker·stop)로 기다린다. 예약 구간 안의 send는 논블로킹이며 예산을 벗어나면 panic한다.

**Tech Stack:** Go 1.25, testify, Prometheus client_golang, PostgreSQL(GORM).

**설계 문서:** [2026-09-15-matching-execution-capacity-design.md](../specs/2026-09-15-matching-execution-capacity-design.md) (설계 확정, 커밋 `12b5e33`)
**브랜치:** `feat/matching-execution-capacity` · **기준 SHA:** `12b5e33` (main `bfdfdd4` + 설계 문서)
**상태:** 계획 확정(2026-09-16, 4차 계획 리뷰). 구현 미착수.

## Global Constraints

- 설계 문서의 계약이 이 계획의 암묵적 요구사항이다. 충돌하면 설계 문서가 이긴다. 이 계획이 설계와 다르게 정한 점은 §계획 결정에 모두 적었다.
- 커밋은 CP 끝에서만. Conventional Commits 한글, `commit-message` 스킬 경유, 마지막 줄 `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`.
- 스테이징은 파일 경로 지정만. `git add -A`·`.`·`-u` 금지. `_workspace/`는 스테이징하지 않는다. amend·rebase·force push 금지. 푸시는 사용자 확인 후.
- 모든 `go test`에 `-count=1`(반복 게이트 제외). 통합 테스트는 `-p 1`. 호스트 실행과 Docker 실행을 동시에 돌리지 않는다.
- 파일을 변이했다 복구하는 mutation 검증을 하지 않는다. 각 테스트가 계약을 직접 단언한다.
- cancel worker(`cancel_command_worker.go`)와 `Match()`의 의미는 바꾸지 않는다.
- 백엔드 저장소는 CRLF. 관련 없는 코드·주석·포맷을 고치지 않는다.
- 시간 기반 단언은 상한만 두고 넉넉하게(예: "짧은 상한" = 500ms, 긴 ticker = 1s). sleep으로 순서를 만들지 않고 observer·채널 장벽을 쓴다.

## 계획 결정 (설계에 없던 구현 선택)

1. **`matching.NewMatchingEngineWithQuantum(cfg QuantumConfig) (*MatchingEngine, error)` 신설.** 서비스 패키지는 단일 엔진의 quantum 필드(unexported)를 바꿀 수 없다. 기본 quantum(조각 시작 137칸)으로는 테스트 12·13에서 park를 만들려면 maker를 천 개 넘게 시드해야 한다. 기존 `NewShardedEngineWithQuantum`과 짝이 맞는 생성자이며 같은 검증을 쓴다. 기존 무인자 생성자는 그대로 둔다.
2. **테스트 13의 "crash"는 park 방치로 만든다.** 설계 §9.2는 crash hook을 적었지만 `crashHook`은 unexported라 서비스 테스트에서 쓸 수 없다. 소비자가 없는 park 엔진은 §2.1 증명에 따라 더 방출하지 않고 오더북도 외부에 드러내지 않으므로, 런타임 2 관점에서 크래시와 같다. 새 export(테스트 전용 hook)를 만들지 않기 위한 선택이다. 단언이 끝난 뒤 cleanup에서 채널을 버리는 소비자를 붙이고 `Stop()`해 goroutine을 회수한다.
3. **Task 순서.** `Start()` 용량 panic(Task 1)은 cap 16·cap 4 테스트를 즉시 깨므로 같은 Task에서 고친다. 예약 panic(Task 4)은 park(Task 3)가 먼저 있어야 sweep 픽스처가 panic하지 않는다. 취소 예약은 cancel phase(Task 5)와 함께 넣는다 — 그 전에는 취소 send가 기존처럼 blocking이다.

## 체크포인트

| CP | Task | 커밋 | 승인 |
|---|---|---|---|
| **A. 엔진** | 1~6 | Task 6 끝에 1개 | 리뷰 세션 판정 후 CP B 진행 |
| **B. 서비스 통합·문서** | 7~9 | Task 9 끝에 1개 | 리뷰 세션 판정 후 푸시 여부 사용자 결정 |

## 중단 조건

설계 계약을 지킬 수 없는 구조적 문제 / 기존 quantum 선택 게이트(C1·C2·C4) 실패 / 전체 게이트 실패 / 설계와 다른 결정이 추가로 필요할 때. 멈추고 보고한다.

## 파일 구조

| 파일 | 책임 | Task |
|---|---|---|
| `internal/matching/quantum_config.go` | `validateExecutionCapacity` | 1 |
| `internal/matching/engine.go` | 생성자 검증·`Start` panic·`NewMatchingEngineWithQuantum` → 관측 호출 → park 상태 기계·`capacityCh` → 예약 emitter → cancel phase·응답 계약 | 1, 2, 3, 4, 5 |
| `internal/matching/sharded.go` | 생성자 검증 → 포워더 capacity 신호 | 1, 3 |
| `internal/matching/observers.go` | `ParkStarted`·`ParkDuration`·`CancelBackpressured`·`ShutdownLatched` | 2 |
| `internal/metrics/metrics.go`, `matching_observers.go` | 지표 2종, EmitBlock Help 문구 | 2 |
| `internal/matching/engine_capacity_test.go` (신규) | §9.2 테스트 1·6·7·8·9·10 + 예약 panic 2종 | 1, 3, 4 |
| `internal/matching/engine_cancel_capacity_test.go` (신규) | §9.2 테스트 2·3·4·5·11 | 5 |
| `internal/matching/engine_sweep_semantics_test.go` | 픽스처 장벽 교체(§9.1) | 3 |
| `internal/matching/engine_scheduler_test.go` | cap 16 두 테스트 Start 전 구성(§9.1) | 1 |
| `internal/matching/engine_observers_test.go` | `TestSliceEmitBlockIncludesMarketOrderDone` 대체(§9.1) | 1, 3 |
| `internal/service/execution_capacity_integration_test.go` (신규) | §9.2 테스트 12·13 | 7, 8 |
| `docs/refactor/README.md`, `docs/ENGINEERING-SUMMARY.md` | 결과 기록 | 9 |

---

# CP A — 엔진

## Task 1: 용량 검증과 생성자

**설계:** §6, §9.1(cap 16·cap 4 테스트), §9.2 테스트 9

- [ ] **Step 1 (RED):** `engine_capacity_test.go`에 테스트 9를 쓴다.
  - `validateExecutionCapacity` 표 테스트: 통과하는 최대 `maxMatchesPerTurn`(= cap-1-cancels), 그 +1 실패, `cap < 2` 실패, `maxConsecutiveCancels > cap-2` 실패, `maxMatchesPerTurn = math.MaxInt`·`maxConsecutiveCancels = math.MaxInt` 실패(합산 overflow가 통과시키던 값).
  - `NewShardedEngineWithQuantum(1, QuantumConfig{MaxMatchesPerTurn: 1023, MaxConsecutiveCancels: 1})` error.
  - `NewMatchingEngineWithQuantum` 같은 경계로 error/성공.
  - 기본 quantum 엔진에 `ExecutionCh = make(chan ExecutionEvent, 16)`을 끼우고 `Start()` → `require.Panics`. nil `ExecutionCh`는 panic하지 않는다.
- [ ] **Step 2:** `go test ./internal/matching -run 'Capacity' -count=1` → 컴파일 실패 또는 FAIL 확인.
- [ ] **Step 3:** 구현. `validateExecutionCapacity`(설계 §6 코드 그대로, 뺄셈만), 두 생성자 검증, `Start()` 첫 줄 검증 후 panic. `NewMatchingEngineWithQuantum`은 `cfg.Validate()` → `NewMatchingEngine()` → quantum 주입 → 용량 검증.
- [ ] **Step 4:** 이 변경으로 깨지는 기존 테스트를 설계 §9.1대로 고친다.
  - `TestBackpressureBlocksIntakeNotLatchedOrder`: quantum (1,1). 워터마크 미만 11칸을 **Start 전에** 채운다. 이후 단계(resting setup, probe, 취소 4건, 차단 주문, 소비 재개)는 그대로. 각 단계의 `free`와 조각 시작 조건 3, 워터마크 12의 관계를 주석으로 남긴다.
  - `TestStopDrainsQueuedOrderAndCancel`: quantum (1,1). resting 주문을 Start 전에 `GetOrderBook("BTC").AddOrder`로 직접 배치, 13칸도 Start 전에 채운다. queued order·cancel·Stop·drain 단언은 그대로.
  - `TestSliceEmitBlockIncludesMarketOrderDone`(cap 4): 기본 quantum이면 panic하므로 이 Task에서는 quantum **(2,1)**(조각 시작 4칸 = cap 4)로 맞춰 기존 단언을 그대로 유지한다. 예산 2라 체결 2건 + MarketOrderDone이 조각 하나에 들어가 원래 구조(dummy 2건 뒤 MarketOrderDone send가 막힘)가 보존된다. (1,1)로 두면 조각이 2개로 나뉘어 "조각 정확히 1개" 단언이 깨진다(구현 세션 Task 1에서 발견). 의미 대체는 Task 3.
  - 그 밖에 `Start()` 전에 작은 채널을 끼우는 테스트가 있으면 같은 원칙으로 quantum을 맞춘다(`grep -n "ExecutionCh = make"`로 전수 확인하고 목록을 보고서에 적는다).
- [ ] **Step 5 (GREEN):** `go test ./internal/matching -count=1` 전체 PASS.

## Task 2: 관측 추가 (행동 변경 0)

**설계:** §8.1

- [ ] **Step 1 (RED):** `engine_observers_test.go`의 `TestNilObserversAreSafe`에 새 4콜백 nil 호출을 추가하고, `internal/metrics`에 어댑터 테스트(기존 `matching_observers_test.go`가 있으면 거기에)를 추가한다: `ParkDuration` → `matching_engine_execution_capacity_park_seconds` 표본 증가, `CancelBackpressured` → `matching_engine_cancel_backpressured_total` 증가.
- [ ] **Step 2:** `observers.go`에 `ParkStarted func()`, `ParkDuration func(time.Duration)`, `CancelBackpressured func()`, `ShutdownLatched func()`와 nil-safe 헬퍼. `metrics.go`에 지표 2종. `NewMatchingEngineObservers`에 `ParkDuration`·`CancelBackpressured` 배선(`ParkStarted`·`ShutdownLatched`는 메트릭 없음). `matching_engine_emit_block_seconds` Help를 "Enqueue observation time of a single ExecutionCh send. Scheduler-path sends are reserved and non-blocking; non-zero values include clock resolution and preemption."로 바꾼다. 대시보드 이름은 유지.
- [ ] **Step 3 (GREEN):** `go test ./internal/matching ./internal/metrics -count=1`.

## Task 3: park 상태 기계와 깨우기

**설계:** §3.1~§3.4, §5, §7, §9.1(sweep 픽스처·observers 테스트), §9.2 테스트 1(park 부분)·6·7·8

- [ ] **Step 1 (RED):** `engine_capacity_test.go`에 쓴다. 공통: quantum (4,2) → 조각 시작 7칸, 긴 ticker는 `snapshotInterval = time.Second`.
  - **테스트 1 (park 부분)**: book에 시장가 매수가 정확히 4건 체결하고 끝나도록 매도 4건을 둔다. `free = 6`(Start 전 채움)에서 시장가 투입 → `ParkStarted` 1회, 오더북·주문 잔량·`tradeSeq`·채널 길이 불변. 채널에서 1건 빼 `free = 7` → `ParkDuration` 1회, Trade 4건 + MarketOrderDone 1건이 순서대로. (방출 수 판별은 Task 4 예약 panic과 함께 완성된다.)
  - **테스트 6 busy loop**: park 진입(`ParkStarted` 장벽) 뒤 소비 없이 300ms 동안 `Turn` 관측 수 ≤ 2×(ticker 0~1 + stop 0 + cancel 0 + capacity wake ≤1)+1 = 5.
  - **테스트 7 재개 지연**: `NewShardedEngineWithQuantum(1, (4,2))`. **Start 전에** 팬인 `se.ExecutionCh`를 cap 4, 샤드 로컬 `ExecutionCh`를 cap 8로 교체하고 `se.shards[0].snapshotInterval = time.Second`(긴 ticker)로 둔다(테스트 11과 같은 구성). maker 20건 이상을 둔 sweep의 첫 두 조각만으로 팬인 포화(포워더 1건 blocking)와 로컬 free < 7이 만들어진다. 현재 park와 그 뒤의 재개를 짝지어야 한다 — 첫 조각 직후 포워더가 아직 돌지 않아 생긴 일시 park가 포워더만으로 끝나며 `ParkDuration`을 남길 수 있고, 그 오래된 값을 읽으면 팬인 소비에 따른 깨우기가 고장 나도 통과한다.
    - `ParkStarted`는 원자 카운터 `started`, `ParkDuration`은 원자 카운터 `finished`와 mutex로 보호한 duration 슬라이스(호출 순서대로 append)로 기록한다. **기록 순서:** `ParkDuration` 콜백은 mutex 아래 duration을 append한 **뒤에** `finished`를 증가시킨다. 테스트는 슬라이스를 mutex 아래에서 읽는다 — 그래야 `finished > finishedBefore`를 본 시점에 `durations[finishedBefore]`가 반드시 존재한다.
    - 장벽: 팬인 `len == cap` **그리고** 샤드 로컬 `cap - len < 7` **그리고** `started > finished`(현재 park 중).
    - 소비 시작 직전 `finishedBefore := finished.Load()`.
    - 팬인 소비 시작.
    - `finished > finishedBefore`가 될 때까지 기다린다(상한 넉넉히).
    - 그 새 재개에 대응하는 duration(슬라이스의 인덱스 `finishedBefore`)만 `< 200ms`이고 1s ticker보다 확실히 짧음을 단언한다.
  - **테스트 8 shutdown 중 park**: park 장벽 → `Stop()` → `ShutdownLatched` 500ms 안 1회 → 300ms 동안 `Turn` 수가 ticker·wake 기반 상한 이하, `Done()` 미종료 → 소비 재개 → sweep 완주 → `Done()` 종료, 이벤트 개수 일치, `ShutdownLatched` 여전히 1회.
  - `latchStop` 세 경로: 4단계 논블로킹, 일반 blocking select, park select 각각에서 Stop해도 `ShutdownLatched` 정확히 1회(세 개의 하위 테스트).
- [ ] **Step 2:** 실행해 RED 확인(park가 없어 send에서 막히거나 observer 미호출).
- [ ] **Step 3:** 구현.
  - `free()`, `hasSliceCapacity()`(뺄셈 비교), `sliceBlockedNow()`, `latchStop()`, `enterParkOnce()`, `finishPark()`, `parkStartedAt`, `capacityCh chan struct{}`(버퍼 1, `NewMatchingEngine`이 생성).
  - `runTurn` 3단계: `activeSweep != nil && hasSliceCapacity()`일 때만 `finishPark()` → `runSlice()`.
  - 4단계와 8단계 stop case를 `latchStop()` 경유로.
  - 6단계: `sliceBlockedNow() && pendingCancel == nil` → `enterParkOnce()` → `parkSelect(ticker)` → `return false`. 기존 "남은 일" 판정은 그 뒤.
  - `parkSelect`: capacityCh, CancelCh(latch), ticker(tickerDue), stop(`shuttingDown`이면 nil). OrderCh 없음.
  - `sharded.go` execution 포워더: `for ev := range shard.ExecutionCh { select { case shard.capacityCh <- struct{}{}: default: }; se.ExecutionCh <- ev }` — **받은 직후** 신호.
- [ ] **Step 4:** 설계 §9.1대로 기존 테스트를 교체한다.
  - `engine_sweep_semantics_test.go`: `sweepEmitCap`·`waitEmitSaturated`를 park 장벽으로 바꾼다. 픽스처는 `maxMatchesPerTurn = 1`, `maxConsecutiveCancels = 8`(조각 시작 10칸), 채널 cap 16. `ParkStarted`를 받는 채널을 픽스처에 두고 `waitParked(t)`로 대기. park 시점 `free ≤ 9`이므로 취소는 칸 1 이상이 남아 거절되지 않음을 주석으로 계산해 둔다(Task 5 이후에도 성립해야 한다: park 시점 `free`는 6~9, 한도 8 이내). B-1~B-4의 의미 단언은 한 줄도 바꾸지 않는다.
  - `TestSliceEmitBlockIncludesMarketOrderDone`: 이름을 `TestParkBeforeFinalSliceIsObservedAndSliceEmitsMarketOrderDone`으로 바꾼다. 구성을 고정한다 — quantum (1,1)(조각 시작 3칸), cap 4, **Start 전 dummy 1건**(free 3), book에 매도 maker 2건, 시장가 매수 1건.
    - 첫 조각: free 3 ≥ 3 → Trade 1건, free 2.
    - 마지막 조각(Trade 1 + MarketOrderDone 1): free 2 < 3 → park. `ParkStarted` 1회, `ParkDuration` 0회, 채널 길이 2.
    - 소비 재개 → `ParkDuration` 1회, 이어서 Trade 1건 + MarketOrderDone 1건이 순서대로 나온다.
    - 기존 dummy 2건 구성(free 2)은 첫 조각 전부터 park해 테스트 의도가 달라지므로 쓰지 않는다. emitBlock 값의 exact 단언은 두지 않는다.
- [ ] **Step 5 (GREEN):** `go test ./internal/matching -count=1`.

## Task 4: 예약 emitter (fail-fast)

**설계:** §8.2, §2.3, §9.2 테스트 1(방출 수 판별)·10, 4차 리뷰 세부 사항 1

- [ ] **Step 1 (RED):**
  - **예산 소진 후 추가 send panic**: 엔진 goroutine 없이 `reserve(2)` 상태를 만들고 `sendExecution` 3회 → 3번째 `require.Panics`. (테스트는 unexported 헬퍼를 직접 부른다.)
  - **예산은 남았지만 채널 준비 안 됨 panic**: cap 1 채널을 가득 채우고 `reserve(1)` 상태에서 `sendExecution` → panic, 채널 내용은 그대로(이벤트를 버리지 않음 — 채워둔 1건만 존재).
  - **예약 밖 blocking 유지**: `reservationActive == false`에서 가득 찬 채널에 send하는 goroutine은 막혀 있다가 소비하면 완료된다.
  - **테스트 1 보강**: 조각 예산이 `maxMatchesPerTurn+1`임을 판별 — 예산을 `maxMatchesPerTurn`으로 잡는 구현은 시장가 MarketOrderDone에서 panic하고, 시작 조건에서 `+1`을 빠뜨린 구현은 `free = 6`에서 조각을 시작한다. 두 경우 모두 테스트 1이 실패함을 테스트 주석에 적는다.
  - **테스트 10 `Match()` 계약**: 소비자가 동시에 읽는 cap 1 채널에서 `Match()`가 대형 sweep을 panic 없이 끝내고, 방출 개수·순서가 기존 기대값과 같다.
- [ ] **Step 2:** RED 확인.
- [ ] **Step 3:** 구현. `reservationActive`, `reservationRemaining`, `beginReservation(n)`, `endReservation()`. `sendExecution`은 `reservationActive`면 remaining 0 panic → 논블로킹 select, default panic → 성공 시 remaining--; 아니면 기존 blocking. `runSlice`는 matchSlice 전에 `beginReservation(maxMatchesPerTurn+1)`을 하고, **두 정상 반환 경로 모두에서** `endReservation()`을 부른다 — `done == false` 조기 반환(`Observers.slice`·`yield` 뒤 `return` 직전)과 `done == true`의 `finishOrder` 이후. 한쪽만 처리하면 예약 상태가 남아 다음 작업의 send가 잘못 판정된다. 두 경로 각각에 대해 `runSlice` 반환 후 `reservationActive == false`를 단언하는 단위 테스트를 Step 1에 추가한다. panic 메시지는 엔진 ID·kind·remaining을 담는다. `Match()` 문서 주석에 "용량 계약 밖, 동기 호출 전용" 추가.
- [ ] **Step 4 (GREEN):** `go test ./internal/matching -count=1`.

## Task 5: cancel phase와 응답 계약

**설계:** §4.1, §4.2, §9.2 테스트 2·3·4·5·11

- [ ] **Step 1 (RED):** `engine_cancel_capacity_test.go`. 공통 quantum (4,2).
  - **테스트 2 공간 0**: active sweep 없이 채널을 Start 전 가득 채움, book에 대상 주문. `CancelOrder` → `ErrCancelOrderBackpressured` 500ms 안, 주문 book에 잔존, `CancelBackpressured` 1회, 이벤트 추가 없음. 1건 소비 → 재취소 → 제거, `OrderCancelled` 정확히 1회.
  - **테스트 3 parked 중 한도 소진**: park 장벽 후 대상 주문 2+3건 취소(한도 C=2, k=3). park 시점 `free`가 5 이상이 되게 구성(C건 처리 후에도 거절 사유가 "한도"이도록). 첫 2건 book 제거·이벤트 2건, 나머지 3건 book 잔존·backpressure·`CancelBackpressured` 3회·이벤트 추가 없음. 소비 재개 → `ParkDuration`, sweep 완주, 거절된 3건 재취소 → 제거.
  - **테스트 4 응답 계약**: (a) `CancelCh`에 버퍼 없는 `ResponseCh` command를 넣고 읽지 않음 → 이어서 넣은 정상 취소와 조각이 처리된다. (b) public `CancelOrder`에 버퍼 없는 `ResponseCh`를 넘겨도 결과 정상 반환.
  - **테스트 5 응답 시점**: 성공 응답을 받은 goroutine이 즉시 `len(ExecutionCh)`를 확인하면 해당 `OrderCancelled`가 이미 들어 있다(소비자 없음).
  - **테스트 11 교차 샤드**: `NewShardedEngineWithQuantum(2, (4,2))`. **Start 전에** 팬인 `se.ExecutionCh`를 cap 4로, 샤드 A·B의 로컬 `ExecutionCh`를 각각 cap 8로 교체한다(조각 시작 7칸 검증 통과). A 로컬을 기본 cap(1024)으로 두면 A가 park하기까지 체결이 천 건 넘게 필요하다. 샤드 배정은 심볼 라운드로빈이므로 A·B 심볼을 먼저 `shardFor`로 고정해 어느 샤드인지 확인한 뒤 채널을 교체한다.
    - 샤드 A에 대형 sweep을 만들고 팬인을 소비하지 않는다. 장벽: 팬인 `len == cap` **그리고** A의 `ParkStarted`.
    - 샤드 B에는 active sweep 없이 resting 주문 여러 건만 둔다. active sweep이 없으면 admission phase가 연속 취소 카운터를 매 turn 초기화하므로, 성공 취소를 반복해 B 로컬 채널을 채울 수 있다.
    - B 로컬 칸이 남는 동안 B 취소는 성공하고 `OrderCancelled`를 낸다. 이를 B 로컬 `len == cap`이 될 때까지 반복한다(B 포워더가 팬인 send에 1건을 들고 막혀 있을 수 있으므로 성공 횟수가 아니라 `len == cap`으로 장벽을 세운다).
    - 그다음 B 취소는 **free 0 때문에** `ErrCancelOrderBackpressured`로 500ms 안에 끝나고 주문은 book에 남는다.
    - 어느 엔진도 panic하지 않는다. 팬인 소비 재개 → A sweep 완주, B의 거절된 취소를 다시 보내면 제거된다.
  - **판정표 경계**: `free == 0 && !quotaLeft && !blocked`(active sweep 없음) → 거절. `!quotaLeft && !blocked && free ≥ 1` → command를 꺼내지 않고 phase 종료(`len(CancelCh)` 불변 확인).
- [ ] **Step 2:** RED 확인.
- [ ] **Step 3:** 구현. `ErrCancelOrderBackpressured`. `cancelPhase()`(설계 §4.1 의사코드 그대로 — 결정 후 꺼냄), `rejectCancel(cmd)`(논블로킹 응답 + `CancelBackpressured`). `processCancel` 순서: `beginReservation(1)` → handleCancel → 제거 시 markDirty·emitOrderCancelled → `endReservation()` → 논블로킹 응답. `CancelOrder`는 `cmd.ResponseCh = make(chan CancelOrderResult, 1)`을 무조건 대입. `ResponseCh` 필드 주석에 계약 두 줄.
- [ ] **Step 4 (GREEN):** `go test ./internal/matching -count=1`.

## Task 6: 엔진 게이트와 CP A 커밋

- [ ] `go build ./... && go vet ./...`
- [ ] `go test -count=20 ./internal/matching`
- [ ] Linux race (PowerShell, 저장소 루트에서): `docker run --rm -v "${PWD}:/src" -w /src golang:1.25 go test -race -count=1 ./internal/matching`
- [ ] quantum 선택 게이트: `go test -tags quantumharness ./internal/matching -run 'TestSelectedConfig' -count=1 -v` — C1·C2·C4 테스트는 `//go:build quantumharness` 파일에 있어 태그 없이는 실행되지 않는다. C1·C2·C4 상한 통과 수치를 보고서에 원문 인용.
- [ ] `go test -run '^$' -bench 'BenchmarkTPS_' -benchmem -count=3 ./internal/matching` — 현 브랜치 수치 기록만(판정 기준 없음, 기준 SHA 비교는 하지 않는다).
- [ ] 백엔드 전체 `go test -p 1 ./... -count=1`(서비스 통합이 기본 엔진을 쓰므로 회귀 확인).
- [ ] 스테이징(파일 지정) → `commit-message` 스킬 → 커밋. **푸시하지 않는다.**
- [ ] 보고: 변경 파일, 교체한 기존 테스트 목록, 위 명령 원문 출력, 커밋 해시. **리뷰 세션 CP A 판정 전에는 CP B를 시작하지 않는다.**

---

# CP B — 서비스 통합·문서

## Task 7: 테스트 12 — outbox 저장 실패 → 회복

**설계:** §9.2 테스트 12

- [ ] **Step 1:** `execution_capacity_integration_test.go`. 하니스는 `cancel_command_outbox_integration_test.go`의 `newCancelPipelineHarness`를 본떠 만들되 엔진은 `matching.NewMatchingEngineWithQuantum(QuantumConfig{MaxMatchesPerTurn: 4, MaxConsecutiveCancels: 2})`, `ExecutionCh`는 Start 전에 cap 16으로 교체, `Observers`에 `ParkStarted`·`CancelBackpressured` 장벽 채널. 심볼은 `harnessSymbol(t)`로 격리, `cleanupServiceUsers` 등 기존 정리 헬퍼 사용.
- [ ] **Step 2:** 시나리오.
  - **저장 오류를 실제로 주입한다.** 기존 `blockableOutboxRepo.block()`은 대기만 하고 해제 후 곧바로 성공해 오류를 한 번도 반환하지 않으므로, 설계 §9.2 테스트 12의 "`OutboxWriter.flushAndForward` 무한 재시도 경로"를 지나지 않는다. `block()`은 다른 통합 테스트가 쓰므로 바꾸지 않고, 이 파일에 decorator를 새로 둔다:
    ```go
    // failingOutboxRepo는 failing이 true인 동안 호출 수를 세고 sentinel error를 반환한다.
    // false가 되면 inner(실제 TradeOutboxRepository)로 위임하고, 성공한 호출 수를 센다.
    type failingOutboxRepo struct {
    	inner     *repository.TradeOutboxRepository
    	failing   atomic.Bool
    	failed    atomic.Int64 // 오류를 반환한 호출 수
    	succeeded atomic.Int64 // inner 호출이 성공한 수
    }
    ```
    `failing.Load()` 확인과 `failed.Add(1)` 사이에 테스트가 `failing = false`로 바꿀 수 있으므로, 해제 직후 `failed`가 한 번 더 늘어도 정상이다. **`failed`의 해제 후 불변은 단언하지 않는다.**
  - OutboxWriter `BatchSize: 1`, `RetryBaseDelay: 5 * time.Millisecond`. 시작 전에 `failing = true`.
  - 장벽 순서(모두 통과하기 전에는 회복시키지 않는다):
    1. `failed ≥ 1` — OutboxWriter가 적어도 한 번 오류를 받고 재시도 루프에 들어갔다.
    2. 엔진 `ParkStarted` — OutboxWriter가 꺼낸 1건 + 채널 16칸이 차 park했다. maker 수를 `1 + 16 + 조각 예산(5)` 이상으로 시드한다.
    3. 아래 취소 3건의 DB 상태 장벽.
  - 취소 대상은 **C+1 = 3건**이다. park 직후에는 정책상 `cancelReserve = 2`칸이 남아 있어 취소 1건은 성공하고 awaiting-outbox로 갈 뿐 backpressure가 생기지 않는다. 그래서 sweep 가격과 겹치지 않는(체결되지 않는) resting 주문 3건을 미리 두고, 하니스의 cancel worker를 시작한 상태에서 park 장벽 뒤에 실제 `OrderService.CancelOrder`로 3건을 요청한다.
    - 처음 2건: quota 안에서 성공, `OrderCancelled` 2건이 채널에 들어간다.
    - 나머지 1건: quota 소진(`blocked && !quotaLeft`)으로 backpressure.
    - **DB에서 직접 단언한다(회복 전, `Eventually`).** observer 횟수만으로는 세 건 모두 잘못 거절한 구현도 통과한다. 세 command를 조회해:
      - `AttemptCount > 0`인 command가 **정확히 1개**
      - 나머지 2개는 `AttemptCount == 0`
      - 세 command 모두 아직 PENDING(성공 2건은 outbox 커밋 전이라 awaiting-outbox, 거절 1건은 백오프)
      - `CancelBackpressured`는 보조 장벽으로만 쓴다. 어느 ID가 재시도됐는지는 고정하지 않는다.
  - 회복: 해제 전 `failed ≥ 1`을 이미 확인한 상태에서 `failing = false`. 회복 증거는 `succeeded ≥ 1` **그리고** 이 심볼의 outbox 행 생성으로만 판정한다.
  - release → 정산을 `settleForwarded`로 끝까지 흘린다.
- [ ] **Step 3:** 단언: 세 취소 command 모두 PROCESSED, 취소 이벤트 정확히 3건, 체결 수가 기대값과 정확히 일치(중복·유실 0), 대상 주문 3건 CANCELLED와 각 release 분개 1건, 원장 검산 4종 위반 0.
- [ ] **Step 4:** `go test -p 1 ./internal/service -run 'ExecutionCapacity' -count=1 -v` PASS. 이어서 `-count=5`로 결정성 확인.

## Task 8: 테스트 13 — durable prefix + undurable suffix 복구

**설계:** §9.2 테스트 13, 4차 리뷰 세부 사항 2, 계획 결정 2

- [ ] **Step 1: 픽스처.** 지정가 매도 maker N=40개(각 수량 1, 같은 가격)와 지정가 매수 taker 1개(수량 40)를 **ledger hold까지 포함해** 시드한다. `CreatedAt`과 ID를 명시적으로 시드해 bootstrap 순서(`created_at ASC, id ASC`)를 고정한다: maker 1..40이 taker보다 이르다. 기존 헬퍼(`seedSettlementRows` 계열)를 확장하거나 새 헬퍼를 만든다. 수수료율·체결가로 사용자별 최종 기대 잔액(USER_AVAILABLE·USER_LOCKED·FEE_INCOME)을 테스트 안에서 계산해 상수로 둔다.
- [ ] **Step 2: 런타임 1.**
  - 엔진: `NewMatchingEngineWithQuantum((4,2))`, `ExecutionCh` cap 16(Start 전), `ParkStarted` 장벽.
  - 소비자: **테스트 전용 종료 가능한 소비자** goroutine — `ExecutionCh`에서 하나씩 받아 `NewTradeOutboxEvent` → `TradeOutboxRepository.InsertBatchAndMarkCancelCommands([row], nil)`로 커밋, 정산으로 넘기지 않음, P=8건 커밋 후 **더 받지 않고 반환**.
  - bootstrap으로 maker 40개와 taker를 엔진에 올린다(런타임 1도 main과 같은 진입 경로).
  - 장벽: **현재 진행 중인 최종 park**를 확인한다. 소비자가 P건을 커밋하는 동안 생겼다가 이미 끝난 과거 park 신호로 통과하면 안 된다.
    - `ParkStarted`·`ParkDuration`을 원자 카운터로 센다.
    - 소비자 반환을 기다린 뒤, `started > finished`가 될 때까지 기다린다(상한 넉넉히). 소비자가 없으므로 이 상태는 이후 바뀌지 않는다.
    - 이 장벽을 통과한 뒤에만 아래 단언과 런타임 2로 넘어간다.
  - **런타임 1 종료 직전 단언**: 이 심볼의 outbox가 정확히 8건이고 모두 PENDING, suffix 행 0.
  - 런타임 1 엔진은 park한 채 둔다(크래시 등가). `t.Cleanup`에 "채널을 버리는 소비자 시작 → `Stop()` → `Done()` 대기"를 **먼저** 등록한다.
- [ ] **Step 3: 런타임 2.**
  - replay: `OutboxReplayer{Repo: symbolScopedReplaySource(심볼), Process: SettleTrade(event.Trade, 0) 성공 여부}` — `outbox_replayer_integration_test.go` 방식. main 순서대로 **replay 먼저**.
  - 새 엔진(`NewMatchingEngineWithQuantum((4,2))`, 기본 cap) + 실제 `OutboxWriter` + `Forward`에서 정산(`settleForwarded` 방식) → `MatchingBootstrapService.BootstrapOpenOrders`.
  - bootstrap은 prefix로 체결된 maker 8개를 제외한 maker 32개(생성 순)와 잔량 32의 taker를 올리고, taker가 나머지를 쓸어간다.
  - 정산 완료를 기다린다(taker FILLED 또는 기대 trade 수 도달, 상한 넉넉히).
- [ ] **Step 4: 단언(정확한 기대값).**
  - 이 심볼 trades 수 == 40, 수량 합 == 40, quote 총액 == 40 × 가격.
  - maker 40개 각각 정확히 1건 체결·FILLED.
  - taker FILLED, 잔량 0.
  - 사용자별 USER_AVAILABLE·USER_LOCKED, FEE_INCOME이 Step 1 기대값과 정확히 일치.
  - 주문별 settlement 분개 수가 기대값과 일치, release 분개 수 기대값과 일치.
  - 이 심볼 outbox PENDING 0.
  - prefix 체결 8 + suffix 재매칭 32 == 40 (런타임 1 커밋 수와 런타임 2 신규 trades 수로 각각 확인).
  - 원장 검산 4종 위반 0(보조).
  - `engine_event_id` 중복 여부는 단언하지 않는다.
- [ ] **Step 5:** `go test -p 1 ./internal/service -run 'ExecutionCapacity' -count=1 -v` PASS, `-count=5` 결정성 확인.

## Task 9: 전체 게이트·문서·CP B 커밋

- [ ] `go build ./... && go vet ./...`
- [ ] 새 스키마(`DROP SCHEMA public CASCADE; CREATE SCHEMA public;`)에서 `go test -p 1 ./... -count=1 -timeout=20m`
- [ ] `go test -p 1 -shuffle=on ./internal/service -count=1 -timeout=20m`
- [ ] Docker Linux race (PowerShell, 저장소 루트, 테스트 DB 컨테이너 `goexchange-postgres-test` 실행 중, 새 스키마):
  `docker run --rm --network go-exchange-back_default -e GOEXCHANGE_TEST_DATABASE_DSN="host=goexchange-postgres-test user=goexchange_test password=goexchange_test_password dbname=goexchange_test port=5432 sslmode=disable" -v "${PWD}:/src" -w /src golang:1.25 go test -race -p 1 ./... -count=1 -timeout=30m`
- [ ] 문서:
  - `docs/refactor/README.md`: 4차 리팩토링 절 또는 새 절에 "매칭 하류 정지 격리" 항목 — 설계·계획 링크, 결과(위 게이트), 3차①이 남긴 "일반 보장은 후속"을 이 작업이 닫았다는 문장. 측정하지 않은 처리량 주장 금지.
  - `docs/ENGINEERING-SUMMARY.md` 247행 "남은 한계"를 갱신: send는 더 이상 무기한 blocking이 아니며 park·backpressure로 응답한다, 단 하류가 죽으면 drain 완료 상한은 없고 공유 팬인·OutboxWriter는 공통 장애 영역이다.
- [ ] 스테이징(파일 지정) → `commit-message` 스킬 → 커밋. **푸시하지 않는다.**
- [ ] 보고: 변경 파일, 위 명령 원문 출력, 커밋 해시. 리뷰 세션 CP B 판정을 요청한다.

---

## 설계 §9.2 테스트 대응표

| 설계 테스트 | Task | 파일 |
|---|---|---|
| 1 경계 | 3, 4 | `engine_capacity_test.go` |
| 2 취소 공간 0 | 5 | `engine_cancel_capacity_test.go` |
| 3 parked 중 한도 소진 | 5 | `engine_cancel_capacity_test.go` |
| 4 응답 계약 | 5 | `engine_cancel_capacity_test.go` |
| 5 성공 응답 시점 | 5 | `engine_cancel_capacity_test.go` |
| 6 busy loop | 3 | `engine_capacity_test.go` |
| 7 재개 지연 | 3 | `engine_capacity_test.go` |
| 8 shutdown 중 park | 3 | `engine_capacity_test.go` |
| 9 용량 검증 | 1 | `engine_capacity_test.go` |
| 10 `Match()` 계약 | 4 | `engine_capacity_test.go` |
| 11 교차 샤드 | 5 | `engine_cancel_capacity_test.go` |
| 12 outbox 실패 회복 | 7 | `execution_capacity_integration_test.go` |
| 13 prefix/suffix 복구 | 8 | `execution_capacity_integration_test.go` |
| 리뷰 세부 1 예약 panic 2종 | 4 | `engine_capacity_test.go` |
| 리뷰 세부 2 런타임 1 outbox 단언 | 8 | `execution_capacity_integration_test.go` |
