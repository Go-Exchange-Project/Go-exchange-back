# 35 (2026-08-23): 취소 command outbox — 내구성 계약 전환과 새 기준선

> **설계**: [2026-08-18-cancel-command-outbox-design.md](../superpowers/specs/2026-08-18-cancel-command-outbox-design.md) ·
> **계획**: [2026-08-19-cancel-command-outbox.md](../superpowers/plans/2026-08-19-cancel-command-outbox.md)
> **선행**: [34 인덱스 단독 수정 재측정](34-2026-08-18-buy-order-index-remeasurement.md)
>
> **성격**: 성능 개선 측정이 아니라 **정확성 부채(B) 1번 항목**의 계약 전환이다.
> 이번 측정의 목적은 "빨라졌는가"가 아니라 **"바뀐 계약이 부하에서 지켜지는가"** 다.

## 1. 기준 SHA와 스키마

| 항목 | 값 |
|---|---|
| **측정 SHA (backend)** | `9523ab968bfb1a4f80b9d08e7c9f45c759513cd3` |
| 측정 SHA (frontend) | `074c447a8f33a65951220ec494c0e28c9ddc369b` |
| Backend CI | run `32482988872` — **success** (unit·vet / Postgres 통합 / Docker build / GHCR publish) |
| Frontend CI | run `32483003454` — **success** (unit·lint·build / Docker build / GHCR publish) |
| 측정 이미지 | `goexchange-back:cancel-outbox` `sha256:854facdaeb8b7d4e71075433ba050d9be43c09758870387dcf43f56e8f829c7e` |

> 이 문서 자체의 커밋 SHA는 문서 안에 적을 수 없다. **측정 SHA는 위의 `9523ab9`이고**,
> 최종 문서 SHA는 커밋 이후 별도로 보고한다.

이미지는 GHCR이 비공개라 pull 대신 `git archive 9523ab9`를 서버 VM으로 전달해 빌드했다
(archive 체크섬 로컬/원격 일치 확인). CI가 검증한 것과 **같은 소스 트리**다.

### migration 007 카탈로그 (측정 DB 실측)

```
cancel_commands_order_unique  | UNIQUE (order_id)            | (full)  | indisunique=t
cancel_commands_pending       | (status = 'PENDING'::text)   | 부분     | indisunique=f
price                         | numeric | is_nullable=NO
goose_db_version              | 7
```

`order_id` UNIQUE가 **부분 인덱스가 아니라는 것**이 이 설계의 핵심이다. `PENDING`만 막으면
command가 `PROCESSED`이고 정산이 아직 끝나지 않은 창에서 두 번째 command가 생겨
`ORDER_RELEASE`가 두 번 날 수 있다.

원본: `_workspace/cancel-command-outbox/gcp/canceloutbox500r1/canceloutbox500r1-db.tgz`
→ `cancel-outbox/integrity-canceloutbox500r1.txt` `[1]`–`[1d]`

---

## 2. 무엇이 바뀌었나 — 202는 "완료"가 아니라 "접수"다

`DELETE /orders/:id`는 이제 매칭 엔진을 **호출하지 않는다.** 주문을 잠근 트랜잭션에서
`cancel_commands` 행을 만들고 커밋한 뒤 `202 Accepted`로 끝난다.

```json
{ "message": "cancellation accepted", "order_id": 123, "command_id": 456, "status": "ACCEPTED" }
```

이후 `CancelCommandWorker`가 그 command를 엔진에 전달하고, `OutboxWriter`가
**execution outbox INSERT와 command `PROCESSED`를 한 트랜잭션**으로 커밋한다.

### 왜 이 순서여야 하는가

기존 `processCancel`은 ① 오더북 제거 → ② `ResponseCh` 응답 → ③ `emitOrderCancelled` 순서다.
**②와 ③ 사이에 프로세스가 죽으면** 사용자는 취소 성공을 받았는데 오더북 제거는 메모리에만
있었고 outbox에도 기록되지 않는다. DB의 주문은 여전히 `PENDING`이므로 재기동 시
bootstrap이 **그 주문을 다시 오더북에 올린다** — 취소했다고 믿은 주문이 다시 체결된다.

