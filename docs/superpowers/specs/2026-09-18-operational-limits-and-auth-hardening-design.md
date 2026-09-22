# 운영 시간 상한과 인증 강화 설계

> 상태: **설계 확정**(2026-09-21, 6차 리뷰). 구현 미착수. 실행 계획: [2026-09-18-operational-limits-and-auth-hardening.md](../plans/2026-09-18-operational-limits-and-auth-hardening.md)
> 기준 SHA: `5e79abc` (main, 매칭 방출 용량 예약 병합 직후)
> 배경: 백엔드 평가에서 가용성 B·보안 C를 묶고 있는 항목 — HTTP·DB 시간 상한 부재, JWT 개발용 secret 폴백, rate limit 부재, `/metrics` 무인증 노출.

## 0. 지금 상태 (코드로 확인한 사실)

| 항목 | 현재 |
|---|---|
| HTTP 서버 | `cmd/main.go:423` `&http.Server{Addr: ":8080", Handler: r}` — read/header/write/idle 상한이 하나도 없다 |
| shutdown | HTTP 10초(main.go:435). **실패해도 그대로 다음 단계로 간다** — hold coordinator 종료·엔진 Stop이 in-flight `CreateOrder`와 겹칠 수 있다 |
| 부팅 DB 작업 | `main.go:55` AutoMigrate + `main.go:75` goose가 **서비스와 같은 풀**에서 돈다. 인덱스 생성·`CREATE INDEX CONCURRENTLY` 포함 |
| DB 설정 | `config/database.go:92-94` 커넥션 수·수명만. `statement_timeout`·`lock_timeout` 없음 |
| DSN | `GOEXCHANGE_DATABASE_DSN`이 있으면 그대로 쓴다(URL 형식도 가능) — 문자열 결합으로 파라미터를 붙이면 형식에 따라 깨진다(`database.go:102`) |
| 정산 재시도 | `settlement_transient.go:29` transient = `40001`·`40P01`·`55P03`. **`57014`는 없다** → statement_timeout이 걸리면 영구 실패로 내구 기록된다 |
| 주문 UNKNOWN | `order_handler.go:130` "같은 키로 재시도하면 같은 상태가 돌아온다. 먼저 상태를 확인하라"가 계약이다 |
| JWT | `internal/auth/token.go:60-66` 미설정이면 `dev-only-change-me` 폴백. `docker-compose.deploy.yml:36`도 같은 문자열로 폴백 |
| rate limit | 없음. 미들웨어는 `auth.go`·`dev_tools.go`뿐 |
| `/metrics` | `main.go:373` 무인증, 서비스 포트(`:8080`). Prometheus 타깃 `backend:8080` |
| pprof | `main.go:51` `:6060` 별도 리스너(상한 없음). runbook은 `ssh -L 6060`으로 **30초** CPU 프로파일을 받는다(`docs/gcp-stress-test-runbook.md:75,79`) |
| 프록시 신뢰 | `gin.Default()`는 모든 프록시를 신뢰 — `c.ClientIP()`가 `X-Forwarded-For`로 위조 가능 |
| WebSocket | `internal/ws/handler.go:105,161,170` 업그레이드 후 자체 deadline(읽기 60초, 쓰기 10초, ping 54초) |
| 부하 하니스 | setup이 한 IP에서 배치 단위로 가입을 동시 전송(`loadtest/order-submission-stress.js:75`). 주문은 VU별 사용자 + 200~500ms 간격이라 사용자당 2~5rps |

## 1. 목표와 비목표

**목표**

1. 모든 인바운드 HTTP 연결에 시간 상한이 있다.
2. **서버에서 실행을 시작한 개별 SQL 문장과 락 대기**에 상한이 있다(§1 비목표의 한계와 함께 읽을 것).
3. 운영 모드에서 JWT secret 없이 부팅되지 않는다.
4. 인증·주문·이체에 요청 상한이 있고, 초과는 `429` + `Retry-After`로 응답한다.
5. `/metrics`·pprof는 서비스 포트와 분리된 관리 포트에서만 제공된다.
6. 시간 상한이 켜진 뒤에도 정산·취소·이체·검산 worker의 재시도·멱등성 계약이 깨지지 않는다.

**비목표 — 이번에 보장하지 않는 것(문구를 과장하지 않는다)**

- **DB pool 획득 대기 상한과 트랜잭션 전체 시간 상한.** `database/sql`은 획득 타임아웃 옵션이 없고 `context`로만 끊을 수 있는데, 서비스·리포지토리가 `context`를 받지 않는다(예: `OrderService.CreateOrder(input)`). 전 계층 배선이 필요해 별도 사이클로 남긴다(§9).
  → 따라서 **"모든 DB 작업이 유한하다"거나 "HTTP shutdown이 10초 안에 끝난다"고 말하지 않는다.** 이 잔여 한계가 §5의 종료 분기를 결정한다.
- refresh token·세션 폐기·계정 잠금, 분산 rate limit, 관리 포트 자체 인증.
- terminal-outbox 원자성, SLO·알림, 구조화 로그(다음 사이클).

## 2. HTTP 시간 상한

### 2.1 서비스 서버

```go
srv := &http.Server{
	Addr:              ":8080",
	Handler:           r,
	ReadHeaderTimeout: 5 * time.Second,   // GOEXCHANGE_HTTP_READ_HEADER_TIMEOUT
	ReadTimeout:       15 * time.Second,  // GOEXCHANGE_HTTP_READ_TIMEOUT
	WriteTimeout:      20 * time.Second,  // GOEXCHANGE_HTTP_WRITE_TIMEOUT
	IdleTimeout:       60 * time.Second,  // GOEXCHANGE_HTTP_IDLE_TIMEOUT
	MaxHeaderBytes:    1 << 20,           // GOEXCHANGE_HTTP_MAX_HEADER_BYTES
}
```

값은 운영 시작점이며 정확성 경계가 아니다. `WriteTimeout`은 요청 읽기 시작부터 응답 쓰기까지를 덮는다.

