# 매칭 엔진 방출 용량 예약 설계 — 하류 정지 격리

> 상태: 설계 확정(2026-09-15, 4차 리뷰). 구현 미착수.
> 기준 SHA: `bfdfdd4` (Go-exchange-back main, 복식부기 원장 병합 직후)
> 선행: [매칭 엔진 두 quantum 설계](2026-08-30-matching-quantum-design.md) — 재개형 sweep(`activeSweep`)이 이 설계의 전제다.

## 0. 왜 필요한가

엔진 goroutine이 하류 정지에 갇힌다. 방출은 [engine.go:871](../../../internal/matching/engine.go)
`sendExecution`의 `me.ExecutionCh <- event` 하나로 나가는데, 여기에 timeout도 취소 경로도 없다.

```
DB·정산 정지
→ OutboxWriter.flushAndForward 무한 재시도 / forwardToSettlementQueue blocking send
→ 팬인 채널(sharded.go, 1024) 포화 → 샤드 포워더 정지
→ 샤드 ExecutionCh(1024) 포화 → 엔진 goroutine이 send에서 정지
```

75% 워터마크(`emitBackpressured`)는 **신규 주문 유입만** 막는다. 이미 시작한 sweep(`runSlice`)과
취소(`processCancel`)는 막지 않으므로 이 둘이 send에서 멈춘다. 그러면:

- 그 샤드의 `CancelOrder`가 1초 타임아웃으로 끝난다(worker가 백오프 재시도 — 유실은 없다).
- 스냅샷 갱신이 멈춘다.
- shutdown drain이 30초 deadline 뒤 포기하고, 다음 부팅 replay가 복구한다.

**유실은 없고 진행성만 깨진다.** 이벤트를 버리는 해법은 오더북과 DB 주문 상태를 어긋나게 하므로
제외한다. 3차①이 "일반 보장은 주문당 emit 상한/재개형 매칭이 전제(후속)"라고 남긴 그 후속이다.
quantum이 재개형 sweep을 만들었으므로 이제 가능하다.

## 1. 목표와 비목표

**목표 — "멈추되 응답하는 엔진"**

1. 비동기 스케줄러 경로의 엔진 goroutine은 `ExecutionCh` send에서 막히지 않는다.
2. 방출할 자리가 없으면 작업을 **시작하지 않고** 기다린다. 이벤트를 버리지 않고 순서도 바꾸지 않는다.
3. 기다리는 동안에도 취소 요청에 정해진 시간 안에 응답한다(성공 또는 backpressure).
4. 기다리는 동안 busy loop가 없고, 하류가 회복되면 ticker 주기에 묶이지 않고 재개한다.
5. `Stop` 인지에는 시간 상한이 있다.

**비목표**

- 샤드별 outbox·팬인 분리(진짜 장애 격리). 공유 팬인과 OutboxWriter는 여전히 공통 장애 영역이다.
- Kafka relay, DB 장애 시 전 샤드 정지 자체.
- 하류가 죽은 상태에서 `Done()`이 닫히는 시간 상한(§7).
- 동기식 `Match()`와 외부 코드의 `ExecutionCh` 직접 write(§2.3).

## 2. 용량 모델

### 2.1 단일 writer 증명과 적용 범위

프로덕션 경로(`NewShardedEngineWithQuantum`, main.go:84)에서 샤드 `ExecutionCh`에 쓰는 것은
그 샤드의 엔진 goroutine 하나뿐이고, 읽는 것은 샤드 포워더 하나뿐이다. 따라서 엔진이 검사한
`free = cap(ExecutionCh) - len(ExecutionCh)`는 검사 뒤에 **줄어들지 않는다.** 검사 뒤 `runSlice`
안에서는 다른 작업이 끼어들지 않으므로, 검사 시점의 `free`만큼은 blocking 없이 send할 수 있다.

이 증명은 코드 전체의 강제 불변식이 아니다. `ExecutionCh`는 exported이고 테스트가 직접 쓴다.
보장 범위는 §2.3에서 한정한다.

### 2.2 네 개의 수

스케줄러 경로의 방출 지점은 세 곳뿐이다(테스트 외 호출 확인).

| 방출 | 위치 | 조각당 최대 |
|---|---|---|
| Trade | `matchSlice` 안 `emitTrade` | `maxMatchesPerTurn` |
| MarketOrderDone | 마지막 조각 `finishOrder` | 1 |
| OrderCancelled | `processCancel` | 취소 1건당 1 |

지정가 `finishOrder`는 book 등록만 하고 방출하지 않는다. 조각 내부에서 취소는 처리되지 않는다.

```
sliceNeed          = maxMatchesPerTurn + 1        // 정확성 하한: 조각 하나가 막히지 않을 조건
cancelNeed         = 1                            // 정확성 하한: 취소 하나가 막히지 않을 조건
cancelReserve      = maxConsecutiveCancels        // 서비스 정책
sliceStartCapacity = sliceNeed + cancelReserve    // 조각 시작 조건
```

