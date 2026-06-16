# 백엔드 테스트 가이드

Go Exchange 백엔드는 두 가지 테스트 경로를 사용합니다.

- 단위 테스트와 대부분의 서비스 테스트는 외부 서비스 없이 실행됩니다.
- PostgreSQL 통합 테스트는 `GOEXCHANGE_TEST_DATABASE_DSN`이 설정되어 있을 때만 실제 DB에 연결됩니다.

`GOEXCHANGE_TEST_DATABASE_DSN`이 비어 있으면 repository/service 통합 테스트는 의도적으로 skip됩니다.
이 방식은 로컬 개발자가 매번 Postgres 테스트 DB를 띄우지 않아도 기본 테스트를 빠르게 돌릴 수 있게 하기 위한 구조입니다.

## 런타임 설정

운영 환경과 로컬 실행 설정은 환경변수로 주입합니다.
실제 DB 비밀번호를 소스 코드, 테스트 코드, 스크립트, 커밋되는 문서에 하드코딩하지 않습니다.

로컬 개발에서는 백엔드가 DB 연결 전에 현재 작업 디렉터리의 `.env.local`을 먼저 읽고, 그다음 `.env`를 읽습니다.
이미 프로세스 환경변수로 설정된 값이 있으면 파일의 값보다 우선합니다.
`.env.local`은 커밋하지 않고, 커밋되는 템플릿은 `.env.example`을 사용합니다.

DB 설정 우선순위:

1. `GOEXCHANGE_DATABASE_DSN`이 있으면 그 값을 그대로 사용합니다. PostgreSQL keyword DSN 또는 GORM PostgreSQL driver가 허용하는 형식을 사용할 수 있습니다.
2. `GOEXCHANGE_DATABASE_DSN`이 없으면 아래 개별 환경변수로 DSN을 조합합니다.

| 환경변수 | 기본값 | 설명 |
| --- | --- | --- |
| `GOEXCHANGE_DB_HOST` | `localhost` | PostgreSQL host |
| `GOEXCHANGE_DB_USER` | `postgres` | PostgreSQL user |
| `GOEXCHANGE_DB_PASSWORD` | 빈 값 | PostgreSQL password. 로컬에서 password 없이 접속 가능한 설정일 때만 비워 둡니다. |
| `GOEXCHANGE_DB_NAME` | `goexchange` | PostgreSQL database name |
| `GOEXCHANGE_DB_PORT` | `5432` | PostgreSQL port |
| `GOEXCHANGE_DB_SSLMODE` | `disable` | PostgreSQL SSL mode. 로컬 외 환경에서는 상황에 맞게 `require` 이상의 설정을 사용합니다. |
| `GOEXCHANGE_DB_CONNECT_TIMEOUT` | `5` | PostgreSQL 연결 timeout, 단위는 초 |
| `GOEXCHANGE_CORS_ALLOWED_ORIGINS` | `http://localhost:3000,http://127.0.0.1:3000` | HTTP API 호출을 허용할 브라우저 origin 목록. LAN 주소로 프론트를 열면 예: `http://192.168.219.100:3000`을 추가합니다. |
| `GOEXCHANGE_MARKET_RULES_PATH` | `config/market_rules.json` | 시장 상태, 수량 정책, 최소 주문 금액, 수수료율, KRW tick size를 읽을 JSON 파일 경로 |

로컬 실행 예시, PowerShell:

```powershell
$env:GOEXCHANGE_DB_HOST="localhost"
$env:GOEXCHANGE_DB_USER="postgres"
$env:GOEXCHANGE_DB_PASSWORD="<local-only-password>"
$env:GOEXCHANGE_DB_NAME="goexchange"
$env:GOEXCHANGE_DB_PORT="5432"
$env:GOEXCHANGE_DB_SSLMODE="disable"
$env:GOEXCHANGE_DB_CONNECT_TIMEOUT="5"
$env:GOEXCHANGE_ENABLE_DEV_TOOLS="true"
$env:GOEXCHANGE_DEV_TOOLS_TOKEN="<local-dev-tools-token>"
$env:GOEXCHANGE_ENABLE_UPBIT="false"
$env:GOEXCHANGE_CORS_ALLOWED_ORIGINS="http://localhost:3000,http://127.0.0.1:3000"
go run ./cmd
```

로컬 실행 예시, bash:

```bash
export GOEXCHANGE_DB_HOST="localhost"
export GOEXCHANGE_DB_USER="postgres"
export GOEXCHANGE_DB_PASSWORD="<local-only-password>"
export GOEXCHANGE_DB_NAME="goexchange"
export GOEXCHANGE_DB_PORT="5432"
export GOEXCHANGE_DB_SSLMODE="disable"
export GOEXCHANGE_DB_CONNECT_TIMEOUT="5"
export GOEXCHANGE_ENABLE_DEV_TOOLS="true"
export GOEXCHANGE_DEV_TOOLS_TOKEN="<local-dev-tools-token>"
export GOEXCHANGE_ENABLE_UPBIT="false"
export GOEXCHANGE_CORS_ALLOWED_ORIGINS="http://localhost:3000,http://127.0.0.1:3000"
go run ./cmd
```

## 인증 설정

| 환경변수 | 기본값 | 설명 |
| --- | --- | --- |
| `GOEXCHANGE_JWT_SECRET` | `dev-only-change-me` | access token 서명에 사용하는 HMAC secret. 로컬 외 환경에서는 강한 secret을 별도로 설정합니다. |

인증 API:

- `POST /auth/register`: `name`, `email`, `password`로 사용자를 생성하고 bearer token을 반환합니다.
- `POST /auth/login`: `email`, `password`로 로그인하고 bearer token을 반환합니다.
- `POST /orders`, `DELETE /orders/:id`: `Authorization: Bearer <token>`이 필요하며, hard-coded user가 아니라 인증된 사용자 ID를 사용합니다.
- `GET /orders`, `GET /orders/:id`, `GET /wallets`, `GET /trades`: 인증된 사용자에게 속한 데이터만 반환합니다.

에러 응답은 구조화된 형태를 사용합니다.

```json
{"error":{"code":"VALIDATION_ERROR","message":"invalid price"}}
```

조회 API query parameter 예시:

- `GET /orders?status=PENDING&coin_symbol=BTC&limit=50`
- `GET /trades?coin_symbol=BTC&limit=50`

조회 응답의 decimal 값은 부동소수점 정밀도 손실을 피하기 위해 JSON string으로 반환합니다.

## 개발용 도구

| 환경변수 | 기본값 | 설명 |
| --- | --- | --- |
| `GOEXCHANGE_ENABLE_DEV_TOOLS` | `false` | `POST /dev/wallets/fund` 같은 개발용 helper route를 활성화합니다. |
| `GOEXCHANGE_DEV_TOOLS_TOKEN` | 빈 값 | 개발용 helper route 호출에 필요한 별도 token입니다. 요청 header에 `X-GoExchange-Dev-Token: <token>`을 보냅니다. |
| `GOEXCHANGE_ENABLE_UPBIT` | `true` | Upbit WebSocket ticker feed를 활성화합니다. 외부 네트워크가 느리거나 필요 없는 로컬 API 테스트에서는 `false`로 둡니다. |

`GOEXCHANGE_ENABLE_DEV_TOOLS=true`일 때 로컬 개발용 지갑 충전을 할 수 있습니다.

```http
POST /dev/wallets/fund
Authorization: Bearer <token>
X-GoExchange-Dev-Token: <local-dev-tools-token>
Content-Type: application/json

{"coin_symbol":"KRW","amount":"1000000"}
```

이 API는 호출자의 지갑만 생성하거나 증가시킵니다.
관리자 백오피스나 실제 입출금 기능 없이 로컬 E2E 주문 테스트를 하기 위한 개발용 경로입니다.
기본값은 비활성화이며, 활성화하더라도 별도 dev token이 필요합니다.

개발용 지갑 충전은 `DEV_FUND` ledger entry도 함께 기록합니다.
따라서 로컬 테스트 중 발생한 잔고 변경도 추적할 수 있습니다.

## 시장 정책 설정

시장 정책은 기본적으로 `config/market_rules.json`에서 읽습니다.
다른 로컬 프로필이나 배포 프로필을 사용하려면 `GOEXCHANGE_MARKET_RULES_PATH`에 다른 JSON 파일 경로를 지정합니다.

설정 파일이 관리하는 값:

- `min_order_notional`: 지정가 주문과 시장가 매수에 적용할 최소 quote 금액입니다. `0`이면 최소 주문 금액 제한을 사용하지 않습니다.
- `fee_rate`: market rules API가 반환하는 기본 거래 수수료율입니다.
- `default_market_status`: `markets`에 없는 코인에 적용할 기본 마켓 상태입니다.
- `default_min_order_quantity`: 기본 최소 주문 수량입니다.
- `default_base_quantity_step`: 기본 base asset 수량 단위입니다.
- `markets`: 코인별 `trading_status`, `min_order_quantity`, `base_quantity_step` override입니다.
- `tick_rules`, `max_tick_size`: KRW 가격 tick size 정책입니다.