**WebSocket은 영향받지 않는다.** `/ws`는 업그레이드 직후 hijack된 연결에 자체 deadline을 매 읽기·쓰기마다 설정한다. 서버 전역 값은 업그레이드 시점까지만 적용된다. §8 T11이 이것을 고정한다.

### 2.2 관리 서버 (별도 상한)

pprof CPU 프로파일 기본 길이는 30초이고 runbook도 `-seconds=30`을 쓴다. 서비스와 같은 20초 `WriteTimeout`을 쓰면 **기존 프로파일 절차가 깨진다.**

```go
adminSrv := &http.Server{
	Addr:              adminAddr,          // GOEXCHANGE_ADMIN_ADDR
	Handler:           adminMux,
	ReadHeaderTimeout: 5 * time.Second,
	WriteTimeout:      120 * time.Second,  // GOEXCHANGE_ADMIN_WRITE_TIMEOUT
	IdleTimeout:       60 * time.Second,
	MaxHeaderBytes:    1 << 20,
}
```

- 허용 최대 프로파일 길이 = `WriteTimeout`보다 짧아야 한다. 기본 120초 → `-seconds=30`은 넉넉하다. 이 관계를 runbook에 적는다.
- **bind 실패는 부팅 실패**(`log.Fatal`)다. 관리 포트가 조용히 없는 상태로 뜨면 지표·프로파일이 사라진다.
- 서비스 서버와 **함께 graceful shutdown** 대상이다.

## 3. DB 시간 상한 — 세 프로필

하나의 값으로는 안 된다. 용도별로 세 풀을 만든다.

| 풀 | 용도 | statement_timeout | lock_timeout | idle_in_transaction_session_timeout | 커넥션 |
|---|---|---|---|---|---|
| **마이그레이션** | 부팅 AutoMigrate + goose | 10m (`GOEXCHANGE_MIGRATION_STATEMENT_TIMEOUT`) | 10s | 0(비활성) | 2, 마이그레이션 후 close |
| **서비스** | HTTP·엔진·정산·outbox·worker | 15s | 3s | 30s | 기존 `GOEXCHANGE_DB_MAX_OPEN_CONNS`(기본 25) |
| **검산** | ReconciliationWorker 전용 | 5m (`GOEXCHANGE_RECONCILIATION_STATEMENT_TIMEOUT`) | 3s | 0(비활성) | 최대 2 |

- **서비스 풀의 세 값은 env로 연다**(2차 전면 개정 때 실수로 빠졌다 — CP A 리뷰에서 복구): `GOEXCHANGE_DB_STATEMENT_TIMEOUT`(기본 15s), `GOEXCHANGE_DB_LOCK_TIMEOUT`(3s), `GOEXCHANGE_DB_IDLE_TX_TIMEOUT`(30s). §3.3대로 `lock_timeout`은 정상 부하에서도 발동할 수 있으므로 **재빌드 없이 조정 가능해야 한다.** 파싱은 strict(잘못된 값이면 부팅 실패).
- **검산 repository는 이 풀만 쓴다.** `ReconciliationWorker.Repository`를 검산 풀로 만든 리포지토리로 주입한다. 워커 종료 시 풀을 close한다.
- DB 총 연결 예산 = 서비스 25 + 검산 2 + (부팅 한정 2). 마이그레이션 풀은 서비스 시작 전에 닫힌다.
- `SET LOCAL`은 쓰지 않는다. 검산을 하나의 긴 트랜잭션으로 묶으면 오래된 snapshot과 락 유지 문제가 생긴다.

### 3.1 적용 방식 — 문자열 결합이 아니라 런타임 파라미터

`GOEXCHANGE_DATABASE_DSN`은 URL 형식일 수도, keyword 형식일 수도 있다. `options=-c ...`를 문자열로 붙이면 형식에 따라 깨진다.

```go
cfg, err := pgx.ParseConfig(dsn)            // URL·keyword 양쪽 처리
cfg.RuntimeParams["statement_timeout"] = "15000"
cfg.RuntimeParams["lock_timeout"] = "3000"
cfg.RuntimeParams["idle_in_transaction_session_timeout"] = "30000"
sqlDB := stdlib.OpenDB(*cfg)
gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{})
```

세 프로필 모두 같은 함수로 만든다(파라미터만 다름). 주의할 점:

- **DB 통계 collector 중복 등록.** 지금 `ConnectDB`는 `collectors.NewDBStatsCollector`를 한 번 등록한다(`database.go:95`). 공통 헬퍼가 이를 그대로 호출하면 두 번째 풀에서 panic한다. **마이그레이션 풀은 등록하지 않고**(곧 닫힌다), 서비스·검산 풀만 서로 다른 이름 라벨로 등록한다.
- GORM open 또는 `SHOW` 검증이 실패하면 이미 연 `*sql.DB`를 close하고 에러를 올린다.

### 3.2 부팅 검증

서비스 풀 연결 직후 `SHOW statement_timeout`, `SHOW lock_timeout`, `SHOW idle_in_transaction_session_timeout` **세 개 모두** 조회해 기대값과 다르면 `log.Fatal`한다. 검산·마이그레이션 풀도 각자 자기 값으로 같은 검증을 한다.

### 3.3 값의 성격 (과장하지 않는다)

- `statement_timeout=15s`: 정상 최장 문장(outbox 배치 INSERT 512행, 정산 트랜잭션)의 로컬 p99가 수십 ms다. 여유가 크다.
- `lock_timeout=3s`: **"정상이면 절대 안 걸리는 값"이 아니다.** 핫 계정 경합에서 요청을 의도적으로 잘라내는 부하 차단값이다. 500 VU에서 계정 분산과 정산 경합에 따라 정상적으로 발생할 수 있다.
- 그래서 **배포 전 게이트**: 대표 부하에서 `55P03`·`57014` 발생 건수와 주문 성공률을 측정해 기록한다(§8 게이트).

## 4. SQLSTATE 경로별 계약

