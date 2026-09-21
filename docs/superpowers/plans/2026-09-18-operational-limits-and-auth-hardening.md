# 운영 시간 상한과 인증 강화 실행 계획

> **For agentic workers:** Steps use checkbox (`- [ ]`) syntax. 각 Task는 RED(실패 테스트) → 구현 → GREEN 순서다.

**Goal:** 인바운드 HTTP 연결과 서버에서 실행되는 SQL 문장·락 대기에 시간 상한을 두고, 그 상한이 켜진 뒤에도 정산·취소·이체 worker의 재시도·멱등성 계약이 깨지지 않게 한다. 함께 JWT secret을 필수화하고, 인증·주문·이체에 요청 상한을 두며, `/metrics`·pprof를 관리 포트로 옮긴다.

**설계 문서(확정):** [2026-09-18-operational-limits-and-auth-hardening-design.md](../specs/2026-09-18-operational-limits-and-auth-hardening-design.md) — 6차 리뷰에서 확정.
**기준 SHA:** `5e79abc` (main) · **브랜치:** 백엔드 `feat/operational-limits`(신규), 프런트 `feat/rate-limit-idempotency`(신규)
**상태:** 계획 확정(2026-09-21, 3차 리뷰). 구현 미착수.

## Global Constraints

- 설계 문서의 계약이 이 계획의 암묵적 요구사항이다. 충돌하면 설계가 이긴다. 설계에 없는 결정이 필요하면 멈추고 보고한다.
- Task 1~9는 RED → 구현 → GREEN. 시간 상한 테스트는 **테스트 전용 짧은 값**(50~200ms)을 쓴다. 경과 시간을 정밀하게 맞추지 않고 **SQLSTATE·동작을 주 판정으로** 쓰며, 테스트 자체의 종료 상한은 1~2초로 넓게 둔다.
- `57014` 판정은 문자열 비교가 아니라 기존 wrapped SQLSTATE 분류기(`settlementErrorSQLState`)를 쓴다.
- 커밋은 **3개**: 백엔드 CP A 1개, 백엔드 CP B 1개, 프런트 1개(Task 10). 각각 `commit-message` 스킬 경유, 마지막 줄 `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`.
- 스테이징은 파일 경로 지정. `git add -A`·`.`·`-u` 금지, `_workspace/` 금지. amend·force push 금지. 푸시는 사용자 확인 후.
- 모든 `go test`에 `-count=1`. 통합 테스트는 `-p 1`. 호스트·Docker 실행을 겹치지 않는다.
- 기존 단건 경로(`processExecutionEvent`)의 "기록 성공·마킹 실패" 중간 상태 처리는 건드리지 않는다.
- 백엔드 저장소는 CRLF. 관련 없는 코드·주석·포맷 수정 금지.
- **프런트 명령은 프런트 저장소에서 실행한다.** 백엔드 디렉터리에서 `npm test`를 돌리지 않는다(`npm --prefix ../Go-exchange-front ...` 또는 해당 저장소로 이동).

## 새 의존성

`golang.org/x/time`이 go.mod에 없다(확인함). rate limiter의 `Reserve`/`Cancel` 의미가 설계 §6.2의 "거절하면서 토큰을 추가 소비하지 않는다"에 필요하다. 직접 구현하면 토큰 복원·시간 계산을 다시 검증해야 하므로 공식 패키지를 쓴다.

- Task 8 Step 1에서 추가한다. **`@latest`를 쓰지 않는다** — 실행 시점에 확인한 구체 버전을 명시해 고정하고, go.mod·go.sum을 커밋에 포함한다.
- `Retry-After`는 하나의 `now`를 잡아 `ReserveN(now, 1)` → `DelayFrom(now)` → 거절이면 `CancelAt(now)` 순서로 계산한다(결정적).

## 체크포인트

| CP | Task | 커밋 | 승인 |
|---|---|---|---|
| **A. DB 시간 상한과 정산 계약** | 1~5 | **Task 5** 끝에 1개(백엔드) | 리뷰 판정 후 CP B |
| **B. HTTP·인증·운영 경계** | 6~9 | **Task 9** 끝에 1개(백엔드) | 리뷰 판정 후 CP C |
| **C. 프런트 429 계약** | 10 | Task 10 끝에 1개(프런트) | 리뷰 판정 후 푸시 여부 사용자 결정 |