코인별 설정 예시:

```json
"XRP": {
  "trading_status": "ACTIVE",
  "min_order_quantity": "1",
  "base_quantity_step": "1"
}
```

`trading_status`는 현재 `ACTIVE`, `HALTED`를 지원합니다.
`HALTED` 마켓은 `GET /markets/rules`에서 `trading_enabled=false`로 내려가며, 신규 매수/매도 주문은 conflict error로 거부됩니다.
기존 주문 취소는 계속 허용해 사용자가 locked balance를 해제할 수 있습니다.

## 지갑 원장

`ledger_entries`는 지갑 잔고 변경과 같은 DB transaction 안에서 기록됩니다.
현재 구조는 지갑 이벤트 원장입니다.

각 row가 기록하는 값:

- `user_id`, `coin_symbol`
- `entry_type`: `DEV_FUND`, `ORDER_HOLD`, `ORDER_RELEASE`, `TRADE_SETTLEMENT`
- `available_delta`, `locked_delta`
- `available_balance_after`, `locked_balance_after`
- `reference_type`, `reference_id`, 선택적인 `reference_key`

현재 write point:

- `POST /dev/wallets/fund`: available balance를 증가시키고 `DEV_FUND` 기록
- `POST /orders`: available에서 locked로 이동시키고 `ORDER_HOLD` 기록
- `DELETE /orders/:id`: 남은 locked를 available로 되돌리고 `ORDER_RELEASE` 기록
- settlement 성공: buyer KRW, buyer coin, seller coin, seller KRW에 대해 총 4개의 `TRADE_SETTLEMENT` 기록

중복 settlement는 기존 idempotent trade를 발견하면 지갑 변경 전에 반환되므로 ledger row를 추가로 쓰지 않습니다.
실패한 settlement도 transaction rollback 때문에 ledger row를 남기지 않습니다.

## 매칭 체결 이벤트 식별자

매칭엔진은 체결 이벤트마다 engine event identity를 부여합니다.

- `engine_sequence`: 실행 중인 매칭엔진 인스턴스 안에서 증가하는 sequence number
- `engine_event_id`: engine instance identity와 sequence를 포함한 문자열

settlement가 명시적인 idempotency key 없는 trade를 받으면 `engine_event_id`를 우선 사용해 `engine:<engine_event_id>` 형태의 key를 생성합니다.
이전 수동 경로나 테스트 경로처럼 engine event id가 없는 경우에는 `coin_symbol`, 주문 ID, 가격, 수량 기반 deterministic payload hash를 fallback으로 사용합니다.

이 구조 덕분에 같은 주문쌍에서 가격과 수량이 같은 서로 다른 engine trade event가 발생해도 동일한 payload hash로 잘못 합쳐질 가능성을 줄입니다.

## WebSocket Origin 허용 설정

| 환경변수 | 기본값 | 설명 |
| --- | --- | --- |
| `GOEXCHANGE_WS_ALLOWED_ORIGINS` | `http://localhost:3000,http://localhost:5173` | 브라우저 WebSocket 연결을 허용할 origin 목록 |
| `WS_ALLOWED_ORIGINS` | 빈 값 | `GOEXCHANGE_WS_ALLOWED_ORIGINS`가 없을 때만 사용하는 하위 호환 alias |
| `GOEXCHANGE_WS_ALLOW_MISSING_ORIGIN` | `false` | Origin header가 없는 명시적인 비브라우저 클라이언트나 테스트에서만 `true`로 설정합니다. |

브라우저는 일반적으로 WebSocket handshake에 `Origin` header를 보냅니다.
Origin이 없는 요청은 기본적으로 거부됩니다.

## 실패 정산 기록

settlement 실패는 `failed_settlements`에 영속 기록됩니다.
매칭엔진이 체결 이벤트를 발행했지만 settlement가 거부한 이벤트를 운영자가 확인할 수 있게 하기 위한 구조입니다.
예를 들어 주문 취소 이후 늦게 도착한 stale trade도 여기에 기록될 수 있습니다.

핵심 동작:

- `trade_idempotency_key`는 필수이며 unique입니다.
- 같은 failed trade가 다시 기록되면 새 row를 만들지 않고 기존 row의 `retry_count`, `error_message`, `updated_at`을 갱신합니다.
- `retry_count`는 최초 1부터 시작합니다.
- `error_message`는 애플리케이션에서 길이를 제한해 저장합니다.
- 성공한 정산은 `trades`, 실패한 정산은 `failed_settlements`에 분리해 저장합니다.

## 실패 정산 확인 처리

`failed_settlements`는 자동 retry queue가 아니라 운영자 확인용 incident log입니다.
row를 `RESOLVED`로 변경하는 것은 운영자가 확인 또는 조치를 완료했다는 감사 정보만 남기는 작업이며, 지갑, 주문, 체결 row를 변경하지 않습니다.

상태 정책:

- `OPEN`: 아직 확인이나 수동 판단이 필요한 상태
- `RESOLVED`: 운영자가 확인했거나 필요한 외부 조치를 마친 상태

resolve audit 필드:

- `resolution`: resolve 시 필수인 짧은 사유
- `resolved_by`: 운영자 또는 system label
- `resolved_at`: repository에서 `RESOLVED` 전환 시 설정
- `notes`: 선택적인 후속 설명

이미 `RESOLVED`인 row를 다시 resolve하면 idempotent no-op으로 처리합니다.
같은 idempotency key의 실패가 다시 기록되면 row는 `OPEN`으로 재오픈되고, retry count가 증가하며, resolution 관련 필드는 초기화됩니다.

간단한 triage category:

- `CANCELLED_ORDER`: 에러에 `CANCELLED`가 포함된 경우
- `IDEMPOTENCY_CONFLICT`: 에러에 `idempotency key conflict`가 포함된 경우
- `INSUFFICIENT_LOCKED_BALANCE`: 에러에 `insufficient locked`가 포함된 경우
- `UNKNOWN`: 그 외

로컬 확인 SQL:

```sql
SELECT *
FROM failed_settlements
WHERE status = 'OPEN'
ORDER BY occurred_at ASC, id ASC
LIMIT 50;
```

## 매칭엔진 시작 복구

매칭엔진은 메모리 기반이므로 서버 시작 시 DB의 open order를 오더북에 복원합니다.
HTTP API를 열기 전에 bootstrap을 완료해 DB에는 open order가 있는데 메모리 오더북은 비어 있는 상태로 요청을 받지 않도록 합니다.

시작 동작:

- 매칭엔진을 먼저 시작합니다.
- settlement consumer와 snapshot consumer를 시작합니다.
- status가 `PENDING` 또는 `PARTIAL`이고 `amount > filled_amount`인 주문을 조회합니다.
- `created_at ASC, id ASC` 순서로 engine에 제출해 시간 우선순위를 최대한 보존합니다.
- `PARTIAL` 주문은 `amount - filled_amount`만 메모리 오더북에 복원합니다.
- `FILLED`, `CANCELLED`, remaining이 0인 주문은 복원하지 않습니다.
- bootstrap 실패 시 서버는 API traffic을 받지 않고 종료됩니다.

복원된 주문끼리 가격 조건이 맞으면 bootstrap 중에도 자연스럽게 체결이 발생할 수 있습니다.
이 경우 settlement consumer가 이미 실행 중이므로 일반 체결과 같은 경로로 정산됩니다.

## 테스트용 PostgreSQL

통합 테스트용 DB는 `docker-compose.test.yml`에 정의되어 있습니다.

- Service: `postgres-test`
- Container: `goexchange-postgres-test`
- Database: `goexchange_test`
- User: `goexchange_test`
- Password: `goexchange_test_password`
- Host port: `55432`

이 DB는 `config/database.go`가 사용하는 개발용 DB와 분리되어 있습니다.

테스트 DB 실행:

```powershell
docker compose -f docker-compose.test.yml up -d
```

```bash
docker compose -f docker-compose.test.yml up -d
```

통합 테스트 DSN 설정:

```powershell
$env:GOEXCHANGE_TEST_DATABASE_DSN="host=localhost user=goexchange_test password=goexchange_test_password dbname=goexchange_test port=55432 sslmode=disable"
```

```bash
export GOEXCHANGE_TEST_DATABASE_DSN="host=localhost user=goexchange_test password=goexchange_test_password dbname=goexchange_test port=55432 sslmode=disable"
```

통합 테스트 실행:

```powershell
go test -run Integration -v ./internal/repository ./internal/service
```

```bash
go test -run Integration -v ./internal/repository ./internal/service
```

