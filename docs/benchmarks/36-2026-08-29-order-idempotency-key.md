# 36번 — 주문 생성 멱등성 키(B-2) 500 VU 측정

## 1. 왜 측정했는가

B-2는 주문 생성 경로에 `Idempotency-Key`를 도입했다. 요청마다 **키 INSERT 1건**과
**outcome UPDATE 1건**이 늘고, `order_idempotency_keys`에 **UNIQUE 인덱스와 부분 인덱스**가
생긴다. 정합성은 통합 테스트가 고정했지만, 이 추가 쓰기가 **부하에서 무엇을 바꾸는지**는
측정하지 않으면 알 수 없다.

목적은 두 가지다.

1. 기존 하드 게이트(주문 가용성·정합성·중복 없음)가 멱등성 도입 후에도 유지되는지
2. 인덱스 크기와 WAL 증가량의 **기준선**을 남겨 보존 정책을 논할 근거를 만드는 것

## 2. 측정 조건

| 항목 | 값 |
|---|---|
| phase | `orderidem500r1` — **1회 실행, 폐기 0** |
| VU | 합산 **500** (load-gen당 250, `VU_LEVEL_SCALE=2`) |
| 구간 | ramp 30초 + hold **10분**, hold window `1787998700`–`1787999300` |
| 구성 | Server `e2-highcpu-4` · DB `e2-highcpu-8` · load-gen `e2-standard-8` ×2 (35번과 동일) |
| 런타임 | partitions **10** · concurrency **8** · goose **8** |
| k6 | v2.1.0+dirty, snap rev **56** (양쪽 `held`) |
| 측정 SHA (backend) | `21a80fec8cad5fdf0ea890ec3b733e45dde95833` |
| 측정 SHA (frontend) | `4438917e5a8d29e690fa1b796c2a54026e603ef5` |
| CI run | backend `33246042906` · frontend `33246063987` (둘 다 success) |
| image digest | `sha256:8f04072b397cc40a2afb161618565a8fab6bd8ae9988f808665ab02b71325e78` |
| iteration | A 207,554 / B 206,893, **interrupted 0** |

35번과 같은 topology·machine type·프로파일이다. 바뀐 것은 **측정 대상 코드**와
**부하 하니스가 주문마다 멱등성 키를 보낸다**는 점이다.

## 3. 사전 게이트 (부하 시작 전, 전부 통과)

| 게이트 | 결과 |
|---|---|
| k6 버전·snap rev·refresh hold | v2.1.0+dirty · rev 56 · `held` — load-gen A·B 모두 |
| settlement partitions·concurrency | **10 / 8** — compose config · 실제 container env · 기동 로그 **세 곳 모두 일치** |
| goose version | **8** |
| `order_idempotency_keys` UNIQUE | `UNIQUE (user_id, idempotency_key)` |
| pending 부분 인덱스 | `btree (updated_at) WHERE (outcome = 'PENDING'::text)`, `indisvalid`·`indisready`=t, `indnkeyatts`=1, `indnatts`=1, `indexprs` NULL |
| dev token | 현재 토큰 → **200**, 잘못된 토큰 → **403**. 값·해시·fingerprint 미출력 |
| workload 초기화 | orders·trades·ledger_entries·wallets·users·cancel_commands·**order_idempotency_keys**·trade_outbox_events·실패기록 4종 **전부 0** |
| bootstrap | `loaded=0 submitted=0 skipped=0 pending=0 partial=0` |
| startup barrier | 두 load-gen 동일 `LOAD_START_AT_MS`, setup 후 공통 시각까지 대기 |

**게이트를 두 번 되돌린 기록 — 숨기지 않는다.**

1. 첫 배포에서 `GOEXCHANGE_SETTLEMENT_CONCURRENCY`가 **4**로 잡혔다(잘못된 디렉터리의 `.env`를
   복사했다). 부하를 시작하지 않고 35번의 `.env`로 교체해 **8**로 맞춘 뒤 세 곳을 다시 확인했다.
2. load-gen의 dev token이 **stale**이어서 `/dev/wallets/fund`가 **403**이었다. 서버 컨테이너의
   값을 파일 relay로 옮긴 뒤 **200**을 확인하고 진행했다. 값·해시는 출력하지 않았다.

두 건 모두 **부하 시작 전에** 발견해 고쳤다. 폐기한 실행은 없다.

## 4. 하드 게이트 결과 — 전부 통과

### 4.1 k6 측 (load-gen A / B)

| 항목 | A | B |
|---|---|---|
| 주문 응답 가용성 (hold) | **100.00%** 202,204 / 0 fail | **100.00%** 201,514 / 0 fail |
| 주문 업무 성공 (hold) | **100.00%** 202,204 / 0 fail | **100.00%** 201,514 / 0 fail |
| 1초 계약 초과 | **0건** | **0건** |
| HTTP 실패 | **0** / 245,557 | **0** / 244,836 |
| k6 checks | 207,554 / **0 실패** | 206,893 / **0 실패** |
| 취소 성공률 | **100.00%** 14,514 | **100.00%** 14,467 |
| 멱등성 계약 위반(400·409) | **0** | **0** |
| 202 PENDING 응답 | **0** | **0** |