## 중단 조건

설계에 없는 결정이 필요할 때 / 기존 정산·멱등성 계약을 바꿔야 할 때 / 게이트 실패 / 부하 하니스 preflight 값 계산이 불가능할 때. 멈추고 보고한다.

## 파일 구조

| 파일 | 책임 | Task |
|---|---|---|
| `config/runtime.go` | 새 env 파서(HTTP·DB·admin·rate limit·trusted proxies) | 1, 6, 8 |
| `config/database.go` | 세 DB 프로필 생성·`SHOW` 검증·collector 등록 분리 | 1 |
| `internal/service/settlement_transient.go` | `57014` 추가 | 2 |
| `internal/service/failed_settlement_service.go` | `STATEMENT_TIMEOUT` 카테고리·분류·transient 판정 | 2 |
| `internal/metrics/metrics.go` | `goexchange_db_timeout_total{sqlstate,path}` | 2 |
| `internal/repository/failed_settlement_repository.go` | `RecordFailuresAndMarkOutboxProcessed` | 3 |
| `internal/service/hold_coordinator.go` | 배치 `57014` → 단건 폴백 금지 | 3 |
| `internal/service/settlement_retry_worker.go` | `stopRun` 전파 | 4 |
| `cmd/main.go` | 세 풀 배선·정산 배치 인계(3) → HTTP·관리 서버·종료 분기(6) → 라우트·미들웨어 배선(8) | 1, 3, 6, 8 |
| `internal/auth/token.go` | JWT 폴백 제거 | 7 |
| `internal/middleware/rate_limit.go` (신규) | 토큰 버킷·2층 락·정리 | 8 |
| `internal/httpapi/response.go` | `CodeRateLimited` | 8 |
| compose 3종·`monitoring/prometheus.yml`·`.env.*.example`·`docs/*` | 운영 설정·문서 | 9 |
| 프런트 `src/components/assets/TransferForm.tsx`(+테스트) | 429에서 키 유지 | 10 |

---

# CP A — DB 시간 상한과 정산 계약

## Task 1: 설정과 세 DB 프로필

**설계:** §3, §6 / **테스트:** C1·C2·D1·D2·D3·D4·D5

- [ ] **Step 1 (RED) — 설정 단위:** 새 env의 기본값·파싱(`strictPositiveEnv` 계열). 0·음수·공백·비정수·잘못된 CIDR은 에러. 프로필 생성 함수가 **URL DSN과 keyword DSN 양쪽**에서 기대 `RuntimeParams`를 만든다(C1·C2).
- [ ] **Step 2 (RED) — DB 통합(실제 PostgreSQL, 짧은 전용 프로필):** 전용 프로필도 운영과 같은 경로(`pgx.ParseConfig` → `RuntimeParams` → `stdlib.OpenDB`)로 만든다.
  - **D1** `statement_timeout=100ms` 프로필에서 `SELECT pg_sleep(1)` → `57014`. 이어서 **같은 `*sql.Conn`**에서 평범한 쿼리가 성공한다(풀에서 새 연결을 받아 우연히 통과하는 것이 아니라, 연결 재사용 계약을 직접 증명한다).
  - **D2** `lock_timeout=100ms` 프로필에서 **연결 두 개를 분리해** 한쪽이 행을 잠그고, 다른 쪽 `FOR UPDATE`가 `55P03`.
  - **D3** 세 `SHOW`(`statement_timeout`·`lock_timeout`·`idle_in_transaction_session_timeout`) 중 하나라도 기대와 다르면 초기화가 실패하고, 열었던 `*sql.DB`가 닫힌다.
  - **D4** 서비스 풀과 검산 풀의 `SHOW statement_timeout` 값이 서로 다르다.
  - **D5** 마이그레이션이 끝난 뒤 그 풀의 `*sql.DB`가 닫혀 있다 — `Ping`(또는 쿼리)이 **오류를 반환한다**로 판정한다. 정확한 오류 문자열을 비교하지 않는다(Go 버전 의존).