`sliceNeed`와 `cancelNeed`는 **정확성 조건**이다. `cancelReserve`는 **서비스 정책**이다 —
"조각 하나를 끝낸 뒤에도, 하류가 전혀 소비하지 않아도 취소 `maxConsecutiveCancels`건은 성공한다."
3차①의 목표(포화 중 취소 우선)를 지키기 위한 요구이며, 사용자가 이 정책을 채택했다(2026-09-15).
기본값에서 `sliceStartCapacity = 128 + 1 + 8 = 137`이고, 회복 처리량 비용은 1024칸 중 8칸이다.

`sliceStartCapacity`는 코드에서 합산으로 계산하지 않는다. 검사는 항상 `free - 1 - maxConsecutiveCancels
>= maxMatchesPerTurn` 형태로 하고, 그 전제(§6의 용량 검증)가 부팅 시 확인돼 있어야 한다.

### 2.3 보장 범위 밖

- **`Match()`**: budget 0으로 무제한 방출한다. 프로덕션 호출이 없고 테스트·벤치 약 30곳이 쓴다.
  이 설계는 `Match()`의 의미를 바꾸지 않는다. godoc에 "용량 계약 밖, 동기 호출 전용"을 명시하고
  §9의 계약 테스트로 방출 개수·순서가 그대로임을 고정한다. `Match()`의 send는 기존처럼 blocking이다(§8.2).
- **`matchSlice`·`finishOrder` 직접 호출**: 테스트의 조각 단위 검증용이다. 용량 검사 대상이 아니다.
- **외부의 `ExecutionCh` write**: 테스트 픽스처만 한다. `Start()` 전에 채운 칸은 "하류가 아직 소비하지
  않은 칸"과 같게 취급되므로 계약이 깨지지 않는다. `Start()` 뒤에 외부가 쓰면 단일 writer 증명이
  깨지므로 금지한다.

## 3. 스케줄러 변경

### 3.1 상태와 헬퍼

```go
// free는 ExecutionCh의 남은 칸이다. nil 채널은 방출이 없는 엔진이므로 무한으로 본다.
func (me *MatchingEngine) free() int

// hasSliceCapacity는 free - 1 - maxConsecutiveCancels >= maxMatchesPerTurn 이다.
func (me *MatchingEngine) hasSliceCapacity() bool

// sliceBlockedNow는 activeSweep != nil && !hasSliceCapacity() 이다.
// **호출 시점의 상태로 매번 다시 계산한다.** turn 시작 값을 캐시하지 않는다 —
// cancel phase가 칸을 쓰면 turn 시작에 false였어도 slice 시점에는 true가 된다.
func (me *MatchingEngine) sliceBlockedNow() bool

// latchStop은 shuttingDown 전이와 ShutdownLatched 관측을 묶는다. 이미 true면 아무것도 하지 않는다.
// stop을 받는 세 경로(4단계 논블로킹 확인, 일반 blocking select, park select)가 모두 이것만 부른다.
func (me *MatchingEngine) latchStop()

// parkStartedAt은 첫 park 진입 시각이다. zero면 park 중이 아니다.
// ticker·취소·신호로 중간에 깨어나도 초기화하지 않는다. slice가 실제로 재개될 때만 초기화한다.
parkStartedAt time.Time

// enterParkOnce는 6단계의 실제 park 진입 직전에만 부른다. parkStartedAt이 zero일 때만 시각을
// 기록하고 ParkStarted를 1회 호출한다. 기존 sweep이든 5단계 admission이 방금 만든 sweep이든
// park 시작 관측은 이 한 곳에서만 일어난다.
func (me *MatchingEngine) enterParkOnce()

// finishPark는 3단계에서 용량이 확보돼 slice를 실행하기 직전에 부른다. park 중이었으면
// ParkDuration을 1회 호출하고 parkStartedAt을 초기화한다. park 중이 아니었으면 no-op.
func (me *MatchingEngine) finishPark()
```

### 3.2 runTurn 의사코드

현재 [runTurn](../../../internal/matching/engine.go)(engine.go:212)의 단계 번호를 유지한다.
`// 변경`으로 표시한 줄이 새 동작이다.

```go
func (me *MatchingEngine) runTurn(ticker *time.Ticker) bool {
	turnStart := time.Now()
	hadActive := me.activeSweep != nil

	// 1. cancel phase — §4.1
	me.cancelPhase() // 변경

	// 2. ticker phase — 변경 없음(스냅샷 send는 논블로킹)
	if me.tickerDue { me.tickerDue = false; me.flushSnapshots() } else { select { case <-ticker.C: me.flushSnapshots(); default: } }

	// 3. slice phase
	if me.activeSweep != nil && me.hasSliceCapacity() { // 변경: 지금 상태로 검사
		me.finishPark() // 변경: park 중이었으면 ParkDuration 1회 후 초기화
		me.runSlice()   // 방출 예산 maxMatchesPerTurn+1의 예약 구간(§8.2)
		me.cancelsSinceProgress = 0 // P-a
	}
	// 용량이 없으면 여기서는 아무것도 하지 않는다. park 시작 관측은 6단계에서만 한다.

	// 3.5 crash hook — 변경 없음

	// 4. stop latch
	select { case <-me.stopCh: me.latchStop(); default: } // 변경: latchStop 경유(이미 true면 no-op)

	// 5. admission — 변경 없음(admit 자체는 방출하지 않는다)
	if !hadActive { me.admitPhase() }

	me.Observers.turn(time.Since(turnStart))

	// 6. park 판정 — 변경
	//    sliceBlockedNow는 여기서 다시 계산한다.
	//    pendingCancel이 있으면 park하지 않는다: 다음 turn의 cancel phase가 반드시 처리하거나 거절한다.
	//    pendingOrder는 park를 막지 않는다: 보존한 채 현재 sweep이 재개될 때까지 기다린다(추월 금지).
	//    tickerDue는 2단계가 소모했으므로 여기서 true일 수 없다.
	//    5단계 admission이 방금 만든 sweep도 여기서 처음 park하므로 park 시작 관측은 여기서만 한다.
	if me.sliceBlockedNow() && me.pendingCancel == nil {
		me.enterParkOnce()
		me.parkSelect(ticker)
		return false
	}
	if me.activeSweep != nil || me.pendingCancel != nil || me.pendingOrder != nil || me.tickerDue {
		return false
	}

	// 7. shutdown drain 완료 판정 — 변경 없음
	// 8. blocking select — stop case만 latchStop 경유로 변경
}
```

