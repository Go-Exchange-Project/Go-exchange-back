# 주문 생성 idempotency key — 설계

> **상태**: 설계. 구현·측정 없음.
> **범위**: B(정확성 부채) 2번 항목. 1번(취소 command outbox)은 [35번](../../benchmarks/35-2026-08-23-cancel-command-outbox.md)으로 완료됐다.
> **앱 코드 기준**: `7bd669b`

## 1. 왜 필요한가

### 1.1 지금은 재시도가 곧 중복 주문이다

`POST /orders`는 응답이 유실되면 클라이언트가 같은 요청을 다시 보낼 수밖에 없다.
현재 서버에는 **그 둘을 구분할 수단이 없다.** 두 번째 요청은 새 주문으로 접수되고,
자금이 두 번 hold되며, 두 번 체결될 수 있다.

돈이 오가는 경로에서 "네트워크가 끊겼다"가 "주문이 두 개 생겼다"로 이어지는 것이므로,
실거래 기준 배포 차단 사유다. 취소는 B-1에서 `order_id` UNIQUE로 닫았지만, **생성은
자연 키가 없다** — 같은 사용자가 같은 심볼에 같은 가격·수량으로 두 번 주문하는 것은
정상이다. 그래서 클라이언트가 주는 키가 필요하다.

### 1.2 현재 경로

```
CreateOrder (order_service.go:114)
  BuildOrder                       검증만, DB 없음
  IsIntakeAdmissible               게이트 → 503
  HoldCoordinator.Submit           배치 경로  ┐ 주문 INSERT + 지갑 UPDATE + 원장 INSERT
    또는 persistAndHold            단건 경로  ┘ 가 한 트랜잭션
  TrySubmitOrder                   엔진 접수(바운디드) → 실패 시 rejectAcceptedOrder
  → 200 {"message":"order accepted","order_id":N}
```

키를 도입하면 순서가 바뀐다. **이미 결정된 요청의 결과는 지금의 상황으로 덮어쓸 수 없다.**
포화·시장 정책·설정처럼 **시간이 지나 바뀌는** 조건은 새 키에만 적용한다.

```
  1. 키 정규화(1~128자)                    위반 → 400/422, 키 미소비
  2. 구문 파싱·정규화(parseOrderRequest)    위반 → 4xx  (요청 자체가 틀렸다)
  3. 지문 입력 구성
  4. 가변 조건 검사 — 거절 직전에만 기존 키를 확인한다
       시장 정책(validateMarketPolicy)
       엔진 미배선
       IsIntakeAdmissible == false
     → 기존 키 조회(FindByUserKeys)
         있고 지문 같음  → 저장된 결과 replay(200/202)
         있고 지문 다름  → 409
         없음(새 키)     → 그 거절 사유 그대로(4xx/503), 키 미소비
  5. hold → 엔진 제출
```

- **구문 오류(2)는 조회하지 않는다.** 파싱조차 안 되는 요청은 지문을 만들 수 없다.
- **조회는 거절할 때만 한다.** 요청마다 미리 조회하면 정상 경로에 SELECT가 하나 는다.
- 무조건 거절하면 ACCEPTED된 요청의 재시도가 replay 대신 503/409를, 다른 지문 요청이 409
  대신 정책 오류를 받는다. 주문 생성 후 시장이 멈추면 정상 재시도가 전부 막힌다.
- **엔진이 없으면 hold보다 먼저 거절한다.** 주문에는 취소와 달리 command outbox가 없어,
  hold부터 잡으면 그 주문과 자금이 처리될 경로 없이 영구히 묶인다.

---

## 2. 목표와 비목표 — **보장의 경계를 먼저 좁힌다**

### 이번 범위가 보장하는 것

- 같은 키의 재시도가 **주문을 하나만 만든다** — 동시·순차·같은 배치·다른 배치 무관
- 같은 키의 재시도가 **같은 `order_id`** 와 **저장된 최선의 결과**를 돌려받는다
- 같은 키에 **다른 요청 내용**이 오면 조용히 성공시키지 않고 **409**
- 주문·hold·멱등성 레코드가 **한 트랜잭션**에서 커밋된다
- **엔진 제출은 대표 요청 하나만 한다**

### ⚠ 이번 범위가 보장하지 **않는** 것

**"첫 HTTP 응답을 그대로 재생"은 보장하지 않는다.** 엔진 제출은 hold 트랜잭션이 끝난
뒤에 일어나므로, 응답을 결정하는 결과가 그 트랜잭션에 들어 있지 않다. 원자적으로
보존하려면 **주문 command outbox**가 필요하고, 그건 이번 범위가 아니다.

정직한 보장은 **"중복 주문 방지 + 같은 `order_id` + 그 시점까지 확정된 최선의 결과"** 까지다.
초안은 이 지점을 과장했다.

### 비목표

- 주문 command outbox — 별도 항목
- 취소·충전 등 다른 엔드포인트의 멱등성
- 분산 환경의 키 조율 — 단일 writer 전제는 그대로다
- 처리량 개선 — 오히려 INSERT가 하나 는다. 회귀 게이트로만 관리한다

---

## 3. 계약

### 3.1 요청

`POST /orders`에 **`Idempotency-Key` 헤더를 필수**로 요구한다.

| 규칙 | 값 |
|---|---|
| 위치 | HTTP 헤더 `Idempotency-Key` |
| 형식 | 공백 제외 1~128자(바이트가 아니라 문자 수. 서버는 rune, DB CHECK는 `length()`로 같은 단위를 쓴다). UUIDv4 권장, 강제하지 않음 |
| 범위 | **사용자별**. UNIQUE는 `(user_id, idempotency_key)`다 |
| 누락·빈 값 | **400** |

> **왜 전역이 아니라 사용자별인가.** 전역 UNIQUE면 남의 키와 충돌해 정상 주문이 409로
> 거절될 수 있고, 키를 알아낸 사람이 남의 주문 `order_id`를 읽을 수 있다.

### 3.2 상태 기계와 응답

레코드의 `outcome`은 **네 상태**를 가진다.