- [ ] **Step 3:** 구현.
  - 공통 헬퍼: 프로필 → `(*gorm.DB, *sql.DB, error)`.
  - 프로필 3종: 마이그레이션(10m/10s/0, 2커넥션), 서비스(15s/3s/30s, 기존 풀 설정), 검산(5m/3s/0, 최대 2).
  - **collector 등록을 연결 생성에서 분리한다.** 현재 `ConnectDB`가 `prometheus.MustRegister`를 직접 부른다(`database.go:95`) — 같은 프로필을 여러 번 여는 테스트에서 중복 등록 panic이 난다. 등록 함수가 `prometheus.Registerer`를 받아 main은 기본 레지스트리로 **풀당 한 번만** 등록하고, 테스트는 `prometheus.NewRegistry()`를 넘긴다. 마이그레이션 풀은 등록하지 않는다.
- [ ] **Step 4:** `cmd/main.go` 배선. 마이그레이션 풀로 AutoMigrate + `dbmigration.Up` 실행 후 close → 서비스 풀을 `config.DB`에 → 검산 풀은 `ReconciliationWorker`에만 주입하고 종료 시 close.
- [ ] **Step 5 (GREEN):** `go test ./config/... -count=1`, `go test -p 1 ./config/... ./internal/repository -run 'DB|Pool|Profile' -count=1 -v`(통합).

## Task 2: SQLSTATE 분류 확장

**설계:** §4, §4.2 / **테스트:** D6·D11 + 메트릭

- [ ] **Step 1 (RED):**
  - **D6** `IsTransientSettlementError`가 wrapped `57014`에 true(기존 3종 유지), `ClassifyFailedSettlement`가 `[SQLSTATE 57014]`를 `STATEMENT_TIMEOUT`으로, `IsTransientFailedSettlementCategory(STATEMENT_TIMEOUT) == true`.
  - **D11** 파급 경로 — 시장가 완료(`isRetryableCompletionError`), 취소 terminal, `retryTransient`에서 `57014`가 재시도로 처리된다.
  - **메트릭**: `goexchange_db_timeout_total{sqlstate,path}`가 **경로별로 1씩** 증가한다. 순수 분류 함수(`IsTransient...`) 호출만으로는 증가하지 않는다(중복 계측 방지).
  - **D8(실제 DB)** 즉시 재시도가 전부 `57014`로 실패하면 `failed_settlements`에 기록이 생기고, 그 카테고리가 `STATEMENT_TIMEOUT`이며 transient로 판정된다. 기록만 보지 말고 **settler 호출 횟수**로 즉시 재시도가 실제로 수행됐는지도 단언한다. **구현 전에는 `57014`가 transient가 아니라 재시도·기록 기대가 어긋나 실패한다.**
  - **D9(실제 DB)** 그 기록을 retry worker가 **선택해** 재처리하고, 성공하면 원본 failure가 resolved 된다. **구현 전에는 `STATEMENT_TIMEOUT` 분류가 없어 worker가 선택하지 않아 실패한다.**
- [ ] **Step 2:** 구현. `pgCodeQueryCanceled = "57014"`, 카테고리 상수·분류 case·transient 목록·카운터.
  - 주석: `57014`는 일반 `query_canceled` 코드라 수동 취소에도 쓰인다. 지금은 요청 context 취소가 없어 transient가 맞지만, 후속 context 전파 작업에서는 `context.Canceled`와 구분해야 한다.
- [ ] **Step 3 (GREEN):** `go test -p 1 ./internal/service ./internal/metrics ./cmd -count=1`. D8·D9는 즉시 재시도·실패 기록 경로가 `cmd/main.go`에 있으므로 **`./cmd`와 실제 DB 통합 테스트를 반드시 포함**한다.

## Task 3: 원자적 인계와 배치 57014 경로

**설계:** §4.3 / **테스트:** D10·D14 + 리포지토리 계약

