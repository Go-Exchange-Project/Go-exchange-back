# 발견: 취소-체결 레이스, 시장가 매수 반올림 오차 (2026-07-17)

> **해소됨 (2026-07-18)**: 두 버그 모두 근본 수정 + 안전망 + 재현 확인까지 완료.
> [10번 완료 문서](10_A-4_취소체결레이스와_시장가매수반올림오차_완료.md) 참고.
> 아래는 원 발견 당시의 기록이다.

- **발견 경위**: [22번 벤치마크](../superpowers/plans/2026-07-17-outbox-ab-and-spike-test.md) 사전 작업 3
  (`loadtest/order-spike-single-symbol.js`)의 로컬 스모크(유저 20명, 55초 축소판) 중 발견.
  이 스크립트는 이 프로젝트에서 **처음으로** 취소(`DELETE /orders/:id`)와 시장가 주문을
  부하 상태에서 실행했다 — 17~21번 벤치마크는 전부 지정가 hold 시나리오만 사용해 이
  두 경로를 한 번도 실측하지 않았다.
- **판단**: 22번 계획 범위(사전 작업 3건 + outbox A/B + 스파이크 측정)를 벗어나는 버그
  수정이 필요해, 22번 측정(GCP)은 진행하지 않고 발견만 기록한다(사용자 확인 완료).
  22번 사전 작업(compose env 라인, 기동 로그, 스파이크 스크립트) 자체는 이 버그와
  무관하게 정상 동작해 커밋한다.

## 버그 1: 취소-체결 레이스 (백로그 A-4의 첫 실증)