shutdown 중 slice가 park하면 7단계에 도달하지 않는다(6단계에서 park). drain 완료는 sweep이 재개돼
끝난 뒤에만 판정된다.

### 3.3 park select

```go
func (me *MatchingEngine) parkSelect(ticker *time.Ticker) {
	stop := me.stopCh
	if me.shuttingDown {
		stop = nil // 닫힌 채널은 항상 준비 상태다. 남겨두면 shutdown 중 park가 busy loop가 된다.
	}
	select {
	case <-me.capacityCh:          // 포워더 소비 신호(§5). 다음 turn이 free를 다시 계산한다.
	case cmd := <-me.CancelCh:     // latch만 한다. 다음 turn cancel phase가 처리 또는 거절한다.
		me.pendingCancel = &cmd
	case <-ticker.C:               // 신호 누락 대비 fallback
		me.tickerDue = true
	case <-stop:
		me.latchStop()
	}
}
```

- **OrderCh는 받지 않는다.** active sweep이 있는 동안 새 주문을 받으면 추월이다(quantum 설계 §1).
- 신호는 "다시 보라"일 뿐 용량 보장이 아니다.

### 3.4 turn 수 상한

quantum 설계의 "turn당 progress 정확히 하나"는 parked turn에서 성립하지 않는다. parked turn의 progress는
0이다. 대신 park select는 원인이 있을 때만 깨어나고, 깨어난 뒤 한 turn 안에 다시 park 판정에 도달한다.
pendingCancel이 남아 park를 건너뛴 turn은 다음 turn의 cancel phase가 그 command를 반드시 소모하므로
연속되지 않는다. 따라서 park가 이어지는 구간 `T`에서:

```
turns(T) ≤ 2 × (capacity wake + cancel latch + ticker wake + stop latch) + 1
```

각 항은 생산자가 보낸 횟수가 아니라 **park select가 실제로 받은 wake 횟수**다. park 전에 `capacityCh`
버퍼에 남아 있던 stale 신호도 wake 1회로 센다. `capacityCh`는 버퍼 1이라 capacity wake 수는 소비 이벤트
수 + 1 이하다. shutdown 중에는 stop latch 항이 더 늘지 않는다(stopCh를 뺐으므로). 이 상한을 §9에서 긴
ticker로 고정한다.

## 4. 취소 경로

### 4.1 cancel phase — 결정 순서

거절 조건은 command 내용이 아니라 엔진 상태로만 정해진다. 그래서 **꺼내기 전에** 행동을 결정하고,
종료할 때는 command를 꺼내지 않는다(꺼낸 command를 되돌리는 경로가 없다).

```go
func (me *MatchingEngine) cancelPhase() {
	rejectBudget := len(me.CancelCh)
	if me.pendingCancel != nil {
		rejectBudget++
	}
	for {
		blocked := me.sliceBlockedNow()
		quotaLeft := me.cancelsSinceProgress < me.maxConsecutiveCancels
		free := me.free()

		var reject bool
		switch {
		case quotaLeft && free >= 1:
			reject = false // 처리
		case free == 0 || (blocked && !quotaLeft):
			reject = true
		default: // !quotaLeft && !blocked && free >= 1
			return // 이번 turn의 slice 또는 admission이 progress를 만든다. 꺼내지 않는다.
		}
		if reject && rejectBudget == 0 {
			return // 꺼내지 않는다
		}
		cmd, ok := me.takeCancel()
		if !ok {
			return
		}
		if reject {
			me.rejectCancel(cmd) // ErrCancelOrderBackpressured, 오더북 무변경, CancelBackpressured 관측
			rejectBudget--
			continue
		}
		me.processCancel(cmd)
		me.cancelsSinceProgress++
	}
}
```

두 조건이 겹치는 경우(`free == 0`이면서 `!quotaLeft && !blocked`)는 `switch` 순서상 **거절**이다.
`!blocked`인데 `free == 0`인 상태는 active sweep이 없거나 slice가 칸 없이도 시작 가능한 상태가 아니므로
실제로는 active sweep이 없을 때만 생긴다 — 이때 기다려도 progress가 칸을 만들지 않으므로 거절이 맞다.