`custom_idempotency_contract_fail`과 `custom_order_pending_outcome`은 요약에 나타나지 않는다 —
k6는 0인 counter를 출력하지 않으며, 서버 측 상태 분포에서도 `POST /orders`는 **status 200만**
관측됐다(400·409·503·202 **0건**).

### 4.2 정합성

| 항목 | 값 |
|---|---|
| `failed_settlements` | **0** |
| `failed_market_completions` | **0** |
| `failed_order_cancellations` | **0** |
| `reconciliation_violations` | **0** |

### 4.3 멱등성 직접 증거 (DB 질의)

| 항목 | 값 |
|---|---|
| 주문 수 | **414,447** |
| 멱등성 키 수 | **414,447** |
| `ORDER_HOLD` 원장 건수 | **414,447** |
| k6 iteration 합계 (A+B) | **414,447** |
| outcome 분포 | `ACCEPTED` **414,447** (PENDING·REJECTED·UNKNOWN **0건**) |
| 키 1건이 여러 주문을 가리킴 | **0** |
| 주문 1건을 여러 키가 가리킴 | **0** |
| 주문 1건에 hold 2건 이상 | **0** |
| 키 없는 주문 | **0** |
| stale PENDING (임계 5분 초과) | **0** |

**iteration 수 = 주문 수 = 키 수 = hold 수가 정확히 일치한다.** 중복도 유실도 없다.
이것이 이 측정에서 가장 강한 증거다 — 비율이 아니라 **네 값의 완전 일치**다.

### 4.4 멱등성 관측 지표 (hold 구간, `increase(...[10m])`)

| 지표 | 값 |
|---|---|
| `order_idempotency_unknown_total` | **0** |
| `order_idempotency_outcome_update_failures_total` | **0** |
| `order_idempotency_stale_pending` (구간 최대) | **0** |
| `order_idempotency_monitor_errors_total` | **0** |

## 5. `POST /orders` 지연 — 새 기준선 (**회귀 판정 없음**)

35번은 `POST /orders` p95를 남기지 않았다. 그래서 이번 실행 **전에** 35번 hold window
(`1787319837`–`1787320437`)에서 **35번과 동일한 PromQL·집계 방식**으로 재구성했다.
같은 서버 VM의 같은 Prometheus TSDB를 썼고, 조회 전에 backend를 재배포하지 않았다.

| 지표 | 35번 (재구성) | 36번 |
|---|---|---|
| p50 | 17.4ms (0.017394351200771832) | **30.7ms** (0.030652493859239564) |
| **p95** | **33.2ms** (0.03323783651596988) | **48.9ms** (0.04893892227116787) |
| p99 | 61.5ms (0.06154959075589687) | **85.1ms** (0.08505049167927409) |
| 상태 무관 p95 | 33.2ms (200만 존재) | **48.9ms** (200만 존재) |
| 관측 수 | 추정 증가량 ≈409,535.8 | 추정 증가량 ≈403,723.4 |

PromQL:
```
histogram_quantile(0.95, sum by (le) (increase(
  http_request_duration_seconds_bucket{method="POST",path="/orders",status="200"}[10m])))
```

k6 측 `http_req_duration` p95는 A 44.65ms / B 44.64ms다(주문 외 요청 포함).

> **이 수치로 회귀를 판정하지 않는다.** 이 저장소에는 `POST /orders` p95에 대해
> **사전 등록된 정량 임계값이나 허용폭이 없다**. 게다가
> [ENGINEERING-SUMMARY](../ENGINEERING-SUMMARY.md) §4가 기록한 대로 **동일 구성에서도 p95의
> run-to-run 분산이 약 35%**다(29번 120.5ms vs 30번 162.7ms). 사후에 관측된 변동을 보고
> 허용폭을 만드는 것은 게이트가 아니라 사후 합리화다.
>
> 따라서 이번 p95는 **새 기준선**과 **35번 참고 비교**로만 기록하고,
> **회귀 여부는 판정하지 않는다.** 이것은 이번 측정의 완료 정의에 남는 **제한**이다.
> 판정하려면 (a) 임계값을 먼저 등록하고 (b) 같은 구성에서 반복 실행으로 분산을 재기 위한
> **추가 유료 실행**이 필요하다 — 이번 승인 범위 밖이다.

가용성 계약은 별개다. **1초 계약 초과가 A·B 모두 0건**이므로, 사전 등록된 게이트는 통과했다.

## 6. 비용 — 인덱스와 WAL (**귀속하지 않는다**)

hold 전후로 측정한 값이다.

| 항목 | before | after |
|---|---|---|
| `order_idempotency_keys` 테이블 | 0 bytes | **84 MB** (87,597,056) |
| `order_idempotency_keys_user_key_unique` | 8,192 bytes | **47 MB** (49,102,848) |
| `order_idempotency_pending_updated_at` | 8,192 bytes | **2,232 kB** (2,285,568) |
| `pg_current_wal_lsn()` | `8/D0292BF0` | `9/9A9AE5C0` |
| WAL 증가량 | — | **3,396,450,768 bytes ≈ 3.16 GiB** |

