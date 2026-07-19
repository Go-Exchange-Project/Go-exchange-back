# ① 접수-매칭 분리 설계 (2차 리팩토링 · 가용성 100%)

- **날짜**: 2026-07-18
- **상태**: 설계 검토 중
- **로드맵**: [2차 리팩토링 ①](../../refactor/README.md) — 가용성 100%의 첫 조각
- **근거**: [22번 벤치마크](../../benchmarks/22-2026-07-18-outbox-ab-and-spike.md) — 스파이크 시 `POST /orders` p95 14.8초.

## 왜 필요한가 (근본 원인, 코드 확인 완료)

`OrderService.CreateOrder`(order_service.go:87)의 순서:

1. `BuildOrder` — 검증(DB 없음).
2. **DB 트랜잭션**: `orderRepo.CreateOrder`(영속화) + `holdOrderAssets`(자금 홀드: 지갑 FOR UPDATE → available→locked → 원장 기록). **커밋.**
3. `MatchingEngine.SubmitOrder(...)` — `me.OrderCh <- order` **블로킹 송신.**
4. 200 반환.

문제는 **3번**이다. `OrderCh`(버퍼 1024)가 꽉 차면 블로킹되는데, 엔진 goroutine이
`ExecutionCh <-`(정산→outbox→DB)에서 멈춰 있으면 `OrderCh`를 못 비운다. 즉
**DB CPU 포화 → ExecutionCh 만석 → 엔진 정지 → OrderCh 만석 → `POST /orders`가
14초 매달림.** DB 병목이 HTTP 응답까지 역류하는 것이다.

**핵심 관찰: 2번 커밋 시점에 주문은 이미 내구·정합적이다.** 자금은 잠겼고(overspend
불가), 주문 행은 영속화됐다(상태 PENDING). 만약 그 직후 크래시해도 **부트스트랩이
PENDING/PARTIAL 주문을 오더북에 재투입**한다(`matching_bootstrap_service`) — "영속화됐지만
아직 엔진에 없음"은 이미 안전하게 처리되는 상태다. 따라서 **응답은 3번(엔진 핸드오프)을
기다릴 이유가 없다.** 엔진 핸드오프는 내구성 요건이 아니라 저지연 매칭을 위한 인메모리
최적화일 뿐이고, 그 백스톱은 부트스트랩이다.

## 목표

`POST /orders`의 **접수 지연을 매칭 처리량에서 분리**해 상한 시간 내로 만든다: 항상
**빠른 접수(200)** 또는 **빠른 거절(503)**, 결코 몇 초 매달림 없음. 목표 부하(≤5,000
제출자, ②가 천장을 그 위로 올린 뒤)에선 사실상 항상 빠른 접수.

## 설계

### 접수 시퀀스 재정의

```
1. BuildOrder(검증)
2. 입장 게이트(최소형): 대상 샤드 OrderCh가 high-watermark 이상이면 → 즉시 503, DB 무접촉.
   (지속 포화 시 DB 낭비 없는 빠른 거절. 정교한 정책은 ④가 formalize.)
3. DB 트랜잭션: 주문 영속화 + 자금 홀드 (커밋) — 기존과 동일. ②가 이 왕복을 배칭으로 빠르게.
4. 바운디드 핸드오프: TrySubmitOrder(order, acceptanceBound)
     - 성공 → 200 (주문은 PENDING, 매칭은 비동기 — 기존 계약 그대로)
     - 바운드 초과(2·4 사이 레이스로 포화, rare-rare) → 보상(홀드 해제 + 주문 종결) + 503
5. 반환
```

**접수 지연 상한 = 홀드 트랜잭션 시간 + acceptanceBound.** `acceptanceBound`는 짧게
(기본 ~100ms, env 조정) — 일시적 버스트(엔진이 잠깐 뒤처짐)는 흡수하되, 지속 포화는
바운드 초과로 거절. 순수 논블로킹(즉시 실패)이 아니라 **짧은 바운디드 대기**인 이유:
목표 부하의 일시 버스트에서 spurious 503을 내지 않기 위함.

### 엔진 인터페이스 변경

`matching.Engine`에 바운디드 제출 추가(기존 `SubmitOrder` 블로킹은 부트스트랩·리플레이용으로
유지 — 그 경로엔 HTTP가 기다리지 않으므로 블로킹이 옳다):

```go
type Engine interface {
    SubmitOrder(*Order)                              // 블로킹 — 부트스트랩/리플레이 전용
    TrySubmitOrder(order *Order, within time.Duration) bool  // 바운디드 — 라이브 HTTP 경로
    CancelOrder(CancelOrderCommand) CancelOrderResult
    RequestOrderBookSnapshot(coinSymbol string, depth int) (OrderBookSnapshot, error)
}
```

- `MatchingEngine.TrySubmitOrder`: `select { case OrderCh <- order: return true; case <-time.After(within): return false }`.
- `ShardedEngine.TrySubmitOrder`: `shardFor(symbol)`로 위임(같은 심볼=같은 샤드 불변 유지).
- `SubmitOrder`는 무변경 — 기존 부트스트랩/리플레이 테스트 그대로 통과.

### 보상(거절 시)

주문이 영속화·홀드된 뒤 핸드오프가 바운드를 넘긴 rare-rare 케이스: 이 주문은 엔진에
들어간 적 없어 어떤 trade도 참조하지 않으므로 **동기 보상이 안전**하다. 기존
`releaseOrderHold` 로직 재사용 — 홀드 전액 해제(주문은 FilledAmount=0) + 주문 상태를
종결로 전이 + 503. 고아 홀드(orphan lock)가 남지 않는다.

### 정합성·내구성 불변식

