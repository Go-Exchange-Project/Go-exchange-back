# Backend Testing

This backend has two test modes:

- Unit tests run without external services.
- Postgres integration tests run only when `GOEXCHANGE_TEST_DATABASE_DSN` is set.

If `GOEXCHANGE_TEST_DATABASE_DSN` is empty, the repository and service integration tests are skipped intentionally.

## Runtime Configuration

Production and local runtime settings should be supplied through environment variables. Do not hard-code real database passwords in source code, tests, scripts, or committed docs.

For local development, the backend also loads `.env.local` and then `.env` from the current working directory before connecting to the database. Existing process environment variables still take precedence over values in those files. Keep `.env.local` uncommitted; use `.env.example` as the committed template.

Database configuration priority:

1. `GOEXCHANGE_DATABASE_DSN` is used as-is when set. It may be a PostgreSQL keyword DSN or another format accepted by the GORM PostgreSQL driver.
2. If `GOEXCHANGE_DATABASE_DSN` is empty, the app builds a DSN from individual variables:

| Variable | Default | Description |
| --- | --- | --- |
| `GOEXCHANGE_DB_HOST` | `localhost` | PostgreSQL host. |
| `GOEXCHANGE_DB_USER` | `postgres` | PostgreSQL user. |
| `GOEXCHANGE_DB_PASSWORD` | empty | PostgreSQL password. Leave empty only for local setups that allow it. |
| `GOEXCHANGE_DB_NAME` | `goexchange` | PostgreSQL database name. |
| `GOEXCHANGE_DB_PORT` | `5432` | PostgreSQL port. |
| `GOEXCHANGE_DB_SSLMODE` | `disable` | PostgreSQL SSL mode. Use `require` or stronger settings where appropriate outside local development. |
| `GOEXCHANGE_DB_CONNECT_TIMEOUT` | `5` | PostgreSQL connection timeout in seconds. |
| `GOEXCHANGE_CORS_ALLOWED_ORIGINS` | `http://localhost:3000,http://127.0.0.1:3000` | Comma-separated browser origins allowed to call the HTTP API. Add your LAN Vite URL, for example `http://192.168.219.100:3000`, when opening the frontend through a network address. |

Local development example:

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

Authentication configuration:

| Variable | Default | Description |
| --- | --- | --- |
| `GOEXCHANGE_JWT_SECRET` | `dev-only-change-me` | HMAC secret used to sign access tokens. Set a strong secret outside local development. |

Development-only tools:

| Variable | Default | Description |
| --- | --- | --- |
| `GOEXCHANGE_ENABLE_DEV_TOOLS` | `false` | Enables authenticated development helper routes such as `POST /dev/wallets/fund`. Never enable this in production. |
| `GOEXCHANGE_DEV_TOOLS_TOKEN` | empty | Required header token for enabled development helper routes. Requests must send `X-GoExchange-Dev-Token: <token>`. |
| `GOEXCHANGE_ENABLE_UPBIT` | `true` | Enables the Upbit WebSocket ticker feed. Set to `false` for local API/order testing when external network startup is slow or unavailable. |

Auth endpoints:

- `POST /auth/register` with `name`, `email`, and `password` creates a user and returns a bearer token.
- `POST /auth/login` with `email` and `password` returns a bearer token.
- `POST /orders` and `DELETE /orders/:id` require `Authorization: Bearer <token>` and use the authenticated user ID instead of a hard-coded user.
- `GET /orders`, `GET /orders/:id`, `GET /wallets`, and `GET /trades` also require `Authorization: Bearer <token>` and only return data scoped to the authenticated user.

When `GOEXCHANGE_ENABLE_DEV_TOOLS=true`, local development can fund the authenticated user's test wallet:

```http
POST /dev/wallets/fund
Authorization: Bearer <token>
X-GoExchange-Dev-Token: <local-dev-tools-token>
Content-Type: application/json

{"coin_symbol":"KRW","amount":"1000000"}
```

This endpoint creates or increments only the caller's wallet balance and exists only to make local end-to-end order testing possible without building an admin back office. It is disabled by default, requires a separate development token when enabled, and is not an operational deposit or accounting path.

Development wallet funding also writes a `DEV_FUND` ledger entry so local balance changes remain auditable during tests.

Error responses use a structured shape:

```json
{"error":{"code":"VALIDATION_ERROR","message":"invalid price"}}
```

Read API query parameters:

- `GET /orders?status=PENDING&coin_symbol=BTC&limit=50`
- `GET /trades?coin_symbol=BTC&limit=50`

Decimal values in read responses are returned as JSON strings so clients do not lose precision by parsing them as floating point numbers.

## Wallet Ledger Entries