엔진 호출은 내구 기록보다 앞설 수 없다. 그래서 응답 의미를 바꿨다.

### ⚠ 사용자가 체감하는 대가 — 추가 체결 가능 창

**202는 "오더북에서 제거됐다"가 아니다.** 응답 시점에 주문은 아직 오더북에 있고,
그 사이에 체결될 수 있다. 이전 계약(200 + `released_amount`)은 응답 시점에 제거가 끝나
있었으므로 이것은 **실제 의미 변화**다.

end-to-end 상한은 **약속하지 않는다.** 엔진의 `CancelOrder`는 enqueue 1초와 response 1초를
순차로 기다려 한 호출만으로 약 2초가 걸릴 수 있고, worker backlog와 DB 왕복은 상한이 없다.
보장하는 것은 **wake-up 신호가 유실돼도 50ms 이내에 다음 polling 시도를 시작한다**는 것뿐이며,
실제 dispatch 시작 시각은 보장하지 않는다. 관측은 §5의 metric으로만 한다.

### 사라진 응답 필드

`released_asset` · `released_amount` · `engine_removed`는 **응답 시점에 알 수 없는 값**이므로
제거했다. 프런트는 202 직후 "취소 요청 접수됨"을 표시하고 주문 조회를 polling해
`CANCELLED`/`FILLED`를 확인한 뒤 최종 문구를 정한다. **polling 시간 초과는 실패가 아니라
"접수됨 · 처리 중"** 이다.

---

## 3. 사전 등록 검증 결과 (로컬)

설계 §6에 측정 전 고정한 항목이다. 각 항목은 **실패하는 것을 먼저 확인한 뒤** 통과시켰다.

| # | 검증 | 테스트 | 결과 |
|---|---|---|---|
| 1 | command commit 후 크래시 → worker 재실행이 완료 | `IntegrationCancelCommandCrashBeforeOutboxIsRecovered` | PASS |
| 2 | outbox commit 후 크래시 → replay가 완료 | `...CrashAfterOutboxIsFinishedByReplay` | PASS |
| 3 | 202 직후 종료해도 주문 부활 0 | `...RestartDoesNotResurrectOrder` | PASS |
| 4 | 동시 100회 취소 → `ORDER_RELEASE` 정확히 1건 | `...ConcurrentRequestsReleaseHoldOnce` | PASS |
| 4b | `PROCESSED` 후 정산 전 재요청 → 여전히 1건 | `...RepeatBeforeSettlementReleasesOnce` | PASS |
| 5 | 이미 체결된 주문 → `NOOP`, hold 해제 0 | `...OnFilledOrderBecomesNoop` | PASS |
| 6 | DB open + 엔진 미발견 → `PENDING` 유지·재시도 | `...StaysPendingWhenEngineMissesOpenOrder` | PASS |
| 8 | wake 유실 → 50ms 이내 다음 polling **시도** | `CancelCommandWorkerPollsWithoutWake` | PASS |
| 8b | 엔진 응답 지연 중 재투입 0 | `...DoesNotDispatchTwiceWhileEngineIsSlow` | PASS |
| 8c | 재투입 간격 지수 증가 | `...BacksOffOnRepeatedEngineErrors` | PASS |
| 8d | outbox 커밋 전 재투입 0 (deadline 넘겨도) | `...HoldsAwaitingOutboxUntilStatusChanges`, `...DoesNotRedispatchWhileOutboxIsBlocked` | PASS |
| 8e | not-found + open → 재시도 / terminal → `NOOP` | `...RetriesNotFoundWhileOrderIsOpen`, `...MarksNoopWhenOrderIsTerminal` | PASS |
| 9 | `RowsAffected` 불일치 → outbox INSERT까지 rollback | `IntegrationTradeOutboxRollsBackOnCancelCommandMismatch` | PASS |

### 판정 기준을 바꾼 두 곳 (중요)

