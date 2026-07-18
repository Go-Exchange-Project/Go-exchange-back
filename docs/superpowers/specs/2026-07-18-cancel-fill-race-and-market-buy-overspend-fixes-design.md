# 정합성 버그 2건 수정 설계: 취소-체결 레이스(A-4) + 시장가 매수 초과 소진

- **날짜**: 2026-07-18
- **상태**: 설계 검토 중
- **발견 문서(스펙 원천)**: [bug-findings 2026-07-17](../../refactor/bug-findings-2026-07-17-cancel-fill-race-and-market-buy-overspend.md)
- **결정**: 버그 1(A-4)은 **정석 — 취소 시퀀서화**(사용자 확정). 버그 2는 근본 수정.
- **처리 순서**: 버그 2 먼저(작고 결정론적·독립적), 버그 1 나중(큼).

## 공통 불변식 (둘 다 지켜야 함)

1. **자금 정합성 100%**: 리컨실리에이션(A-6) 위반 0, 원장-지갑 일치, 자산 총량 보존.
2. **write-ahead(A-3)**: 정산은 outbox에 커밋된 이벤트만. 백프레셔·무한 재시도 유지.
3. **심볼 내 순서**: 엔진 방출 순서 = outbox ID 순서 = 정산 처리 순서.
4. **크래시 내구성**: 커밋 전 크래시 = 안 일어난 일(부트스트랩 재투입), 커밋 후 = 리플레이 완결.
5. **멱등성**: 리플레이·재시도가 이벤트를 여러 번 처리해도 결과 동일.

---

# 버그 2: 시장가 매수 초과 소진 (근본 수정 + 안전망)

## 근본 원인 (코드 확인 완료)

`internal/matching/engine.go`의 `matchMarketBuy`:

```go
maxQtyByQuote := order.QuoteAmount.Div(sellLevel.Price)   // shopspring Div = 16자리 반올림
tradeQty := decimal.Min(maxQtyByQuote, sellOrder.Amount)
executionQuote := sellLevel.Price.Mul(tradeQty)
order.QuoteAmount = order.QuoteAmount.Sub(executionQuote)  // 예산 소진 체결에서 음수 가능
```

**예산 소진(클램프) 체결**(`maxQtyByQuote < sellOrder.Amount`)에서 `Div`의 반올림이
올림 방향이면 `executionQuote = price × round(budget/price) > budget`이 된다. 결과:
- `order.QuoteAmount`(잔여 예산)가 **음수** → `MarketOrderDone.RemainingQuoteAmount` 음수.
- `order.FilledQuoteAmount` 총합이 예산을 +1.8775e-9 초과(관측값, 체계적 잔차).

이후 `completeMarketBuyOrder`(order_service.go:242)의 엄격 예산 검사
(`spentQuoteWithFees.GreaterThan(order.QuoteAmount)`)가 실패 → 그 실패를 기록하려는
`failed_market_completions` INSERT가 음수 `remaining_quote_amount`로 CHECK 제약
(`ck_failed_market_completions_remaining_quote_non_negative`, model 태그) 위반 →
**이중 실패**. 안전망이 무음으로 붕괴, 로그에만 남음.

## 수정: 3계층

### 1. 근본 — 엔진이 예산 초과 체결을 만들지 않는다

`matchMarketBuy`의 클램프 체결에서 **quantity를 `price × qty ≤ 잔여예산`이 되도록 내림**한다.
`executionQuote`만 클램프하면 안 된다 — trade는 `price`·`quantity`로 저장되고 정산은
`tradeQuoteAmount = price × quantity`로 **재계산**하므로(settlement_service.go), quantity
자체가 예산 내여야 정산까지 일관된다. 내림으로 생기는 dust(잔여 예산)는
`RemainingQuoteAmount`로 반환돼 hold가 해제된다 — 정상 동작이다.

