# ③ 취소 우선 스케줄링 구현 계획 (2차 리팩토링 · 가용성)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development(권장) 또는 superpowers:executing-plans로 태스크별 실행. Steps use checkbox (`- [ ]`) syntax.

**Goal:** 엔진 루프의 동등 `select`를 priority select로 바꿔 취소를 신규 주문보다 먼저 드레인함으로써 취소 굶주림(22번 43.9% 실패의 P1)을 구조적으로 없앤다.

**Architecture:** 변경은 `MatchingEngine.Start`의 `select` 한 곳뿐이다. 2단계 select — 먼저 `CancelCh`를 논블로킹으로 확인해 대기 취소를 전부 드레인(`continue`), 없으면 `default`로 빠져 기존 블로킹 `select`(주문/취소/ticker/stop). 오더북 단일 writer 제약상 취소는 엔진 goroutine에서만 실행 가능하므로 goroutine 내 우선순위가 유일한 방법.

**Tech Stack:** Go (채널 select).

**스펙 문서:** `docs/superpowers/specs/2026-07-20-cancel-priority-scheduling-design.md`

## Global Constraints

- 변경은 `MatchingEngine.Start`의 select 한 곳만. `processOrder`/`processCancel`/`drainPendingWork`/`emit*`/shutdown 도미노 전부 무변경.
- `CancelCh` 버퍼(1024)·`OrderService.CancelOrder`의 1초 send-timeout 무변경(P2 안전 밸브).
- ShardedEngine 무수정(각 샤드가 이 엔진을 씀).
- 기존 엔진 단위·동시성 테스트가 무수정 그린이어야 한다(도미노·처리 로직 불변의 증거).
- 취소 실패율 실증은 ⑤(23번). **성공 기준은 "취소가 신규 주문보다 먼저 처리됨을 결정론적으로 증명 + 회귀 그린"까지** — 실패율 수치 주장 금지.
- 커밋은 태스크 단위 commit-message 스킬(author→reviewer, 한글). Bash 실패 시 PowerShell.

---

### Task 1: priority select + 결정론적 우선순위 테스트

**Files:**
- Modify: `internal/matching/engine.go` (`Start`의 select)
- Test: `internal/matching/engine_test.go`

**Interfaces:**
- 변경 없음(내부 스케줄링만). `Start`/`Stop`/`Done`/`ExecutionCh` 등 기존 시그니처 그대로.

- [x] **Step 1: 실패 테스트** — 취소가 신규 주문보다 먼저 `ExecutionCh`에 방출되는지 결정론적으로 검증. 취소 대상(price 200 매도)과 매칭 대상(price 100 매도)을 분리 시드하고, 채널을 **미리 채운 뒤** 기동:

```go
func TestEngineProcessesCancelsBeforeNewOrders(t *testing.T) {
	me := NewMatchingEngine() // 아직 Start 안 함 — 채널 선주입 가능(버퍼 1024)
	book := me.GetOrderBook("BTC")
	const M, N = 5, 5

	// 취소 대상: price 200 매도 M건(ID 1..M).
	for i := 1; i <= M; i++ {
		book.AddOrder(&Order{ID: uint(i), UserID: 100, CoinSymbol: "BTC",
			Side: model.OrderSideSell, OrderType: model.OrderTypeLimit,
			Price: decimal.NewFromInt(200), Amount: decimal.NewFromInt(1)})
	}
	// 매칭 대상: price 100 매도 N건(ID 1001..). 들어올 매수와 체결돼 Trade 방출.
	for i := 1; i <= N; i++ {
		book.AddOrder(&Order{ID: uint(1000 + i), UserID: 101, CoinSymbol: "BTC",
			Side: model.OrderSideSell, OrderType: model.OrderTypeLimit,
			Price: decimal.NewFromInt(100), Amount: decimal.NewFromInt(1)})
	}
	// CancelCh 선주입: price 200 매도 취소 M건(Removed=true → OrderCancelled 방출).
	for i := 1; i <= M; i++ {
		me.CancelCh <- CancelOrderCommand{CoinSymbol: "BTC", OrderID: uint(i),
			Side: model.OrderSideSell, Price: decimal.NewFromInt(200)}
	}
	// OrderCh 선주입: price 100 매수 N건 → price 100 매도와 체결 → Trade N건.
	for i := 1; i <= N; i++ {
		me.OrderCh <- &Order{ID: uint(2000 + i), UserID: uint(200 + i), CoinSymbol: "BTC",
			Side: model.OrderSideBuy, OrderType: model.OrderTypeLimit,
			Price: decimal.NewFromInt(100), Amount: decimal.NewFromInt(1)}
	}

	me.Start()
	defer func() { me.Stop(); <-me.Done() }()

	kinds := make([]string, 0, M+N)
	for len(kinds) < M+N {
		select {
		case ev := <-me.ExecutionCh:
			switch {
			case ev.OrderCancelled != nil:
				kinds = append(kinds, "cancel")
			case ev.Trade != nil:
				kinds = append(kinds, "trade")
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out after %d events: %v", len(kinds), kinds)
		}
	}
	// 우선순위 실증: 첫 M개가 전부 cancel(동등 select였다면 섞였을 것).
	for i := 0; i < M; i++ {
		assert.Equalf(t, "cancel", kinds[i], "first %d events must be cancels (priority), got %v", M, kinds)
	}
}
```