| outcome | 의미 | 재요청 응답 |
|---|---|---|
| `PENDING` | 주문·hold는 커밋됐고 **그 뒤의 결과가 durable하게 기록되지 않았다** | **202** `{order_id, status:"PENDING", idempotent_replay:true}` |
| `ACCEPTED` | 엔진 접수 완료 | **200** `{message, order_id, idempotent_replay:true}` |
| `REJECTED` | 엔진 접수 실패, hold 해제·`REJECTED` 종결이 **같은 트랜잭션에서 확정됨** | **503** + `order_id` |
| `UNKNOWN` | 엔진 접수 실패 후 hold 해제까지 실패했고, **그 사실을 기록하는 데 성공했다** | **503** + `order_id` + `status` + "주문 조회로 확인" |

> **`PENDING`은 "아직 진행 중"만 뜻하지 않는다.** 이후 UPDATE가 실패해도 `PENDING`에
> 머문다 — 즉 `PENDING`은 **"이 시점 이후를 서버가 durable하게 알지 못한다"** 는 뜻이다.
> 오래된 `PENDING`은 정상 진행이 아니라 **관측 대상**이다(§3.6).

**최초 실패도 같은 응답을 준다.** 저장된 결과의 재요청만 `order_id`를 받고 최초 실패는
평범한 503을 받으면, 같은 상태가 두 가지 응답으로 보인다. 엔진 접수에 실패하면 서비스가
결과와 오류를 **함께** 돌려주고, 핸들러는 그 결과로 `order_id`·`status`를 싣는다.

| 최초 접수 실패 | durable outcome | 응답 |
|---|---|---|
| 보상 성공(hold 해제·`REJECTED` 확정) | `REJECTED` | **503** + `order_id` + `status:"REJECTED"` |
| 보상 실패, `UNKNOWN` 기록 성공 | `UNKNOWN` | **503** + `order_id` + `status:"UNKNOWN"` |
| 보상 실패, `UNKNOWN` 기록도 실패 | `PENDING` | **503** + `order_id` + `status:"PENDING"` |

마지막 줄이 핵심이다. 기록이 실패했으면 DB는 여전히 `PENDING`이므로 응답도 `PENDING`이라고
말해야 한다 — `UNKNOWN`이라고 하면 저장된 상태보다 앞서 말하는 것이다.

**안내 문구는 outcome별로 다르다.** "같은 키로 재시도하면 된다"고 뭉뚱그리면 안 된다.
키는 이미 그 결과에 묶여 있어 재시도해도 같은 응답이 돌아온다.

| outcome | 안내 |
|---|---|
| `REJECTED` | 되돌리기가 끝났다. **새 주문에는 새 키**가 필요하다 |
| `UNKNOWN`·`PENDING` | 자동으로 해결되지 않는다. **주문 상태를 먼저 조회**해야 한다 |

| 그 밖의 상황 | 응답 |
|---|---|
| 첫 요청 성공 | **200** `{message, order_id}` (현행 그대로) |
| 같은 키 + 다른 지문 | **409** `{error:{code: IDEMPOTENCY_KEY_REUSED}}` |
| 키 누락·형식 위반 | **400** |
| 검증 실패(잔액·정책 등) | 기존 4xx 그대로. **키를 소비하지 않는다**(§3.4) |

`PENDING` 재요청이 **202**인 이유: 주문은 실재하지만 결과가 아직 없다. 200을 주면
"접수됐다"는 거짓이 되고, 503을 주면 "없다"는 거짓이 된다. B-1의 취소 202와 같은 논리다.

### 3.3 지문 — 버전 + 모호하지 않은 인코딩

```
fingerprint_version = 1
fingerprint = sha256( lp("v1") | lp(user_id) | lp(coin_symbol) | lp(side) |
                      lp(order_type) | lp(decimal_string(price)) |
                      lp(decimal_string(amount)) | lp(decimal_string(quote_amount)) )

lp(s) = uint32_be(len(s)) || s          # 길이-prefix
```

**단순 연결을 쓰지 않는다.** `"BTC" + "SELL"`과 `"BTCS" + "ELL"`이 같은 입력 문자열이 되면
서로 다른 요청이 같은 지문을 갖는다. 길이-prefix로 필드 경계를 모호하지 않게 만든다.

**DTO를 통째로 직렬화해 해시하지 않는다.** 필드 추가·키 순서·JSON 표현 변경만으로 기존
키가 전부 깨진다. 필드를 **명시적으로 나열**하고, 목록이 바뀌면 버전을 올린다.

- **decimal은 JSON 숫자나 부동소수점이 아니라 문자열**로 넣는다. 후행 0은 제거하되
  (`1.50` = `1.5`) **자릿수는 자르지 않는다** — 자르면 그 아래 자리만 다른 주문이 같은 지문을 받는다
- 지문은 시크릿이 아니므로 로그·문서에 남겨도 된다

#### 버전 규칙

| 상황 | 처리 |
|---|---|
| 신규 레코드 | **현재 버전**으로 계산해 저장 |
| 기존 키 재요청 | 저장된 레코드의 **그 버전 규칙으로** 다시 계산해 비교 |
| 새 필드가 기존 기본 동작과 **같은 값** | 이전 버전 규칙으로 비교해 **replay 허용** |
| 새 필드가 **주문 의미를 바꿈** | **409** |

비교는 항상 "저장된 버전"의 규칙으로 한다. 배포만으로 기존 재시도가 409가 되지 않는다.

### 3.4 키 소비 — **커밋 후에는 삭제하지 않는다**

| 단계 | 키 소비 | 근거 |
|---|---|---|
| `BuildOrder` 검증 실패 | **안 함** | DB에 아무것도 안 썼다 |
| 유입 게이트 503(**새 키만**) | **안 함** | 접수 자체가 없었다. 기존 키는 503이 아니라 replay/409로 처리한다(§1.2) |
| hold 트랜잭션 실패 | **안 함** | 통째로 롤백된다 |
| **hold 검증 실패(잔액 부족 등)** | **안 함 — §4.3의 명시적 삭제 필요** | 트랜잭션은 커밋되지만 이 주문은 없다 |
| hold 성공 후 커밋 | **소비 — 이후 삭제하지 않는다** | 주문이 실재한다 |
| 엔진 접수 실패 → `REJECTED`/`UNKNOWN` | **유지** | 아래 |