- 정확한 decimal 내림 기법은 **재현 테스트로 확정**한다(후보: `budget.Sub(budget.Mod(price)).Div(price)`로 floor, 또는 `price.Mul(tradeQty) > budget`일 때 quantity를 한 단위 감. 부동 정밀도 함정이 있으므로 테스트로 검증). 불변식: 클램프 체결 후 `order.QuoteAmount ≥ 0` 항상 참.
- 시장가 매도(`matchMarketSell`)는 quantity 기준 소진이라 이 문제가 없다(확인) — 건드리지 않는다.
- **엔진 수정이므로 신중**: 기존 매칭 단위·동시성 테스트가 무수정 그린이어야 한다(가격-시간 우선순위·체결량 계약 불변). 바뀌는 것은 클램프 체결의 quantity 정밀도뿐.

### 2. 완화 — 완료 검사 방어

근본(1)이 초과를 원천 차단하므로 `completeMarketBuyOrder`의 예산 검사는 정상 케이스를
더는 실패시키지 않는다. 검사 자체는 **버그 방어선으로 유지**한다(엔진에 또 다른 초과
경로가 생기면 잡아야 함) — 단, 이 검사가 실패해 기록으로 넘어갈 때 3이 CHECK 위반을
막는다.

### 3. 안전망 — 실패 기록이 CHECK를 위반할 수 없게

`FailedMarketCompletionService.RecordFailure`(또는 그 입력 생성 지점)에서
`RemainingQuoteAmount`를 저장 전 `decimal.Max(0, ...)`로 clamp한다. "실패를 기록하려다
또 실패"하는 이중 실패 패턴을 원천 차단 — 근본(1)과 독립적으로 안전망을 견고화한다.

- **동종 결함 점검**: `failed_settlements`(`FailedSettlementService.RecordFailure`)와
  그 CHECK 제약도 같은 "기록 입력이 제약 위반 가능" 패턴이 있는지 점검하고, 있으면
  같은 방식으로 clamp. 발견 문서의 "별도 점검 가치" 항목.

## 검증 (버그 2)

1. 엔진 단위: 다중 레벨 분산 오더북에 MARKET BUY 예산 소진 → 클램프 체결 후
   `order.QuoteAmount ≥ 0`, `FilledQuoteAmount ≤ budget`(재현 테스트, 근본 확인).
2. 서비스: `completeMarketBuyOrder`가 근본 수정 후 정상 완료(FILLED, dust 해제).
3. 안전망: 음수 `RemainingQuoteAmount` 입력에도 `RecordFailure`가 CHECK 위반 없이
   저장(clamp 검증) — 통합 테스트.
4. 기존 매칭·정산 테스트 무수정 그린 + 리컨실리에이션 위반 0.

---

# 버그 1: 취소-체결 레이스 (A-4) — 취소 시퀀서화

## 근본 원인 (코드 확인 완료)

`OrderService.CancelOrder`(order_service.go:129)는 **DB 트랜잭션에서 현재 DB 상태 기준으로
`CANCELLED` 커밋 → 그 뒤 엔진에 취소 요청** 순서다. 엔진이 이미 체결시켰지만 outbox
파이프라인 지연으로 DB에 미반영인 경우:
- 뒤늦은 trade 정산이 `CANCELLED cannot be settled` / `insufficient locked KRW`로 실패
  (`failed_settlements` durable 기록) — 실측 24건.
- 엔진이 "이미 없음" 반환 → 핸들러가 500(order_handler.go:178-181) — 실측 21/154건.

DB 상태로 취소 가능성을 판단하는 것과 엔진의 실제 체결이 outbox 지연만큼 어긋난다.

## 설계: 취소를 체결과 같은 등급의 엔진 이벤트로

**주문 상태 전이 소유권을 엔진 시퀀스로 옮긴다.** 취소가 엔진 FIFO를 타면 "이 주문의
선행 체결들 → 취소"가 자동 정렬되고, 정산 워커가 그 순서로 처리해 취소 시점엔 이미 모든
체결이 정산돼 있다 — 잔여·hold가 정확하고 `CANCELLED cannot be settled`가 성립 불가.

