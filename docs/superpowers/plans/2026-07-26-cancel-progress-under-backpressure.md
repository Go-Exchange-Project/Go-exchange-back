# 3차 ① 취소 진행성 확보(P2 완화) 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:executing-plans로 태스크별 실행.
> Steps use checkbox (`- [ ]`). superpowers:test-driven-development로 RED→GREEN.

**Goal:** 하류(`ExecutionCh`) 포화 시 엔진 루프가 신규 주문을 안 꺼내(nil-채널) 취소 emit용
헤드룸을 남기고, `IsIntakeAdmissible`에 하류 조건을 더한다. 별도 goroutine·히스테리시스 없음.
**완화이지 일반 보장 아님**(스펙 참조) — ⑤ 부하 한정 성공 기준 = 취소 인프라 실패 0.

**스펙 문서:** `docs/superpowers/specs/2026-07-26-cancel-progress-under-backpressure-design.md`

## Global Constraints

- 변경은 `engine.go`에 집중: `emitBackpressured` 헬퍼 + `Start` 루프 nil-채널 억제 + `IsIntakeAdmissible`
  하류 조건. `emitTrade`/매칭/`drainPendingWork`/shutdown 도미노·`sharded.go` 무변경.
- `0.75`는 측정용 초기 운영값(정합성 경계 아님) — 상수 주석에 성격 명시.
- **게이트 해제는 다음 snapshot ticker(테스트 2ms, 운영 100ms)에 재평가** — 테스트는 드레인 직후
  즉시 소비를 기대하지 말고 `assert.Eventually`(한 ticker 이상)로 확인.
- 기존 엔진·취소·샤딩·③ 테스트 무수정 그린 + `-race`. 커밋은 commit-message 스킬(author→reviewer, 한글).
- 통합 테스트 DSN(포트 55432). Bash 실패 시 PowerShell.

---

### Task 1: 하류 인지 게이트 (TDD)

**Files:**
- Modify: `internal/matching/engine.go`
- Test: `internal/matching/engine_test.go`

**Interfaces:**
- `emitBackpressured() bool`(내부). `IsIntakeAdmissible` 시그니처 불변(반환값에 하류 조건 AND).

- [x] **Step 1: RED 테스트 4종** — `engine_test.go`(기존 `newTestEngine()` snapshotInterval 2ms 사용):

```go
// (A) 게이트가 신규 주문을 억제 — 게이트 없는 코드에서 확실히 RED
func TestEngineSuppressesNewOrdersWhenExecutionBackpressured(t *testing.T) {
	me := newTestEngine()
	W := int(float64(cap(me.ExecutionCh)) * engineEmitHighWatermarkRatio)
	for i := 0; i < W; i++ { me.ExecutionCh <- ExecutionEvent{} } // 소비자 없이 W까지 채움
	// 체결을 만들지 않는 non-crossing limit buy(상대 매도 없음 → emit 없음)
	me.OrderCh <- &Order{ID: 1, CoinSymbol: "BTC", Side: model.OrderSideBuy,
		OrderType: model.OrderTypeLimit, Price: decimal.NewFromInt(100), Amount: decimal.NewFromInt(1)}
	me.Start()
	defer func() { me.Stop(); <-me.Done() }()

	time.Sleep(30 * time.Millisecond) // 여러 ticker 주기
	assert.Equal(t, 1, len(me.OrderCh), "게이트 켜진 동안 주문이 안 꺼내져야 함")

	for i := 0; i < W; i++ { <-me.ExecutionCh } // W 아래로 드레인
	assert.Eventually(t, func() bool { return len(me.OrderCh) == 0 }, time.Second, 3*time.Millisecond,
		"드레인 후 다음 ticker에 주문이 처리돼야 함")
}

// (B) 취소 진행 확인(실제 목적) + 억제 동시 단언
func TestEngineProcessesCancelsWhenExecutionBackpressured(t *testing.T) {
	me := newTestEngine()
	book := me.GetOrderBook("BTC")
	book.AddOrder(&Order{ID: 1, UserID: 100, CoinSymbol: "BTC", Side: model.OrderSideSell,
		OrderType: model.OrderTypeLimit, Price: decimal.NewFromInt(200), Amount: decimal.NewFromInt(1)})
	W := int(float64(cap(me.ExecutionCh)) * engineEmitHighWatermarkRatio)
	for i := 0; i < W; i++ { me.ExecutionCh <- ExecutionEvent{} } // 게이트 on, OrderCancelled emit 헤드룸 남김
	me.OrderCh <- &Order{ID: 2, CoinSymbol: "BTC", Side: model.OrderSideBuy, OrderType: model.OrderTypeLimit,
		Price: decimal.NewFromInt(100), Amount: decimal.NewFromInt(1)} // 게이트 동안 처리되면 안 됨
	me.Start()
	defer func() { me.Stop(); <-me.Done() }()

	done := make(chan CancelOrderResult, 1)
	go func() {
		done <- me.CancelOrder(CancelOrderCommand{CoinSymbol: "BTC", OrderID: 1,
			Side: model.OrderSideSell, Price: decimal.NewFromInt(200)})
	}()
	select {
	case res := <-done: // 1s 타임아웃보다 훨씬 짧은 넉넉한 마진 내 성공
		require.NoError(t, res.Err)
		assert.True(t, res.Removed)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("게이트 하에서 취소가 데드라인 내 처리되지 않음 (P2 재현)")
	}
	assert.Equal(t, 1, len(me.OrderCh), "게이트 동안 신규 주문은 유지돼야 함(억제 동시 증명)")
}

// (C) IsIntakeAdmissible 하류 조건
func TestIsIntakeAdmissibleFalseWhenExecutionBackpressured(t *testing.T) {
	me := NewMatchingEngine()
	assert.True(t, me.IsIntakeAdmissible("BTC")) // 하류 여유 → true
	W := int(float64(cap(me.ExecutionCh)) * engineEmitHighWatermarkRatio)
	for i := 0; i < W; i++ { me.ExecutionCh <- ExecutionEvent{} }
	assert.False(t, me.IsIntakeAdmissible("BTC"), "하류 포화면 OrderCh 비어도 false")
}

// (D) emitBackpressured 경계
func TestEmitBackpressuredBoundary(t *testing.T) {
	var nilEngine *MatchingEngine
	assert.False(t, nilEngine.emitBackpressured())
	me := NewMatchingEngine()
	assert.False(t, me.emitBackpressured()) // 빈 채널
	unbuffered := &MatchingEngine{ExecutionCh: make(chan ExecutionEvent)} // cap 0
	assert.False(t, unbuffered.emitBackpressured())
}
```