- **overspend 불가**: 자금 홀드는 여전히 3번에서 동기·원자적. 접수 분리는 홀드를
  비동기화하지 않는다(그건 ②/인메모리의 영역, 이 스펙 아님).
- **유실 0**: 성공 경로는 주문이 영속화+엔진 접수 둘 다. 거절 경로는 영속화됐던 주문을
  보상으로 깨끗이 종결. 어느 쪽도 "영속화됐는데 영원히 안 매칭"이 없다.
- **부트스트랩 백스톱**: 3번 커밋 후 4번 전에 크래시해도 PENDING 주문을 부트스트랩이
  재투입(기존 동작). 접수 분리가 이 창을 넓히지 않는다 — 오히려 명시적으로 활용한다.
- **A-3/A-4 무영향**: ExecutionCh 백프레셔(정합성 보호)는 그대로. 바꾸는 건 OrderCh
  유입 측의 블로킹 → 바운디드뿐이다.

## API 계약 영향 (프론트 — 최소)

- **기존 계약 그대로**: `POST /orders`는 원래도 엔진 핸드오프(매칭 전)에서 200을
  반환했다 — 주문은 PENDING으로 접수되고 매칭은 비동기다. ①은 이 "접수" 부분이 엔진
  유입에 안 막히게 할 뿐, 반환 의미는 동일하다.
- **신규**: 포화 시 **503**(재시도 안내) 응답이 추가된다 — 목표 부하 이하에선 거의
  발생 안 하고, 목표 초과의 안전 마진이다. (503 본문·`Retry-After` 등 정책은 ④.)

## ①/④ 경계 (명시)

- **① (이 스펙)**: 응답을 매칭에서 분리 — 핸드오프를 바운디드로, 절대 무한 블로킹 없음.
  최소 입장 게이트(채널 high-watermark)로 지속 포화 시 빠른 거절. "항상 바운디드"를 보장.
- **④ (별도 스펙)**: 입장 정책 정교화 — `Retry-After`/에러 코드, 임계값·히스테리시스,
  엔드포인트별 정책, 취소 면제(③과 연동), load-shed 메트릭. ①의 rare-rare 보상 경로를
  pre-DB 게이트 강화로 더 줄임.

## 확정할 결정 (플랜에서)

1. **`acceptanceBound` 기본값**: ~100ms 제안(env `GOEXCHANGE_ACCEPTANCE_TIMEOUT_MS`).
   측정으로 조정 — 목표 부하 버스트를 흡수하되 매달림 체감이 없는 최소값.
2. **거절 주문의 종결 상태**: **확정 — 신규 `REJECTED`**(사용자 확정). "시스템 부하로
   거절"을 "유저 취소(`CANCELLED`)"와 구분해 관측 가능성·정직성을 확보한다. 부트스트랩은
   PENDING/PARTIAL만 로드하므로 REJECTED는 자연 제외되고, 정산 파이프라인은 이 주문을
   본 적 없다(엔진 미진입). 주문 상태 enum + `ck_orders_status` CHECK 제약 마이그레이션
   1건 수반. 상태 전이 검증(정산·취소 경로)이 REJECTED를 터미널로 인식하는지 점검.
3. **입장 게이트 watermark**: OrderCh 용량 대비 비율(예: 90%). ④에서 히스테리시스로 정교화.

## 검토한 대안

- **지속 펌프 feed(HTTP는 영속화만, 백그라운드가 DB→엔진 급전)**: 접수를 엔진에서 완전
  분리하나, 매칭 지연(주문이 DB에 머묾) + "영속화됐지만 미급전" 추적 + 급전 루프가
  백로그 시 같은 엔진-정지 문제를 재배치. 복잡도 대비 이득 낮음 — 바운디드 핸드오프로
  충분(주문은 이미 커밋 후 부트스트랩 대상이므로 별도 급전 불필요). 기각.
- **무조건 접수(거절 없음)**: 고정 하드웨어에서 목표 초과 시 무한 적체 → 매달림 재발.
  "빠른 거절이 매달림보다 정직하다"는 목표 정의와 배치. 기각.
- **순수 논블로킹(바운디드 대기 없이 즉시 실패)**: 목표 부하의 일시 버스트에서 spurious
  503 남발. 짧은 바운디드 대기가 버스트를 흡수하면서도 지속 포화는 거절. 기각.

## 검증 계획

1. 단위: `TrySubmitOrder` 바운디드 동작(여유 시 즉시 true / 포화 시 바운드 후 false) —
   MatchingEngine·ShardedEngine 양쪽. 짧은 티커 테스트 패턴 계승.
2. 서비스: CreateOrder가 (a) 여유 시 200·엔진 접수, (b) 포화 시 503·홀드 해제(고아 락 0)·
   주문 종결. 통합 테스트(실 DB)로 보상의 원장·잔고 정합 확인.
3. 회귀: 기존 CreateOrder·부트스트랩·outbox·리플레이·정산 통합 테스트 무수정 그린
   (SubmitOrder 블로킹 경로 불변의 증거). 리컨실리에이션 위반 0.
4. `-race`(matching·service·cmd).
5. **가용성 실증은 ⑤(23번)** — ①만으론 천장이 안 올라가므로(②가 필요) 접수 지연의
   최종 수치는 ②·③·④와 함께 23번에서. 이 스펙의 성공 기준은 "블로킹 제거 + 보상 정합"
   까지다.

## 범위 밖

- ② 자금 홀드 배칭(천장 올리기), ③ 취소 우선 경로, ④ 입장 정책 정교화 — 각 별도 스펙.
- 자금 홀드의 비동기화/인메모리화(대공사, 측정 후 조건부).
- 프론트 503 재시도 UX(별도 리포).