`55P03`(lock_timeout)과 `57014`(statement_timeout)는 **둘 다 트랜잭션을 abort시킨다** — 부분 적용이 남지 않는다. 하지만 경로마다 후속 처리가 다르다.

| 경로 | 55P03 | 57014 | 근거 |
|---|---|---|---|
| 정산(`SettleTrade`·배치), FailedSettlement 재시도 | transient (현재도 그렇다) | **transient로 추가한다** — `settlement_transient.go`의 목록에 `57014`를 넣는다 | 트랜잭션이 통째로 롤백됐다. 영구 실패로 내구 기록하면 사람이 손봐야 하는 항목이 부하 때마다 쌓인다 |
| hold coordinator(주문 잠금 배치) | 배치 실패 → **즉시 단건 폴백**(백오프 재시도가 아니다, `hold_coordinator.go:463`) | §4.3의 예산 정책을 적용 | 배치가 롤백되므로 단건 재처리가 안전하다. 다만 시간 증폭을 막아야 한다 |
| outbox writer | 무한 재시도(현행) | 무한 재시도(현행) | 이미 모든 오류를 재시도한다. 변경 없음 |
| cancel command worker | `scheduleRetry`(현행 default 분기) | 같음 | 취소는 포기하지 않는다 |
| transfer poller | **상태가 그대로 남아 다음 poll 주기에 재시도**(로그만) | 같음 | `HandleStatusCheckFailure`는 외부 `GetTransferStatus` 실패 전용 경로다(`transfer_status_poller.go:69-79`). DB timeout은 위치별로: `DueForCheck` 실패 → 다음 주기 재scan / `Dispatch` 실패 → RECEIVED 유지 / `ResolveTransfer`·`RecordObservation` 실패 → 롤백 후 PROCESSING 유지 / `HandleStatusCheckFailure` 자체 실패 → 일정 미전진, 다음 주기 재시도 |
| 검산 worker | 검사 실패 카운터 + 로그(현행) | 같음 | 5분 프로필이라 정상 경로에서 도달하기 어렵다 |
| HTTP 동기 처리(주문 외) | 503 `SERVICE_UNAVAILABLE` | 503 `SERVICE_UNAVAILABLE` | 요청 단위 재시도가 안전하다 |
| **HTTP 주문 생성** | 아래 §4.1 | 아래 §4.1 | 멱등키 상태에 따라 다르다 |

### 4.1 주문 생성에서의 timeout

기존 멱등키 계약(`order_handler.go:96-131`)을 **바꾸지 않는다.** timeout은 다음 둘 중 하나로 귀결된다.

1. **멱등키 레코드가 남지 않았다**(키 등록 트랜잭션 자체가 abort). 서버에 아무 상태가 없으므로 503 `SERVICE_UNAVAILABLE`로 응답한다. 클라이언트는 **같은 키로 재요청**하면 된다 — 프런트 OrderForm이 이미 5xx에서 키를 유지한다.
2. **레코드가 남았다**(PENDING/UNKNOWN 등 durable outcome 존재). 기존 분기 그대로 202 PENDING 또는 503 UNKNOWN을 반환한다. 이때 안내 문구는 지금과 같다 — "같은 키로 재시도하면 같은 상태가 돌아온다. 상태를 먼저 확인하라". **"재시도하면 해결된다"고 쓰지 않는다.**

즉 설계가 새로 정하는 것은 "timeout 오류를 어떤 HTTP 상태로 바꾸는가"뿐이고, 멱등성 의미는 기존 규약을 그대로 따른다.

### 4.2 관측

`55P03`·`57014`를 경로 라벨과 함께 세는 카운터를 추가한다(`goexchange_db_timeout_total{sqlstate,path}`). 단위는 **시도(attempt)마다 1** — 한 논리 작업이 4번 timeout이면 4건이다(DB에 실제로 가해진 부하를 나타낸다). §3.3 게이트와 이후 SLO 작업이 이 값을 쓴다.

**계측 경로 행렬**(빠지면 부하 측정에서 정책 발동이 0건으로 보인다):

| path 라벨 | 위치 | 필수 |
|---|---|---|
| `settlement` | 단건 정산 재시도 | ✅ |
| `market_completion` | 시장가 완료 재시도 | ✅ |
| `cancellation` | 취소 terminal 재시도 | ✅ |
| `settlement_batch` | 정산 **배치** 실패(§4.3 인계 직전) | ✅ — 500 VU에서 정책이 실제로 발동하는 주요 지점 |
| `hold_batch` | hold **배치** 실패(§4.3 503 직전) | ✅ — 같은 이유 |
| `retry_worker` | 장기 재시도(§4.4 판정 지점, phase 공통) | ✅ |
| 그 밖(poller·검산 등) | 로그만 | 선택 |

**모든 경로가 `55P03`과 `57014`를 함께 센다.** `57014`만 세면, 배치가 `55P03`으로 실패한 뒤 단건 폴백이 성공했을 때 그 lock timeout이 0건으로 보인다 — §3.3의 "배포 전 `55P03` 발생 건수 측정"이 어긋난다. `40P01`·`40001` 등 다른 transient는 시간 상한이 아니므로 세지 않는다.

**`IsTransientSettlementError`에 `57014`를 넣으면 함께 바뀌는 경로**(전수): 단건 정산 재시도(`main.go:943`), 시장가 주문 완료(`isRetryableCompletionError`, `main.go:917`), 취소 terminal 처리(`main.go:867`), 실패 기록 저장 재시도(`retryTransient`, `main.go:554`). 의도한 효과이며 §8 테스트 대응표에 넣는다.

### 4.3 배치 실패 후 단건 폴백의 시간 증폭

hold는 배치 실패 시 즉시 단건 폴백하고(`hold_coordinator.go:463`), 정산도 배치 실패 시 최대 배치 크기만큼 단건 경로로 재처리한다(`main.go:729-743`). 15초 `statement_timeout`이 **공통 DB 과부하** 때문에 발생하면 단건 폴백도 줄줄이 같은 상한에 걸려, coordinator·정산 worker가 한 배치에 수 분간 묶인다.