`ledger_entries` records wallet balance changes in the same database transaction that changes the wallet. It is a wallet event ledger, not a full double-entry accounting system yet.

Each row records:

- `user_id` and `coin_symbol`.
- `entry_type`: `DEV_FUND`, `ORDER_HOLD`, `ORDER_RELEASE`, or `TRADE_SETTLEMENT`.
- `available_delta` and `locked_delta`.
- `available_balance_after` and `locked_balance_after`.
- `reference_type`, `reference_id`, and optional `reference_key`.

Current write points:

- `POST /dev/wallets/fund`: credits available balance and writes `DEV_FUND`.
- `POST /orders`: moves available to locked and writes `ORDER_HOLD`.
- `DELETE /orders/:id`: releases remaining locked balance and writes `ORDER_RELEASE`.
- successful settlement: writes four `TRADE_SETTLEMENT` rows, one for buyer KRW, buyer coin, seller coin, and seller KRW.

Settlement duplicates do not write extra ledger rows because `SettleTrade` returns before wallet mutation when it sees an existing idempotent trade. Failed settlements also do not write ledger rows because the wallet transaction rolls back.

Known limits:

- There is no public ledger API yet.
- Fees are not modeled.
- The ledger is per wallet balance change; a later accounting phase should introduce exchange-level double-entry accounts and stronger reconciliation reports.

WebSocket origin configuration:

| Variable | Default | Description |
| --- | --- | --- |
| `GOEXCHANGE_WS_ALLOWED_ORIGINS` | `http://localhost:3000,http://localhost:5173` | Comma-separated list of allowed browser WebSocket origins. |
| `WS_ALLOWED_ORIGINS` | empty | Backward-compatible alias used only when `GOEXCHANGE_WS_ALLOWED_ORIGINS` is unset. |
| `GOEXCHANGE_WS_ALLOW_MISSING_ORIGIN` | `false` | Set to `true` only for explicit non-browser clients or tests that cannot send an `Origin` header. |

Browsers normally send an `Origin` header for WebSocket handshakes. Requests without `Origin` are rejected by default.

## Failed Settlement Records

Settlement failures are persisted in `failed_settlements` so an operator can inspect trade events that the matching engine emitted but settlement rejected. This includes expected guard failures such as a stale trade arriving after an order was cancelled.

This table is a minimal durable record, not an automatic retry system. Successful settlements still write to `trades`; failed settlements stay separate so rejected events do not look like settled trades.

Key behavior:

- `trade_idempotency_key` is required and unique. If the same failed trade is recorded again, the existing row is updated instead of inserting duplicate rows.
- `retry_count` starts at `1` and increments on repeated records for the same `trade_idempotency_key`.
- `error_message` stores the current internal settlement error string, truncated by the application. Avoid adding secrets or user credentials to errors.
- `status` starts as `OPEN`. `RESOLVED` is reserved for a later operator/reconciliation workflow.

This can later grow into a retry worker or reconciliation process, but Phase 2 only guarantees that failed settlement events are not lost after a log line.

## Failed Settlement Reconciliation

`failed_settlements` is an operator-facing outbox/incident log, not an automatic retry queue. Marking a row `RESOLVED` only records that an operator investigated or handled the incident; it must not mutate wallets, orders, or trades.

Status policy:

- `OPEN`: the failure still needs investigation or a manual decision.
- `RESOLVED`: an operator has completed review, taken any external action if needed, or decided that no action is required.

Resolution audit fields:

- `resolution`: required when resolving; short reason such as `stale engine event reviewed`.
- `resolved_by`: optional until auth/JWT exists; use an operator or system label when available.
- `resolved_at`: set by the repository when the status changes to `RESOLVED`.
- `notes`: optional context for follow-up.

Repeated failure records for a previously resolved `trade_idempotency_key` reopen the row by setting status back to `OPEN`, incrementing `retry_count`, clearing resolution fields, and storing the latest error. Resolving an already `RESOLVED` row is treated as an idempotent no-op.

Simple triage categories are currently string-based:

- `CANCELLED_ORDER`: error contains `CANCELLED`; often a stale engine event after cancellation.
- `IDEMPOTENCY_CONFLICT`: error contains `idempotency key conflict`.
- `INSUFFICIENT_LOCKED_BALANCE`: error contains `insufficient locked`.
- `UNKNOWN`: fallback.

These categories are for operator sorting only. They are not retry decisions. A future phase should replace string matching with structured settlement error codes.

Useful local SQL:

```sql
SELECT *
FROM failed_settlements
WHERE status = 'OPEN'
ORDER BY occurred_at ASC, id ASC
LIMIT 50;
```

## Matching Bootstrap Recovery