### 흐름

```
CancelOrder API:
  1. DB 트랜잭션: 소유권·취소가능 상태 검증만(FOR UPDATE). hold 해제·CANCELLED 커밋은
     여기서 하지 않는다. (상태를 CANCELLING 같은 중간 상태로 표시할지는 아래 결정)
  2. 엔진에 CancelOrderCommand 전송 → ResponseCh로 Removed 여부 즉시 수신.
  3. 응답: "취소 접수됨"(Removed 여부 포함). DB 확정(hold 해제·CANCELLED)은 파이프라인.

엔진 processCancel (Removed=true일 때):
  - 오더북에서 잔여분 제거(기존) + ExecutionCh에 OrderCancelled 이벤트 방출(신규).
  - 이 이벤트는 그 주문의 선행 체결들 뒤에 enqueue된다(단일 스레드 순서).

정산 파이프라인 (OrderCancelled 이벤트 소비):
  - 새 서비스 메서드 ProcessOrderCancellation: 주문 FOR UPDATE →
    (선행 체결이 이미 정산돼 order.Filled 최신) → 잔여 = Amount − Filled 해제 →
    status CANCELLED 커밋. 멱등(이미 CANCELLED면 no-op).
```

### 확정할 설계 결정 (실행 세션이 스펙대로 구현)

1. **outbox 이벤트 타입 추가**: `model.TradeOutboxEventType`에 `ORDER_CANCELLED` 추가.
   `TradeOutboxEvent`의 CHECK 제약(`ck_trade_outbox_event_type`, 현재 `IN ('TRADE',
   'MARKET_ORDER_DONE')`)에 `'ORDER_CANCELLED'` 추가 — **마이그레이션 필요**(gorm
   AutoMigrate는 기존 CHECK를 갱신하지 않으므로 `migrations/`에 ALTER 추가). 신규 배포는
   무해, 기존 outbox 행과 호환.

2. **이벤트 페이로드**: `matching.OrderCancelled{OrderID, CoinSymbol, Side, ...}` —
   `ProcessOrderCancellation`이 DB에서 주문을 조회하므로 최소 식별자만 담아도 된다.
   `EngineEventID`로 멱등 키 부여(엔진 시퀀스 기반, trade와 동일 규칙).

3. **엔진 방출**: `emitTrade`/`emitMarketOrderDone` 옆에 `emitOrderCancelled`. `processCancel`이
   `result.Removed`일 때 방출. **부분 체결 잔여 취소**: 잔여분이 오더북에 있으므로
   Removed=true, 선행 체결들은 이미 ExecutionCh에 있음 → 순서 보장.
   **완전 체결/체결 중**: Removed=false → 이벤트 없음(취소할 잔여 없음), API는 "이미
   체결됨"으로 응답(409, 500 아님).

4. **API 의미론 변경 (프론트 영향)**: 취소가 즉시 확정 → **접수 기반**으로 바뀐다. 취소
   직후 주문 조회 시 아직 PENDING/PARTIAL일 수 있고(파이프라인 지연), 곧 CANCELLED가
   된다. 응답 형식은 `EngineRemoved`(접수 성공 여부)를 유지하되 `status`의 의미를
   "접수됨"으로 문서화. **별도 리포 프론트는 이 세션 범위 밖** — 백엔드 계약 변경만
   기록하고, 프론트 폴링이 CANCELLING/PENDING을 어떻게 표시할지는 후속.