`55P03`은 핫 계정 하나만 분리하면 단건이 성공할 수 있어 폴백이 유효하다. `57014`는 과부하·비싼 쿼리일 가능성이 커 같은 정책을 쓸 수 없다. **정하는 규칙:**

| 경로 | 배치 오류가 `57014`일 때 |
|---|---|
| hold coordinator | 단건 폴백을 하지 않는다. 배치의 모든 요청에 503(`SERVICE_UNAVAILABLE`)을 돌려준다 — 주문은 아직 접수되지 않았고 클라이언트가 같은 멱등키로 재시도할 수 있다 |
| 정산 배치 | 단건 폴백을 하지 않는다. **배치의 trade 전부를 한 트랜잭션으로 `STATEMENT_TIMEOUT` failure에 인계**한다(아래 원자적 인계). 탐침을 두지 않는다 |

`55P03`과 그 밖의 오류는 기존 폴백 동작을 그대로 둔다. 새 환경변수는 만들지 않는다.

**탐침을 두지 않는 이유.** 탐침에 `processSingleOutboxEvent`를 그대로 쓰면 내부 즉시 재시도와 실패 기록까지 돌아 호출자가 최종 SQLSTATE를 알 수 없고, 2건만 해도 최악 `2 × 즉시 재시도 횟수 × 15초`다. 배치 크기 때문에 걸린 것이라면 retry worker의 단건 재처리가 곧 성공하고, DB 전체가 느린 것이라면 §4.4의 중단 규칙이 첫 timeout에서 멈춘다.

**내구 소유권은 하나뿐이다.** 현재 규약은 "실패 기록이 성공하면 처리가 내구적으로 인계됐다 → outbox를 PROCESSED"다(`main.go:607-615`의 `handled`). 실패 기록과 outbox PENDING을 함께 남기면 런타임 retry worker와 다음 부팅 replay가 같은 trade를 이중 소유한다.

**인계 자체도 한 번에 한다(원자적 배치 인계).** 단건 `RecordFailure`·`MarkProcessed`를 32번 반복하면 정산 증폭을 없앤 자리에 **저장 단계의 증폭**(32 × 15초)이 그대로 생긴다. 그래서 새 리포지토리 메서드 하나로 묶는다 — `InsertBatchAndMarkCancelCommands`(배치 INSERT + 배치 UPDATE를 한 커밋에 묶는 기존 선례, `trade_outbox_repository.go:32`)와 같은 모양이다.

```
RecordFailuresAndMarkOutboxProcessed(items []SettlementFailureHandoff) error
  // SettlementFailureHandoff{ Failure *model.FailedSettlement; OutboxID uint64 }
  = 한 트랜잭션: failed_settlements 배치 upsert + 그 outbox 행 배치 PROCESSED
```

**입력·행 수 계약**(조용한 부분 성공을 막는다):

- failure와 outbox ID는 **일대일**이다. 병렬 슬라이스 두 개가 아니라 한 항목 구조체로 받아 대응 오류 자체를 없앤다.
- 거부: 빈 입력, `nil` failure, `OutboxID == 0`, 중복 `OutboxID`, 중복 `trade_idempotency_key`. **`InsertBatchAndMarkCancelCommands`의 중복 제거를 복사하지 않는다** — 취소 command는 중복이 정상이지만 여기서 중복은 입력 불변식 위반이다.
- failure upsert와 outbox update 모두 `RowsAffected == 항목 수`를 검사한다.
- outbox update는 `id IN (...) AND status = 'PENDING'`으로 제한한다.
- 하나라도 행 수가 어긋나거나 SQL이 실패하면 **트랜잭션 전체 rollback**.
- conflict 시 갱신 범위는 기존 `RecordFailure`와 같다(오류 메시지·OPEN 상태·retry count·해결 정보 초기화).
- 파라미터 수는 문제가 없다 — 이 경로의 실질 상한은 정산 배치 32건(`main.go:564`)이고, failure 필드를 모두 바인딩해도 1000개 미만이다. 다른 곳에서 재사용하게 되면 최대 항목 수를 따로 고정한다.

결과는 둘뿐이다.

| 결과 | FailedSettlement | Outbox | dispatcher에 돌려줄 undurable 주문 ID |
|---|---|---|---|
| 인계 commit | 전부 OPEN / `STATEMENT_TIMEOUT` | 전부 **PROCESSED** | 없음 |
| 인계 rollback | 없음 | 전부 PENDING(다음 부팅 replay가 소유) | **배치 전체의 maker·taker 주문 ID** |

- "기록은 됐는데 마킹만 실패"라는 중간 상태를 이 경로에서는 만들지 않는다(원자적이므로). 기존 단건 경로(`processExecutionEvent`)의 그 상태는 그대로 둔다 — 그쪽은 멱등 재처리로 이미 설계돼 있다.
- rollback이면 배치 전체를 `undurableOrderIDs`로 반환해 dispatcher가 그 주문들의 terminal을 quarantine하게 한다(기존 `settleTradeBatchWithFallback` 반환 규약, `main.go:735-742`).

### 4.4 내구(장기) 재시도 분류까지 연결한다

즉시 재시도가 소진되면 `failed_settlements`에 기록되고, **장기 재시도는 별도 분류표를 본다.** 지금 분류는 `DEADLOCK`·`SERIALIZATION_FAILURE`·`LOCK_TIMEOUT`뿐이라(`failed_settlement_service.go:22-41`), `[SQLSTATE 57014]` 태그가 붙어도 `UNKNOWN`으로 분류돼 **retry worker가 건너뛴다.**

그래서 함께 추가한다:

- `FailedSettlementCategoryStatementTimeout = "STATEMENT_TIMEOUT"`
- `ClassifyFailedSettlement`가 `[SQLSTATE 57014]` 태그를 이 카테고리로 매핑(태그는 `settlementErrorMessage`가 이미 붙인다, `failed_settlement_service.go:198`)
- `IsTransientFailedSettlementCategory`에 포함

