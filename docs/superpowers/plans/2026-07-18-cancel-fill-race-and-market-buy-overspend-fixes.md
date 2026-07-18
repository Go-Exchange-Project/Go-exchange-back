# 정합성 버그 2건 수정 구현 계획: 취소-체결 레이스(A-4) + 시장가 매수 초과 소진

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:test-driven-development(버그 재현 테스트 먼저) + superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** 로컬 스모크(`loadtest/order-spike-single-symbol.js`)가 처음 실증한 정합성 버그 2건을 근본 수정한다. 버그 2(시장가 매수 초과 소진)를 먼저, 버그 1(A-4 취소-체결 레이스, **취소 시퀀서화**)을 나중. 완료 후 22번 벤치마크를 재개할 수 있다.

**설계 문서:** `docs/superpowers/specs/2026-07-18-cancel-fill-race-and-market-buy-overspend-fixes-design.md`
**발견 문서:** `docs/refactor/bug-findings-2026-07-17-cancel-fill-race-and-market-buy-overspend.md`

## Global Constraints

- **TDD 필수**: 각 버그마다 재현 테스트(현재 코드에서 실패)를 먼저 쓰고, 통과하도록 수정한다.
- **기존 계약 테스트 무수정 그린**: 매칭 단위·동시성, outbox writer·리플레이·크래시, 정산 통합, graceful shutdown 도미노. 테스트를 고쳐야 통과한다면 동작을 바꾼 것이니 재검토.
- **자금 정합성**: 각 수정 후 리컨실리에이션(A-6) 위반 0을 통합 테스트로 확인.
- 통합 테스트: 테스트 DB + DSN(포트 55432), `-v`로 SKIP 0 확인. 컨테이너 죽으면 `docker compose -f docker-compose.test.yml up -d --wait`(이름 충돌 시 `docker start goexchange-postgres-test`).
- 커밋은 태스크 단위, `commit-message` 스킬(author→reviewer), 한글.
- 로컬 셸: Bash 도구가 셸 프로파일 문제로 실패하면 PowerShell 도구로 전환, 커밋 서브에이전트에는 git diff/log를 프롬프트로 전달 + Read로 파일 검증.

---

# 버그 2: 시장가 매수 초과 소진 (먼저 — 작고 독립적)

### Task 2A: 엔진 근본 수정 — 예산 초과 체결 차단

**Files:** `internal/matching/engine.go`, `internal/matching/engine_test.go`(또는 market 테스트 파일)

- [ ] **Step 1: 재현 테스트** — 다중 가격 레벨(1~5틱 분산) 오더북에 MARKET BUY를 예산 소진까지 체결시켜, **현재 코드에서** 클램프 체결 후 `order.QuoteAmount < 0`(또는 `FilledQuoteAmount > budget`)이 되는 것을 단언 → FAIL(버그 실재 확인). 재현이 안 되면 발견 문서의 정확한 가격·수량 조합(50000 기준가, epsilon +1.8775e-9)을 참고해 조합을 맞춘다.
- [ ] **Step 2: 근본 수정** — `matchMarketBuy`의 클램프 체결(`maxQtyByQuote < sellOrder.Amount`)에서 `price × tradeQty ≤ order.QuoteAmount`가 되도록 quantity를 내림. 정확한 decimal 기법은 Step 1 테스트로 확정(후보: `budget.Sub(budget.Mod(price)).Div(price)` floor, 또는 초과 시 quantity 조정 — 부동 정밀도 함정 주의). 불변식: 클램프 후 `order.QuoteAmount ≥ 0` 항상.
- [ ] **Step 3: 통과 + 회귀** — 재현 테스트 PASS. `go test ./internal/matching/... -race -count=1` → 기존 매칭·동시성 테스트 무수정 그린(가격-시간 우선순위·체결량 계약 불변의 증거). `matchMarketSell`은 건드리지 않았음을 확인.
- [ ] **Step 4: Commit** — 초안: `fix(matching): 시장가 매수 예산 소진 체결의 반올림 초과 차단`

### Task 2B: 안전망 — 실패 기록이 CHECK를 위반하지 않게

**Files:** `internal/service/failed_market_completion_service.go`, 통합 테스트. 점검: `failed_settlement_service.go`.

- [ ] **Step 1: 재현 테스트(통합)** — 음수 `RemainingQuoteAmount`로 `RecordFailure` 호출 → **현재** CHECK 제약 위반(`ck_failed_market_completions_remaining_quote_non_negative`) INSERT 실패 → FAIL.
- [ ] **Step 2: clamp** — `RecordFailure`(또는 입력 생성 지점)에서 `RemainingQuoteAmount = decimal.Max(0, input)`. 저장 전 clamp로 CHECK 위반 원천 차단.
- [ ] **Step 3: 동종 결함 점검** — `FailedSettlementService.RecordFailure`와 그 model CHECK 제약(`failed_settlements`)에 같은 "기록 입력이 제약 위반 가능" 패턴이 있는지 점검. 있으면 같은 방식 clamp + 테스트, 없으면 점검 결과를 완료 문서에 한 줄 기록.
- [ ] **Step 4: 통과 + 회귀** — 재현 테스트 PASS(CHECK 위반 없이 저장), 기존 failed_* 테스트 그린.
- [ ] **Step 5: Commit** — 초안: `fix(settlement): 시장가 완료 실패 기록의 음수 잔여 quote clamp로 이중 실패 차단`