기존 백로그에 **이미 알려진 위험**으로 등재돼 있었다(`README.md` A-4: "취소-체결 레이스의
카운터파티 오더북 발산", 승격 조건 "A-3 outbox 구조 위에서" — A-3는 이미 완료). 이번
스모크가 A-3 완료 후 첫 실증 재현이다.

### 근본 원인

`OrderService.CancelOrder`([internal/service/order_service.go:129](../../internal/service/order_service.go))는
DB 트랜잭션 안에서 주문을 `FindByIDForUpdate`로 잠그고 현재 DB 상태 기준 `remaining`을
계산해 `CANCELLED`로 커밋한 **뒤에** 매칭 엔진에 별도로 취소 커맨드를 보낸다. 그런데
엔진은 이미 그 주문을 체결시켜 오더북에서 제거했을 수 있다 — 그 체결의 outbox
이벤트가 아직 커밋/정산 파이프라인을 통과 중이라 DB의 주문 상태에는 반영되지 않은
상태다. 즉 "DB 상태로 취소 가능 여부 판단"과 "엔진의 실제 체결 여부"가 outbox 지연만큼
어긋날 수 있다(A-3 write-ahead 설계의 알려진 트레이드오프).

이 레이스가 두 갈래로 터진다:
1. **정산 실패(`failed_settlements`)**: 방금 체결된 trade가 정산을 시도하는 시점에는
   이미 CancelOrder가 주문을 `CANCELLED`로 바꿔놓은 뒤라 `settle trade failed: buy order
   N status CANCELLED cannot be settled`(또는 매수자 hold가 이미 해제돼
   `buyer has insufficient locked KRW balance`)로 durable 실패 기록이 남는다.
2. **취소 API 500**: `OrderService.CancelOrder`가 DB 커밋 후 엔진에 취소를 보내면
   (`internal/service/order_service.go:189-195`) 엔진이 "이미 없음"을 반환하고,
   핸들러(`internal/handler/order_handler.go:178-181`)는 `result.Status == "CANCELLED"`인데
   `err != nil`인 이 케이스를 500으로 매핑한다.

### 실측 (로컬 스모크, 유저 20명, 55초)

- 취소 시도 154건 중 **500 21건(13.6%)**, 200 122건, 409(이미 체결, 정상) 11건.
- `failed_settlements` durable 실패 24건 — 그중 21개 주문이 `status CANCELLED cannot be
  settled`, 나머지는 `buyer has insufficient locked KRW balance` 변종(같은 레이스의 다른
  타이밍 표현).

### 영향

22번 2부(급등락 스파이크 내성)의 정합성 판정 기준 ③(`failed_settlements = 0`)은 취소
폭주가 스펙에 명시된 시나리오라 **이 버그가 고쳐지지 않는 한 결정론적으로 실패**한다.
GCP 규모(3,000 VU)에서는 레이스 윈도우에 걸리는 취소 건수가 절대적으로 늘어나 로컬보다
빈도가 높아질 것으로 예상된다.

### 권고

백로그 A-4("주문 상태 전이 소유권을 엔진으로") 승격. 상세 설계는 별도 계획 필요 —
후보 방향: CancelOrder가 엔진에 먼저 취소를 요청하고 그 결과(Removed 여부)를 보고 나서만
DB 상태를 CANCELLED로 커밋(현재는 반대 순서), 또는 주문 상태 전이를 엔진 시퀀스에
종속시켜 outbox 이벤트가 CancelOrder보다 먼저 도착하도록 순서를 보장.

## 버그 2: 시장가 매수 반올림 오차로 인한 이중 실패 (신규, 백로그 미등재)

### 근본 원인

MARKET BUY가 여러 지정가 레벨에 걸쳐 분할 체결되면(오더북에 가격이 분산된 정상적인
상황 — 이번 스파이크 스크립트의 메이커가 1~5틱에 가격을 분산시켜 만든 조건), 체결마다
`quote_amount` 소진액을 decimal로 나눠 계산하는 과정에서 총 소진액이 예산을 아주 작은
epsilon만큼 초과한다(관측값 전부 `50000.0000000018775`, 즉 예산 대비 **+1.8775e-9** 고정
오프셋 — 난수 노이즈가 아니라 나눗셈 반올림의 체계적 잔차로 보인다). 이 초과가
`CompleteMarketOrder`를 실패시키고(`market buy order N spent quote amount ... exceeds
quote budget ...`), 그 실패를 기록하려는 `failed_market_completions` INSERT 자체가
`ck_failed_market_completions_remaining_quote_non_negative` CHECK 제약(잔여 quote가
음수면 거부)에 걸려 **다시 실패**한다 — 실패의 안전망(A-1/A-2가 만든 durable 실패
기록 체계)이 이 케이스에서는 작동하지 않고 로그에만 남는다.

### 실측 (로컬 스모크, 유저 20명, 55초)

- 이 경로를 탄 **서로 다른 주문 97건**(로그의 distinct order_id 기준) — k6 커스텀
  카운터 `custom_market_success`(179, 매수+매도 합산) 대비 상당한 비율. 시장가 매수가
  둘 이상의 가격 레벨에 걸쳐 체결되면 사실상 상시 재현되는 것으로 보이며, 스파이크
  시나리오에 국한된 문제가 아니다(메이커 가격 분산이 있는 오더북이면 언제든 발생 가능).
- `failed_market_completions` INSERT 실패가 **매 시도마다** 발생 — 즉 이 97건은 재시도
  워커(`SettlementRetryWorker`)가 계속 재시도해도 영구히 실패하며 테이블에 흔적조차
  남지 않는다.

### 영향

- 22번 2부 판정 기준 ③(`failed_market_completions = 0`)이 결정론적으로 실패한다.
- 더 심각한 것은 **관측 가능성 붕괴**다: 이 실패는 durable 기록조차 남기지 못해
  운영자가 `failed_market_completions` 테이블만 봐서는 문제를 인지할 수 없다(로그
  grep에만 의존해야 함).

### 권고

두 방향 모두 필요:
1. **즉시 완화**: `CompleteMarketOrder`가 계산한 초과분을 0으로 클램프(또는 매우 작은
   epsilon 이하는 완료 처리로 간주)해 정상 케이스를 실패로 오분류하지 않게 한다.
2. **안전망 자체의 결함**: `failed_market_completions` INSERT가 실패할 수 있는 입력을
   원천 차단하거나(예: `remaining_quote_amount`를 저장 전 0 이상으로 clamp), CHECK
   제약이 있는 한 애플리케이션 코드가 그 제약을 위반하지 않는다는 보장이 필요하다 —
   이번처럼 "실패를 기록하려다 또 실패"하는 이중 실패 패턴은 다른 안전망(예:
   `failed_settlements`)에도 유사 위험이 있는지 별도 점검 가치가 있다.

## 다음 단계

두 버그 모두 별도 계획(`docs/superpowers/plans/`)으로 분리해 설계·수정·검증한 뒤,
22번(outbox 상한 실증 + 급등락 스파이크 내성)을 재개한다. 22번 1부(outbox A/B)는 이
버그들과 무관하지만, 계획 문서가 1·2부를 한 세션으로 묶어놨으므로 함께 보류한다.