즉시 재시도가 전부 실패하면 **내구 기록이 생기는 것이 정상이다**(그래야 이벤트가 유실되지 않는다). 판별해야 할 것은 "기록이 안 생긴다"가 아니라 "transient 분류로 기록되고 retry worker가 다시 집어 해결한다"이다.

**retry worker의 중단 규칙(circuit break).** 지금 worker는 한 failure가 실패해도 `continue`로 다음 failure를 계속 본다(`settlement_retry_worker.go:102-128`). 배치 32건을 한꺼번에 넘기면 최악 `32 × 15초`를 한 RunOnce에서 순차로 쓴다. 그래서:

> **원래 failure 카테고리와 무관하게**, 이번 재시도가 돌려준 오류가 `57014`이면 그 failure의 갱신을 시도한 뒤 **`RunOnce` 전체를 중단**한다. 다음 tick에서 이어간다.

**이것은 `RunOnce` 최상위 계약이다.** 지금 `RunOnce`는 반환값 없는 세 함수를 차례로 부른다(`settlement_retry_worker.go:85-89`). settlement 루프 안에서 `return`해도 completion·cancellation 단계는 그대로 돌아 같은 증폭이 이어진다. 그래서:

- 세 phase(`retryFailedSettlements`·`retryFailedCompletions`·`retryFailedCancellations`)가 **중단 여부를 반환**한다(`stopRun bool` 또는 동등한 값).
- 어느 phase에서든 이번 실제 실행 오류가 `57014`이면 갱신을 시도한 뒤 `stopRun=true`를 최상위까지 전달한다.
- `RunOnce`는 `stopRun`을 받으면 **남은 항목과 남은 phase를 실행하지 않는다.**
  - settlement에서 발생 → completion·cancellation을 아예 시작하지 않는다.
  - completion에서 발생 → 남은 completion과 cancellation을 건너뛴다.
  - cancellation에서 발생 → 남은 cancellation을 건너뛴다.
- 판정 기준은 "저장된 카테고리"가 아니라 **이번 시도의 반환 오류**다. 원래 `DEADLOCK`·`LOCK_TIMEOUT`이던 failure도 과부하에서는 이번 시도가 `57014`로 끝날 수 있다.
- **갱신(`RecordFailure`) 자체가 실패해도** 로그만 남기고 중단한다 — 기록 DB가 timeout인 상황에서 다음 항목을 계속 두드리면 안 된다.
- `57014`가 아닌 오류(`DEADLOCK` 등)는 기존대로 다음 항목으로 진행한다.

이것은 구현 방식이 아니라 **과부하 시 무엇을 계속할지 정하는 운영 계약**이므로 여기서 확정한다(계획서로 미루지 않는다).

## 5. 종료(shutdown) 분기

현재는 `srv.Shutdown`이 실패해도 hold coordinator 종료·엔진 Stop으로 진행한다. pool 획득 대기가 무한일 수 있는 이상(§1 비목표) in-flight `CreateOrder`가 남은 채 엔진을 닫는 경쟁이 성립한다.

**정하는 계약 — 두 서버를 분리한다.** 파이프라인 종료의 안전 조건은 **서비스 서버**가 drain됐는지뿐이다. 관리 서버(pprof·metrics)는 주문 경로와 무관하다.

| 결과 | 처리 |
|---|---|
| 서비스 Shutdown 성공 | 기존 순서(hold coordinator → cancel worker → 엔진 → outbox → 정산)로 drain |
| 서비스 Shutdown 실패(timeout) | **파이프라인을 닫지 않는다.** 사유를 로그로 남기고 비정상 종료 |
| 관리 Shutdown 실패 | 관리 서버만 `Close()`하고, 서비스가 성공했으면 파이프라인 drain을 **계속한다** |

- **두 서버가 timeout 예산을 공유하지 않는다.** 각자 자기 `context.WithTimeout`을 쓴다. 30초 CPU 프로파일이 진행 중이라고 해서 서비스 drain 예산을 뺏기면 안 된다.
- **비정상 종료 방식:** `os.Exit(1)`을 직접 부르지 않고 **주입 가능한 exit 함수**(`exitFunc func(int)`, 기본 `os.Exit`)를 통해 호출한다. 그래야 테스트가 "hold coordinator 종료가 호출되지 않았다 / 엔진 Stop이 호출되지 않았다 / 종료 코드 1"을 단언할 수 있다. `defer`는 실행되지 않는다 — 그것이 의도다(닫지 않고 죽는다). **테스트의 주입 exit 함수는 반환할 수 있으므로, 호출 직후 명시적으로 `return`해 drain 경로로 흘러가지 않게 한다.**
- 이유: 닫는 것보다 **안 닫고 죽는 편이 안전하다.** 채널 close와 in-flight send가 겹치면 panic이거나 유실이고, 죽으면 부팅 replay(main.go:181) → bootstrap(main.go:312) → cancel worker 장벽이 복구한다. 커밋 전 트랜잭션은 연결 종료로 롤백되고, outbox에 없는 인메모리 체결은 DB 주문이 open이라 bootstrap 후 다시 매칭된다.

## 6. 인증 강화

### 6.1 JWT secret 필수화

`NewTokenManagerFromEnv`의 폴백을 제거한다. 미설정·공백이면 `ErrInvalidJWTSecret` → `main`이 `log.Fatal`. "개발 모드에서만 폴백" 분기는 두지 않는다.

**함께 고쳐야 하는 것**(리뷰가 지적한 전수):