### Task 2C: E2E 재현 확인

- [ ] **Step 1** — 로컬 백엔드 + 임시 DB로 다중 레벨 오더북 만들고 MARKET BUY 분할 체결 → `failed_market_completions` 0건, 시장가 주문 FILLED, 리컨실리에이션 위반 0을 SQL로 확인(수정 전엔 로그에 이중 실패, 후엔 클린).

---

# 버그 1: 취소-체결 레이스 (A-4) — 취소 시퀀서화

### Task 1A: outbox 이벤트 타입 `ORDER_CANCELLED` + 마이그레이션

**Files:** `internal/model/trade_outbox_event.go`, `migrations/00X_order_cancelled_event.sql`(신규), `internal/testdb/`(필요 시)

- [ ] **Step 1** — `TradeOutboxEventTypeOrderCancelled = "ORDER_CANCELLED"` 추가. model의 `ck_trade_outbox_event_type` CHECK 태그에 `'ORDER_CANCELLED'` 추가.
- [ ] **Step 2** — 마이그레이션 파일: 기존 CHECK를 DROP → `IN ('TRADE','MARKET_ORDER_DONE','ORDER_CANCELLED')`로 재생성(gorm AutoMigrate는 기존 CHECK 미갱신이므로 필수). 기존 마이그레이션 번호 규칙 확인 후 다음 번호.
- [ ] **Step 3** — `go build` + 마이그레이션이 테스트 DB에 적용되는지(`dbmigration.Up`) 통합 경로로 확인. 기존 outbox 행 호환.
- [ ] **Step 4: Commit** — 초안: `feat(outbox): ORDER_CANCELLED 이벤트 타입과 CHECK 제약 마이그레이션 추가`

### Task 1B: 엔진 — 취소 이벤트 방출

**Files:** `internal/matching/engine.go`, `internal/matching/engine_test.go`

- [ ] **Step 1: 실패 테스트** — 취소(Removed=true) 시 `ExecutionCh`에 `OrderCancelled` 이벤트가 방출되고, 완전 체결 주문 취소(Removed=false)면 방출 없음을 단언 → FAIL.
- [ ] **Step 2: 구현** — `ExecutionEvent`에 `OrderCancelled *OrderCancelled` 필드 + `OrderCancelled{OrderID,CoinSymbol,Side}` 타입. `emitOrderCancelled`. `processCancel`이 `result.Removed`일 때 방출(선행 체결들 뒤에 enqueue — 단일 스레드 순서). `EngineEventID` 멱등 키(trade와 동일 규칙, `nextTradeEvent` 계열).
- [ ] **Step 3: 통과 + 순서 테스트** — 부분 체결 후 잔여 취소 시 ExecutionCh에서 "체결들 → OrderCancelled" 순서 단언. `go test ./internal/matching/... -race` 그린(기존 취소 테스트 포함).
- [ ] **Step 4: Commit** — 초안: `feat(matching): 취소 시 OrderCancelled 실행 이벤트 방출 (A-4)`

### Task 1C: outbox 인코딩/디코딩 + 리플레이 라우팅

**Files:** `internal/service/outbox_writer.go`(`NewTradeOutboxEvent`, `ExecutionEventFromOutbox`), 테스트

- [ ] **Step 1: 실패 테스트** — `NewTradeOutboxEvent(OrderCancelled 이벤트)` → 올바른 타입·페이로드 행, `ExecutionEventFromOutbox` 왕복 복원 → FAIL.
- [ ] **Step 2: 구현** — 두 함수에 `ORDER_CANCELLED` 분기(JSON 마샬/언마샬, trade·MarketOrderDone 패턴 그대로).
- [ ] **Step 3: 통과 + 회귀** — outbox writer·리플레이 기존 테스트 무수정 그린.
- [ ] **Step 4: Commit** — 초안: `feat(outbox): OrderCancelled 이벤트 인코딩/디코딩 추가`

### Task 1D: 정산 파이프라인 — `ProcessOrderCancellation`

**Files:** `internal/service/order_service.go`(신규 메서드), `cmd/main.go`(`processExecutionEvent` 분기), 통합 테스트