> **⚠ WAL 수치의 출처를 정확히 밝힌다.** 위 두 LSN은 부하 직전 preflight 실행과 부하 직후
> 첫 수집 실행에서 각각 캡처한 값이고, 원본은 `raw-db-preflight.txt`·`raw-db-after.txt`로
> 이 디렉터리에 함께 보존했다.
>
> **`orderidem500r1-db.tgz` 안의 `after.txt`는 `9/9A9FDB80`으로 위와 다르다.** packaging
> 스크립트가 질의를 **한 번 더** 돌렸기 때문이고, 그 사이(약 1분)에 idle·autovacuum·지표
> 스크레이프가 만든 **325,056 bytes**만큼 LSN이 더 나아간 **이후 시점 snapshot**이다.
> 두 값 모두 진짜지만 **같은 순간이 아니다** — tgz만으로는 위 증가량을 재계산할 수 없으므로
> 원본 두 파일을 별도로 넣었다. tgz는 재포장하지 않았고 5개 checksum은 그대로 유효하다.

414,447건 기준으로 테이블+두 인덱스 합계는 약 **133 MB**다(참고: `pg_total_relation_size`로는
145 MB).

> **⚠ 이 WAL 증가량은 "멱등성이 추가한 순증 WAL"이 아니다.** `pg_current_wal_lsn()` 전후 차이는
> 같은 구간에 돌아간 **시스템 전체 workload의 WAL**이다 — 주문·체결·정산·원장·outbox·취소가
> 모두 섞여 있다. 대조군(멱등성 없는 같은 부하)이나 격리 실험 없이 이 값을 멱등성에 귀속하면
> 안 된다. 비용 귀속이 필요하면 **별도 통제 실험**이 필요하다(§8 후속 항목).

> **⚠ 부분 인덱스 2,232 kB를 "정상 상태 크기"로 읽지 않는다.** 측정 종료 시점의 PENDING 행은
> **0건**이다. 그런데도 인덱스가 2 MB인 것은 414,447건이 `PENDING`으로 들어왔다가 `ACCEPTED`로
> 전이하면서 남긴 **죽은 엔트리가 아직 회수되지 않았기 때문**이다. 즉 이 값은 **관측 구간의
> churn 흔적**이지 정상 상태 크기가 아니다. `PENDING → 최종 상태` 전이는 414,447회 일어났다.
> 이 값을 exact churn 비용으로 단정하지 않는다 — autovacuum 타이밍이 섞여 있다.

## 7. 산출물

`_workspace/order-idempotency-key/gcp/orderidem500r1/`

| 파일 | sha256 |
|---|---|
| `orderidem500r1-loadgen-a.tgz` | `b1e70256833a638a176aff5fd5bd3bdbbaad37244c3e8da392453589119906f2` |
| `orderidem500r1-loadgen-b.tgz` | `876febb33fac6441f85af721d8886f57d6e1caf66af8be76cc27646157a0d925` |
| `orderidem500r1-server.tgz` | `bee77074d757fb90977fe3080c5e64920401b68317ba406fd7a963a262f671ff` |
| `orderidem500r1-prom.tgz` | `3f3ba9b1b1fbc7f88cbf60583ae1ad2e6be3f1ae3932d347874c14d3245509d3` |
| `orderidem500r1-db.tgz` | `409e2f620d66f06407e89d79ac9345809479a962cbce96544e14fc1920de513b` |

runbook §7.5 순서를 따랐다. 원본 summary는 VM에만 두고, `setup_data`(사용자별 JWT 250개 ×2)를
redaction metadata로 치환한 뒤 **metrics 불변**을 확인하고, JWT·`token`·Bearer·Authorization·
`GOEXCHANGE_JWT_SECRET` 패턴을 스캔해 **히트 0건**을 확인한 다음에만 tgz로 묶었다.
VM에서 계산한 checksum과 회수본 checksum이 **5개 모두 일치**한다.

## 8. 남긴 것

| 항목 | 왜 남겼는가 |
|---|---|
| **p95 회귀 판정** | 사전 등록된 정량 게이트가 없다. 임계값 등록 + 분산 재측정을 위한 **추가 유료 실행**이 필요하다 |
| **멱등성의 순증 WAL·인덱스 비용 귀속** | 대조군 없는 단일 실행이라 시스템 전체 WAL에서 분리할 수 없다. **별도 통제 실험** 필요 |
| **부분 인덱스 정상 상태 크기** | 이번 값은 회수 전 churn 흔적이다. vacuum 이후 안정 크기는 미측정 |
| **키 보존 정책** | 414,447건에 133 MB다. 보존 기간·정리 주기는 이 기준선 위에서 따로 정해야 한다 |
| **PENDING·REJECTED·UNKNOWN 경로의 부하 관측** | 이번 실행에서는 **0건** 발생했다. 그 경로들은 통합 테스트로만 고정돼 있고 부하에서는 미관측이다 |