통합 테스트 setup은 `AutoMigrate`를 먼저 실행한 뒤 goose migration runner를 적용합니다.
따라서 model struct 기반 스키마와 명시적인 DB constraint를 함께 검증합니다.

전체 백엔드 테스트:

```powershell
go test ./...
```

```bash
go test ./...
```

테스트 DB와 volume 제거:

```powershell
docker compose -f docker-compose.test.yml down -v
```

```bash
docker compose -f docker-compose.test.yml down -v
```

## GitHub Actions CI

백엔드 저장소는 `.github/workflows/backend-ci.yml`에 GitHub Actions workflow를 가지고 있습니다.

workflow는 두 job으로 나뉩니다.

- `Unit tests and vet`: `go mod download`, `go test ./...`, `go vet ./...`, `go test -count=20 ./internal/matching`을 실행합니다. 이 job에서는 `GOEXCHANGE_TEST_DATABASE_DSN`을 설정하지 않으므로 통합 테스트가 의도적으로 skip됩니다.
- `Postgres integration tests`: `postgres:16-alpine` service를 시작하고 `GOEXCHANGE_TEST_DATABASE_DSN`을 설정한 뒤 `go test -run Integration -v ./internal/repository ./internal/service`를 실행합니다.

로컬 Docker Compose는 개발자의 기존 PostgreSQL 5432 포트와 충돌하지 않도록 host port `55432`를 사용합니다.
GitHub Actions service container는 job 안에서 `localhost:5432`로 접근하므로 CI DSN은 아래 값을 사용합니다.

```text
host=localhost user=goexchange_test password=goexchange_test_password dbname=goexchange_test port=5432 sslmode=disable
```

integration job은 항상 `GOEXCHANGE_TEST_DATABASE_DSN`을 설정하므로 repository/service 통합 테스트가 skip되지 않고 실제 PostgreSQL에 연결됩니다.

## Goose 마이그레이션 러너

개발 편의를 위해 `AutoMigrate`는 유지하되, 명시적인 constraint와 index는 `internal/dbmigration`의 goose migration runner로 적용합니다.

현재 시작/테스트 흐름:

1. GORM `AutoMigrate`가 model struct 기준으로 기본 table을 생성하거나 갱신합니다.
2. `dbmigration.Up(db)`가 `migrations/`의 goose migration을 실행합니다.
3. goose는 적용된 migration version을 DB version table에 기록해 같은 migration을 raw script처럼 반복 적용하지 않습니다.

첫 migration인 `migrations/001_constraints.sql`은 그동안 누적된 constraint와 index를 정리한 baseline migration입니다.
기존 로컬 DB에 이미 일부 constraint가 있을 수 있어 idempotent하게 작성되어 있습니다.

SQL 파일을 직접 `psql`에 넣기보다 애플리케이션 실행 경로를 통해 migration을 적용합니다.

```powershell
go run ./cmd
```

```bash
go run ./cmd
```

통합 테스트에서 migration 적용 경로를 검증하려면 `GOEXCHANGE_TEST_DATABASE_DSN`을 설정한 뒤 실행합니다.

```powershell
go test -run Integration -v ./internal/repository ./internal/service
```

```bash
go test -run Integration -v ./internal/repository ./internal/service
```

baseline migration은 goose를 통해 반복 실행해도 안전하게 작성되어 있습니다.
다만 기존 데이터가 constraint를 만족해야 적용됩니다.
예를 들어 `wallets(user_id, coin_symbol)` 중복 row, NULL 또는 blank `trades.idempotency_key`, 음수 balance, 0 이하 trade price/quantity, 0 이하 order amount가 있으면 migration이 실패할 수 있습니다.

baseline migration의 `Down`은 의도적으로 no-op입니다.
이미 `AutoMigrate`나 이전 수동 실행으로 생긴 column/constraint와 migration으로 생긴 항목을 안전하게 구분하기 어렵기 때문입니다.

## 통합 테스트 동작 방식

통합 테스트는 현재 model struct 기준으로 `AutoMigrate`를 실행한 뒤 `internal/dbmigration`을 통해 goose migration을 적용합니다.
테스트마다 생성한 user ID를 기준으로 `trades`, `orders`, `wallets`, `users` row를 cleanup합니다.

통합 테스트는 같은 테스트 DB를 공유하므로 `t.Parallel` 없이 실행하는 전제를 둡니다.
패키지 단위 병렬 실행을 강제로 적용하려면 cleanup 전략을 먼저 확장해야 합니다.
