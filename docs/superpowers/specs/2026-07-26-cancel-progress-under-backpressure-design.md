# 3차 ① 하류 포화 시 신규 주문 억제로 취소 진행성 확보 설계

- **날짜**: 2026-07-26
- **상태**: 설계 검토 중
- **로드맵**: [3차 리팩토링 ①](../../refactor/README.md) — 취소 500(P2) 완화
- **근거**: 23번(⑤)에서 취소 48.7% 실패의 지배 원인은 P2 — 엔진 goroutine이 `emitTrade`의
  블로킹 `ExecutionCh` 송신에서 멈추면 ③의 취소 우선 select 자체가 안 돌아 취소가 굶는다.

## 주장의 범위 (먼저 정직하게)

이 설계는 **일반적 취소 진행 보장이 아니라 완화책**이다. 핵심 반례: **신규 주문 한 건이
무제한 체결을 만들 수 있다** — 매칭 루프(`matchSell`/`matchBuy`, engine.go:463 등)는 상대
주문을 만날 때마다 **동기식으로 `ExecutionCh`에 emit**(engine.go:458)하고, 체결 수 상한이
없다. 예: `ExecutionCh`가 767일 때 신규 주문 하나를 꺼냈는데 300개 maker와 체결되면, 257번째
emit에서 채널이 꽉 차 엔진이 **다시 멈춘다 — 루프 상단의 게이트로 돌아오기도 전에.**

- **채택**: 하류 인지 admission gate / 엔진 루프의 신규 주문 억제 / 별도 goroutine 없이 순서 보존.
- **보장(목표)**: **현재 ⑤ 부하에서 취소 타임아웃 제거.** ⑤ k6는 주문·maker 수량이 대부분
  `0.001`이라 주문 하나당 체결 수가 사실상 작다 → 이 부하에선 매우 효과적일 가능성이 높다.
- **비보장**: 임의 fan-out 주문(한 주문이 대량 체결) + 하류 완전 정지(드레인 0).
- **후속(일반 보장의 전제)**: 주문당 최대 emit 수를 제한하거나, 매칭을 **재개 가능한 단위로
  쪼개기** 전에는 일반적 취소 진행 보장은 불가.

즉 제목은 "취소 500 완전 제거"가 아니라 **"하류 포화 시 신규 주문 억제로 취소 진행성 확보"**
이고, 실증에서 취소 타임아웃 0건이면 **그 범위 안에서** 성공으로 판정한다.

## 설계

**핵심**: 엔진이 `ExecutionCh <-`에서 막힐 확률을 크게 줄인다 — 하류가 밀리면 신규 주문을 안
꺼내 취소/종결 emit용 헤드룸을 남긴다. 별도 goroutine 없음, 방출 순서(정산 의존)는 직접
ordered 송신 유지로 자동 보존.

### 1. 게이트 헬퍼 (두 게이트가 공유)

```go
// 0.75 is an operational starting point, not a correctness boundary.
// It reserves 256 slots at the default capacity to absorb execution
// events already being produced by the current order and cancellations.
const engineEmitHighWatermarkRatio = 0.75

func (me *MatchingEngine) emitBackpressured() bool {
	if me == nil || me.ExecutionCh == nil || cap(me.ExecutionCh) == 0 {
		return false // 버퍼 없는/미설정 채널은 억제 대상 아님
	}
	threshold := int(float64(cap(me.ExecutionCh)) * engineEmitHighWatermarkRatio)
	return len(me.ExecutionCh) >= threshold
}
```

### 2. 엔진 루프 신규 주문 억제 (Start, ③ 우선 취소 뒤)

하류가 밀리면 **OrderCh를 nil로** 비활성화 → 신규 주문을 안 꺼냄. 취소·ticker·stop만 처리.
취소는 남은 헤드룸(`cap−W`)으로 emit.

```go
orderCh := me.OrderCh
if me.emitBackpressured() {
	orderCh = nil // 신규 주문 억제 — 취소 emit 헤드룸 확보 (nil 케이스는 발화 안 함)
}
select {
case cmd := <-me.CancelCh: me.processCancel(cmd)
case order := <-orderCh:   me.processOrder(order)
case <-ticker.C:           me.flushSnapshots()
case <-me.stopCh:          /* graceful shutdown 그대로 — drainPendingWork는 별도 경로 */
}
```

### 3. 하류 인지 입장 게이트 (IsIntakeAdmissible)