소단계마다 GREEN을 확인하고 넘어간다.

- [ ] **Step 1 (RED) — 리포지토리:** `RecordFailuresAndMarkOutboxProcessed(items []SettlementFailureHandoff) error`(실제 DB).
  - 정상: N건 upsert + 그 outbox N건 PROCESSED.
  - 거부: 빈 입력, `nil` failure, `OutboxID == 0`, 중복 `OutboxID`, 중복 `trade_idempotency_key`.
  - 행 수 불일치: outbox 중 하나가 이미 PROCESSED면(`status='PENDING'` 조건) **전체 rollback** — failure도 남지 않는다. 단, 이미 존재하던 failure 행이 지워진다는 뜻이 아니라 **이 트랜잭션의 생성·갱신이 하나도 반영되지 않는다**는 뜻으로 기대값을 쓴다.
  - conflict upsert 범위가 기존 `RecordFailure`와 같다(`error_message`·`status=OPEN`·`retry_count+1`·resolution/resolved_by/notes/resolved_at 초기화, `failed_settlement_repository.go:35-47`).
- [ ] **Step 2:** 구현 → 리포지토리 GREEN. `InsertBatchAndMarkCancelCommands`(`trade_outbox_repository.go:32`)의 트랜잭션 모양을 따르되 **중복 제거는 하지 않는다**(중복은 입력 불변식 위반 → 에러).
- [ ] **Step 3 (RED) — hold:** 배치가 `57014`로 실패하면 단건 폴백을 하지 않는다. service 계층에는 HTTP 상태가 없으므로 **`fallbackPerRequest`가 호출되지 않고 모든 결과가 unavailable 계열 오류**인지로 판정한다(HTTP 503 매핑 확인은 D7·D12가 맡는다). `55P03`·그 밖의 오류는 기존 폴백 유지.
- [ ] **Step 4:** 구현 → hold GREEN.
- [ ] **Step 5 (RED) — 정산·dispatcher:**
  - **D10** `settleTradeBatchWithFallback`이 `57014`를 받으면 단건 폴백 없이 전원 인계. commit이면 failure 전원 OPEN/`STATEMENT_TIMEOUT` + outbox 전원 PROCESSED + `undurableOrderIDs` 비어 있음. rollback이면 failure 0건 + outbox 전원 PENDING + 배치 전체 maker·taker가 `undurableOrderIDs`.
  - **D14** ① commit → terminal이 dependency guard로 내구 defer된다. ② rollback → 주문이 quarantine되어 terminal processor가 호출되지 않고 terminal outbox가 PENDING으로 남는다.
- [ ] **Step 6:** 구현 → GREEN. `go test -p 1 ./internal/repository ./internal/service ./cmd -count=1`.

## Task 4: retry worker의 RunOnce 중단

**설계:** §4.4 / **테스트:** D13 (D8·D9는 Task 2로 옮겼다 — Task 2 구현만으로 통과하므로 여기서는 RED가 아니다)

- [ ] **Step 1 (RED) — D13:** `RunOnce` 전체를 호출한다.
  - settlement 첫 `57014` → 남은 settlement·completion·cancellation 호출 수 0.
  - completion 첫 `57014` → 남은 completion·cancellation 호출 수 0.
  - cancellation 첫 `57014` → 남은 cancellation 호출 수 0.
  - 원래 카테고리가 `DEADLOCK`이어도 이번 오류가 `57014`면 같은 중단.
  - failure 갱신까지 실패해도 같은 중단.
  - 대조군: `57014`가 아닌 transient 오류면 다음 항목으로 계속 진행.
- [ ] **Step 2:** 구현. 세 phase가 중단 여부를 반환하고 `RunOnce`가 이후 항목·phase를 건너뛴다.
- [ ] **Step 3 (GREEN):** `go test -p 1 ./internal/service ./cmd -count=1`.

## Task 5: CP A 게이트와 커밋