**3번** — 처음에는 `trades` 테이블 행 수로 판정했으나 **그 테이블은 `SettlementService`가
채운다.** 하니스는 정산을 돌리지 않으므로 실제로 체결돼도 0이 나온다. sentinel 주문으로
crossing 주문의 처리 완료를 보장한 뒤 **오더북 스냅샷**(ask 100 존재 · bid 100 부재)으로
판정하도록 고쳤다. 취소 제거를 생략하는 mutation에서 실패하는 것을 확인했다.

**8d(통합)** — 상태로는 구분되지 않는다. 재투입되면 엔진이 not-found를 주고 주문은 아직
open이므로 worker는 `NOOP`이 아니라 `RecordAttempt` + backoff로 가며 상태는 그대로
`PENDING`이다. 판정 기준을 **`attempt_count`** 로 바꿨고, `awaiting_outbox` 보류를 제거하는
mutation에서 실패하는 것을 확인했다.

---

## 4. GCP 500 VU — 실행 조건과 게이트

### 실행 조건

| 항목 | 값 |
|---|---|
| phase | `canceloutbox500r1` — **1회 실행, 폐기 0** |
| VU | 합산 **500** (load-gen당 250, `VU_LEVEL_SCALE=2`) |
| 구간 | ramp 30초 + hold **10분**, hold window `1787319837`–`1787320437` |
| 구성 | Server `e2-highcpu-4` · DB `e2-highcpu-8` · load-gen `e2-standard-8` ×2 (34번과 동일) |
| 런타임 | partitions **10** · concurrency **8** · goose **7** |
| k6 | v2.1.0+dirty, snap rev **56** (양쪽 `held`, v2.2.0 rev 57은 `disabled,held`) |
| iteration | A 209,845 / B 210,429, **interrupted 0** |

**concurrency는 3단계 모두 실측했다.** `.env`에는 `GOEXCHANGE_SETTLEMENT_CONCURRENCY=4`가
들어 있어(34번에서도 선언값과 실제값이 어긋났던 지점) 프로젝트 디렉터리의 `.env`를 8로
고쳐 override한 뒤, **최종 기동 컨테이너 기준으로** 확인했다.

| 단계 | 값 |
|---|---|
| `docker compose config` | `GOEXCHANGE_SETTLEMENT_CONCURRENCY: "8"` |
| 컨테이너 `printenv` | `8` |
| 기동 로그 | `settlement partitions=10 concurrency=8` |

부하 시작 전 DB는 `cancel_commands`를 포함해 **11개 테이블을 truncate**했고,
`matching bootstrap completed: loaded=0`과 **`startup barrier passed: recovered cancel
commands drained`** 를 확인했다. dev token은 양쪽 load-gen에서 `container vs loadgen: MATCH`
(cmp), 구 토큰 **403** / 현재 토큰 **200**으로 확인했으며 값·해시·지문은 남기지 않았다.

### 사전 등록 게이트 — 둘 다 PASS

| 게이트 | 사전 등록 기준 | A | B | 결과 |
|---|---|---|---|---|
| hold `sli_cancel_success{vu_level:500}` | **100.00000%** | 14,813 pass / **0 fail** | 14,728 pass / **0 fail** | **PASS** |
| `custom_cancel_fail` 및 status 0·5xx 취소 | **0건** | 0 | 0 | **PASS** |

404/409는 정상 경쟁이라 분모에서 제외된다(설계대로). 전체 구간(ramp 포함)으로도 fail 0이다
(A 15,181 / B 15,086).

### 기존 하드 게이트 (함께 보고, 새 합격선 만들지 않음)

| 항목 | A | B |
|---|---|---|
| 주문 가용성 (hold) | 204,377 / 0 fail | 205,075 / 0 fail |
| 주문 업무 성공 (hold) | 204,377 / 0 fail | 205,075 / 0 fail |
| 1초 초과 | **0건** | **0건** |
| HTTP 실패 | 0 / 248,425 | 0 / 248,895 |
| k6 checks | 209,845 / 0 | 210,429 / 0 |
| 정합성 4종 | `failed_settlements` · `failed_market_completions` · `failed_order_cancellations` · `reconciliation_violations` **모두 0** | |

### 계약이 부하에서 지켜졌다는 직접 증거

세 경로가 **같은 수** 로 맞아떨어진다.