OrderCh 조건 + 하류 조건 → 밀리면 문에서 빠른 503(④ Retry-After), DB 홀드 작업 전에 셰딩.

```go
return len(me.OrderCh) < int(float64(cap(me.OrderCh))*orderIntakeHighWatermarkRatio) &&
	!me.emitBackpressured()
```

### 동작

`ExecutionCh ≥ 75%`면 엔진이 신규 주문을 멈추고(취소는 계속) OutboxWriter가 계속 드레인 →
`ExecutionCh`가 cap 아래로 → 취소 emit이 room을 얻어 `CancelCh` 1초 타임아웃을 피한다. **단
위 "반례"대로 진행 중인 한 주문의 fan-out이 헤드룸을 초과하면 여전히 막힐 수 있다** — ⑤
부하에선 fan-out이 작아 실효적.

처리량은 불변(여전히 정산 한계 = ②) — ①은 "정지"를 "우아한 백오프"로 바꿀 뿐.

**ShardedEngine**: 샤드별 MatchingEngine이라 게이트·IsIntakeAdmissible이 샤드마다 자동 적용
(`sharded.go` 무변경). 단일 심볼 BTC(⑤)는 한 샤드.

## 검증 계획 (TDD — RED이 확실한 두 테스트)

기존 테스트를 재검토: **취소와 주문을 동시에 넣으면 ③의 상단 우선 select가 이미 취소를 먼저
처리**하므로 확실한 RED가 안 된다. 대신:

1. **게이트가 신규 주문을 억제**(게이트 없는 코드에서 확실히 RED):
   - `ExecutionCh`를 정확히 W까지 채움 → `OrderCh`에 **주문만** 주입 → 일정 시간 `OrderCh`
     길이 유지(주문이 안 꺼내짐) 단언 → 이후 `ExecutionCh`를 W 아래로 드레인 → 주문 처리 확인.
2. **취소 진행 확인**(실제 목적 검증):
   - 게이트가 켜진 상태에서 오더북에 기존 주문 시드 → `CancelOrder` 호출 → **1초 전에 성공**
     확인 → 동시에 신규 주문은 처리되지 않음 확인.
3. `IsIntakeAdmissible` 단위: `ExecutionCh ≥ W`면 OrderCh 비어도 false; 아래면 true.
4. `emitBackpressured` 경계: 버퍼 없는 채널(`cap==0`)·nil이면 false.
5. 회귀: 기존 엔진·취소·샤딩·③ 테스트 그린. graceful shutdown(`drainPendingWork` 별도 경로) 무영향.

## 0.75와 히스테리시스

- `0.75`는 **측정용 초기 운영값**(정합성·진행 보장에서 도출된 값 아님) — cap 1,024 기준 헤드룸
  256. 이름·주석에 성격을 드러낸다(위 상수 코멘트).
- 75% 경계에서 767↔768 진동 가능하나 **히스테리시스는 넣지 않는다** — 실측에서 빈번한
  on/off가 확인될 때만 low-watermark 추가(④의 측정 기반 원칙과 동일).

## 후속 관측 (일반화 전에 재측정 시 필요)

이 완화의 실제 효과·한계를 재측정에서 관측: **주문 한 건당 execution event 최대·p95·p99** /
게이트 진입 횟수 / 게이트 상태 지속 시간 / `ExecutionCh` 최대 점유율 / 게이트 진입 후에도 발생한
blocked emit / 취소 타임아웃 수. (기본 관측은 기존 `matching_engine_channel_length{execution}`
게이지 + ③ 취소 SLI + ④ `admission_rejected`로 커버; 주문당 fan-out 지표는 후속 신설.)

## 검토한 대안

- **B) emit 경로 분리(내부 버퍼 + 방출 goroutine)**: 내부 버퍼는 OutboxWriter와 중복이고,
  버퍼가 차면 엔진→버퍼 핸드오프가 새 블록 지점이라 결국 같은 게이트가 필요하며, 두 채널
  우선순위는 방출 순서를 뒤바꿔 정산 순서 불변식을 깰 위험. 기각(A가 더 적은 코드·순서 보존).
- **채널/타임아웃 확대**: 장애 시점을 늦출 뿐 근본 아님. 기각.

## 범위 밖 / 후속

- **일반적 취소 진행 보장**: 주문당 최대 emit 제한 또는 매칭을 재개 가능 단위로 분할(별도 설계).
- ② BTC 처리량(pprof 진단 선행), 히스테리시스(측정 후 조건부), 주문당 fan-out 메트릭 신설.