- **거절이 한도를 쓰지 않는 이유**: parked turn은 admitPhase를 건너뛰고(`hadActive`) 조각도 없어서
  카운터가 풀리지 않는다. 거절이 한도를 쓰면 cancel phase가 영영 돌지 않아 취소가 1초 타임아웃으로
  떨어진다.
- **처리는 parked 중에도 한도를 쓰는 이유**: 취소는 빈자리 1이면 되고 조각은 137이 필요하다.
  parked 중 취소 처리에 한도가 없으면 취소가 빈자리를 계속 먹어 sweep이 영영 재개되지 못한다.
- **거절 예산을 phase 시작 길이로 묶는 이유**: 거절 중에도 새 취소가 계속 들어오면 한 turn이 끝나지 않는다.
- 하류가 칸을 계속 반환하는 한, 한도 소진 → 거절 → 소비로 칸 회복 → slice 재개 → 카운터 초기화 순으로
  진행하므로 교착과 sweep 기아가 없다.

### 4.2 성공 경로 순서와 응답 계약

```
1. free ≥ 1 확인 (§4.1)
2. handleCancel — 오더북 제거
3. 제거됐으면 markDirty, emitOrderCancelled — 방출 예산 1의 예약 구간(§8.2). 1에서 칸을 확인했으므로 막히지 않는다
4. ResponseCh로 결과 응답 — 논블로킹
```

현재(engine.go:417)는 응답을 이벤트보다 먼저 보낸다. 순서를 바꿔 "성공 응답을 받았는데 이벤트가 아직
enqueue되지 않았다" 상태를 없앤다.

**응답 채널 계약**

- `CancelOrder`는 호출자가 넘긴 `ResponseCh`를 **무시하고 항상 내부 버퍼 1 채널을 만든다.** 현재는
  `ResponseCh == nil`일 때만 만든다(engine.go:555). 호출자가 버퍼 없는 채널을 넘기면, 엔진의 논블로킹
  응답이 `CancelOrder`의 수신 select 진입보다 먼저 일어나 응답이 버려지는 경합이 생기기 때문이다.
  현재 `CancelOrder`에 `ResponseCh`를 넘기는 호출자는 없다(확인함).
- `CancelCh`에 직접 command를 넣는 경로는 테스트 전용이다(sweep 픽스처가 유일). 이 경로의 `ResponseCh`는
  nil이거나 버퍼 1 이상이어야 한다. 엔진은 논블로킹으로 보내므로 계약을 어긴 테스트는 응답을 못 받을 뿐
  엔진은 멈추지 않는다.
- `CancelOrderCommand.ResponseCh`의 필드 주석에 위 두 규칙을 적는다.

cancel worker는 바꾸지 않는다. `ErrCancelOrderBackpressured`는 `applyResult`의 default 분기
(`scheduleRetry`, 100ms→5s 백오프)로 간다. `CancelOrder`의 enqueue·response 1초 타임아웃도 그대로다.

## 5. 깨우기 프로토콜

- 샤드 엔진마다 `capacityCh chan struct{}`(버퍼 1)를 둔다. `NewMatchingEngine`이 만든다.
- 샤드 포워더(sharded.go `Start`의 execution 포워더)는 `shard.ExecutionCh`에서 이벤트를 **받은 직후**
  `capacityCh`에 논블로킹 send한다. 버퍼 1이라 신호가 뭉쳐도 포워더가 막히지 않는다.
- 포워더가 팬인 채널로 넘기는 send는 그대로 blocking이다. 포워더는 엔진 goroutine이 아니므로
  목표 1과 무관하고, 팬인 포화는 결국 샤드 채널 포화로 나타나 park로 흡수된다.
- 포워더가 없는 소비자(단일 `MatchingEngine`을 직접 읽는 테스트·서비스 통합 테스트)는 신호가 없다.
  ticker fallback으로 재개한다. 운영 경로가 아니므로 허용한다.
- 1ms polling은 쓰지 않는다. 장시간 장애에서 샤드마다 계속 깨어나는 비용이 있다.

## 6. 용량 검증

`QuantumConfig.Validate()`는 실제 채널 용량을 모르므로 거기서는 검증하지 않는다. 검증은 합산 없이
뺄셈으로 한다 — 큰 env 값에서 `maxMatchesPerTurn + 1 + maxConsecutiveCancels`가 `int` overflow로 음수가
되면 검증을 통과하고 실제 send에서 다시 멈추기 때문이다.

```go
// executionCapacity = cap(ExecutionCh). 두 quantum 값은 Validate()로 ≥ 1이 보장된 상태다.
func validateExecutionCapacity(executionCapacity, maxMatchesPerTurn, maxConsecutiveCancels int) error {
	if executionCapacity < 2 {
		return error // 취소 여유 1과 조각 1을 담을 수 없다
	}
	if maxConsecutiveCancels > executionCapacity-2 {
		return error // 우변(executionCapacity-1-maxConsecutiveCancels)이 1 미만이 된다
	}
	if maxMatchesPerTurn > executionCapacity-1-maxConsecutiveCancels {
		return error
	}
	return nil
}
```