- [ ] `go build ./... && go vet ./...`
- [ ] 새 스키마에서 `go test -p 1 ./... -count=1 -timeout=20m`
- [ ] Docker Linux race(Task 9 Step 5의 명령과 동일)
- [ ] 스테이징(파일 지정) → `commit-message` 스킬 → 커밋. 푸시하지 않는다.
- [ ] 보고 후 멈춘다. 리뷰 CP A 판정 전에는 CP B를 시작하지 않는다.

---

# CP B — HTTP·인증·운영 경계

## Task 6: HTTP 상한·관리 서버·종료 분기

**설계:** §2, §5, §7 / **테스트:** H1~H9 + D7·D12

- [ ] **Step 1 (RED) — HTTP 수준:** 실제 loopback 리스너 + 테스트 전용 짧은 상한.
  - H1 slowloris(헤더 지연) / H2 느린 body / H3 느린 핸들러 / H4 idle은 **raw TCP**로 EOF 확인(`http.Client` 금지) / H5 `MaxHeaderBytes`.
  - H6 WebSocket: 테스트 서버 `WriteTimeout` 50~100ms에서도 업그레이드된 연결이 더 오래 살아 메시지를 주고받는다(기존 동작 보존 — 구현 전에도 통과할 수 있다. 회귀 방지용).
  - H7 관리 포트: 서비스 `/metrics` 404, 관리 `/metrics` 200, pprof는 env off면 404·on이면 200.
  - H8 관리 서버 graceful shutdown, bind 실패 시 부팅 실패.
  - H9 종료 분기(주입한 exit 함수): 서비스 Shutdown 실패 → hold coordinator·엔진 Stop **미호출** + 종료 코드 1. 관리 Shutdown 실패 + 서비스 성공 → drain 정상 수행.
- [ ] **Step 2 (RED) — DB 오류의 HTTP 매핑:**
  - **D7** 주문 생성 경로에서 timeout이 났을 때 — 멱등 레코드가 없으면 503, 있으면 기존 outcome 응답(202 PENDING / 503 UNKNOWN). 응답과 DB 상태가 일치한다.
  - **D12** 대표 비주문 경로는 **로그인으로 고정한다**(판별력이 가장 크다). `FindByEmail`이 wrapped `55P03`·`57014`를 반환할 때, 실제 HTTP 요청으로 `AuthHandler`를 호출해:
    - timeout만 **503**으로 변환된다(지금처럼 invalid credentials로 숨기지 않는다).
    - 사용자 없음·비밀번호 오류는 그대로 **401**이다(인증 실패 계약을 깨뜨리는 구현을 잡는다).
    - 구현 시 handler 매핑뿐 아니라 `AuthService.Login`이 인식된 timeout을 일반 invalid credentials로 덮지 않아야 한다.
- [ ] **Step 3:** 구현.
  - 서비스 `http.Server`에 4종 상한 + `MaxHeaderBytes`(전부 env).
  - 관리 서버: `GOEXCHANGE_ADMIN_ADDR`, `WriteTimeout` 120s(env), `/metrics` 이전, pprof 등록(env 게이트), `:6060` 리스너 제거, bind 실패 `log.Fatal`.
  - 종료: 서비스·관리 각자 `context.WithTimeout`(예산 비공유). 서비스 성공 시에만 drain. 서비스 실패 시 `exitFunc(1)` **직후 명시적 return**. 관리 실패는 `Close()` 후 진행.
  - 필요한 경우에만 handler의 DB 오류 매핑을 고친다(D12가 요구하는 최소 범위).
- [ ] **Step 4 (GREEN):** `go test -p 1 ./cmd ./internal/handler ./internal/... -count=1`.

## Task 7: JWT 필수화

**설계:** §6.1 / **테스트:** A1

- [ ] **Step 1 (RED):** `NewTokenManagerFromEnv` — 미설정·공백은 `ErrInvalidJWTSecret`, **값이 있으면 성공**(대조군을 반드시 넣는다. 없으면 항상 실패하는 구현도 통과한다).
- [ ] **Step 2:** 폴백 제거. `main`이 실패 시 `log.Fatal`.
- [ ] **Step 3:** 영향 전수 수정 — `t.Setenv`가 필요한 테스트를 `grep`으로 찾아 전부 고치고 목록을 보고한다. 바이너리를 기동하는 CI·E2E가 있으면 secret을 명시한다.
- [ ] **Step 4 (GREEN):** `go test ./internal/auth ./cmd -count=1` **그리고** grep 게이트 — 저장소에 `dev-only-change-me`가 0건이다(문서 설명 문구 포함. 남길 이유가 있으면 보고). 둘 다 통과해야 Task 7 완료다.