- `docker-compose.deploy.yml:36`의 `${GOEXCHANGE_JWT_SECRET:-change-me-to-a-long-random-secret}` → `:?GOEXCHANGE_JWT_SECRET is required`.
- `TESTING.md:75`의 "기본값 `dev-only-change-me`" → 필수 표기, `go run ./cmd` 개발 절차에 secret 설정 추가.
- 바이너리를 실제 기동하는 CI·E2E가 있으면 secret을 명시(전수 확인 후 보고).
- 테스트는 `t.Setenv`로 설정. 영향 범위를 전수 확인한다.
- secret 길이 하한은 요구하지 않는다(기존 배포 값을 깨뜨린다). 목표는 "알려진 기본값으로 뜨지 않는다"이다.

### 6.2 rate limit

**미들웨어 배치**

- 인증 limiter: `POST /auth/login`, `POST /auth/register`에 **핸들러 앞**, 키는 클라이언트 IP. 두 경로는 **각각 별도 버킷**을 쓴다(로그인 실패 폭주가 가입을 막지 않게).
- 사용자 limiter: `AuthRequired` **뒤**에 배치(사용자 ID가 있어야 키가 나온다). 대상은 `POST /orders`, `DELETE /orders/:id`, `POST /transfers/*`.

**기본값**(전부 env, 부하 프로필에서 조정)

| 대상 | 키 | rps | burst |
|---|---|---|---|
| login / register (각각) | IP | 1 | 10 |
| 주문 생성·취소 | 사용자 ID | 20 | 40 |
| 이체 요청 | 사용자 ID | 2 | 5 |

**동시성 계약**

- 저장소는 `sync.Map`이 아니라 **mutex로 보호하는 map**이다. `sync.Map` + last-use로는 정리와 요청이 겹칠 때 같은 키의 버킷이 둘 생겨 상한을 우회할 수 있다.
- **락을 두 층으로 나누고 획득 순서를 고정한다.** map 자체(조회·삽입·삭제)는 저장소 mutex, 한 버킷의 토큰 연산(`Reserve`·`Cancel`)과 마지막 사용 시각 갱신은 그 entry의 mutex로 보호한다. 순서를 정하지 않으면 "요청이 entry 포인터를 얻고 map lock을 놓은 사이 cleanup이 그 entry를 지우고, 다음 요청이 같은 키로 새 entry를 만들어" 한 키에 limiter가 둘 생긴다.
  - 요청: map lock → entry 조회·생성 → **entry lock** → map unlock → 토큰 연산·`lastUsed` 갱신 → entry unlock
  - cleanup: map lock → entry lock → `lastUsed` 재확인 → 필요하면 delete → entry unlock → map unlock
  - 모든 키의 토큰 연산을 map mutex 하나로 묶지 않는다(서로 다른 사용자가 한 락에서 직렬화된다).
- `Retry-After`는 `limiter.Reserve()`로 계산한 뒤 **`Cancel()`로 토큰을 되돌린다** — 거절하면서 토큰을 추가 소비하지 않는다. 값은 지연 초를 올림한 정수, 최소 1.
- 정리 goroutine: 주기 10분, 마지막 사용 후 10분 이상 유휴인 버킷만 삭제하며 같은 mutex 아래서 한다. **종료 시 중단**된다(main의 backgroundCtx 사용).
- 잘못된 설정(rps ≤ 0, burst < 1, 파싱 불가, 잘못된 CIDR)은 **부팅 실패**.
- 초과 응답: `429`, 새 코드 `CodeRateLimited = "RATE_LIMITED"`, `Retry-After` 헤더.
- 인스턴스 로컬이라는 한계를 문서에 적는다(분산은 MSA 단계 문제).

### 6.3 프록시 신뢰

- `r.SetTrustedProxies(nil)`이 기본 — `RemoteAddr`만 쓴다.
- `GOEXCHANGE_TRUSTED_PROXIES`(콤마 구분 CIDR)가 설정된 경우에만 `X-Forwarded-For`를 신뢰한다. 잘못된 CIDR은 부팅 실패.

### 6.4 부하 프로필과의 충돌

실제 충돌 지점은 주문이 아니라 **인증**이다. stress 하니스 setup은 한 IP에서 가입을 배치로 동시 전송하므로 기본 burst 10이면 첫 배치부터 실패한다. 주문은 사용자당 2~5rps라 기본 20rps와 충돌하지 않는다.

- limiter를 **끄지 않는다**(끄면 운영 미들웨어 경로를 우회한 다른 시스템을 측정한다).
- **예시값이 아니라 결정적 계약으로 정한다.** `burst ≥ 배치 크기`만으로는 부족하다 — 배치가 연달아 빠르게 나가면 세 번째 배치부터 걸린다. 다음을 모두 만족시킨다:
  - `docker-compose.stress.yml`에 인증 `rps`·`burst`를 **setup의 실제 전송률보다 높게** 설정한다. 계획 단계에서 하니스의 `TOTAL_USERS`·`SETUP_BATCH_SIZE`와 배치 간격으로 필요한 rps를 계산해 값을 정한다(현재 기본 하니스 기준 계산을 계획서에 적는다).
  - **preflight**: 부하 본 실행 전에 setup만 돌려 `429`가 **한 건이라도 나오면 중단**한다. k6 setup 단계에서 상태 코드를 검사해 즉시 실패시킨다.
  - 실제 적용값과 preflight 결과를 runbook에 기록한다.

## 7. 관리 포트와 접근 경계

- `/metrics`를 서비스 포트에서 제거하고 관리 서버로 옮긴다. pprof도 같은 서버에 등록한다(`GOEXCHANGE_ENABLE_PPROF`가 true일 때만). `:6060` 전용 리스너는 없앤다.
- 기본 `GOEXCHANGE_ADMIN_ADDR=127.0.0.1:9101`. 컨테이너에서는 `0.0.0.0:9101`로 설정한다(Docker 포트 포워딩은 컨테이너 eth0로 들어오므로 루프백 바인드면 접근 불가 — `main.go:45` 주석의 기존 근거와 같다).
- compose:
  - `docker-compose.stress.yml`: `127.0.0.1:9101:9101`만 published(기존 `127.0.0.1:6060:6060` 대체). **SSH 터널 기반 pprof 절차를 유지한다.**
  - `docker-compose.prod.yml`·`deploy.yml`: published 하지 않는다.