> **⚠ 초안은 여기서 틀렸다.** "엔진 접수 실패 시 키를 해제한다"고 썼는데, 그건 멱등성의
> 목적과 정면으로 충돌한다. 키가 사라지면 재시도가 **두 번째 주문을 만든다.**

**규칙: 주문이 커밋된 뒤에는 어떤 실패에서도 키를 삭제하지 않는다.** 새 주문을 원하면
클라이언트가 **새 키**를 만든다. 그게 "새 주문 의도"의 표현이다.

#### 엔진 접수 실패의 실제 semantics (코드 확인)

`TrySubmitOrder`(`matching/engine.go:332`)는 채널 send와 timer의 `select` 이분기다.
`select`는 정확히 한 case만 고르므로 **`false`는 "큐에 들어가지 않았다"가 결정적**이다.
**이 경로에 "전달 여부 불확실"은 없다.**

**진짜 불명 창은 그다음이다.** `rejectAcceptedOrder`가 실패하면
(`order_service.go:159-161`) 주문은 **`PENDING`인 채 hold가 잡힌 상태로 남는다.**

```go
if rerr := s.rejectAcceptedOrder(order); rerr != nil {
    return nil, fmt.Errorf("order intake saturated and hold release failed for order %d: %w", order.ID, rerr)
}
```

여기서 키를 삭제했다면 재시도가 **두 번째 주문 + 두 번째 hold**를 만들고 첫 hold는 그대로
남는다 — 자금이 두 배로 묶인다. 이 경우 `REJECTED`로 단정하지 않는다.

#### ⚠ `UNKNOWN`은 보장 상태가 아니라 best-effort 기록이다

`rejectAcceptedOrder`를 실패시킨 DB 장애는 **바로 뒤의 `UNKNOWN` UPDATE도 실패시킬 수
있다.** 그러면 durable 상태는 `UNKNOWN`이 아니라 여전히 `PENDING`이다. 엔진 접수에
성공한 뒤 `ACCEPTED` UPDATE만 실패하는 경우도 같다.

**따라서 "UNKNOWN으로 남긴다"를 보장으로 쓰지 않는다.** 실제 전이는 이렇다.

```
hold 트랜잭션 커밋
  └─ PENDING

TrySubmitOrder = true
  ├─ ACCEPTED 갱신 성공 → ACCEPTED
  └─ 갱신 실패          → PENDING 유지 + metric/log

TrySubmitOrder = false
  ├─ 보상 트랜잭션 성공 → REJECTED      (hold 해제 · 주문 REJECTED · outcome을 한 트랜잭션에)
  └─ 보상 실패
       ├─ UNKNOWN 갱신 성공 → UNKNOWN
       └─ UNKNOWN 갱신 실패 → PENDING 유지 + metric/log
```

- **`REJECTED` outcome 갱신은 보상 트랜잭션 안에 넣는다.** hold 해제·주문 `REJECTED`
  전환과 **함께 커밋되거나 함께 롤백**돼야 한다. 밖에 두면 "hold는 풀렸는데 outcome은
  `PENDING`"인 상태가 생긴다
- `UNKNOWN`은 **성공하면 좋은 기록**이다. 실패해도 정합성은 깨지지 않는다 —
  `PENDING`이 그 사실("이후를 알지 못한다")을 이미 정확히 표현한다
- 어느 경로든 **중복 주문은 생기지 않는다.** 키가 남아 있기 때문이다

> **⚠ 이 raw error는 현재 400으로 매핑된다.** `serviceErrorStatus`의 default가
> `StatusBadRequest`이기 때문이다 — B-1에서 `CancelOrder`에 대해 고친(`e0ef22a`) 것과
> **같은 클래스의 결함이 `CreateOrder`에도 남아 있다.** 이번 작업에서 함께 고친다.

#### 누가 해결하는가 — 자동 복구는 하지 않는다

**이번 범위에서는 `UNKNOWN`도 stale `PENDING`도 자동으로 해결하지 않는다.**

| 경로 | 이번 범위 |
|---|---|
| 재요청 시 보상 재시도 | **하지 않는다** — hold 해제 재시도는 이미 해제된 경우와 구분이 필요하고, 그 판정은 주문 상태 재조회를 요구한다 |
| 부팅 시 복구 | **하지 않는다** |
| 운영 부채 | **명시한다** — `UNKNOWN`과 **오래된 `PENDING`** 둘 다 사람이 조회해 처리한다 |

발생 빈도를 모른 채 자동 보상을 넣으면 그 보상 경로가 더 큰 위험이 된다.
**먼저 세고, 그다음에 고친다.**

### 3.5 관측 계약 — counter 하나로는 부족하다

`UNKNOWN`만 세면 **가장 흔할 실패(= outcome UPDATE 자체의 실패)가 보이지 않는다.**
그 경우 레코드는 `PENDING`에 머물고 아무 counter도 오르지 않는다.

| 지표 | 종류 | 무엇을 잡는가 |
|---|---|---|
| `order_idempotency_unknown_total` | Counter | 보상 실패 후 `UNKNOWN` 기록에 **성공한** 건수 |
| `order_idempotency_outcome_update_failures_total` | Counter | `ACCEPTED`/`REJECTED`/`UNKNOWN` **UPDATE 자체가 실패**한 건수 |
| `order_idempotency_stale_pending` | Gauge | 임계(예: 5분)를 넘긴 `PENDING` 레코드 수 — 주기적 DB 조회로 갱신 |

**stale `PENDING` gauge가 핵심이다.** UPDATE 실패도, 프로세스가 hold 커밋 직후 죽은
경우도 여기로 드러난다. counter는 "그 순간 코드가 살아 있었다"를 전제하지만 gauge는
그렇지 않다.

#### gauge 갱신 계약

```sql
SELECT count(*) FROM order_idempotency_keys
WHERE outcome = 'PENDING' AND updated_at < now() - interval '5 minutes';
```