| 경로 | 값 |
|---|---|
| backend 로그의 `DELETE` 응답 — **202** | **30,267** |
| k6 취소 성공 (A 15,181 + B 15,086) | **30,267** |
| `cancel_commands` 종결 (PROCESSED 30,195 + NOOP 72) | **30,267** |

- `DELETE` 응답 분포: **202 30,267 · 409 45,779 · 200 0** — 200이 0이라는 것은 구 계약이
  남아 있지 않다는 뜻이다
- **`cancel_commands` PENDING 잔여 0** — 모든 command가 종결 상태에 도달했다
- **`ORDER_RELEASE`가 2건 이상인 주문 0개.** CANCELLED **72,355**건 : `ORDER_RELEASE`
  **72,355**건 = **1:1**
- execution outbox 전량 `PROCESSED`, PENDING 0
- `cancel_command_awaiting_outbox_deadline_total` 증가량 **0** — outbox 커밋이 한 번도
  정체되지 않았다
- backend 로그 error·panic **0줄**

### 산출물

`_workspace/cancel-command-outbox/gcp/canceloutbox500r1/`

| 파일 | sha256 |
|---|---|
| `canceloutbox500r1-db.tgz` | `ae1374cf79281269e1367ea7114ec16b9e72849635e8ae79d22d14aac8897de1` |
| `canceloutbox500r1-loadgen-a.tgz` | `8d721a2b8c1c2c61b408b36cbd42ab99f2b558a49c374324cb9cf8d66ea5f328` |
| `canceloutbox500r1-loadgen-b.tgz` | `ec388f29d9634d23d6c678fe4ad3e3d8200d017c9042b0966ab7b7bcc052627f` |
| `canceloutbox500r1-prom.tgz` | `9d6c55a6ae6928287056fc69a62c74bb4762cb501643905c50b18e0fe4cdb673` |
| `canceloutbox500r1-server.tgz` | `73ce805a198ce567e7c817dac236b279e43d1ed98431dc6df0e2d86415c9c8fa` |

k6 summary의 `setup_data`에는 사용자별 JWT 500개가 들어 있었다. **보관 전에 제거**했고
(개수만 남김), 압축을 다시 풀어 JWT·token 패턴 **0건**을 재검증했다. 체크섬은 정리 후 값이다.

---

## 5. 새 기준선 — 합격선이 아니다

**이 절의 수치에는 PASS/FAIL이 없다.** 취소 계약이 이번에 처음 바뀌었으므로 비교 대상이
없고, 다음 변경분의 회귀 게이트로 쓰기 위해 기록만 한다.

hold 구간 `1787319837`–`1787320437` 고정, `increase(...[10m])` 기준.

| 지표 | 값 |
|---|---|
| `cancel_command_latency_seconds` p50 | **16.6ms** (0.016611788495201302) |
| p95 | **49.4ms** (0.0493718920934858) |
| p99 | **180.3ms** (0.18028787878787522) |
| 관측 수 | **추정 증가량 ≈29,547.23** |
| HTTP `DELETE /orders/:id` 202 p95 | **7.9ms** (0.007947934448617272) |
| `cancel_command_awaiting_outbox_deadline_total` 증가량 | **0** |

> **관측 수를 정수로 읽지 않는다.** `increase()`는 구간 경계를 보간하므로 `29547.23`은
> 이벤트 개수가 아니라 **추정 증가량**이다. 정확한 종결 건수는 §4의 `cancel_commands`
> 30,267(= PROCESSED 30,195 + NOOP 72)이며, 그 값은 DB에서 직접 센 것이다.

`cancel_command_latency_seconds`는 **command commit → `PROCESSED`/`NOOP` 커밋**까지다.
§2에서 밝힌 대로 애플리케이션은 이 구간의 상한을 약속하지 않는다 — 이 히스토그램은
나중에 상한을 논하기 위한 유일한 근거다.

PromQL과 응답 JSON 원본: `canceloutbox500r1-prom.tgz`

---

## 6. 34번과 비교하지 않는 이유

**34번 수치를 PASS/FAIL 비교에 사용하지 않았다.** 참고만 한다.