## Task 8: rate limit과 프록시 신뢰

**설계:** §6.2, §6.3 / **테스트:** A2~A6 + 라우터 수준

- [ ] **Step 1:** `golang.org/x/time` 추가(버전 고정) → `go mod tidy`.
- [ ] **Step 2 (RED) — 미들웨어 단위:**
  - A2 버스트까지 통과 → 초과 429 + `Retry-After` ≥ 1 + `RATE_LIMITED`. 키가 다르면 무영향. 거절이 토큰을 추가 소비하지 않는다(거절 후 기대 시점에 통과).
  - A3 **결정적** 경쟁: 요청이 entry를 얻은 시점과 cleanup 삭제 시도를 채널 장벽으로 맞물리게 해 같은 키에 limiter가 둘 생기지 않고 총 허용 수가 상한을 넘지 않는다. `-race`는 보조.
  - A4 login과 register가 서로 다른 버킷.
  - A5 프록시 신뢰: 목록이 비면 `X-Forwarded-For` 무시(`RemoteAddr` 기준), 신뢰 CIDR 설정 시에만 반영. 잘못된 CIDR은 부팅 실패.
  - A6 비활성이면 통과.
- [ ] **Step 3 (RED) — 실제 라우터 배선:** 단위 테스트만으로는 배선이 틀려도 통과한다. 실제 라우터로 확인한다.
  - 미인증 요청은 user limiter보다 먼저 **401**을 받는다(인증 뒤 배치 확인).
  - 서로 다른 사용자 ID는 서로 다른 버킷을 쓴다.
  - login과 register가 실제 라우트에서도 별도 버킷이다.
  - 주문 생성·취소와 이체가 각각 **지정된 한도**를 쓴다(주문 한도로 이체가 제한되지 않는다).
- [ ] **Step 4:** 구현. `internal/middleware/rate_limit.go` — mutex map + entry mutex, 락 획득 순서는 설계 §6.2 그대로. 하나의 `now`로 `ReserveN`→`DelayFrom`→거절 시 `CancelAt`. 정리 goroutine은 backgroundCtx로 종료.
- [ ] **Step 5:** 배선. 인증 limiter는 `/auth/login`·`/auth/register` 핸들러 앞(IP 키, 각각 별도 버킷), 사용자 limiter는 `AuthRequired` 뒤. `r.SetTrustedProxies(nil)` 기본 + env. `CodeRateLimited` 추가.
- [ ] **Step 6 (GREEN):** `go test ./internal/middleware ./internal/httpapi ./cmd -count=1`.

## Task 9: 운영 설정·문서와 CP B 게이트

**설계:** §6.4, §7, §8

- [ ] **Step 1:** 부하 한도 계산. `loadtest/*.js`의 `TOTAL_USERS`·`SETUP_BATCH_SIZE`·배치 간격으로 setup의 실제 인증 전송률을 계산해 `docker-compose.stress.yml`의 인증 `rps`·`burst`를 정한다(계산 과정을 보고서에 적는다).
- [ ] **Step 2:** setup preflight — k6 setup에서 가입·로그인 응답이 `429`면 즉시 실패시킨다.
- [ ] **Step 3:** 설정 파일.
  - compose 3종: `GOEXCHANGE_ADMIN_ADDR=0.0.0.0:9101`, stress만 `127.0.0.1:9101:9101` published(`127.0.0.1:6060:6060` 제거), prod·deploy는 미공개.
  - `docker-compose.deploy.yml:36`의 JWT 폴백을 `:?GOEXCHANGE_JWT_SECRET is required`로.
  - `monitoring/prometheus.yml` 타깃 `backend:8080` → `backend:9101`.
  - `.env.prod.example`·`.env.deploy.example`·`.env.stress.example`에 새 env 추가.
