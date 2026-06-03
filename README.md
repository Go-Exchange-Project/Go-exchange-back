# Go Exchange Backend

Go Exchange Backend는 업비트 스타일의 현물 거래소 MVP를 목표로 만든 Go 기반 백엔드입니다.
사용자 인증, 주문 접수, 인메모리 매칭엔진, 체결 정산, 지갑 hold/release, 수수료, 원장 기록, WebSocket 호가/체결 스트림을 하나의 모듈러 모놀리스 안에서 구현합니다.

이 프로젝트는 실제 입출금이나 운영용 보관 지갑을 제공하지 않습니다. 로컬 시연과 E2E 검증을 위해 개발 전용 지갑 충전 API를 제공합니다.

## 주요 기능

| 영역 | 상태 | 설명 |
| --- | --- | --- |
| 회원가입/로그인 | 완료 | JWT 기반 인증, 보호 API의 사용자 스코프 적용 |
| 지정가 주문 | 완료 | BUY/SELL, 가격 tick size, 최소 주문금액, 수량 step 검증 |
| 시장가 주문 | 완료 | 시장가 매수는 KRW 예산, 시장가 매도는 수량 기준으로 처리 |
| 매칭엔진 | 완료 | 심볼별 오더북, 가격 우선/시간 우선, 부분 체결, 다중 체결 |
| Self-trade 방지 | 완료 | 같은 사용자의 반대 주문은 매칭 대상에서 제외 |
| 지갑 hold/release | 완료 | 주문 접수 시 available -> locked, 취소/미체결 시장가 잔액 release |
| 체결 정산 | 완료 | DB 트랜잭션 안에서 trade/order/wallet/ledger 반영 |
| 수수료 | 완료 | 매수/매도 수수료를 KRW로 부과 |
| 원장 기록 | 완료 | DEV_FUND, ORDER_HOLD, ORDER_RELEASE, TRADE_SETTLEMENT 기록 |
| 멱등 정산 | 완료 | engine event id/idempotency key 기반 중복 정산 방지 |
| 실패 정산 기록 | 완료 | settlement 실패를 failed_settlements에 기록하고 operator resolve 가능 |
| 재시작 복구 | 완료 | 서버 시작 시 DB의 PENDING/PARTIAL 주문을 매칭엔진에 bootstrap |
| 실시간 스트림 | 완료 | WebSocket orderbook/trade broadcast, slow client drop, origin whitelist |
| 마이그레이션 | 완료 | goose 기반 SQL migration runner |
| 실제 입출금 | 미지원 | 사업/보안/보관 지갑 영역으로 MVP 범위 밖 |
| 운영 관측성 | 부분 | 기본 log 중심. structured logging, metrics, readiness는 남은 작업 |

자세한 완료 기준은 [docs/MVP_CHECKLIST.md](docs/MVP_CHECKLIST.md)를 참고하세요.

## 기술 스택

- Go 1.25.7
- Gin 1.12.0
- GORM 1.31.1
- PostgreSQL driver 1.6.0
- PostgreSQL 18.x local verification
- goose 3.27.1
- shopspring/decimal 1.4.0
- google/btree 1.1.3
- gammazero/deque 1.2.1
- Gorilla WebSocket 1.5.3
- golang.org/x/crypto 0.50.0
- testify 1.11.1

## 구조

```text
cmd/                      app bootstrap, routing, goroutine wiring
config/                   DB/env/runtime config
internal/auth/            password hashing, JWT
internal/dbmigration/     goose migration runner
internal/handler/         HTTP handlers
internal/httpapi/         structured response/error helpers
internal/matching/        in-memory matching engine and order book
internal/middleware/      auth/dev-tool middleware
internal/model/           GORM models
internal/repository/      DB query and transaction helpers
internal/service/         order, settlement, wallet, market policy services
internal/ws/              WebSocket hub/handler
migrations/               versioned SQL migrations
```

상세 설계는 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)를 참고하세요.

## 실행 방법

PostgreSQL이 실행 중이고 `goexchange` 데이터베이스가 있어야 합니다.

```powershell
createdb -h 127.0.0.1 -p 5432 -U postgres goexchange
```

로컬 설정 파일을 만듭니다.

```powershell
Copy-Item .env.example .env.local
```

예시:

```text
GOEXCHANGE_DB_PASSWORD=<local-postgres-password>
GOEXCHANGE_MARKET_RULES_PATH=config/market_rules.json
GOEXCHANGE_ENABLE_DEV_TOOLS=true
GOEXCHANGE_DEV_TOOLS_TOKEN=<local-dev-tools-token>
GOEXCHANGE_ENABLE_UPBIT=false
GOEXCHANGE_CORS_ALLOWED_ORIGINS=http://localhost:3000,http://127.0.0.1:3000
GOEXCHANGE_WS_ALLOWED_ORIGINS=http://localhost:3000,http://127.0.0.1:3000
```

서버 실행:

```powershell
go run ./cmd
```

서버 시작 시 GORM AutoMigrate로 기본 테이블을 맞춘 뒤 goose migration을 적용합니다.
정상 시작하면 `/ping`, `/auth/*`, `/orders`, `/wallets`, `/trades`, `/markets/rules`, `/ws`를 사용할 수 있습니다.

## API 요약

| Method | Path | 인증 | 설명 |
| --- | --- | --- | --- |
| GET | `/ping` | 없음 | 헬스 체크 |
| POST | `/auth/register` | 없음 | 사용자 생성, JWT 반환 |
| POST | `/auth/login` | 없음 | 로그인, JWT 반환 |
| GET | `/markets/rules?coin_symbol=BTC` | 없음 | 시장 정책 조회 |
| POST | `/orders` | 필요 | 지정가/시장가 주문 생성 |
| DELETE | `/orders/:id` | 필요 | 미체결/부분체결 주문 취소 |
| GET | `/orders` | 필요 | 내 주문 목록 |
| GET | `/orders/:id` | 필요 | 내 주문 단건 |
| GET | `/wallets` | 필요 | 내 지갑 목록 |
| GET | `/trades` | 필요 | 내 체결 목록 |
| POST | `/dev/wallets/fund` | 필요 + dev token | 로컬 개발용 지갑 충전 |
| GET | `/ws` | origin whitelist | orderbook/trade stream |

Decimal 값은 JSON string으로 주고받습니다.

## 테스트

기본 테스트:

```powershell
go test ./...
go vet ./...
go test -count=20 ./internal/matching
```

PostgreSQL 통합 테스트는 `GOEXCHANGE_TEST_DATABASE_DSN`이 있을 때만 실제 DB에 연결합니다.

```powershell
docker compose -f docker-compose.test.yml up -d
$env:GOEXCHANGE_TEST_DATABASE_DSN="host=localhost user=goexchange_test password=goexchange_test_password dbname=goexchange_test port=55432 sslmode=disable"
go test -run Integration -v ./internal/repository ./internal/service
docker compose -f docker-compose.test.yml down -v
```

더 자세한 테스트/환경 설명은 [TESTING.md](TESTING.md)를 참고하세요.

## 시연

로컬 시연은 프론트엔드와 함께 진행합니다.

1. 백엔드 `.env.local`에서 `GOEXCHANGE_ENABLE_DEV_TOOLS=true`와 `GOEXCHANGE_DEV_TOOLS_TOKEN`을 설정합니다.
2. 백엔드를 `go run ./cmd`로 실행합니다.
3. 프론트엔드를 `npm run dev`로 실행합니다.
4. 계정 A/B를 만들고, A는 BTC를 충전해 매도, B는 KRW를 충전해 매수합니다.
5. 체결 후 주문 상태, 지갑 available/locked, 수수료, 평균매수가, 체결 내역을 확인합니다.

상세 순서는 [docs/DEMO_SCENARIO.md](docs/DEMO_SCENARIO.md)를 참고하세요.

## 현재 한계

- 실제 원화/가상자산 입출금, 보관 지갑, KYC/AML, 관리자 백오피스는 없습니다.
- matching engine event sequence는 프로세스 내 메모리 sequence입니다. 영구 global event stream은 아직 없습니다.
- ledger는 wallet event ledger입니다. exchange-level double-entry accounting은 아직 아닙니다.
- failed settlement는 operator 확인/resolve 중심이며 자동 재정산 worker는 없습니다.
- 운영 관측성은 아직 기본 로그 중심입니다. metrics, tracing, readiness, graceful shutdown은 후속 작업입니다.
- 부하 테스트와 race test는 별도 환경 정리가 필요합니다.