- `monitoring/prometheus.yml` 타깃 `backend:8080` → `backend:9101`.
- `docs/gcp-stress-test-runbook.md`: `ssh -L 6060` → `ssh -L 9101`, 프로파일 URL `http://localhost:9101/debug/pprof/profile`, `:8080/metrics`를 직접 curl하는 절차가 있으면 `:9101`로. 과거 benchmark 기록 문서는 고치지 않는다.
- Grafana 대시보드는 Prometheus datasource를 쓰므로 쿼리는 그대로다.

## 8. 테스트 계획

시간 상한 테스트는 **테스트 전용 짧은 값**을 쓴다. 기본값(15s·3s·60s)을 실시간으로 기다리지 않는다.

**설정 단위**

- C1 각 env의 기본값·파싱. 잘못된 값(0, 음수, 공백, 비정수, 잘못된 CIDR)은 에러.
- C2 세 DB 프로필이 각자 기대 RuntimeParams를 만든다(URL DSN·keyword DSN 양쪽에서).

**인증·rate limit 단위**

- A1 `NewTokenManagerFromEnv`: 미설정·공백 → `ErrInvalidJWTSecret`, 값 있으면 성공.
- A2 rate limit: 버스트까지 통과 → 초과 시 429 + `Retry-After` ≥ 1 + `RATE_LIMITED`. 키가 다르면 서로 영향 없음. 거절이 토큰을 추가 소비하지 않음(거절 후 대기하면 예상 시점에 통과).
- A3 정리와 요청 경쟁: **결정적으로** 검증한다. 요청이 entry를 얻은 시점과 cleanup의 삭제 시도 시점을 테스트 훅(채널 장벽)으로 맞물리게 한 뒤, 같은 키에 limiter가 둘 생기지 않고 총 허용 수가 상한을 넘지 않음을 단언한다. `-race` 반복은 보조로만 쓴다.
- A4 login과 register가 서로 다른 버킷.
- A5 프록시 신뢰: `X-Forwarded-For`를 보내도 신뢰 목록이 비면 `RemoteAddr` 기준. 신뢰 CIDR 설정 시에만 헤더 반영.
- A6 limiter 비활성(`ENABLED=false`)이면 통과.

**HTTP 수준**(실제 loopback 리스너, 테스트 전용 짧은 상한)

- H1 slowloris: 헤더를 천천히 보내면 `ReadHeaderTimeout` 근처(넓은 상한)에서 연결이 닫힌다.
- H2 느린 body: `ReadTimeout` 초과 시 끊긴다.
- H3 느린 핸들러: `WriteTimeout` 초과 시 응답이 끊긴다.
- H4 idle: raw TCP로 응답을 끝까지 읽고 idle을 넘긴 뒤 다음 read에서 EOF(`http.Client`는 재연결을 숨기므로 쓰지 않는다).
- H5 `MaxHeaderBytes` 초과 → 431/닫힘.
- H6 WebSocket: 테스트 서버 `WriteTimeout`을 50~100ms로 낮추고, 업그레이드된 연결이 그보다 오래 살아 메시지를 주고받는다. ping/pong 주기 자체는 기존 ws 테스트에 맡긴다.
- H7 관리 포트: 서비스 `/metrics` 404, 관리 `/metrics` 200. pprof는 env off면 관리 포트에서도 404, on이면 200.
- H8 관리 서버 graceful shutdown, bind 실패 시 부팅 실패.
- H9 종료 분기(§5): 주입한 exit 함수로 확인한다. 서비스 Shutdown 실패 → hold coordinator 종료·엔진 Stop이 **호출되지 않고** 종료 코드 1. 관리 Shutdown 실패 + 서비스 성공 → 파이프라인 drain이 정상 수행된다.

**DB 통합**(실제 PostgreSQL, 50~200ms 전용 프로필)

- D1 `statement_timeout`: `pg_sleep`이 `57014`로 끝나고, 그 커넥션이 풀로 돌아가 다음 쿼리가 정상 동작한다.
- D2 `lock_timeout`: 잠긴 행에 대한 `FOR UPDATE`가 `55P03`으로 끝난다.
- D3 부팅 검증: 세 파라미터 중 하나라도 기대와 다르면 실패.
- D4 검산 풀: 검산 커넥션의 `statement_timeout`이 서비스 풀과 다르다(`SHOW`).
- D5 마이그레이션 풀: 프로필이 적용되고 마이그레이션 후 close된다.
- D6 GORM/pgx로 감싸진 `55P03`·`57014`가 §4 표대로 분류된다 — 특히 `IsTransientSettlementError(57014) == true`.
- D7 주문 경로 timeout: 멱등키 레코드가 없을 때 503, 있을 때 기존 outcome 응답. 응답과 DB 상태가 일치한다.
- D8 정산 즉시 재시도: `57014`가 transient로 처리돼 재시도된다. **즉시 재시도가 모두 실패하면 내구 기록이 생기는 것이 정상이다**(유실 방지). 그 기록의 카테고리가 `STATEMENT_TIMEOUT`이고 `IsTransientFailedSettlementCategory`가 참이다.
- D9 장기 재시도 연결: `[SQLSTATE 57014]` 태그가 붙은 실패 기록을 SettlementRetryWorker가 **선택해** 재처리하고, 성공하면 원본 failure가 resolved 된다.
- D10 배치 증폭 방지(§4.3): hold 배치가 `57014`로 실패하면 단건 폴백 없이 배치 전원 503. 정산 배치는 단건 폴백 없이 **한 트랜잭션**으로 인계된다 — commit이면 전원 OPEN/`STATEMENT_TIMEOUT` + outbox 전원 PROCESSED + undurable 없음, rollback이면 failure 0건 + outbox 전원 PENDING + 배치 전체 주문 ID가 undurable로 반환된다.
- D13 retry worker 중단 규칙(§4.4): **`RunOnce` 전체를 호출해** 확인한다.
  - settlement의 첫 `57014` 이후: 남은 settlement·completion·cancellation 호출 수가 **0**.
  - completion의 첫 `57014` 이후: 남은 completion과 cancellation 호출 수가 0.
  - cancellation의 첫 `57014` 이후: 남은 cancellation 호출 수가 0.
  - 원래 카테고리가 `DEADLOCK`이어도 이번 오류가 `57014`면 같은 중단.
  - failure 갱신까지 실패해도 같은 중단 결과.
  - `57014`가 아닌 오류면 다음 항목으로 계속 진행(기존 동작).