1. **측정 하니스가 바뀌었다.** 취소 SLI 정의가 `200/(200+인프라 실패)`에서
   `(200|202)/(…)`로 바뀌었다. `sli-classify.js`에 202를 추가하지 않으면 취소 성공률이
   0%로 보인다 — 같은 이름의 지표지만 같은 것을 세지 않는다.
2. **`matching` 패키지가 바뀌었다.** `CancelOrderCommand`·`OrderCancelled`에 `CommandID`가
   추가됐고, 취소 경로에 DB 쓰기가 하나 늘었다. 34번까지의 기준선(`82b4d7f` 계열)과
   같은 바이너리가 아니다.
3. **취소의 의미가 다르다.** 34번의 취소 응답은 "제거 완료"였고 이번은 "접수"다.
   응답 시간을 나란히 놓으면 다른 일을 한 두 값을 비교하게 된다.

이번 측정이 34번과 공유하는 것은 **topology와 워크로드 형태**뿐이다. 그래서 §4의 기존
하드 게이트는 "함께 보고"하되 새 합격선을 만들지 않았고, §5는 기준선으로만 남겼다.

**최고 검증 용량은 여전히 합산 500 VU다.** 이번 측정은 500에서만 수행했고 750은 시도하지
않았다.

---

## 7. 결론과 남은 것

### 확정된 것

- 취소 의도가 **엔진 호출 전에 내구 기록**되고, 크래시 지점과 무관하게 **재실행 또는
  outbox replay 중 하나로 덮인다** — 로컬 검증 1·2·3
- execution outbox 저장과 command 종결이 **한 커밋**이라 두 상태가 갈라지지 않는다 — 검증 9
- 500 VU 10분에서 **취소 acceptance 100%, 인프라 실패 0**, `ORDER_RELEASE` 중복 0
- API가 실제 보장과 같은 말을 한다(202 / `ACCEPTED`)

### 아직 아닌 것

- **`maxConsecutiveCancels` 재튜닝은 하지 않았다.** 취소 도착 패턴이 HTTP 직접 투입에서
  worker dispatch로 바뀌었으므로 이 dispatch 패턴 위에서 다시 정해야 한다(B의 다음 항목).
- **end-to-end 상한은 여전히 없다.** §5의 분포가 쌓인 뒤에 논한다.
- 750 VU 재확인, 625 VU 해상도, 잔여 성장 진단은 **보류 목록**에 그대로 있다.

---

## 부록 A. 측정 조건과 무관한 운영 변경

**측정이 끝난 뒤** 증거 복구 과정에서 발생한 변경이다. 측정 조건에는 영향이 없다.

- `goexchange-stress-allow-iap-ssh` 방화벽 규칙의 target tag에 `goexchange-server`를 추가했다
  (`goexchange-db,goexchange-server`). source `35.235.240.0/20`과 tcp:22는 그대로 두었고,
  기존 직접 SSH 규칙은 수정하지 않았다.
- 이유: 측정 후 VM을 정지·재기동하면서 외부 IP가 재할당됐고, 직접 SSH 규칙이 허용하던
  고정 IP와 접속 IP가 달라져 server VM에 접근할 수 없었다. DB VM은 이미 IAP 대상이라
  영향이 없었다. IAP 대역은 홈 IP 허용보다 좁다.

## 부록 B. 증거 회수 방식

§1(카탈로그)·§4(정합성·command 상태·`ORDER_RELEASE`)·§5(Prometheus)의 원본은
**측정 종료 후 VM을 재기동해 변경되지 않은 영구 디스크에서 조회**한 것이다.

- DB VM: 재기동 후 Postgres 컨테이너만 시작해 조회. 재기동 뒤 부하·backend 기동 없음
- server VM: **backend 컨테이너를 기동하지 않고**(Exited 유지) Prometheus만 시작해 조회 —
  새 샘플이 추가되지 않았다
- 두 VM 모두 회수 직후 정지하고 `TERMINATED`를 조회로 확인했다
- `cpu-db`는 측정 당시 파일이 영구 디스크에 남아 있어 그대로 회수했다. **새 CPU 측정으로
  대체하지 않았다**

각 원본 파일 헤더에 회수 시각과 조건을 기록했다.