- `NewShardedEngineWithQuantum`: 샤드마다 위 검증. 실패하면 error → main.go:88의 `log.Fatal`.
- `MatchingEngine.Start()`: `ExecutionCh != nil`이면 goroutine을 띄우기 전에 같은 검증, 실패 시 panic.
  테스트 픽스처가 채널을 바꿔 끼운 경우를 잡는 프로그래밍 오류 검사다.
- 기존 75% 워터마크는 유입 게이트로 유지한다. 기본값에서 워터마크의 남는 칸(256)이 조각 시작 조건(137)보다
  크다. quantum을 올려 이 관계가 뒤집히면 유입은 열려 있는데 조각은 park하는 구간이 생긴다 — 정확성
  문제는 아니며 기록만 한다.

## 7. 종료 계약

두 계약을 분리한다.

| 계약 | 상한 | 근거 |
|---|---|---|
| `Stop` 인지(`ShutdownLatched`) | 있음 — park 중에도 park select가 stop을 받는다 | §3.3 |
| `Done()` 닫힘 | **하류가 죽어 있으면 없음** — 드레인은 용량을 기다린다 | active sweep을 선점하지 않는다(quantum 설계) |

`ShutdownLatched`는 stop을 받는 세 경로 어느 것으로 들어오든 **정확히 한 번** 발생한다(`latchStop`).

하류가 죽은 채 shutdown하면 main의 30초 deadline(main.go:454) 뒤 프로세스가 끝나고, 다음 부팅의
bootstrap(DB 미체결 주문)과 outbox replay가 복구한다. 이 정책은 바꾸지 않는다. 이 복구 창은 §9.2 테스트
13으로 새로 고정한다.

## 8. 계측과 예약 위반 처리

### 8.1 관측

| 관측 | 형태 | 의미 |
|---|---|---|
| `ParkStarted func()` (신규) | 메트릭 없음 — 테스트 장벽용 | 6단계 `enterParkOnce`의 첫 park 진입(admission 직후 park 포함). 중간 깨어남에서 다시 호출하지 않는다 |
| `ParkDuration func(d time.Duration)` (신규) | `matching_engine_execution_capacity_park_seconds` | 첫 park 진입부터 slice 실제 재개까지 |
| `CancelBackpressured func()` (신규) | `matching_engine_cancel_backpressured_total` | §4.1 거절 수 |
| `ShutdownLatched func()` (신규) | 메트릭 없음 — 테스트 관측용 | stop latch 1회 |
| `EmitBlock`, `Slice(emitBlock)` (기존) | 유지 | enqueue 관측 시간의 낮은 분포를 보는 용도로만 쓴다. **0이 아니라는 이유로 위반으로 해석하지 않는다** — `time.Since`에는 클럭 해상도와 goroutine 선점 시간이 들어간다 |

`matching_engine_emit_block_seconds`의 Help 문구("This send has no timeout.")를 새 의미에 맞게 고친다.
기존 대시보드 이름은 바꾸지 않는다.

### 8.2 예약 위반은 fail-fast

예약이 틀리면(구현 결함) 이 설계가 없애려는 **조용한 엔진 정지**가 그대로 재발한다. 그래서 스케줄러
경로의 방출은 명시적인 예약 상태 안에서만 일어나고, 예약을 벗어나면 즉시 실패한다.

**시작 조건과 방출 예산은 다르다.**

| 작업 | 시작 조건(칸) | 방출 예산 `reservationRemaining` |
|---|---|---|
| 조각 | `maxMatchesPerTurn + 1 + maxConsecutiveCancels` (기본 137) | `maxMatchesPerTurn + 1` (기본 129) — 취소 여유분 제외 |
| 성공 취소 | 1 | 1 |

예산에 취소 여유분을 넣으면, 조각이 예상보다 많은 130번째 이벤트를 내도 취소 여유분을 침범할 뿐 panic하지
않아 결함이 숨는다.

**예약 상태**

```go
reservationActive    bool // 예약 구간 여부. remaining으로 추론하지 않는다.
reservationRemaining int
```

- 작업 시작: `reservationActive = true`, `reservationRemaining = 예산`.
- 예약 구간 안의 send(`sendExecution`이 `reservationActive`로 분기):
  - `reservationRemaining == 0`이면 panic — 예산보다 많이 방출하려 한다.
  - `select { case ExecutionCh <- ev: reservationRemaining--; default: panic(...) }` — 예산이 남았는데 칸이
    없으면 단일 writer 전제나 시작 조건 계산이 틀렸다.
  - default에서 이벤트를 **버리지 않는다** — 버리면 오더북과 DB가 어긋난다.
- 작업 종료: 남은 예산을 버리고 `reservationActive = false`.
- `remaining > 0`만으로 예약 여부를 판정하지 않는다. 예산을 넘는 이벤트가 나오는 순간 remaining이 0이 되어
  blocking `Match()` 경로로 잘못 빠지기 때문이다.
- 예약 구간 밖의 send(`reservationActive == false`, 즉 `Match()` 경로)는 기존 blocking send를 유지한다.
  `Match()`는 소비자가 동시에 읽는 테스트에서 채널이 순간적으로 가득 찰 수 있고, 거기서 panic하면 정상
  테스트가 깨진다.

**panic 이후에 보장하는 것과 보장하지 않는 것**