| 항목 | 결정 |
|---|---|
| 소유 컴포넌트 | **전용 `OrderIdempotencyMonitor`.** 정산 worker에 얹지 않는다 — 책임이 섞인다 |
| 실행 간격 | **시작 직후 1회 즉시 조회, 그다음 30초 ticker** |
| 생명주기 | 서버 lifecycle context로 시작·종료. `backgroundCtx` 취소로 멈춘다 |
| 조회 실패 | **gauge를 0으로 덮지 않는다.** 마지막 값을 유지하고 오류 counter·로그를 남긴다 |
| 임계 | 5분(설정 가능) |

> **0으로 덮으면 안 되는 이유.** DB가 불안정할 때 gauge가 0으로 떨어지면 "문제가 사라졌다"로
> 읽힌다. 실제로는 **관측이 사라진 것**이다. 조회 실패는 그 자체로 알림 대상이다.

> **시작 직후 즉시 조회하는 이유.** 30초를 먼저 기다리면 재기동 직후 창에서 stale
> `PENDING`이 보이지 않는다 — 프로세스가 hold 커밋 직후 죽어서 생긴 레코드(§6-8g)가
> 정확히 그 창에 있다. 관측 지연 없이 성립해야 한다.

**모든 outcome 변경은 `updated_at`과 하나의 UPDATE 문에서 원자적으로 갱신한다.**
outcome 변경으로 부분 인덱스에서 빠지고, `updated_at`은 상태 전이 시각과 감사 근거를
보존한다.

#### ⚠ 이 gauge가 인덱스 결정을 바꾼다

주기 조회가 생겼으므로 **"스캔 대상이 없어 시간 인덱스를 만들지 않는다"는 앞선 판단은
더 이상 성립하지 않는다.** 하루 최대 6,048만 행 가정에서 인덱스 없이 30초마다 조회하면
매번 대형 테이블을 스캔한다.

```sql
CREATE INDEX order_idempotency_pending_updated_at
    ON order_idempotency_keys (updated_at)
    WHERE outcome = 'PENDING';
```

**부분 인덱스라 정상 상태에서는 거의 비어 있다.** 다만 **모든 주문이 잠시 `PENDING`을
거치므로 INSERT·DELETE churn이 생긴다** — 주문마다 인덱스 엔트리가 하나 들어왔다 나간다.
그래도 30초마다의 full scan보다 낫고, 그 비용은 §6-10에서 측정한다.

### 3.6 보존 — 주문 수명에 연동한다

**고정 TTL로 삭제하지 않는다.** 24시간 후 일괄 삭제하면 **아직 살아 있는 주문에 같은 키를
다시 써서 두 번째 주문을 만들 수 있다.**

| 규칙 | 값 |
|---|---|
| 원칙 | **주문이 존재하는 동안 유지** |
| 정리 시점 | **주문 보존 정책과 결합**. 주문을 지우지 않는 한 키도 지우지 않는다 |
| TTL이 필요해지면 | **주문이 terminal이 된 뒤부터** 계산(`PENDING`/`PARTIAL`/`UNKNOWN`은 만료 대상 아님) |
| 첫 구현 | **독립 cleanup worker를 만들지 않는다** |

#### 먼저 실측할 것 — 비용

35번에서 500 VU 10분에 주문 42만 건. 지속 부하 환산 시 **하루 약 6,048만 행**이다.
보존 기간 숫자보다 다음을 먼저 잰다.

- `(user_id, idempotency_key)` UNIQUE 인덱스의 **크기 증가율**
- **`order_idempotency_pending_updated_at` 부분 인덱스의 크기와 churn** — 모든 주문이
  `PENDING`을 거치므로 엔트리가 들어왔다 나간다
- INSERT 하나가 늘어남에 따른 **WAL 증가량**, 그리고 **`PENDING` → 최종 상태 전환이
  만드는 WAL**(부분 인덱스 엔트리 삭제 포함)
- 대량 삭제를 하게 될 때의 **삭제 비용과 autovacuum 영향**

33번에서 확인했듯 이 시스템의 비용은 크기 축에 민감하다.

---

## 4. 구현이 건드리는 지점 — 데이터 흐름을 고쳐야 한다

**이건 테이블만 추가하는 작업이 아니다.** 현재 배치 경로에 세 곳의 구조적 문제가 있다.

### 4.1 owner / follower 분리 — **엔진 제출은 한 번만**

현재 `CreateOrder`는 `HoldCoordinator.Submit`이 반환한 뒤 **호출자마다 각자
`TrySubmitOrder`를 호출**한다(`order_service.go:127`, `:143`). 대표 주문의 결과를 두 요청에
그냥 전달하면 **hold는 한 번인데 엔진 제출이 두 번**이 된다.

hold 결과에 역할을 실어 보낸다.

| 역할 | 의미 | 엔진 제출 |
|---|---|---|
| **owner** | 이 요청이 주문을 실제로 만들었다 | **한다** |
| **follower** | 같은 키의 중복(같은 배치 또는 기존 레코드) | **하지 않는다** |

follower의 응답은 어느 쪽 follower인지에 따라 다르다.

- **기존 레코드를 따라가는 follower**: 조회한 레코드의 `outcome`을 읽어 replay한다(§3.2).
  owner가 아직 제출 중이면 `PENDING` → **202**, 이미 접수됐으면 `ACCEPTED` → **200**이다.
- **같은 배치의 follower**: 별도로 조회해 온 `Existing` 레코드가 없다(행 자체는 leader의
  트랜잭션에서 함께 커밋됐다). 방금 커밋된 상태가 `PENDING`임을 알고 있으므로 그대로
  `PENDING` → **202**를 돌려준다.

어느 쪽이든 follower는 owner를 기다리지 않는다 — 기다리면 배치 지연이 요청 지연으로
전파되고, 202가 그 상태를 정직하게 표현한다.

> **이 분리가 없으면 §6-4가 통과할 수 없다.** 원장 `ORDER_HOLD`는 1건인데 엔진에는
> 주문이 두 번 들어가고, 두 번 체결될 수 있다.