The matching engine is in-memory, so server startup now restores open DB orders into the order books before the HTTP API is opened.

Startup behavior:

- The engine starts first, then settlement and snapshot consumers start.
- The bootstrap step loads orders with status `PENDING` or `PARTIAL` and `amount > filled_amount`.
- Orders are submitted to the engine in `created_at ASC, id ASC` order to preserve time priority as closely as the DB allows.
- `PARTIAL` orders are submitted with `amount - filled_amount` as the in-memory remaining quantity.
- `FILLED`, `CANCELLED`, and zero-remaining rows are not restored.
- If bootstrap fails, the server exits instead of accepting new API traffic with DB open orders missing from memory.

Bootstrap can naturally generate trades when restored open orders cross. The settlement consumer is already running before bootstrap begins, so those trades can flow through normal settlement. Ledger reconciliation and engine-level event sequencing remain separate future work.

## Test Postgres

The test database is defined in `docker-compose.test.yml`.

- Service: `postgres-test`
- Container: `goexchange-postgres-test`
- Database: `goexchange_test`
- User: `goexchange_test`
- Password: `goexchange_test_password`
- Host port: `55432`

This database is separate from the development database used by `config/database.go`.

Start the test database:

```powershell
docker compose -f docker-compose.test.yml up -d
```

```bash
docker compose -f docker-compose.test.yml up -d
```

Set the integration test DSN:

```powershell
$env:GOEXCHANGE_TEST_DATABASE_DSN="host=localhost user=goexchange_test password=goexchange_test_password dbname=goexchange_test port=55432 sslmode=disable"
```

```bash
export GOEXCHANGE_TEST_DATABASE_DSN="host=localhost user=goexchange_test password=goexchange_test_password dbname=goexchange_test port=55432 sslmode=disable"
```

Run the integration tests:

```powershell
go test -run Integration -v ./internal/repository ./internal/service
```

```bash
go test -run Integration -v ./internal/repository ./internal/service
```

The integration test setup runs `AutoMigrate` first, then applies `migrations/001_constraints.sql` so the tests exercise the explicit DB constraints as well as the model structs.

Run all backend tests:

```powershell
go test ./...
```

```bash
go test ./...
```

Stop and delete the test database volume:

```powershell
docker compose -f docker-compose.test.yml down -v
```

```bash
docker compose -f docker-compose.test.yml down -v
```

## GitHub Actions CI

The backend repository has a GitHub Actions workflow at `.github/workflows/backend-ci.yml`.

The workflow has two jobs:

- `Unit tests and vet` runs `go mod download`, `go test ./...`, `go vet ./...`, and `go test -count=20 ./internal/matching` without `GOEXCHANGE_TEST_DATABASE_DSN`. Integration tests are skipped in this job by design.
- `Postgres integration tests` starts a `postgres:16-alpine` service and sets `GOEXCHANGE_TEST_DATABASE_DSN`, then runs `go test -run Integration -v ./internal/repository ./internal/service`.

Local Docker Compose exposes Postgres on host port `55432` to avoid colliding with a developer's local Postgres. GitHub Actions service containers are reached from the job on `localhost:5432`, so CI uses this DSN:

```text
host=localhost user=goexchange_test password=goexchange_test_password dbname=goexchange_test port=5432 sslmode=disable
```

Because the integration job always sets `GOEXCHANGE_TEST_DATABASE_DSN`, repository and service integration tests connect to Postgres instead of taking the skip path.

## Constraint Migration

`AutoMigrate` is still kept for development convenience. The SQL file in `migrations/001_constraints.sql` is the first explicit schema contract for constraints and indexes. Keep applying it after `AutoMigrate` until a migration runner is introduced.

Apply the constraint SQL manually with `psql`:

```powershell
psql "$env:GOEXCHANGE_TEST_DATABASE_DSN" -f migrations/001_constraints.sql
```

```bash
psql "$GOEXCHANGE_TEST_DATABASE_DSN" -f migrations/001_constraints.sql
```

The migration is safe to run repeatedly, but existing data must already satisfy the constraints. It will fail if, for example, there are duplicate `wallets(user_id, coin_symbol)` rows, NULL or blank `trades.idempotency_key` values, negative balances, non-positive trade price/quantity values, or non-positive order amounts.

## Integration Test Behavior

The integration tests call `AutoMigrate` for the current model structs before running, then apply `migrations/001_constraints.sql`. They do not introduce a full migration framework.

Each integration test uses generated user IDs and removes rows for those user IDs from `trades`, `orders`, `wallets`, and `users` during cleanup. The tests are intended to be run without `t.Parallel`; avoid forcing package-level parallelism against the same test database until the cleanup strategy is expanded.