- [x] **Step 2: 실패 확인** — Run: `go test ./internal/matching/... -run TestEngineProcessesCancelsBeforeNewOrders -v` → FAIL(현재 동등 select라 cancel/trade가 섞여 첫 M개가 전부 cancel이 아님). **주의**: 무작위 select라 드물게 우연히 통과할 수 있으니, 실패를 확실히 보려면 `-count=5`로 반복 실행해 최소 한 번은 FAIL 나는지 확인.

- [x] **Step 3: 구현** — `Start`의 select를 2단계 priority select로 교체(engine.go:140-160):

```go
func (me *MatchingEngine) Start() {
	go func() {
		defer close(me.doneCh)
		ticker := time.NewTicker(me.interval())
		defer ticker.Stop()
		for {
			// 취소 우선: 대기 중 취소를 먼저 논블로킹으로 전부 드레인.
			select {
			case cmd := <-me.CancelCh:
				me.processCancel(cmd)
				continue
			default:
			}
			// 취소가 없을 때만 주문/ticker/stop.
			select {
			case cmd := <-me.CancelCh:
				me.processCancel(cmd)
			case order := <-me.OrderCh:
				me.processOrder(order)
			case <-ticker.C:
				me.flushSnapshots()
			case <-me.stopCh:
				me.drainPendingWork()
				me.flushSnapshots()
				if me.ExecutionCh != nil {
					close(me.ExecutionCh)
				}
				close(me.SnapshotCh)
				return
			}
		}
	}()
}
```

- [x] **Step 4: 통과 + 회귀** — Run: `go test ./internal/matching/... -run TestEngineProcessesCancelsBeforeNewOrders -count=5 -v` → PASS(5회 전부 — 결정론적). `go test ./internal/matching/... -race -count=1` → 기존 엔진 단위·동시성·샤딩 테스트 무수정 그린.

- [x] **Step 5: Commit** — 초안: `perf(matching): 엔진 루프를 취소 우선 스케줄링으로 전환 (2차 ③)`

---

### Task 2: 전체 검증 + 완료 문서 + README

**Files:**
- Create: `docs/refactor/13_2차③_취소_우선_스케줄링_완료.md`
- Modify: `docs/refactor/README.md`(2차 ③ ✅)

- [x] **Step 1: 전체 검증** — `go build ./...` + `go vet` + `go test ./... -count=1`(통합 SKIP 0) + `go test ./internal/matching/... ./internal/service/... ./cmd/... -race -count=1` → 전부 PASS. 특히 취소 라우팅(ShardedEngine)·정산·부트스트랩 통합 무수정 그린.

- [x] **Step 2: 완료 문서** — `13_2차③_취소_우선_스케줄링_완료.md`: 왜(22번 취소 43.9% 실패의 P1=select 불공정) / 어떻게(엔진 루프 2단계 priority select, 오더북 단일 writer 제약상 유일한 구조적 방법, CancelCh 버퍼·타임아웃·ShardedEngine 무변경) / 결과(결정론적 우선순위 테스트, 회귀 그린, **취소 실패율 실증은 ⑤/23번 병기 — 수치 주장 금지**). P2(엔진 정지)는 ①②④의 몫임을 명기.

- [x] **Step 3: README** — 2차 표 ③ 🔨→✅ + 완료 문서 링크.

- [x] **Step 4: Commit + 푸시 + CI** — author→reviewer, `gh run watch` 그린.

---

## 다음 (범위 밖)

④(입장 정책 정교화 — ①의 엔진-유입 게이트 + ②의 코디네이터 입력 만석 두 입장 지점 통합·Retry-After·히스테리시스), ⑤(23번 실증 — 스파이크에서 취소 실패율·주문 접수 p95를 ①②③④ 종합으로 측정). ③의 취소 우선순위 효과는 ⑤에서 취소 500율 0으로 확인.