### 4.2 트랜잭션 안에서의 순서

```
0. (트랜잭션 밖) 배치 내 (user_id, key) 그룹화 — §4.4
1. 멱등성 레코드 배치 INSERT ... ON CONFLICT DO NOTHING RETURNING (user_id, key)
   → 반환된 것 = owner 후보, 반환 안 된 것 = 기존 키(follower)
2. follower의 레코드 조회 — order_id·fingerprint·version·outcome
3. owner 후보로 기존 hold 로직 수행(지갑 락 → 검증 → 주문 INSERT → 지갑 UPDATE → 원장)
4. hold 검증에 실패한 owner 후보의 키를 이 트랜잭션에서 삭제 — §4.3
5. 성공한 owner의 레코드에 order_id 설정, outcome = 'PENDING' 배치 UPDATE
```

**5에서 `outcome`을 `ACCEPTED`로 쓰지 않는다.** 엔진 제출은 이 트랜잭션 밖에서 일어나므로,
커밋 시점에 확정된 사실은 "주문과 hold가 존재한다"까지다. `PENDING`이 그 사실의 정확한
표현이다.

이후 전이는 §3.4의 상태 전이도를 따른다.

| 엔진 제출 | outcome 기록 | 트랜잭션 |
|---|---|---|
| 성공 | `ACCEPTED` | 별도 UPDATE. **실패하면 `PENDING` 유지** + metric |
| 실패 | `REJECTED` | **보상 트랜잭션 안에서** hold 해제·주문 `REJECTED`와 **함께 커밋** |
| 실패 + 보상 실패 | `UNKNOWN` | best-effort UPDATE. **실패하면 `PENDING` 유지** + metric |

> **⚠ `REJECTED`를 보상 트랜잭션 밖에 두면 안 된다.** "hold는 풀렸는데 outcome은
> `PENDING`"인 상태가 생기고, 재요청이 202를 받아 아직 진행 중인 것처럼 보인다.
> `rejectAcceptedOrder`가 이 UPDATE를 포함하도록 고친다.

> **⚠ UPDATE가 실패하면 레코드는 `PENDING`에 머문다.** 재요청은 202를 받고 주문 조회로
> 확인하게 된다. 이것이 §2에서 "첫 HTTP 응답 재생은 보장하지 않는다"고 좁힌 이유이고,
> §3.5의 stale `PENDING` gauge가 필요한 이유다.

### 4.3 hold 검증 실패는 키를 소비하면 안 된다

`HoldBatch`는 잔액 부족 주문을 `results[i]`에 격리하고 **`continue`로 넘어간 뒤 나머지와
함께 커밋**한다(`hold_coordinator.go:113-135`). 전원 실패도 `return nil`로 **트랜잭션을
커밋**한다(`hold_coordinator.go:137`).

즉 §4.2의 1단계에서 **모든 키를 먼저 INSERT하면, 검증에 실패한 주문의 키까지 커밋된다.**
그러면 "검증 실패는 키를 소비하지 않는다"(§3.4)가 깨지고, 사용자는 같은 키를 다시 쓸 수
없게 된다.

**이번 트랜잭션에서 삽입했지만 hold 검증에 실패한 키는 커밋 전에 삭제한다.**

- 이번에 삽입한 키만 지운다. **기존 키(follower)는 건드리지 않는다**
- **전원 실패 조기 반환 경로에도 같은 정리가 필요하다** — `len(passing) == 0`에서
  `return nil` 하기 전에 삭제해야 한다

### 4.4 SQL 이전에 배치 안에서 먼저 그룹화한다

같은 배치에 같은 `(user_id, key)`가 두 번 들어올 수 있다. SQL의 `ON CONFLICT` 세부 동작에
맡기면 어느 요청에 결과를 줄지가 불명확하다. **애플리케이션에서 먼저 그룹화한다.**

| 배치 내 상황 | 처리 |
|---|---|
| 같은 키 · **같은 지문** | 하나를 **owner**, 나머지는 **follower**. owner만 엔진 제출 |
| 같은 키 · **다른 지문** | **결정적으로** 하나만 진행(도착 순서가 앞선 것)하고 나머지는 **409** |

"결정적으로"가 중요하다. map 순회 순서에 맡기면 같은 입력이 실행마다 다른 결과를 낸다.

### 4.5 배치 실패 fallback도 같은 계약이어야 한다

`processBatch`(`hold_coordinator.go:247`)는 `HoldBatch` 실패 시 **주문마다
`persistAndHold`로 개별 재시도**한다. 이 경로에도 같은 순서(§4.2)와 정리(§4.3)가 필요하고,
**배치 실패가 키를 소비하면 안 된다.**

### 4.6 필요한 변경

| 위치 | 변경 |
|---|---|
| 신규 테이블 | `order_idempotency_keys` (migration 008) |
| `internal/model/order_idempotency_key.go` | 모델 + `outcome` 상수 4종 |
| `internal/service/order_fingerprint.go` | 길이-prefix 버전형 지문 |
| `internal/repository/order_idempotency_repository.go` | 배치 INSERT-or-conflict, 조회, order_id·outcome UPDATE, **이번 트랜잭션 삽입분 삭제** |
| `service.CreateOrderInput` | `IdempotencyKey string` 추가 |
| `holdResult` | **`Role`(owner/follower)과 기존 레코드 정보 추가** |
| `HoldCoordinator.HoldBatch` | §4.2 순서 + §4.3 정리 + §4.4 그룹화 |
| `persistAndHold` | 단건 경로에도 동일 |
| `OrderService.CreateOrder` | **follower는 엔진 제출을 건너뛴다**, replay 응답 분기 |
| `rejectAcceptedOrder` | **`REJECTED` outcome UPDATE를 이 트랜잭션에 포함**. 키 삭제 없음 |
| `CreateOrder`의 실패 처리 | 보상 실패 시 `UNKNOWN` best-effort 기록, **실패해도 진행**, 5xx 매핑 |
| `OrderHandler.CreateOrder` | 헤더 파싱, 400/409/202 매핑, `idempotent_replay` |
| `internal/metrics` | `order_idempotency_unknown_total`, `order_idempotency_outcome_update_failures_total`, `order_idempotency_stale_pending`(Gauge), `order_idempotency_monitor_errors_total` |
| `internal/service/order_idempotency_monitor.go` | **전용 monitor** — 30초 주기 stale `PENDING` 조회로 gauge 갱신. 조회 실패 시 gauge 유지 + 오류 counter |
| `cmd/main.go` | `startOrderIdempotencyMonitor(backgroundCtx, config.DB)`로 시작·종료 |