- 보장: 다음 부팅의 outbox replay와 DB 미체결 주문 bootstrap이 **데이터 일관성**을 복구한다. panic 시점에는
  조각의 앞쪽 이벤트가 이미 outbox에 커밋됐고 뒤쪽은 메모리에만 있을 수 있다 — 이 혼합 창을 §9.2 테스트 13이
  고정한다.
- 보장하지 않음: **진행성의 자동 복구.** 예약 계산 결함이 결정적이면 재기동 후 같은 주문에서 다시 panic해
  crash loop에 들어갈 수 있다. 결함을 운영에서 드러내는 대가다.
- panic은 전 샤드를 함께 끝낸다. 예약 결함은 테스트로 잡혀야 하는 프로그래밍 오류이므로, 운영에서 조용히
  멈추는 것보다 드러나는 쪽을 택한다.

## 9. 테스트 계획

모든 경계 테스트는 판별력을 위해 작은 quantum을 쓴다. 예: `maxMatchesPerTurn=4, maxConsecutiveCancels=2`
→ 조각 시작 조건 7. snapshot ticker는 테스트 목적에 맞게 명시한다(busy loop·재개 지연 테스트는 긴 ticker).

### 9.1 기존 테스트 대체

| 테스트 | 현재 기법 | 변경 |
|---|---|---|
| `engine_sweep_semantics_test.go` B-1~B-4 | cap 16을 소비자 없이 채워 엔진을 send에 묶음(`waitEmitSaturated`) | `maxMatchesPerTurn=1`로 조각을 최대화하고, 채널 용량을 조각 몇 개 뒤 `hasSliceCapacity`가 거짓이 되게 잡는다. **`ParkStarted`를 장벽으로** sweep이 조각 사이에 멈췄음을 확인 → 취소 투입 → 소비 재개 → 개수·순서 확인. 기존 의미 단언(not-found, 잔량 등록, 체결 회피, MarketOrderDone 1회)은 그대로. 취소가 거절되지 않도록 park 시점 `free ≥ 1`이고 한도가 남아 있음을 계산해 주석으로 남긴다 |
| `TestBackpressureBlocksIntakeNotLatchedOrder`, `TestStopDrainsQueuedOrderAndCancel` (cap 16, 기본 quantum) | 기본 quantum이 cap을 넘어 새 `Start()`에서 panic. 또 두 테스트 모두 `Start()` **뒤에** `ExecutionCh`를 직접 채운다(engine_scheduler_test.go:318, :398) — §2.3 단일 writer 전제 위반이고, §8.2 fail-fast에서는 엔진의 예약 검사와 테스트 write가 겹치면 panic할 수 있다 | 테스트 quantum을 cap과 호환되게 줄인다(예: (1,1) → 조각 시작 조건 3). 외부 채우기는 **`Start()` 전에만** 한다 — Start 후 write 대안은 두지 않는다(확인과 write 사이에 엔진 상태가 바뀌고 §2.3에 어긋난다). `TestBackpressureBlocksIntakeNotLatchedOrder`: 워터마크 미만 11칸을 Start 전에 채운다. `TestStopDrainsQueuedOrderAndCancel`: resting 주문을 Start 전에 book에 직접 배치하고 13칸도 Start 전에 채운 뒤, queued order·cancel과 shutdown drain만 검증한다. 원래 의도(워터마크 중 신규 유입 억제, 이미 접수된 주문·취소 드레인)는 유지된다. 각 단계에서 조각 시작 조건과 워터마크(12)가 유지되는지 계산해 주석으로 남긴다 |
| `TestSliceEmitBlockIncludesMarketOrderDone` (cap 4) | 마지막 MarketOrderDone send를 막아 slice emitBlock 포함을 확인 | send가 더는 막히지 않으므로 의도가 사라진다. "마지막 조각 전 대기는 `ParkStarted`→`ParkDuration`으로 관측되고, 그 조각은 MarketOrderDone까지 방출한다"로 대체. emitBlock 값의 exact 단언은 하지 않는다 |
| `engine_slice_test.go` cap 16 | `matchSlice` 직접 호출, `Start()` 없음 | 변경 없음(§2.3 범위 밖) |

### 9.2 신규 테스트

**엔진 단위 (`internal/matching`)**

1. **경계 — `+1` 누락을 잡는다.** 시장가 주문이 정확히 `maxMatchesPerTurn`건을 체결하고 같은 조각에서
   `MarketOrderDone`까지 내도록 book을 구성한다.
   - `free = (maxMatchesPerTurn + 1 + maxConsecutiveCancels) - 1`: 조각이 시작되지 않는다 — 오더북, 주문
     잔량, `tradeSeq`, 채널 길이 모두 불변. `ParkStarted` 1회.
   - `free = maxMatchesPerTurn + 1 + maxConsecutiveCancels`: 조각이 panic 없이 끝나고 Trade
     `maxMatchesPerTurn`건 + MarketOrderDone 1건이 순서대로 채널에 있다. `ParkDuration` 1회.
2. **취소 공간 0.** 대상 주문은 book에 남고 `ErrCancelOrderBackpressured`가 짧은 상한 안에 온다.
   `CancelBackpressured` 1회. 공간 1 회복 뒤 같은 취소는 제거되고 `OrderCancelled`는 정확히 1회.