- [ ] **Step 1: 실패 테스트(통합)** — 미체결/부분체결 주문에 `ProcessOrderCancellation` → 잔여 hold 정확 해제 + CANCELLED + 원장 기록, 멱등(2회 호출 no-op), 이미 FILLED면 no-op → FAIL.
- [ ] **Step 2: 구현** — `ProcessOrderCancellation(OrderCancelled)`: 주문 FOR UPDATE → status가 이미 CANCELLED/FILLED면 no-op(멱등) → 잔여 = Amount − FilledAmount 계산(선행 체결 정산이 이미 반영됐으므로 정확) → `releaseOrderHold` → CANCELLED 커밋. `processExecutionEvent`(cmd/main.go)에 `event.OrderCancelled != nil` 분기 추가(handled/markedInTx 계약 준수 — trade 경로와 동일하게 outbox 흡수 마킹 가능하면 적용).
- [ ] **Step 3: 통과 + 회귀** — 통합 테스트 PASS. 기존 정산 통합·리플레이 그린. 리컨실리에이션 위반 0.
- [ ] **Step 4: Commit** — 초안: `feat(settlement): OrderCancelled 이벤트로 주문 취소를 정산 파이프라인에서 확정 (A-4)`

### Task 1E: CancelOrder API 접수 기반으로 전환

**Files:** `internal/service/order_service.go`(`CancelOrder`), `internal/handler/order_handler.go`, 테스트

- [ ] **Step 1: 실패 테스트** — CancelOrder가 DB에서 hold 해제·CANCELLED를 **하지 않고**(파이프라인 위임), 검증(소유권·취소가능·시장가 금지)만 수행 후 엔진에 커맨드 전송하고 Removed 여부로 접수 응답. Removed=false면 409("이미 체결"). 500 매핑 제거 → FAIL.
- [ ] **Step 2: 구현** — `CancelOrder`: 트랜잭션은 검증 전용(FOR UPDATE로 소유권·상태 확인, hold 해제·CANCELLED 커밋 제거). 엔진 커맨드 → Removed=true면 "접수" 응답, Removed=false면 409. 핸들러의 `result.Status=="CANCELLED"→500` 분기 제거, 409 매핑. 중복 취소 요청은 멱등(Task 1D가 흡수)으로 안전함을 테스트.
- [ ] **Step 3: 레이스 재현 → 해소(통합, 이 태스크의 핵심)** — 발견 문서 시나리오(체결 in-flight 중 취소)를 결정론적으로 재현: 시퀀서화 전 로직이면 `failed_settlements` 발생, 시퀀서화 후엔 **0**. 취소 API도 500 없이 200/409. 
- [ ] **Step 4: 통과 + 회귀** — 전체 정산·핸들러 테스트 그린.
- [ ] **Step 5: Commit** — 초안: `fix(order): 취소를 엔진 시퀀스 접수 기반으로 전환해 취소-체결 레이스 제거 (A-4)`

### Task 1F: 크래시 내구성 + graceful shutdown

**Files:** 크래시 주입 통합 테스트(A-3 패턴 계승), 부트스트랩 확인

- [ ] **Step 1: 크래시 후(커밋 후)** — 취소 이벤트 outbox 커밋 후 크래시 주입 → 부팅 리플레이가 `ProcessOrderCancellation` 재처리 → CANCELLED 완결, 부트스트랩이 재투입 안 함(리플레이 먼저 → 부트스트랩 나중 순서). A-3 결정론적 주입 패턴.
- [ ] **Step 2: 크래시 전(커밋 전)** — 취소 이벤트 커밋 전 크래시 → 주문 유효(취소 미접수), 부트스트랩 재투입 정상.
- [ ] **Step 3: shutdown 도미노** — 취소 이벤트도 ExecutionCh 드레인 대상임을 확인(엔진 Stop → 잔여 취소 이벤트 flush → 정산 처리). 기존 도미노 테스트 그린.
- [ ] **Step 4: Commit** — 초안: `test(order): 취소 시퀀서화의 크래시 내구성·드레인 검증 추가`

---

# 마무리

### Task Z: 문서 + README + 푸시

- [ ] **Step 1: 전체 검증** — `go build ./...` + `go vet` + `go test ./... -count=1`(통합, SKIP 0) + `go test ./internal/matching/... ./internal/service/... ./cmd/... -race -count=1` 전부 PASS.
- [ ] **Step 2: 완료 문서** — `docs/refactor/`에 완료 문서 추가(왜: 발견 문서 링크 / 어떻게: 버그2 3계층·버그1 시퀀서화 요지 / 결과: 재현→해소 테스트 요약, 리컨실리에이션 위반 0). README 백로그 표: **A-4 ✅**, **시장가 매수 반올림 오차 ✅**(둘 다 완료 문서 링크). 발견 문서에도 "해소됨" 상단 표기.
- [ ] **Step 3: API 계약 변경 기록** — 취소 API가 접수 기반으로 바뀐 것을 별도 리포 프론트가 알 수 있게 완료 문서(또는 API 문서)에 명시. 프론트 UX(접수→확정 폴링)는 후속.
- [ ] **Step 4: Commit + 푸시 + CI** — `gh run watch` 그린 확인.

## 22번 재개 (범위 밖)

이 두 수정이 머지되면 `docs/superpowers/plans/2026-07-17-outbox-ab-and-spike-test.md`의 22번(outbox 상한 실증 + 급등락 스파이크 내성)을 재개한다. 스파이크 프로파일의 취소·시장가 경로가 이제 정합성 위반 0으로 통과해야 한다(2부 판정 기준 ③).