> **gauge 조회는 `outcome = 'PENDING'`을 SQL 리터럴로 고정한다.** 파라미터로 넘기면
> PostgreSQL이 generic plan에서 부분 인덱스 predicate와의 일치를 증명하지 못해
> `order_idempotency_pending_updated_at`을 쓰지 못한다. 실측에서 파라미터 형태는
> `enable_seqscan = off`로 눌러도 Seq Scan으로 떨어졌다.

> **이 테이블은 AutoMigrate에 넣지 않는다.** B-1에서 확인한 대로, migration이 만든 UNIQUE를
> GORM이 자기 명명규칙으로 DROP하려 해 두 번째 부팅부터 실패한다(SQLSTATE 42704).

### 4.7 스키마 (migration 008)

```sql
CREATE TABLE IF NOT EXISTS order_idempotency_keys (
    id                  BIGSERIAL PRIMARY KEY,
    user_id             BIGINT      NOT NULL,
    idempotency_key     TEXT        NOT NULL,
    fingerprint         TEXT        NOT NULL,
    fingerprint_version INT         NOT NULL,
    order_id            BIGINT,
    outcome             TEXT        NOT NULL DEFAULT 'PENDING',  -- PENDING | ACCEPTED | REJECTED | UNKNOWN
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT order_idempotency_keys_user_key_unique UNIQUE (user_id, idempotency_key),
    CONSTRAINT order_idempotency_keys_key_length
        CHECK (length(btrim(idempotency_key)) BETWEEN 1 AND 128),
    CONSTRAINT order_idempotency_keys_outcome_check
        CHECK (outcome IN ('PENDING','ACCEPTED','REJECTED','UNKNOWN'))
);
```

```sql
-- 위 블록은 conname 존재만 본다. 같은 이름의 잘못된 제약이 이미 있으면 조용히 통과하므로
-- (인덱스의 IF NOT EXISTS와 같은 구멍), 실제 정의를 확인하고 어긋나면 실패시킨다.
-- 기대 문자열은 PostgreSQL 16.14와 18.4의 pg_get_constraintdef 출력이 동일함을 확인했다.
DO $$
DECLARE
    expected CONSTANT text[][] := ARRAY[
        ARRAY['order_idempotency_keys_user_key_unique',
              $def$UNIQUE (user_id, idempotency_key)$def$],
        ARRAY['order_idempotency_keys_key_length',
              $def$CHECK (((length(btrim(idempotency_key)) >= 1) AND (length(btrim(idempotency_key)) <= 128)))$def$],
        ARRAY['order_idempotency_keys_outcome_check',
              $def$CHECK ((outcome = ANY (ARRAY['PENDING'::text, 'ACCEPTED'::text, 'REJECTED'::text, 'UNKNOWN'::text])))$def$]
    ];
    constraint_name text;
    want text;
    got text;
BEGIN
    FOR i IN 1 .. array_length(expected, 1) LOOP
        constraint_name := expected[i][1];
        want := expected[i][2];

        SELECT pg_get_constraintdef(oid) INTO got
        FROM pg_constraint
        WHERE conrelid = 'order_idempotency_keys'::regclass
          AND conname = constraint_name;

        IF got IS NULL THEN
            RAISE EXCEPTION 'constraint % is missing on order_idempotency_keys', constraint_name;
        END IF;

        -- 공백만 다른 경우는 같은 제약으로 본다.
        IF regexp_replace(got, '\s+', '', 'g') <> regexp_replace(want, '\s+', '', 'g') THEN
            RAISE EXCEPTION 'constraint % has an unexpected definition: %', constraint_name, got;
        END IF;
    END LOOP;
END $$;
```

```sql
-- stale PENDING gauge 조회 전용(§3.5). 정상 상태에서는 거의 비어 있다.
CREATE INDEX IF NOT EXISTS order_idempotency_pending_updated_at
    ON order_idempotency_keys (updated_at)
    WHERE outcome = 'PENDING';

-- IF NOT EXISTS는 "같은 이름의 다른 인덱스"도 조용히 통과시킨다(006에서 확인한 구멍).
-- 같은 Up 안에서 카탈로그로 검증하고, 어긋나면 migration을 실패시켜 version 8이
-- 기록되지 않게 한다.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_class index_rel
        JOIN pg_index index_meta ON index_meta.indexrelid = index_rel.oid
        JOIN pg_am access_method ON access_method.oid = index_rel.relam
        JOIN pg_attribute column_meta
          ON column_meta.attrelid = index_meta.indrelid
         AND column_meta.attnum = index_meta.indkey[0]
        WHERE index_rel.relname = 'order_idempotency_pending_updated_at'
          AND index_meta.indrelid = 'order_idempotency_keys'::regclass
          AND index_meta.indisready
          AND index_meta.indisvalid
          AND NOT index_meta.indisunique
          AND access_method.amname = 'btree'
          AND index_meta.indnkeyatts = 1
          AND index_meta.indnatts = 1
          AND column_meta.attname = 'updated_at'
          AND index_meta.indexprs IS NULL
          AND pg_get_expr(index_meta.indpred, index_meta.indrelid)
              = '(outcome = ''PENDING''::text)'
    ) THEN
        RAISE EXCEPTION
            'order_idempotency_pending_updated_at is missing, invalid, or has the wrong definition';
    END IF;
END $$;
```

**`IF NOT EXISTS`만으로는 부족하다.** 같은 이름의 잘못된 인덱스(예: predicate 없는 전체
인덱스, 다른 컬럼, 중단된 build의 `indisvalid=false` 잔해)가 있으면 생성문이 **조용히
성공**하고 goose는 version 8을 기록한다. 그러면 gauge 조회가 full scan으로 떨어지거나
플래너가 인덱스를 쓰지 않는데도 아무도 모른다. 006에서 같은 구멍을 확인했으므로 같은
방식으로 막는다 — **생성·검증·예외가 한 Up에 있어야 한다.**