5. **크래시 내구성 (체결과 대칭)**:
   - 취소 이벤트 **커밋 전 크래시**: 취소는 "안 일어난 일". 주문은 여전히 오더북 대상 →
     부트스트랩이 재투입(정상, 사용자는 "접수"만 받았고 재요청 가능).
   - 취소 이벤트 **커밋 후 크래시**: outbox PENDING → 부팅 리플레이가
     `ProcessOrderCancellation` 재처리 → CANCELLED. **리플레이(먼저) → 부트스트랩(나중)**
     순서라, 리플레이가 CANCELLED로 만든 주문은 부트스트랩이 재투입하지 않는다
     (부트스트랩은 PENDING/PARTIAL만). 일관.

6. **리플레이·정산 워커 라우팅**: `ExecutionEvent`에 `OrderCancelled *OrderCancelled`
   필드 추가, `ExecutionEventFromOutbox`·`NewTradeOutboxEvent` 인코딩/디코딩,
   `processExecutionEvent`에 취소 분기(`ProcessOrderCancellation` 호출). 리플레이는
   `ExecutionEvent`를 그대로 Process로 넘기므로 자동 커버.

7. **DB 중간 상태(CANCELLING) 여부** — 두 안:
   - **(a) 중간 상태 없음(권장)**: CancelOrder API는 검증만, DB는 안 건드림. 파이프라인이
     PENDING/PARTIAL → CANCELLED 직접 전이. 더 단순. 단 취소 접수~확정 사이 창에 같은
     주문에 또 취소 요청이 오면 중복 이벤트 → 멱등으로 흡수(두 번째 CANCELLED no-op).
   - (b) CANCELLING 중간 상태: 접수 즉시 표시해 중복 요청·조회를 명확히. 상태 enum·전이
     검증·부트스트랩 제외 로직이 늘어 복잡. **(a)로 가되, 중복 취소 요청의 멱등 처리를
     테스트로 못박는다.**

8. **hold 해제 위치**: 현재 CancelOrder가 트랜잭션에서 `releaseOrderHold`. 시퀀서화 후엔
   `ProcessOrderCancellation`이 수행(정산과 같은 원장 기록 경로). 이중 해제 방지: 멱등
   키 + status 검사(CANCELLED면 no-op)로 보장.

## 검증 (버그 1)

1. 엔진 단위: 취소 → `OrderCancelled` 이벤트 방출(Removed=true), 완전 체결 주문 취소 →
   방출 없음(Removed=false). 심볼 FIFO에서 "체결들 → 취소" 순서 보존.
2. 서비스: `ProcessOrderCancellation` — 미체결/부분체결 취소 시 잔여 hold 정확 해제 +
   CANCELLED, 멱등(중복 처리 no-op), 이미 FILLED면 no-op.
3. **레이스 재현 → 해소**: 발견 문서의 시나리오(체결 in-flight 중 취소)를 결정론적으로
   재현하는 통합 테스트 — 시퀀서화 전엔 `failed_settlements` 발생, 후엔 0.
4. 크래시: 취소 이벤트 커밋 후 크래시 주입 → 리플레이가 CANCELLED 완결, 부트스트랩
   재투입 안 함(A-3 크래시 테스트 패턴 계승).
5. 크래시: 취소 이벤트 커밋 전 크래시 → 부트스트랩 재투입, 주문 유효(취소 미접수).
6. 기존 outbox·리플레이·크래시 계약 테스트 무수정 그린 + 리컨실리에이션 위반 0 +
   graceful shutdown 도미노 유지(취소 이벤트도 드레인 대상).

## 검토한 대안 (기각)

- **순서 역전 + Removed 기반 거부**: 500→409는 해결하나 부분 체결 in-flight 레이스가
  남아 `failed_settlements`가 0이 안 됨(정합성 목표 미달). 사용자 확정으로 기각.
- **DB 중간 상태 CANCELLING**: 결정 7 참조 — 복잡도 대비 이득 낮아 기각((a) 채택).

## 범위 밖

- 별도 리포 프론트엔드의 취소 UX(접수→확정 폴링).
- 22번 벤치마크(이 두 수정 후 재개).
- 시장가 취소(현재도 금지 — `market orders cannot be cancelled`, 유지).