Run: `go test ./internal/matching/... -run 'Suppress|ProcessesCancelsWhenExecution|IntakeAdmissibleFalseWhenExecution|EmitBackpressured' -v` → FAIL/컴파일 실패(미구현).

- [x] **Step 2: 구현** — `engine.go`:

상수 + 헬퍼:
```go
// 0.75 is an operational starting point, not a correctness boundary.
// It reserves 256 slots at the default capacity to absorb execution
// events already being produced by the current order and cancellations.
const engineEmitHighWatermarkRatio = 0.75

func (me *MatchingEngine) emitBackpressured() bool {
	if me == nil || me.ExecutionCh == nil || cap(me.ExecutionCh) == 0 {
		return false
	}
	threshold := int(float64(cap(me.ExecutionCh)) * engineEmitHighWatermarkRatio)
	return len(me.ExecutionCh) >= threshold
}
```

`Start` 루프의 두 번째 select 앞에 nil-채널 억제(engine.go:148-153):
```go
			orderCh := me.OrderCh
			if me.emitBackpressured() {
				orderCh = nil // 하류 포화 — 신규 주문 억제, 취소 emit 헤드룸 확보
			}
			select {
			case cmd := <-me.CancelCh:
				me.processCancel(cmd)
			case order := <-orderCh:
				me.processOrder(order)
			case <-ticker.C:
				me.flushSnapshots()
			case <-me.stopCh:
				// ... 기존 그대로 ...
			}
```

`IsIntakeAdmissible`(engine.go:322)에 하류 조건 AND:
```go
func (me *MatchingEngine) IsIntakeAdmissible(coinSymbol string) bool {
	return len(me.OrderCh) < int(float64(cap(me.OrderCh))*orderIntakeHighWatermarkRatio) &&
		!me.emitBackpressured()
}
```

Run: 위 4종 → PASS.

- [x] **Step 3: 회귀 + race** — `go test ./internal/matching/... -count=1`(기존 엔진·취소·샤딩·③ 그린)
  + `go test ./internal/matching/... -race -count=1`. 특히 `TestEngineProcessesCancelsBeforeNewOrders`(③)는
  ExecutionCh를 안 채우므로 게이트 off로 무영향임을 확인.
- [x] **Step 4: Commit** — 초안: `feat(matching): 하류 포화 시 신규 주문 억제로 취소 진행성 확보 (3차 ①)`

---

### Task 2: 전체 검증 + 완료 문서 + README

**Files:**
- Create: `docs/refactor/17_3차①_취소_진행성_확보_완료.md`
- Modify: `docs/refactor/README.md`(3차 ① ✅)

- [x] **Step 1: 전체 검증** — `go build ./...` + `go vet` + `go test ./... -count=1`(통합 SKIP 0,
  DSN 55432) + `go test ./internal/matching/... ./internal/service/... ./cmd/... -race -count=1` → 전부 PASS.
  정산·부트스트랩·샤딩 통합 무수정 그린.
- [x] **Step 2: 완료 문서** — `17_3차①_취소_진행성_확보_완료.md`: 왜(⑤의 P2 = 엔진 emit 블로킹 →
  취소 굶주림) / 어떻게(하류 인지 게이트 헬퍼·nil-채널 억제·IsIntakeAdmissible 하류 조건, 별도
  goroutine 없이 순서 보존) / 결과(4 테스트, 회귀 그린). **완화이지 일반 보장 아님**(무제한 fan-out·
  하류 완전 정지 비보장), **게이트 해제 ~100ms 폴링 지연**, **정산 천장은 안 높임(②)**, **취소 0
  실증은 재측정(23번 재실행 or 후속)** 을 명기 — 수치 주장 금지.
- [x] **Step 3: README** — 3차 표 ① 🔨→✅ + 완료 문서 링크.
- [x] **Step 4: Commit + 푸시 + CI** — author→reviewer, `gh run watch` 그린.

---

## 다음 (범위 밖)

② BTC 처리량(pprof 진단 선행 — 정산 전달 vs OutboxWriter 바인딩 확정 후 워터마크식 정산 병렬화 or
엔진 분할). 일반 취소 진행 보장(주문당 emit 상한 or 재개형 매칭), 히스테리시스, 주문당 fan-out
메트릭, wake 신호는 측정 후 조건부 후속. **취소 인프라 실패 0 실증은 ③의 SLI(sli_cancel_success)로
23번 재측정에서 판정.**