3. **parked 중 한도 소진.** park 상태에서 취소 C+k건(C = `maxConsecutiveCancels`, 대상 주문 모두 book에
   있음)을 넣는다.
   - 첫 C건: 주문이 book에서 제거되고 `OrderCancelled`가 C건 방출된다.
   - C+1번째부터: 주문은 book에 그대로, 응답은 backpressure, `CancelBackpressured` k회, 이벤트 추가 없음.
   - 소비 재개 → sweep이 재개(`ParkDuration`)하고 끝까지 체결, 이후 거절됐던 취소를 다시 넣으면 제거된다.
4. **응답 계약.**
   - `CancelCh`에 버퍼 없는 `ResponseCh`를 가진 command를 넣고 읽지 않아도, 엔진은 다음 취소와 조각을 계속
     처리한다.
   - public `CancelOrder`에 버퍼 없는 `ResponseCh`를 넘겨도 결과를 정상 반환한다(내부 채널 사용).
5. **성공 응답 시점.** 성공 응답을 받은 순간 해당 `OrderCancelled`가 이미 채널에 있다.
6. **busy loop 부재.** 긴 ticker(예: 1s)로 park시킨 뒤 소비 없이 일정 시간(예: 300ms) 동안 `Turn` 관측 수가
   §3.4 상한(ticker 발화 0~1회, 신호 0, 취소 0 기준) 이하다.
7. **재개 지연.** 긴 ticker(예: 1s)의 `ShardedEngine`에서 `ParkStarted`로 park 진입을 먼저 확인하고, 그다음
   포워더 뒤 팬인을 소비해 칸을 반환한다. `ParkDuration`이 ticker 주기보다 훨씬 짧게 관측된다.
8. **shutdown 중 park.** 긴 ticker로 park시킨 뒤 `Stop()`:
   - `ShutdownLatched`가 짧은 상한 안에 정확히 1회.
   - 소비 없는 동안 stop 채널로 인한 추가 turn이 없다 — `Turn` 수가 ticker 발화·실제 wake 수 기반 상한 이하.
   - `Done()`은 닫히지 않는다.
   - 소비 재개 → sweep 완주 → `Done()` 닫힘, 이벤트 개수 일치, `ShutdownLatched`는 여전히 1회.
9. **용량 검증.** `validateExecutionCapacity`의 정확한 경계(통과하는 최대값, 1 초과), `executionCapacity < 2`,
   `maxConsecutiveCancels > cap-2`, `math.MaxInt` 입력(합산 overflow가 통과시키던 값)이 모두 기대대로.
   `NewShardedEngineWithQuantum` error와 `Start()` panic 경로 각 1건.
10. **`Match()` 계약.** 설계 전후로 `Match()`의 방출 개수·순서가 같다. 소비자가 동시에 읽는 작은 채널에서도
    panic하지 않는다(예약 구간 밖 blocking send).
11. **교차 샤드.** 2샤드 `ShardedEngine`에서 팬인을 소비하지 않는다. 먼저 팬인 채널 길이 == cap이고 대상
    샤드 A가 `ParkStarted`를 냈음을 확인한다. 그 상태에서:
    - 샤드 B의 로컬 칸이 남아 있으면 B의 취소는 성공한다.
    - B의 로컬 칸도 0이 되면 B의 취소는 backpressure로 제한 시간 안에 끝난다.
    - 어느 엔진도 panic하지 않고, 팬인 소비 재개 뒤 두 샤드 모두 완주한다.

**서비스 통합 (`internal/service`, 실제 PostgreSQL)**

12. **outbox 저장 실패 → 회복.** OutboxWriter repo가 오류를 내도록 주입한다.
    - 장벽: OutboxWriter가 현재 batch로 이미 꺼낸 이벤트와 채널 버퍼를 모두 채워 엔진이 실제로 park할 만큼
      체결을 만든다. `ParkStarted` 또는 `CancelBackpressured`를 **먼저 확인한 뒤에만** 회복시킨다 — 몇 건만
      만들면 저장이 실패해도 엔진은 park하지 않아 잘못된 구현도 통과한다.
    - 확인: 포화 중 취소가 backpressure로 재시도되고, 회복 뒤 취소 command가 PROCESSED, 체결·취소 이벤트
      유실·중복 0, 원장 검산 4종 통과.