> predicate 문자열 비교는 Postgres가 정규화한 형태(`(outcome = 'PENDING'::text)`)와
> 맞춰야 한다. 구현 시 실제 출력으로 확인한다.

- `order_id`만 **nullable**이다. §4.2의 1단계에서는 아직 모른다
- `outcome`은 **첫 INSERT부터 `PENDING`**이다(`NOT NULL DEFAULT 'PENDING'`). 커밋 시점의
  상태는 "이 시점 이후를 서버가 durable하게 알지 못한다"로 이미 확정돼 있다
- `(user_id, idempotency_key)` UNIQUE가 중복 방지의 **유일한** 근거다
- **부분 인덱스는 지금 만든다.** 30초 주기 gauge 조회가 생겼으므로 "스캔 대상이 없다"는
  전제가 사라졌다. 모든 주문이 `PENDING`을 거치므로 **churn이 생기고**, 그 비용은 §6-10에서
  측정한다
- **`created_at` 전체 인덱스는 만들지 않는다.** cleanup worker가 없어 여전히 스캔 대상이
  없고, 보존 정책은 §3.6의 비용 측정 결과를 보고 정한다

---

## 5. 동시성

서로 다른 배치의 동시 요청:

1. 둘 다 애플리케이션 검사를 통과한다(아직 아무 행도 없다)
2. 먼저 도달한 트랜잭션이 UNIQUE 인덱스에 행을 넣는다
3. 두 번째 트랜잭션의 INSERT는 **첫 트랜잭션이 커밋/롤백할 때까지 블록**된다
4. 첫 트랜잭션이 커밋되면 두 번째는 conflict → **follower** → 저장된 `order_id`·`outcome` 반환
5. 첫 트랜잭션이 롤백되면 두 번째의 INSERT가 성공 → **owner**

**애플리케이션 코드에 lock이 없다.** DB 제약이 직렬화한다.
같은 배치 안의 중복은 §4.4가 SQL 이전에 처리한다.

> **⚠ 배치 경로의 부작용.** 3번의 블록은 두 번째 요청이 속한 **배치 전체**를 대기시킨다.
> 부하에서 확인해야 할 항목이다.

---

## 6. 사전 등록 검증

각 항목은 **실패하는 것을 먼저 확인한 뒤** 통과시킨다.

| # | 검증 | 방법 |
|---|---|---|
| 1 | 키 누락 → 400, 주문 0건 | 헤더 없이 요청 |
| 2 | 공백·초과 길이 → 400 | 경계값 |
| 3 | 같은 키·같은 지문 순차 재시도 → 같은 `order_id`, `idempotent_replay` | **`ORDER_HOLD` 원장 건수 = 1** 로 판정 |
| 4 | **같은 키 동시 100회 → 주문 1건 AND 엔진 제출 1회** | 원장 1건 + **fake engine의 `TrySubmitOrder` 호출 수 = 1** |
| 4b | **같은 배치·같은 키·같은 지문 2건** | 배치 크기를 키워 한 배치에. 원장 1건, **엔진 제출 1회** |
| 4c | **같은 배치·같은 키·다른 지문 2건** | 하나만 진행, 나머지 409. **같은 입력 반복 실행 시 같은 쪽이 성공**(결정성) |
| 5 | 같은 키·다른 지문 → 409, 원래 주문 무변경 | 수량만 바꿔 재요청 |
| 6 | 지문 정규화 — `1.50` vs `1.5` 동일 | 단위 테스트 |
| 6b | 지문 **경계 모호성** — `("BTC","SELL")` vs `("BTCS","ELL")`가 **다른 지문** | 단위 테스트 |
| 6c | 지문 **버전** — v1 저장 키는 v1 규칙으로 비교 | 현재 버전 상수를 2로 올려도 v1 golden 해시가 그대로 |
| 6d | 지문이 **자릿수를 보존** — 19번째 소수 자리만 다른 두 주문이 다른 지문 | 단위 테스트 |
| 7 | 검증 실패는 키를 소비하지 않는다 | 잔액 부족 4xx 후 같은 키로 정상 요청 → 성공 |
| 7b | **혼합 배치(성공 1 + 잔액 부족 1)** | 실패 키가 **DB에 없고** 재사용 가능. 성공 키는 남음 |
| 7c | **전원 실패 배치** | 조기 반환 경로에서도 삽입한 키 전부 삭제됨 |
| 8 | 엔진 접수 실패 후 같은 키 재시도 → **새 주문 없음** | 같은 `order_id`, `outcome=REJECTED`, 주문 1건 |
| 8b | **보상 성공과 `REJECTED` 기록이 함께 커밋되거나 함께 롤백** | 보상 트랜잭션 중간 실패를 강제 → hold도 안 풀리고 outcome도 `PENDING`(부분 반영 0) |
| 8c | **hold 해제 실패 → `outcome=UNKNOWN`, 키 유지, 5xx** | 재시도가 같은 `order_id` 반환, hold 중복 0 |
| 8d | **보상 실패 + `UNKNOWN` UPDATE도 실패 → `PENDING` 유지** | 중복 주문 0, `outcome_update_failures_total` 증가 |
| 8e | **엔진 접수 성공 후 `ACCEPTED` UPDATE 실패 → `PENDING` 유지** | 같은 키 재요청은 **202**, 주문·hold·엔진 제출 여전히 **각 1** |
| 8f | **`PENDING` 창의 재요청 → 202** | outcome UPDATE 전에 재요청, 주문 1건 |
| 8g | **hold 커밋 직후 프로세스 종료 → stale `PENDING` 관측** | 재기동 후 gauge에 그 레코드가 잡힌다 |
| 9 | 배치 실패 fallback에서도 계약 유지 | `HoldBatch` 강제 실패 → 3·4·7b 재확인 |
| 9b | **monitor 생명주기** | `backgroundCtx` 취소로 정지. 조회 실패 시 **gauge를 0으로 덮지 않고** 마지막 값 유지 + `monitor_errors_total` 증가 |
| 9c | **`updated_at` 원자 갱신** | 모든 outcome 전이에서 `outcome`과 `updated_at`이 **한 UPDATE 문**에서 함께 바뀐다(전이 시각·감사 근거 보존) |
| 9d | **migration 008 카탈로그** | 부분 인덱스가 `indisready`·`indisvalid`·btree·단일 키 `updated_at`·non-unique·predicate `outcome='PENDING'` |
| 9e | **같은 이름의 잘못된 인덱스 → migration 실패** | 예: 전체 인덱스를 같은 이름으로 만들어 둔 뒤 008 실행 → `RAISE EXCEPTION`, **goose version 8이 기록되지 않음** |
| 9f | **같은 이름의 잘못된 제약 → migration 실패** | UNIQUE 범위·키 길이 상한·outcome 목록을 각각 틀리게 만들어 둔 뒤 008 실행 → 세 경우 모두 `RAISE EXCEPTION`, **version 8 미기록** |
| 10 | **비용 실측** | UNIQUE 인덱스 크기 증가율, **부분 인덱스 크기·churn**, WAL 증가량, **`PENDING` → 최종 상태 전환 WAL** 을 부하 전후로 기록(§3.6) |
| 11 | 회귀 | 주문 SLI·p95가 새 기준선 대비 회귀 없음(**게이트**) |