- [ ] **Step 4:** 문서. `docs/gcp-stress-test-runbook.md`(`ssh -L 6060` → `9101`, 프로파일 URL, `:8080/metrics` 절차, 적용한 rate limit 값과 preflight), `TESTING.md`(JWT 필수·새 env), `docs/EC2_DEPLOYMENT.md`·`docs/DOCKER_DEPLOYMENT.md`. 과거 benchmark 기록은 고치지 않는다.
- [ ] **Step 5:** 게이트.
  - `go build ./... && go vet ./...`
  - 새 스키마 `go test -p 1 ./... -count=1 -timeout=20m`
  - `go test -p 1 -shuffle=on ./internal/service -count=1`
  - Docker Linux: `docker run --rm --network go-exchange-back_default -e GOEXCHANGE_TEST_DATABASE_DSN="host=goexchange-postgres-test user=goexchange_test password=goexchange_test_password dbname=goexchange_test port=5432 sslmode=disable" -v "${PWD}:/src" -w /src golang:1.25 go test -race -p 1 ./... -count=1 -timeout=30m`
- [ ] **Step 6:** 스테이징(파일 지정) → `commit-message` 스킬 → 커밋(백엔드 두 번째). 푸시하지 않는다.
- [ ] **Step 7:** 보고 후 멈춘다. 리뷰 CP B 판정 전에는 CP C를 시작하지 않는다.

---

# CP C — 프런트 429 계약

## Task 10: TransferForm이 429에서 멱등키를 유지한다

**설계:** §6.2(429는 서버에 도달했지만 처리되지 않았다) / 저장소: `Go-exchange-front`, **기준 SHA `666b6ce`(main)**, 새 브랜치 `feat/rate-limit-idempotency`

현재 `TransferForm.tsx`는 `err.status < 500`이면 키를 버린다. 429도 여기 걸려 재시도가 **새 `client_request_key`**로 나간다. OrderForm에는 429 회귀 테스트가 있지만 TransferForm에는 없다.

- [ ] **Step 1 (RED):** `TransferForm.test.tsx`에 429 응답 후 재제출이 **같은 `client_request_key`**를 쓰는 테스트를 추가한다(현재 코드에서 실패).
- [ ] **Step 2:** 구현. 429를 키 폐기 대상에서 제외한다(4xx 폐기 규칙에 429 예외). 주석에 근거를 적는다 — 429는 서버가 요청을 판정한 것이 아니라 **처리하지 않고 거절**한 것이다.
- [ ] **Step 3 (GREEN):** 프런트 저장소에서 `npm test -- --run src/components/assets/TransferForm.test.tsx`, 이어서 OrderForm 기존 429 테스트를 포함해 `npm test -- --run`.
- [ ] **Step 4:** `npm run lint && npm run build`.
- [ ] **Step 5:** 스테이징(파일 지정) → `commit-message` 스킬 → 커밋(프런트). 푸시하지 않는다.
- [ ] **Step 6:** 보고. E2E는 백엔드 rate limit이 켜진 환경이 필요하므로 이번 범위에서 돌리지 않는다(필요성은 리뷰에서 판단).

---

## 설계 §8 테스트 대응표

| 설계 테스트 | Task |
|---|---|
| C1 설정 파싱 / C2 DSN 프로필 | 1 |
| D1 statement_timeout / D2 lock_timeout | 1 |
| D3 부팅 검증 / D4 검산 풀 / D5 마이그레이션 풀 close | 1 |
| D6 분류 / D11 파급 경로 / 타임아웃 카운터 | 2 |
| D10 배치 인계 / D14 dispatcher 연결 | 3 |
| D8 내구 기록 / D9 장기 retry 해결 | 2 |
| D13 RunOnce 중단 | 4 |
| D7 주문 경로 응답 / D12 비주문 HTTP 503 | 6 |
| H1~H9 | 6 |
| A1 | 7 |
| A2~A6 + 라우터 배선 | 8 |
| 프런트 429 계약 | 10 |
