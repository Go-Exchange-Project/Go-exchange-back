# ④ 입장 거절 품질 설계 (2차 리팩토링 · 가용성)

- **날짜**: 2026-07-21
- **상태**: 설계 검토 중
- **로드맵**: [2차 리팩토링 ④](../../refactor/README.md) — 가용성 100%의 넷째 조각
- **근거**: ①②가 이미 "매달림 0, 빠른 503" 하드 보장을 delivered. ④는 그 **거절의 품질**만 더한다 — 재시도 폭풍 방지(Retry-After)와 셰딩 관측(메트릭).

## 왜 필요한가 (그리고 왜 작은가)

과부하 시 "매달림 없이 빠르게 거절한다"는 하드 보장은 **이미 ①②가 제공**한다:
- **①의 게이트**: `OrderCh` 90% 워터마크 → DB 작업 전 503. 100ms 바운디드 핸드오프 →
  타임아웃 시 보상 + 503.
- **②의 코디네이터**: 입력 채널 만석 → 논블로킹 503.
- 둘 다 `SERVICE_UNAVAILABLE`(503) 반환 — 모든 과부하 경로가 유한 시간에 503을 낸다.

따라서 ④는 "매달림을 없앤다"가 아니라 **거절 품질 두 가지**만 더한다:

1. **Retry-After 부재**: 현재 503엔 `Retry-After` 헤더가 없어 클라이언트가 즉시
   재시도한다 → 과부하 증폭(retry storm). 백오프 힌트가 필요하다.
2. **셰딩 관측 부재**: 과부하에서 얼마나·어디서 거절했는지 측정할 카운터가 없다.
   ⑤(23번)가 "목표 초과 시 우아한 셰딩"을 실증하려면 이 수치가 필요하다.

**의도적으로 하지 않는 것**(로드맵 스케치에 있었으나 실제 코드 위에서 부적합/과함):
- **두 게이트 통합**: ①은 엔진 `OrderCh`를, ②는 코디네이터 입력을 보호한다 — 서로
  다른 자원·용량이라 단일 정책·임계값이 맞지 않는다. 통합은 추상화만 늘리고 이득이
  없다. 둘을 그대로 두되 Retry-After·메트릭만 일관 적용한다.
- **히스테리시스**: 90% 고정 워터마크의 경계 flapping을 매끄럽게 하지만, 단순
  워터마크도 "초과분만큼 셰딩"으로 자기 조절된다. ⑤에서 flapping이 실제 문제로
  관측되면 그때 붙인다(측정 기반).

## 설계

### 1. Retry-After 헤더 (재시도 폭풍 방지)

`internal/httpapi/response.go`의 `WriteError`에서 `status == http.StatusServiceUnavailable`
일 때 `Retry-After: <초>` 헤더를 세팅한다. **중앙화** — CreateOrder의 두 503(①게이트·
②코디네이터)과 향후 모든 503이 자동으로 헤더를 받는다(DRY). 현재 코드의 503은 전부
과부하(Unavailable)라 안전하고, 어떤 503이든 "잠시 후 재시도"는 타당하다.

- 값: 상수 `defaultRetryAfterSeconds = 1`(정수 초). 클라이언트가 ~1초 백오프 →
  과부하 드레인 → 회복. env화는 필요 시 후속(지금은 상수, YAGNI).

### 2. 셰딩 메트릭 (⑤ 관측 수단)

`internal/metrics/metrics.go`에 `orders_admission_rejected_total{stage}` 카운터
(`prometheus.CounterVec`, 라벨 `stage`). 거절 지점 3종을 구분:

- `engine_gate` — ①의 pre-DB 게이트(`IsIntakeAdmissible` false).
- `engine_handoff` — ①의 바운디드 핸드오프 타임아웃(`TrySubmitOrder` false → 보상).
- `coordinator` — ②의 코디네이터 입력 만석(`Submit` 논블로킹 실패).

각 거절 지점이 자기 `stage`를 `Inc()`한다:
- `OrderService.CreateOrder`: 게이트 거절 시 `engine_gate`, 핸드오프 타임아웃 시
  `engine_handoff`.
- `HoldCoordinator.Submit`: 입력 만석 시 `coordinator`.

⑤에서 "목표 초과 시 어디서 얼마나 셰딩했나"를 이 분포로 측정한다.

### 변경 범위 (외과수술적)

- `httpapi/response.go`: `WriteError`에 503 헤더 세팅 한 블록.
- `metrics.go`: 카운터 1개 + 필요 시 테스트.
- `order_service.go`: 두 거절 지점에 `Inc()` (metrics import 추가).
- `hold_coordinator.go`: 한 거절 지점에 `Inc()`.

**로직 무변경** — 거절 동작(503 반환·보상)은 이미 ①②가 수행하고, ④는 그 응답에
헤더를 붙이고 카운트할 뿐이다. 하드 보장·정합성·정산·엔진 무영향.

## 검토한 대안

- **두 게이트를 단일 admission controller로 통합**: 위 "의도적으로 하지 않는 것" 참조.
  서로 다른 자원이라 부적합. 기각.
- **Retry-After를 각 핸들러에서 개별 세팅**: 중복(DRY 위반). `WriteError` 중앙화가 우월.
  기각.
- **Retry-After 랜덤 지터**: 동시 백오프 충돌(재-thundering)을 흩뜨리나, 단일 상수로도
  충분하고 클라이언트가 자체 지터를 넣는 게 정석. 지금은 상수, 필요 시 후속. 기각.

## 검증 계획

1. **Retry-After**: 503 응답에 `Retry-After` 헤더가 실림 — `WriteError`(또는 핸들러)
   단위 테스트로 헤더 존재·값 단언.
2. **셰딩 메트릭**: 세 거절 지점 각각에서 `orders_admission_rejected_total{stage}`가
   증분 — 기존 ①②의 거절 테스트(게이트 503, 코디네이터 만석 503)에 메트릭 단언을
   추가하거나 전용 테스트.
3. 기존 ①②③ 테스트 무수정 그린 + `-race`(service·cmd).
4. 전체 스위트(통합 SKIP 0) 그린.
5. **셰딩률·재시도 폭풍 완화 실증은 ⑤(23번)** — ④는 수치를 주장하지 않는다. 성공
   기준은 "Retry-After 실림 + 메트릭 증분 + 회귀 그린"까지.

## 범위 밖

- ⑤(23번 실증 — 스파이크에서 취소 실패율·주문 접수 p95·셰딩 분포를 ①②③④ 종합 측정).
- 두 게이트 통합, 히스테리시스(측정 후 조건부).
- Retry-After env화·지터(필요 시 후속).
- API 계약: `POST /orders`의 성공 응답은 불변. 신규는 503에 `Retry-After` 헤더 추가뿐.