> **판정 기준을 상태로 두지 않는다.** 3·4는 `ORDER_HOLD` 원장 건수로, 4·4b는 **엔진 제출
> 호출 수**로 판정한다. B-1에서 확인했듯 상태만 보면 "두 번 실행됐지만 결과가 같아 보이는"
> 경우를 놓친다.

---

## 7. 결정 요약

| 항목 | 결정 |
|---|---|
| (a) 키 필수화 | ✅ 승인 — 누락·빈 값 400, UNIQUE는 `(user_id, key)` |
| (b) 재시도 응답에 현재 상태 | 싣지 않는다. `outcome` 기반으로 재구성. `UNKNOWN`만 "조회로 확인" |
| (c) 보존 | ✅ 주문 수명 연동. 고정 TTL·cleanup worker 없음. 비용 먼저 실측 |
| (d) 지문 | ✅ 버전형 + 길이-prefix 인코딩 |
| **owner/follower** | **엔진 제출은 owner만** (§4.1) |
| **hold 검증 실패 키 정리** | **커밋 전 삭제** (§4.3) |
| **outcome 초기값** | **`PENDING`** — 커밋 시점에 확정된 사실만 쓴다 (§4.2) |
| **`REJECTED` 기록** | **보상 트랜잭션 안에서** hold 해제·주문 종결과 함께 커밋 (§3.4) |
| **`UNKNOWN`** | **보장이 아니라 best-effort.** 실패하면 `PENDING` 유지 (§3.4) |
| **관측** | `unknown_total` + **`outcome_update_failures_total`** + **stale `PENDING` gauge** (§3.5) |
| **gauge 소유** | **전용 `OrderIdempotencyMonitor`**, 30초 주기, lifecycle context, 조회 실패 시 gauge 유지 (§3.5) |
| **부분 인덱스** | `(updated_at) WHERE outcome='PENDING'` — gauge 조회가 생겨 **지금 만든다**. 006과 같은 방식으로 **같은 Up에서 카탈로그 검증** (§4.7) |
| **자동 복구** | 없음. `UNKNOWN`과 stale `PENDING` **둘 다** 운영 부채로 명시 (§3.4) |

---

## 8. 클라이언트 영향

| 대상 | 변경 |
|---|---|
| `src/lib/api.ts` | `createOrder`가 `Idempotency-Key` 헤더 전송 |
| `OrderForm.tsx` | **사용자 주문 시도마다 키를 한 번 생성**. 네트워크 재시도는 **같은 키**. 사용자가 다시 제출하면 **새 키**. **202 응답 처리 추가** |
| E2E | 중복 제출 → 같은 `order_id`, `idempotent_replay: true` |
| k6 3종 + 공개 spike | **iteration마다 새 키**, 단 **그 iteration 안의 재시도는 같은 키** |

> **k6에서 가장 틀리기 쉬운 지점.** iteration 간 키 재사용은 두 번째부터 전부 replay가 되어
> **주문이 생성되지 않는다.** 반대로 iteration 안의 재시도마다 새 키를 만들면 멱등성을
> 전혀 검증하지 못한다.

---

## 9. 완료 정의

1. §6의 27개 검증이 **RED → GREEN**으로 기록됐다
2. 키 없는 요청이 400이고, 모든 클라이언트가 키를 보낸다
3. 동시 100회에서 **`ORDER_HOLD` 1건 AND 엔진 제출 1회**
4. **혼합 배치에서 실패 키가 소비되지 않는다**
5. 배치·fallback 두 경로가 같은 계약을 지킨다
6. **커밋된 키가 어떤 실패에서도 삭제되지 않는다**(§4.3의 미커밋 정리는 예외)
7. **`REJECTED` 기록이 보상 트랜잭션과 함께 커밋/롤백된다**(부분 반영 0)
8. **어떤 outcome UPDATE 실패에서도 중복 주문이 생기지 않고 `PENDING`이 유지된다**
9. `CreateOrder`의 hold 해제 실패가 400이 아니라 5xx로 매핑된다
10. 지표 4종이 노출되고, **stale `PENDING`이 실제로 관측된다**
11. **monitor가 lifecycle로 시작·종료되고, 시작 직후 1회 즉시 조회하며, 조회 실패가
    gauge를 0으로 덮지 않는다**
12. **migration 008이 부분 인덱스를 카탈로그로 검증하고, 어긋나면 version 8을 기록하지 않는다**
11. 자동 해결이 없다는 것과 운영 부채 범위가 문서에 있다
12. 인덱스 크기·WAL 증가량이 기록됐다
13. 주문 SLI·p95 회귀 게이트를 통과했다