- D14 dispatcher 연결(§4.3 표): ① 배치 인계 commit → `undurableOrderIDs`가 비고, terminal은 dependency guard가 open failure를 보고 내구 defer한다. ② 배치 인계 rollback → maker·taker가 quarantine되어 terminal processor가 호출되지 않고 terminal outbox가 PENDING으로 남는다.
- D11 `IsTransientSettlementError` 파급 경로: 시장가 완료·취소 terminal·실패 기록 저장 재시도에서도 `57014`가 재시도로 처리된다.
- D12 비주문 HTTP 경로: 대표 경로 하나(예: 로그인 또는 이체 조회)에서 wrapped `55P03`·`57014`가 **실제 503 응답**으로 매핑된다(일부 handler가 raw DB 오류를 400으로 내리는 경로가 있어 함수 단위 분류만으로는 부족하다).

**게이트**

- `go build ./... && go vet ./...`
- 새 스키마 `go test -p 1 ./... -count=1`
- Docker Linux `go test -race -p 1 ./... -count=1`
- **프런트 단위 테스트**: `429 RATE_LIMITED`는 새로 관측 가능한 응답 계약이다. OrderForm·TransferForm 멱등키 테스트를 게이트에 포함한다(기존 정책이 429를 유지로 처리하는지 확인).
- **부하 관측**(배포 전): 대표 부하에서 `goexchange_db_timeout_total{sqlstate}` 건수와 주문 성공률을 기록한다. 판정 기준은 500 VU 재측정 작업에서 정한다.

## 9. 후속

- DB pool 획득 상한·요청 취소 전파(`context` 전 계층 배선).
- 분산 rate limit, refresh token·세션 폐기, 계정 잠금.
- 관리 포트 자체 인증.

## 10. 진행 순서

| # | 단계 | 체크포인트 |
|---|---|---|
| 1 | 이 설계 리뷰 | **CP 설계** |
| 2 | 구현 계획서 | — |
| 3 | 구현: ① 설정·세 DB 프로필 ② HTTP·관리 서버·종료 분기 ③ SQLSTATE 분류 ④ JWT·rate limit·프록시 ⑤ compose·runbook·문서 | — |
| 4 | 게이트 + 리뷰 | **CP 구현** |

## 부록 A. 리뷰 반영 이력

- 1차 설계 리뷰: 부팅 마이그레이션 풀 분리(P1), DSN 형식 무관 런타임 파라미터(P1), SQLSTATE 경로별 계약표와 `57014` transient 추가(P1), HTTP shutdown 실패 시 파이프라인 미종료 분기(P1), 관리 서버 별도 WriteTimeout·bind 실패·shutdown(P1), rate limiter 배치·eviction 동시성·`Retry-After` 계산(P2), stress compose·runbook 갱신 대상, 테스트 짧은 프로필화와 T 번호 정정, "프런트 계약 불변" 문구 철회.
- 2차 설계 리뷰: `STATEMENT_TIMEOUT` 내구 카테고리와 장기 retry worker 연결(§4.4), D8을 "기록 없음"에서 "transient 기록 → retry worker 해결"로 정정, 배치 단건 폴백의 시간 증폭 정책(§4.3), shutdown 안전 조건을 서비스/관리로 분리하고 주입 가능한 exit 함수 명시(§5), transfer poller·hold coordinator 행을 실제 동작으로 정정, `IsTransientSettlementError` 파급 경로 전수(§4.2), 비주문 HTTP 503 매핑 테스트(D12), rate limiter 2층 락(§6.2), stress 인증 한도를 preflight 포함 결정적 계약으로(§6.4), DB 통계 collector 중복 등록 주의(§3.1).
- 3차 설계 리뷰: 정산 탐침 제거(내구 소유권 이중화·시간 예산 미정의) → 배치 `57014`는 전원 `STATEMENT_TIMEOUT` 내구 인계, 실패 기록 성공 시 outbox PROCESSED·기록 실패 시에만 PENDING(§4.3 소유권 표), retry worker가 재차 `57014`를 받으면 RunOnce 중단(§4.4), D10 기대값을 소유권 규약에 맞게 정정하고 D13 추가, rate limiter map/entry 락 획득 순서 고정과 A3 결정적 경쟁 테스트(§6.2·§8), 구현 시 `exitFunc(1)` 직후 명시적 return.
- 4차 설계 리뷰: 내구 인계 자체의 증폭을 원자적 배치 인계로 차단(새 `RecordFailuresAndMarkOutboxProcessed`, 결과는 commit/rollback 둘뿐, rollback 시 배치 전체를 undurable로 반환), retry worker circuit break 판정을 저장된 카테고리가 아니라 **이번 시도의 반환 오류**로 넓히고 failure 갱신 실패 시에도 RunOnce 종료, D10을 원자적 결과로 정정하고 dispatcher 연결(D14) 추가.
- 5차 설계 리뷰: circuit break를 `RunOnce` 최상위 계약으로 확정(세 phase가 중단 여부를 반환, 발생 지점 이후의 남은 항목·phase를 실행하지 않음 — completion·cancellation 판단을 계획서로 미루지 않는다), 원자적 메서드의 입력·행 수 계약 명시(일대일 항목 구조체, 중복·0·빈 입력 거부, `RowsAffected` 검사, `status='PENDING'` 조건, 전체 rollback, 중복 제거 복사 금지), D13을 `RunOnce` 전체 호출과 phase별 발생원으로 확장.