13. **신규 — durable prefix + undurable suffix 복구.** 현재 `crashHook`은 matching 단위 테스트에서만 쓰이고,
    기존 서비스 취소 복구 테스트는 이 창을 덮지 않는다. §8.2 panic 직후와 같은 혼합 창을 재현한다.
    - **런타임 1**: 실제 PostgreSQL, 실제 outbox repo. 소비자는 **테스트 전용 종료 가능한 소비자**다 —
      `OutboxWriter`는 저장 실패를 무한 재시도하므로 같은 프로세스에서 버리면 goroutine이 샌다. 이 소비자는
      이벤트를 실제 repo로 커밋하되 정산으로 넘기지 않고(outbox 행 PENDING 유지), 정해진 건수 P 뒤 저장을 멈추고
      반환한다.
    - 지정가 taker 하나가 maker N개를 쓸어가는 sweep을 만든다(`maxMatchesPerTurn`을 작게 해 여러 조각). 커밋 건수
      == P이고 `ParkStarted`가 온 시점(prefix는 outbox, suffix는 채널·메모리에만 있음)을 장벽으로 확인한 뒤 crash
      hook으로 엔진을 끝낸다.
    - **런타임 2**: 새 엔진·실제 OutboxWriter·정산 파이프라인으로 main과 같은 순서로 부팅한다 — outbox
      replay(main.go:181) → DB 미체결 주문 bootstrap(main.go:312). replay가 prefix를 정산하고, bootstrap이 잔량이
      남은 taker와 남은 maker를 오더북에 올려 suffix를 재매칭한다. bootstrap의 정렬 기준은 계획서에서 코드로
      확인해 기대값을 정한다.
    - 확인 — **정확한 기대값으로 단언한다.** 서로 합이 맞는지만 보면 둘 다 0이거나 함께 부족해도 통과한다.
      - 최종 체결 수량 합과 quote 총액이 기대값과 정확히 일치
      - maker별 정확히 한 번 체결, 각 maker 최종 상태
      - taker 최종 상태와 잔량
      - 사용자별 USER_AVAILABLE·USER_LOCKED와 FEE_INCOME의 정확한 기대 잔액
      - 주문별 settlement·release 분개 개수
      - outbox 최종 상태: 미처리 행 0
      - 재부팅 전 커밋된 prefix 체결량 + 재부팅 후 재매칭된 suffix 체결량 == 기대 총량
      - 원장 검산 4종 통과 — 보조 근거다. 중복 복식분개도 균형은 맞으므로 단독 근거로 쓰지 않는다
    - `engine_event_id` 중복 0은 근거로 쓰지 않는다. 재기동 엔진은 새 ID를 만들어 경제적 중복도 서로 다른 ID를 갖는다.

### 9.3 검증 게이트

- `go build ./... && go vet ./...`
- `go test -count=20 ./internal/matching`
- Linux `go test -race ./internal/matching`
- quantum 선택 게이트(C1/C2/C4) 통과 — 기존 상한 유지
- 전체 백엔드: `go test -p 1 ./... -count=1`, Docker `-race -p 1 ./...`
- `BenchmarkTPS_*` 3회 — `Match()` 경로 무변경 확인용. 운영 경로 처리량과 500 VU 회귀는 이 설계의
  범위가 아니다(MSA 준비 목록 7번).

## 10. 범위 밖 · 후속

- 샤드별 outbox·팬인 분리로 진짜 장애 격리.
- `매칭 → 로컬 내구 outbox → Kafka relay` 경계. OutboxWriter가 이미 로컬 내구 경계이므로 relay는
  outbox 테이블을 읽는 별도 소비자로 붙인다.
- 운영 경로 벤치마크와 500 VU 회귀.
- park·거절 메트릭의 SLO·알림 연결.

## 11. 진행 순서

| # | 단계 | 체크포인트 |
|---|---|---|
| 1 | 이 설계 리뷰 | **CP 설계** |
| 2 | 구현 계획서 작성(테스트 먼저 — §9.1 대체와 §9.2 신규를 RED로) | — |
| 3 | 구현 | — |
| 4 | §9.3 게이트 전체, 리뷰 | **CP 구현** |

## 부록 A. 리뷰 반영 이력

- 1차 방향 리뷰: 정확성 조건과 정책 분리, 소비자 신호 깨우기, 응답 순서, 생성자 수준 용량 검증, `Match()` 범위, 종료 계약 분리.
- 2차 설계 리뷰: cancel phase 결정 순서 의사코드화, 동적 `sliceBlockedNow`, pendingOrder가 park를 막지 않음,
  `latchStop` 단일 전이, `ResponseCh` 내부 채널 계약, `ParkStarted`/`ParkDuration` 분리, EmitBlock exact-zero 해석
  폐기와 예약 위반 fail-fast, 뺄셈 기반 overflow 안전 검증, 테스트 판별력 보강(1·3·4·6·7·8·11·12), 테스트 13 신규화.
- 3차 설계 리뷰: park 진입 관측을 6단계 `enterParkOnce`로 일원화(admission 직후 park 포함), turn 상한 항을 실제 wake
  수로 정의, 예약 상태(`reservationActive`)와 방출 예산 129/1 분리, panic 후 데이터 복구와 진행성 비보장 구분,
  Start 후 외부 write 대안 제거, 테스트 13을 durable prefix + undurable suffix와 정확한 경제적 결과 단언으로 강화.
- 4차 리뷰: **설계 확정.** 계획서에 넣을 세부 사항 두 가지:
  - 예약 예산 소진 후 추가 send, 예산이 남았지만 채널이 준비되지 않은 send가 각각 panic하는 단위 테스트.
  - 테스트 13 런타임 1 종료 직전, 해당 심볼 outbox가 정확히 P건 PENDING이고 suffix 행이 없다는 단언.
    bootstrap 정렬은 `FindOpenOrdersForBootstrap`의 `created_at ASC, id ASC` — 픽스처가 `CreatedAt`과 ID를 명시적으로
    시드해 기대 체결 순서를 고정한다.
