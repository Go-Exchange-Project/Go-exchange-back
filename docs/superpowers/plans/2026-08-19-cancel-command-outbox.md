# 취소 command outbox 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 취소 의도를 먼저 PostgreSQL에 내구 기록하고, 매칭엔진 제거 이벤트와 command 완료를 하나의 outbox 트랜잭션으로 묶어 크래시 뒤에도 취소가 부활하지 않게 한다.

**Architecture:** `DELETE /orders/:id`는 주문을 잠근 트랜잭션에서 `cancel_commands`를 생성하거나 기존 command를 반환한 뒤 `202 Accepted`로 끝난다. 별도 `CancelCommandWorker`가 PENDING command를 매칭엔진에 전달하고, `OutboxWriter`가 `ORDER_CANCELLED` execution outbox 행과 command `PROCESSED`를 원자 커밋한다. worker는 `dispatching → awaiting_outbox`를 메모리에 유지하며 DB에서 `PROCESSED`/`NOOP`가 확인되기 전에는 같은 command를 재투입하지 않는다.

**Tech Stack:** Go 1.25.7, Gin 1.12, GORM 1.31, PostgreSQL, goose 3.27, Prometheus client_golang, React 18, TypeScript 5.8, Vitest, Playwright, k6

**Spec:** [`docs/superpowers/specs/2026-08-18-cancel-command-outbox-design.md`](../specs/2026-08-18-cancel-command-outbox-design.md)

## Global Constraints

- 응답 순서는 **command DB commit → non-blocking wake-up → 202**다. commit 전에는 성공 응답을 보내지 않는다.
- `cancel_commands.order_id`는 전체 UNIQUE다. command가 `PROCESSED`이고 정산 전인 창에도 두 번째 command를 만들지 않는다.
- command에는 `coin_symbol`, `side`, `price`를 모두 저장한다. 특히 `price NUMERIC NOT NULL`이 없으면 엔진 명령을 복원할 수 없다.
- execution outbox INSERT와 command `PROCESSED` UPDATE는 **한 GORM `Transaction()`·한 commit**이다. `commandIDs`는 nonzero dedup, UPDATE는 `status='PENDING'`, 행 수 불일치는 outbox INSERT까지 rollback한다.
- `OutboxWriter`는 직렬화에 성공한 취소 이벤트의 `CommandID`만 command UPDATE 대상으로 삼는다. 직렬화하지 못한 이벤트가 command만 완료시키면 안 된다.
- worker 상태는 `dispatching`, `awaiting_outbox`, `backoff` 세 단계다. 엔진 성공 반환은 완료가 아니며 `awaiting_outbox`는 DB 상태 확인 전 삭제하지 않는다.
- `awaiting_outbox` deadline은 로그·metric 전용이다. 만료가 phase 전환이나 재투입을 만들면 안 된다.
- not-found는 DB 주문 상태로 판정한다. `PENDING`/`PARTIAL`이면 backoff, `FILLED`/`CANCELLED`이면 `NOOP`이다.
- wake-up이 합쳐지거나 유실돼도 기본 polling 간격 50ms로 다음 **조회 시도**를 시작한다. 실제 dispatch 시작 시각의 상한은 약속하지 않는다.
- 부팅 순서는 execution outbox replay → matching bootstrap → cancel worker 시작·복구 command drain → HTTP listen이다. drain 실패 시 서버를 열지 않는다.
- 종료 순서는 HTTP drain → hold coordinator → cancel worker 정지 → matching engine drain → outbox flush → settlement drain이다.
- API·프런트·E2E·k6의 202 계약을 같은 변경 집합에서 완성한다. UI polling timeout은 실패가 아니라 “접수됨 · 처리 중”이다.
- B 이후 성능 수치를 34번과 PASS/FAIL로 직접 비교하지 않는다. 500 VU 10분 1회에서 취소 acceptance 100%, 인프라 실패 0건만 게이트로 쓰고 HTTP p95·`cancel_command_latency_seconds`는 새 기준선으로 기록한다.
- 백엔드와 프런트는 별도 Git 저장소다. 각 저장소에서 관련 파일만 stage하고 커밋 전 `commit-message` 스킬을 사용한다. 기존 `_workspace/` 산출물과 프런트의 선행 커밋을 되돌리거나 섞지 않는다.
- 각 코드 작업은 RED → GREEN 순서다. 백엔드 단위 게이트는 `go test ./... -race`, 통합 게이트는 DSN을 설정한 `go test -run Integration -p 1`, 프런트 게이트는 `npm test && npm run lint && npm run build`다.

---

## File Structure

| 파일 | 책임 | 태스크 |
|---|---|---|
| `migrations/007_cancel_commands.sql` | command 테이블·UNIQUE·CHECK·PENDING 인덱스 | 1 |
| `internal/model/cancel_command.go` | command 상태와 GORM 모델 | 1 |
| `internal/repository/cancel_command_repository.go` | 생성/중복 조회, PENDING scan, 상태 조회, NOOP·attempt 갱신 | 1 |
| `internal/dbmigration/cancel_command_integration_test.go` | migration 007 카탈로그 계약 | 1 |
| `internal/repository/cancel_command_repository_integration_test.go` | repository SQL 계약 | 1 |
| — | `CancelCommand`는 **AutoMigrate에 등록하지 않는다**(아래 주의) | 1, 6 |
| `internal/matching/engine.go`, `internal/matching/engine_test.go` | `CommandID`를 취소 이벤트까지 보존 | 2 |
| `internal/repository/trade_outbox_repository.go` | outbox INSERT + command PROCESSED 원자 커밋 | 3 |
| `internal/service/outbox_writer.go` | 배치에서 성공적으로 직렬화된 command ID 수집 | 3 |
| `internal/repository/trade_outbox_repository_integration_test.go` | 정상·dedup·행 불일치 rollback | 3 |
| `internal/service/outbox_writer_test.go` | command ID 전달·기존 배치 순서 회귀 | 3 |
| `internal/service/order_service.go` | 엔진 직접 호출 제거, command create-or-get + wake | 4 |
| `internal/handler/order_handler.go` | 202 `ACCEPTED` 응답 | 4 |
| `internal/service/service_integration_test.go` | API 서비스의 중복·권한·terminal 계약 | 4 |
| `internal/handler/order_handler_integration_test.go` | 실제 HTTP status/body 계약 | 4 |
| `internal/service/cancel_command_worker.go` | polling, in-flight, backoff, DB 상태 분기 | 5 |
| `internal/service/cancel_command_worker_test.go` | 검증 8/8b/8c/8d/8e | 5 |
| `internal/metrics/metrics.go`, `internal/metrics/metrics_test.go` | command latency·awaiting 경고 지표 | 5 |
| `cmd/cancel_command_lifecycle.go`, `cmd/cancel_command_lifecycle_test.go` | 부팅 장벽과 worker 종료 순서 | 6 |
| `cmd/main.go` | repository/worker/wake/startup/shutdown 실제 배선 | 6 |
| `internal/service/cancel_command_outbox_integration_test.go` | 크래시 창·부활 방지·원장 1회 통합 검증 | 7 |
| `src/lib/api.ts`, `src/lib/api.test.ts` (front) | 202 타입·단건 주문 조회 | 8 |
| `src/lib/cancelPolling.ts`, `src/lib/cancelPolling.test.ts` (front) | terminal polling과 abort/timeout | 8 |
| `src/components/trading/AuthPanel.tsx`, `AuthPanel.test.tsx` (front) | 접수/완료/체결/UI lifecycle | 8 |
| `tests/e2e/exchange.spec.ts` (front) | 즉시 단언을 eventual assertion으로 전환 | 9 |
| `_workspace/loadtest/sli-classify.js`, selftest, `order-spike-availability.js` | 200·202 취소 SLI | 9 |
| `loadtest/order-spike-single-symbol.js` | 공개 spike 하니스의 202 허용 | 9 |
| `docs/benchmarks/35-2026-08-19-cancel-command-outbox.md` | 새 계약의 게이트·기준선·비교 조건 | 10 |
| `README.md`, `docs/refactor/README.md`, `docs/ENGINEERING-SUMMARY.md` | API와 프로젝트 상태 갱신 | 10 |

---

### Task 1: `cancel_commands` 스키마와 repository

**Files:**
- Create: `migrations/007_cancel_commands.sql`
- Create: `internal/model/cancel_command.go`
- Create: `internal/dbmigration/cancel_command_integration_test.go`
- Create: `internal/repository/cancel_command_repository.go`
- Create: `internal/repository/cancel_command_repository_integration_test.go`
- Modify: `internal/dbmigration/runner_test.go`
- Modify: `internal/testdb/integration.go`

**Interfaces:**
- Produces:
  - `model.CancelCommandStatusPending|Processed|Noop`
  - `model.CancelCommand`
  - `repository.NewCancelCommandRepository(db)`
  - `(*CancelCommandRepository).WithTx(tx)`
  - `CreateOrGet(command) (persisted *model.CancelCommand, created bool, err error)`
  - `FindPending(limit int) ([]model.CancelCommand, error)`
  - `FindStatuses(ids []uint64) ([]model.CancelCommand, error)`
  - `MarkNoop(id uint64) (*model.CancelCommand, error)`
  - `RecordAttempt(id uint64, message string) error`
  - `CountPending() (int64, error)`

- [ ] **Step 1: migration 정적·카탈로그 RED 테스트 작성**

`runner_test.go`는 007 파일에 `price NUMERIC NOT NULL`, `UNIQUE (order_id)`, 세 상태 CHECK, `WHERE status = 'PENDING'`이 모두 있는지 단언한다. `cancel_command_integration_test.go`는 `pg_constraint`와 `pg_indexes`를 조회해 UNIQUE가 부분 인덱스가 아니고 PENDING 인덱스만 부분인지 확인한다.

Run:

```powershell
go test ./internal/dbmigration -run CancelCommand -v
```

Expected: FAIL — migration 007 파일이 없다.

- [ ] **Step 2: 모델과 migration 007 작성**

`internal/model/cancel_command.go`의 공개 계약:

```go
type CancelCommandStatus string

const (
	CancelCommandStatusPending   CancelCommandStatus = "PENDING"
	CancelCommandStatusProcessed CancelCommandStatus = "PROCESSED"
	CancelCommandStatusNoop      CancelCommandStatus = "NOOP"
)

type CancelCommand struct {
	ID           uint64              `gorm:"primaryKey"`
	OrderID      uint                `gorm:"not null"`
	UserID       uint                `gorm:"not null"`
	CoinSymbol   string              `gorm:"not null"`
	Side         OrderSide           `gorm:"not null"`
	Price        decimal.Decimal     `gorm:"type:numeric;not null"`
	Status       CancelCommandStatus `gorm:"not null;default:PENDING"`
	AttemptCount int                 `gorm:"not null;default:0"`
	LastError    string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}
```

**`cancel_commands` 스키마는 007이 단독으로 소유한다.** `CREATE TABLE IF NOT EXISTS` 뒤에 `ALTER COLUMN`과 이름이 고정된 두 constraint를 보강하는 형태는 재실행·부분 적용 상태에 대한 **방어**이지 AutoMigrate 선행을 전제한 것이 아니다.

> **⚠ `&model.CancelCommand{}`를 AutoMigrate 목록에 넣지 않는다.** 넣으면 GORM이 DB 컬럼의 unique 여부와 모델 태그를 비교해 007이 만든 `cancel_commands_order_unique`를 **자기 명명규칙 이름**(`uni_cancel_commands_order_id`)으로 DROP하려 하고, 그 이름은 없으므로 SQLSTATE 42704로 실패한다. `cmd/main.go`는 AutoMigrate → goose 순서이므로 **두 번째 부팅부터 매번** 죽는다. 모델에 `uniqueIndex:cancel_commands_order_unique`를 달아도 GORM은 UNIQUE 인덱스를, 007은 UNIQUE 제약을 만들어 이름이 충돌한다. `gorm:"unique"`는 같은 컬럼에 제약을 두 개 만든다.

```sql
CONSTRAINT cancel_commands_order_unique UNIQUE (order_id)
CONSTRAINT cancel_commands_status_check
  CHECK (status IN ('PENDING','PROCESSED','NOOP'))

CREATE INDEX IF NOT EXISTS cancel_commands_pending
  ON cancel_commands (id) WHERE status = 'PENDING';
```

Down은 data-bearing command를 자동 삭제하지 않도록 `SELECT 1` no-op으로 둔다. rollback이
필요하면 먼저 PENDING 0과 백업을 확인한 별도 운영 절차에서 처리한다.

- [ ] **Step 3: migration GREEN 확인**

```powershell
$env:GOEXCHANGE_TEST_DATABASE_DSN='host=localhost user=goexchange_test password=goexchange_test_password dbname=goexchange_test port=55432 sslmode=disable'
go test -run CancelCommand -v -p 1 ./internal/dbmigration
```

Expected: version 7, 컬럼/UNIQUE/CHECK/부분 인덱스 모두 PASS.

- [ ] **Step 4: repository RED 테스트 작성**

통합 테스트는 다음을 각각 검증한다.

1. 같은 `order_id` 두 번 `CreateOrGet` → 행 1개, 같은 ID, `created=true` 후 `false`.
2. `FindPending` → ID 오름차순·limit 적용, PROCESSED/NOOP 제외.
3. `FindStatuses` → 요청 ID만 반환하고 status·created_at·updated_at 보존.
4. `MarkNoop` → PENDING만 NOOP, 없는 ID와 이미 terminal인 ID는 조용히 덮어쓰지 않음.
5. `RecordAttempt` → attempt_count 원자 증가, last_error 갱신, status는 PENDING 유지.

Run:

```powershell
go test ./internal/repository -run IntegrationCancelCommand -v
```

Expected: FAIL — repository가 없다.

- [ ] **Step 5: repository 최소 구현**

`CreateOrGet`는 Postgres transaction을 abort시키는 unique violation을 일부러 발생시키지 않는다. 주문 lock과 별개로 DB 방어가 되도록 `ON CONFLICT DO NOTHING` 후 기존 행을 조회한다.

```go
result := r.DB.Clauses(clause.OnConflict{
	Columns:   []clause.Column{{Name: "order_id"}},
	DoNothing: true,
}).Create(command)
if result.Error != nil {
	return nil, false, result.Error
}
if result.RowsAffected == 1 {
	return command, true, nil
}
var existing model.CancelCommand
if err := r.DB.Where("order_id = ?", command.OrderID).First(&existing).Error; err != nil {
	return nil, false, err
}
return &existing, false, nil
```

`MarkNoop`와 `RecordAttempt`는 `WHERE id=? AND status='PENDING'`을 사용하고 `updated_at`을 명시적으로 갱신한다. `FindStatuses(nil)`은 빈 slice와 nil error, `CountPending`은 정확한 DB 건수를 반환한다.

- [ ] **Step 6: migration 멱등성과 전체 repository 검증**

`internal/testdb/integration.go`는 **바꾸지 않는다** — 테이블은 AutoMigrate 뒤에 실행되는 `dbmigration.Up`이 만든다. 007이 두 번 실행돼도 no-op인지 확인하기 위해 두 번 연속 실행한다.

```powershell
go test -run Integration -v -p 1 ./internal/dbmigration ./internal/repository
go test ./internal/model ./internal/repository -race
```

Expected: 모두 PASS, 두 번째 migration 실행은 no-op.

- [ ] **Step 7: Task 1 커밋**

```powershell
git add migrations/007_cancel_commands.sql internal/model/cancel_command.go internal/dbmigration/runner_test.go internal/dbmigration/cancel_command_integration_test.go internal/repository/cancel_command_repository.go internal/repository/cancel_command_repository_integration_test.go internal/testdb/integration.go
git commit -F _workspace/commit-draft.md
```

권장 subject: `feat(cancel): 취소 command 영속 저장소 추가`

---

### Task 2: 매칭엔진에 `CommandID` 전파

**Files:**
- Modify: `internal/matching/engine.go`
- Modify: `internal/matching/engine_test.go`
- Modify: `internal/matching/sharded_test.go`

**Interfaces:**
- Consumes: Task 1의 `uint64` command ID
- Produces:
  - `matching.CancelOrderCommand.CommandID uint64`
  - `matching.OrderCancelled.CommandID uint64`

- [ ] **Step 1: RED 테스트 작성**

기존 `TestCancelOrder_EmitsOrderCancelledEvent`의 command에 `CommandID: 77`을 넣고 다음 단언을 추가한다.

```go
assert.Equal(t, uint64(77), event.OrderCancelled.CommandID)
```

ShardedEngine 테스트도 ID 88이 소유 shard의 이벤트까지 보존되는지 확인한다.

Run: `go test ./internal/matching -run 'CancelOrder.*CommandID|EmitsOrderCancelled' -v`

Expected: FAIL — 두 구조체에 필드가 없다.

- [ ] **Step 2: 필드와 복사 구현**

```go
type CancelOrderCommand struct {
	CommandID  uint64
	CoinSymbol string
	OrderID    uint
	Side       model.OrderSide
	Price      decimal.Decimal
	ResponseCh chan CancelOrderResult
}

type OrderCancelled struct {
	CommandID    uint64
	OrderID      uint
	CoinSymbol   string
	Side         model.OrderSide
	EngineEventID string
}
```

`emitOrderCancelled`에서 `CommandID: cmd.CommandID`를 복사한다. `processCancel`의 응답→emit 순서는 이번 태스크에서 바꾸지 않는다. worker가 이 순서를 전제로 `awaiting_outbox`를 유지한다.

- [ ] **Step 3: matching 회귀 검증**

```powershell
go test ./internal/matching -race
go test -count=20 ./internal/matching
```

Expected: 기존 순서·취소·sharding 테스트와 신규 ID 테스트 모두 PASS.

- [ ] **Step 4: Task 2 커밋**

권장 subject: `feat(matching): 취소 이벤트에 command 식별자 전파`

---

### Task 3: execution outbox와 command 완료 원자 커밋

**Files:**
- Modify: `internal/repository/trade_outbox_repository.go`
- Modify: `internal/repository/trade_outbox_repository_integration_test.go`
- Modify: `internal/service/outbox_writer.go`
- Modify: `internal/service/outbox_writer_test.go`

**Interfaces:**
- Consumes: Task 1의 `cancel_commands`, Task 2의 `OrderCancelled.CommandID`
- Produces:
  - `InsertBatchAndMarkCancelCommands(events []*model.TradeOutboxEvent, commandIDs []uint64) error`
  - 기존 `InsertBatch(events)`는 nil ID wrapper로 유지
  - `outboxBatchInserter.InsertBatchAndMarkCancelCommands(events []*model.TradeOutboxEvent, commandIDs []uint64) error`

- [ ] **Step 1: 원자성 RED 통합 테스트 작성**

다음 네 테스트를 추가한다.

- 정상 mixed batch: trade + cancel을 INSERT하고 cancel command만 PROCESSED.
- ID `[7,7,0]`: 7 하나로 dedup되어 RowsAffected 1.
- ID `[valid, missing]`: RowsAffected 1/2 불일치 error, outbox 행 0, valid command PENDING.
- command ID 없음: UPDATE 없이 기존 outbox INSERT 동작 유지.

Run: `go test ./internal/repository -run IntegrationTradeOutbox.*CancelCommand -v`

Expected: FAIL — 새 메서드가 없다.

- [ ] **Step 2: repository 트랜잭션 구현**

핵심은 두 SQL을 한 transaction에 넣고 행 수를 exact match하는 것이다.

```go
func (r *TradeOutboxRepository) InsertBatchAndMarkCancelCommands(events []*model.TradeOutboxEvent, commandIDs []uint64) error {
	ids := dedupeNonzeroUint64(commandIDs)
	return r.DB.Transaction(func(tx *gorm.DB) error {
		if len(events) > 0 {
			if err := tx.Create(&events).Error; err != nil {
				return err
			}
		}
		if len(ids) == 0 {
			return nil
		}
		now := time.Now().UTC()
		result := tx.Model(&model.CancelCommand{}).
			Where("id IN ? AND status = ?", ids, model.CancelCommandStatusPending).
			Updates(map[string]interface{}{
				"status": model.CancelCommandStatusProcessed,
				"updated_at": now,
			})
		if result.Error != nil {
			return result.Error
		}
		if int(result.RowsAffected) != len(ids) {
			return fmt.Errorf("mark cancel commands processed affected %d rows, expected %d", result.RowsAffected, len(ids))
		}
		return nil
	})
}
```

`InsertBatch`는 `return r.InsertBatchAndMarkCancelCommands(events, nil)`이다.

- [ ] **Step 3: OutboxWriter RED 테스트 작성**

fake repository가 받은 command ID slice를 기록하도록 바꾸고 다음을 검증한다.

1. 취소 이벤트 2개와 trade 1개 → nonzero ID만 전달.
2. 같은 ID 취소 이벤트 2개 → repository가 dedup 계약을 지키며 1회 mark.
3. 빈/직렬화 실패 이벤트 → 그 이벤트의 ID가 전달되지 않음.
4. repository error 2회 후 성공 → 같은 batch/ID로 재시도하고 Forward는 1회.

Run: `go test ./internal/service -run OutboxWriter -v`

Expected: FAIL — fake와 interface의 새 메서드가 없다.

- [ ] **Step 4: OutboxWriter 구현**

`NewTradeOutboxEvent(event)`가 성공한 직후에만 `event.OrderCancelled.CommandID`를 수집한다. 배치 원본에서 먼저 모으면 직렬화 실패 command만 PROCESSED가 되는 유실 경로가 생긴다.

```go
row, err := NewTradeOutboxEvent(event)
if err != nil {
	w.logf("outbox: drop unencodable execution event: %v", err)
	continue
}
rows = append(rows, row)
forwarded = append(forwarded, event)
if event.OrderCancelled != nil && event.OrderCancelled.CommandID != 0 {
	commandIDs = append(commandIDs, event.OrderCancelled.CommandID)
}
```

flush retry loop은 `InsertBatchAndMarkCancelCommands(rows, commandIDs)`를 성공할 때까지 기존처럼 무한 재시도한다. 성공한 뒤에만 settlement로 Forward한다.

- [ ] **Step 5: 원자성·기존 경로 GREEN 확인**

```powershell
go test ./internal/repository ./internal/service -run 'TradeOutbox|OutboxWriter' -race -v
$env:GOEXCHANGE_TEST_DATABASE_DSN='host=localhost user=goexchange_test password=goexchange_test_password dbname=goexchange_test port=55432 sslmode=disable'
go test -run IntegrationTradeOutbox -v -p 1 ./internal/repository
```

Expected: 행 불일치에서 mixed batch 전체 rollback, 기존 trade/market/cancel 직렬화·순서 PASS.

- [ ] **Step 6: Task 3 커밋**

권장 subject: `feat(outbox): 취소 command 완료를 execution 배치에 원자 흡수`

---

### Task 4: 취소 API를 내구 접수 `202`로 전환

**Files:**
- Modify: `internal/service/order_service.go`
- Modify: `internal/service/order_service_test.go`
- Modify: `internal/service/service_integration_test.go`
- Modify: `internal/handler/order_handler.go`
- Create: `internal/handler/order_handler_integration_test.go`

**Interfaces:**
- Consumes: Task 1의 `CreateOrGet`
- Produces:
  - `OrderService.CancelCommandRepository *repository.CancelCommandRepository`
  - `OrderService.CancelCommandWake func()`
  - `CancelOrderResult{OrderID uint; CommandID uint64; Status string}`
  - HTTP `202` body `{message, order_id, command_id, status:"ACCEPTED"}`

- [ ] **Step 1: 서비스 RED 테스트를 새 계약으로 변경**

기존 “호출 즉시 engine removed/released estimate” 단언을 제거하고 다음 계약을 테스트한다.

- PENDING/ PARTIAL 지정가 주문 → command PENDING, 주문·wallet 변화 없음.
- callback 안에서 command를 조회할 수 있음 → wake는 commit 후 실행.
- 같은 주문 동시 100회 → 모두 같은 command ID, DB 행 1개.
- command PROCESSED·주문 open → 같은 ID로 다시 202 의미의 성공.
- 주문 FILLED/CANCELLED → 409 conflict, 신규 command 없음.
- 다른 사용자·시장가 주문 → 기존 403/409 유지.

Run: `go test ./internal/service -run 'CancelOrder|CancelCommand' -v`

Expected: FAIL — 현재 결과는 CANCELLED/release 추정치이며 engine을 직접 호출한다.

- [ ] **Step 2: `OrderService.CancelOrder` 최소 구현**

트랜잭션 안에서 `FindByIDForUpdate` → 소유권/상태/지정가 검증 → command create-or-get 순서로 실행한다. terminal 검증을 command 조회보다 먼저 하므로 최종 상태 반영 후 재요청은 409다.

```go
type CancelOrderResult struct {
	OrderID   uint
	CommandID uint64
	Status    string
}

const CancelOrderAcceptedStatus = "ACCEPTED"
```

생성할 command는 lock으로 읽은 주문 스냅샷을 그대로 쓴다.

```go
candidate := &model.CancelCommand{
	OrderID:    order.ID,
	UserID:     order.UserID,
	CoinSymbol: order.CoinSymbol,
	Side:       order.Side,
	Price:      order.Price,
	Status:     model.CancelCommandStatusPending,
}
persisted, _, err := s.CancelCommandRepository.WithTx(tx).CreateOrGet(candidate)
```

트랜잭션이 성공한 뒤에만 `CancelCommandWake()`를 호출한다. wake가 nil이어도 command는 내구 기록됐으므로 50ms polling이 복구한다. `estimateCancelRelease`가 더 이상 다른 곳에서 쓰이지 않으면 함수와 관련 import를 제거한다.

- [ ] **Step 3: handler RED→GREEN**

통합 handler 테스트는 Gin context에 인증 user ID를 넣고 실제 test DB service를 호출해 status code와 JSON을 검증한다.

```json
{
  "data": {
    "message": "cancellation accepted",
    "order_id": 123,
    "command_id": 456,
    "status": "ACCEPTED"
  }
}
```

`OrderHandler.CancelOrder`는 engine 오류 특별 매핑과 release 필드를 제거하고 `http.StatusAccepted`를 쓴다. 권한·validation·conflict 매핑은 기존 `writeServiceError`를 유지한다.

- [ ] **Step 4: 서비스·HTTP GREEN 확인**

```powershell
$env:GOEXCHANGE_TEST_DATABASE_DSN='host=localhost user=goexchange_test password=goexchange_test_password dbname=goexchange_test port=55432 sslmode=disable'
go test -run 'IntegrationCancel|CancelOrder' -v -p 1 ./internal/service ./internal/handler
go test ./internal/service ./internal/handler -race
```

Expected: commit-before-wake, 동시 100회 단일 command, 202 body, 기존 4xx PASS.

- [ ] **Step 5: Task 4 커밋**

권장 subject: `feat(api): 주문 취소를 내구 command 접수로 전환`

---

### Task 5: `CancelCommandWorker` 상태 기계와 관측성

**Files:**
- Create: `internal/service/cancel_command_worker.go`
- Create: `internal/service/cancel_command_worker_test.go`
- Modify: `internal/metrics/metrics.go`
- Modify: `internal/metrics/metrics_test.go`

**Interfaces:**
- Consumes: Task 1 repository, Task 2 `matching.CancelOrderCommand`
- Produces:
  - `NewCancelCommandWorker(commands, orders, engine)`
  - `(*CancelCommandWorker).Wake()`
  - `(*CancelCommandWorker).Run(ctx)`
  - `(*CancelCommandWorker).WaitUntilDrained(ctx) error`
  - `cancel_command_latency_seconds`
  - `cancel_command_awaiting_outbox_deadline_total`

- [ ] **Step 1: 순수 상태·polling RED 테스트 작성**

fake command store, fake order reader, 응답을 제어하는 fake engine으로 설계 검증 8/8b/8c/8d/8e를 작성한다.

- wake 채널을 미리 채워 두 번째 신호를 합쳐도 다음 tick이 `FindPending`을 호출한다.
- 엔진 응답을 1초 지연하는 동안 같은 ID의 engine 호출은 1회다.
- 반복 timeout은 재시도 간격이 100ms→200ms→400ms→최대 5s로 증가한다.
- 성공 반환 뒤 repository status를 PENDING에 고정하고 awaiting 경고 deadline을 넘겨도 engine 호출은 1회다. PROCESSED로 바꾸면 in-flight에서 해제된다.
- not-found + open은 RecordAttempt/backoff이고 NOOP가 아니다. terminal일 때만 NOOP다.

Run: `go test ./internal/service -run CancelCommandWorker -race -v`

Expected: FAIL — worker가 없다.

- [ ] **Step 2: worker 인터페이스와 상태 구현**

production 기본값을 고정한다.

```go
const (
	defaultCancelCommandPollInterval = 50 * time.Millisecond
	defaultCancelCommandInitialBackoff = 100 * time.Millisecond
	defaultCancelCommandMaxBackoff = 5 * time.Second
	defaultCancelCommandAwaitingWarnAfter = 5 * time.Second
	defaultCancelCommandMaxDispatch = 8
	defaultCancelCommandScanLimit = 128
)

type cancelCommandPhase uint8

const (
	cancelCommandDispatching cancelCommandPhase = iota
	cancelCommandAwaitingOutbox
	cancelCommandBackoff
)

type cancelCommandInFlight struct {
	phase          cancelCommandPhase
	nextAttemptAt  time.Time
	backoff        time.Duration
	awaitingSince  time.Time
	warningEmitted bool
}
```

`Run`의 한 goroutine만 in-flight map을 소유한다. engine 호출은 최대 8개 goroutine에서 수행하고 결과 채널로만 상태 소유자에게 돌아온다. tick/wake마다 순서는 다음과 같다.

1. awaiting ID를 `FindStatuses` 한 번으로 조회해 PROCESSED/NOOP만 삭제하고 latency 관측.
2. 5초가 지난 awaiting ID는 경고·counter를 딱 한 번 기록하되 phase는 유지.
3. free dispatch slot만큼 PENDING ID 순으로 조회.
4. `dispatching`/`awaiting_outbox`, 또는 아직 `nextAttemptAt` 전인 `backoff`는 제외.
5. dispatch 직전에 map을 `dispatching`으로 넣고 goroutine 시작.

- [ ] **Step 3: 결과 분기 구현**

- Removed=true/err=nil → `awaiting_outbox`; 삭제하지 않음.
- `ErrCancelOrderNotFound` → `OrderRepository.FindByID`로 현재 DB 상태 조회.
  - open → `RecordAttempt`, 다음 backoff.
  - terminal → `MarkNoop`; 반환 row의 `updated_at-created_at`을 관측하고 삭제.
- 그 외 error → `RecordAttempt`, 지수 backoff.
- process context 취소 → 신규 scan/dispatch 중지, **이미 시작한 engine 호출이 모두 반환할 때까지** 기다린 뒤 `Run` 반환. worker는 자체 상한을 두지 않는다 — `MatchingEngine.CancelOrder`는 enqueue 1초와 response 1초를 순차로 기다려 한 호출만으로 약 2초가 걸릴 수 있고, 그보다 짧은 상한으로 먼저 반환하면 살아 있는 호출과 뒤이은 엔진 정지가 경쟁한다. **종료 상한은 lifecycle이 소유한다**(Task 6).

`WaitUntilDrained`는 `Wake()` 후 DB `CountPending()`을 반복 조회한다. 0일 때만 성공하며 context 만료·DB 오류는 error다.

- [ ] **Step 4: metrics RED→GREEN**

`cancel_command_latency_seconds`는 `matchLatencyBuckets`를 사용한다. awaiting warning은 Counter다.

```go
CancelCommandLatency = promauto.NewHistogram(prometheus.HistogramOpts{
	Name: "cancel_command_latency_seconds",
	Help: "Time from durable cancel command creation to PROCESSED or NOOP commit.",
	Buckets: matchLatencyBuckets,
})

CancelCommandAwaitingOutboxDeadlineTotal = promauto.NewCounter(prometheus.CounterOpts{
	Name: "cancel_command_awaiting_outbox_deadline_total",
	Help: "Cancel commands still awaiting the atomic outbox commit after the warning deadline.",
})
```

testutil로 두 metric 이름과 awaiting deadline 이후 counter 1회만 증가함을 검증한다.

- [ ] **Step 5: worker 전체 GREEN 확인**

```powershell
go test ./internal/service ./internal/metrics -run 'CancelCommand|AwaitingOutbox' -count=20 -race -v
```

Expected: timing 테스트 20회 안정 PASS, deadline 이후에도 중복 dispatch 0.

- [ ] **Step 6: Task 5 커밋**

권장 subject: `feat(cancel): 내구 command dispatch worker 추가`

---

### Task 6: 부팅 장벽·종료 순서·실제 배선

**Files:**
- Create: `cmd/cancel_command_lifecycle.go`
- Create: `cmd/cancel_command_lifecycle_test.go`
- Modify: `cmd/main.go`

**Interfaces:**
- Consumes: Task 4 wake callback, Task 5 worker
- Produces:
  - `runCancelCommandStartupBarrier(ctx, bootstrap, startWorker, drain) error`
  - `stopCancelCommandWorker(cancel, done, timeout) error`

- [ ] **Step 1: lifecycle RED 테스트 작성**

startup test는 closure 호출 순서를 기록해 `bootstrap → start_worker → drain`을 단언한다. drain channel을 막아 둔 동안 barrier가 반환하지 않는 것도 확인한다. shutdown test는 worker의 진행 중 engine 호출을 풀기 전에는 `stopCancelCommandWorker`가 반환하지 않음을 확인한다.

Run: `go test ./cmd -run CancelCommandLifecycle -race -v`

Expected: FAIL — helper가 없다.

- [ ] **Step 2: lifecycle helper 최소 구현**

```go
func runCancelCommandStartupBarrier(
	ctx context.Context,
	bootstrap func(context.Context) error,
	startWorker func(),
	drain func(context.Context) error,
) error {
	if err := bootstrap(ctx); err != nil {
		return fmt.Errorf("matching bootstrap: %w", err)
	}
	startWorker()
	if err := drain(ctx); err != nil {
		return fmt.Errorf("recovered cancel command drain: %w", err)
	}
	return nil
}
```

shutdown helper는 cancel 함수를 호출한 뒤 done 또는 timeout을 기다린다.

> **⚠ timeout이 나면 `me.Stop()`으로 진행하지 않는다.** 로그만 남기고 다음 단계로 가면 worker가 아직 엔진을 호출하고 있는 채로 엔진을 정지시켜 Task 5에서 닫은 경쟁을 그대로 다시 연다. timeout은 "worker가 끝나지 않았다"는 사실이지 "끝났다고 쳐도 된다"가 아니다.
>
> 선택지는 둘뿐이다.
> - **계속 기다린다**(권장): 상한을 넉넉히 잡고, 초과 시 경고를 남기며 대기를 이어간다. `Run`은 시작한 호출이 반환하면 반드시 끝나므로 무한 대기가 아니다.
> - **프로세스 종료 경로로 전환한다**: graceful 종료를 포기하고 즉시 종료한다. 미완 command는 내구 기록돼 있으므로 재기동 후 부팅 장벽(§4.4)이 처리한다.
>
> 어느 쪽이든 **엔진 정지 단계로는 넘어가지 않는다.**

- [ ] **Step 3: `cmd/main.go` 배선**

다음 순서를 코드상 그대로 만든다.

1. goose 007 적용. **AutoMigrate 목록은 건드리지 않는다**(Task 1의 주의 참조 — 넣으면 두 번째 부팅부터 SQLSTATE 42704).
2. `cancelRepo`와 `cancelWorker` 생성.
3. `orderService.CancelCommandRepository = cancelRepo`, `CancelCommandWake = cancelWorker.Wake`.
4. 기존 execution outbox replay와 stale market finalizer 완료.
5. settlement queues와 `OutboxWriter` 시작.
6. matching bootstrap 실행.
7. 별도 worker context로 `cancelWorker.Run` 시작, `WaitUntilDrained` 성공.
8. 그 뒤에만 Gin router 생성과 `ListenAndServe`.

부팅 drain context는 30초다. 실패하면 `log.Fatal`로 HTTP를 열지 않는다.

shutdown은 기존 체인에 cancel worker를 정확히 삽입한다.

```text
srv.Shutdown
holdCoordinator.Shutdown
cancelCancelWorker
wait cancelWorkerDone
me.Stop
wait outboxWriterDone
wait settlement workers
cancelBackground
```

- [ ] **Step 4: 배선 검증**

```powershell
go test ./cmd -race -v
go vet ./...
$buildOutput = Join-Path $env:TEMP 'goexchange-cancel-outbox.exe'
go build -trimpath -o $buildOutput ./cmd
Remove-Item -LiteralPath $buildOutput -ErrorAction SilentlyContinue
```

Expected: lifecycle 순서 PASS, main build PASS, HTTP listen이 drain 뒤에만 존재.

- [ ] **Step 5: Task 6 커밋**

권장 subject: `feat(runtime): 복구 취소 drain을 서버 부팅 장벽에 연결`

---

### Task 7: 크래시 창·멱등성 통합 검증

**Files:**
- Create: `internal/service/cancel_command_outbox_integration_test.go`
- Modify: `internal/service/order_cancellation_integration_test.go`

**Interfaces:**
- Consumes: Tasks 1–6 전체 pipeline
- Produces: 설계 §6의 backend 검증 1–9에 대한 실행 증거

- [ ] **Step 1: 통합 harness 작성**

실제 Postgres, MatchingEngine, OutboxWriter, CancelCommandWorker, OrderService를 연결한다. harness는 다음 제어 채널을 제공한다.

- outbox repository 호출 직전 block/release
- settlement Forward 수신 보류/재개
- worker 시작/정지
- 새 engine으로 bootstrap 재현

테스트 종료 시 생성한 command/outbox/order/ledger/wallet/user 행만 ID로 정리한다.

- [ ] **Step 2: crash-window 테스트 1–3 RED→GREEN**

1. command commit 뒤 worker 시작 전 종료를 모사 → 새 engine bootstrap + worker → command PROCESSED, 주문 CANCELLED, hold 해제.
2. command PROCESSED/outbox PENDING에서 live settlement를 중단 → `OutboxReplayer`로 주문 CANCELLED.
3. 202 결과 직후 새 runtime → bootstrap이 주문을 복원해도 startup drain 뒤 book에서 제거. barrier 반환 뒤 crossing live 주문을 넣어 해당 maker와 trade가 0인지 확인.

Run:

```powershell
go test ./internal/service -run 'IntegrationCancelCommandCrash|IntegrationCancelCommandRestart' -v -p 1
```

- [ ] **Step 3: 멱등·race 테스트 4/4b/5/6 RED→GREEN**

- 같은 주문 100개 goroutine 취소 → command 1, 모든 성공 결과 command ID 동일.
- worker/outbox로 command PROCESSED를 만든 뒤 settlement Forward를 막고 재요청 → 기존 ID 202 의미 성공, command 1. settlement 재개 후 `ORDER_RELEASE` 정확히 1.
- command 생성 뒤 주문을 FILLED로 만든 상태에서 engine not-found → NOOP, retry loop 종료, release 0.
- DB open + engine not-found → 일정 관측 창 동안 PENDING, attempt_count 증가, NOOP 0.

- [ ] **Step 4: outbox 행 불일치와 awaiting 창 재검증**

repository Task 3 테스트와 별도로 실제 worker event의 command ID를 다른 terminal 상태로
선변경해 OutboxWriter transaction이 실패하는지 확인한다. 그 batch에 섞은 trade outbox도
0이어야 한다. 테스트가 끝나도록 wrapper repository는 첫 실패를 test channel에 알리고,
테스트가 DB rollback을 확인한 뒤 command를 다시 PENDING으로 복구해 다음 retry를 성공시킨다.
production 무한 retry 자체는 Task 3에서 검증한다.

- [ ] **Step 5: 전체 통합·race GREEN**

```powershell
$env:GOEXCHANGE_TEST_DATABASE_DSN='host=localhost user=goexchange_test password=goexchange_test_password dbname=goexchange_test port=55432 sslmode=disable'
go test -run Integration -v -p 1 ./internal/dbmigration ./internal/repository ./internal/service ./internal/handler
Remove-Item Env:GOEXCHANGE_TEST_DATABASE_DSN
go test ./... -race
```

Expected: 설계 backend 검증 1–9 PASS, race detector PASS.

- [ ] **Step 6: Task 7 커밋**

권장 subject: `test(cancel): command outbox 크래시 복구 계약 검증`

---

### Task 8: 프런트 202 계약과 terminal polling

**Repository:** `C:\Users\dksco\OneDrive\Desktop\GoExchange\Go-exchange-front`

**Files:**
- Modify: `src/lib/api.ts`
- Modify: `src/lib/api.test.ts`
- Create: `src/lib/cancelPolling.ts`
- Create: `src/lib/cancelPolling.test.ts`
- Modify: `src/components/trading/AuthPanel.tsx`
- Modify: `src/components/trading/AuthPanel.test.tsx`

**Interfaces:**
- Produces:
  - `CancelOrderResponse{message, order_id, command_id, status:"ACCEPTED"}`
  - `fetchOrder(token, orderID, signal?)`
  - `pollCancelOutcome(fetchCurrent, signal, options) -> "CANCELLED" | "FILLED" | null`

- [ ] **Step 1: API 타입 RED 테스트**

`api.test.ts`에서 DELETE mock이 202와 새 body를 반환하도록 하고 `cancelOrder`가 `command_id`와 `ACCEPTED`를 보존하는지 단언한다. `fetchOrder`는 Authorization과 AbortSignal을 전달하는지 확인한다.

Run: `npm test -- --run src/lib/api.test.ts`

Expected: FAIL — 타입과 `fetchOrder`가 없다.

- [ ] **Step 2: API 구현**

```ts
export interface CancelOrderResponse {
  message: string;
  order_id: number;
  command_id: number;
  status: "ACCEPTED";
}

export async function fetchOrder(
  token: string,
  orderID: number,
  signal?: AbortSignal,
): Promise<{ order: Order }> {
  return apiRequest<{ order: Order }>(`/orders/${orderID}`, { token, signal });
}
```

기존 `released_asset`, `released_amount`, `engine_removed` 타입은 삭제한다.

- [ ] **Step 3: polling helper RED 테스트**

fake timer로 네 경우를 검증한다.

- PENDING→CANCELLED 반환.
- PARTIAL→FILLED 반환.
- 10초 timeout → null, throw 아님.
- AbortController abort → 추가 fetch 0, AbortError를 호출자에게 전파하지 않고 null.

production 기본값은 interval 250ms, timeout 10s다.

- [ ] **Step 4: polling helper 구현**

```ts
export type CancelOutcome = "CANCELLED" | "FILLED" | null;

export async function pollCancelOutcome(
  fetchCurrent: (signal: AbortSignal) => Promise<Order>,
  signal: AbortSignal,
  options: { intervalMs?: number; timeoutMs?: number } = {},
): Promise<CancelOutcome> {
  const intervalMs = options.intervalMs ?? 250;
  const timeoutMs = options.timeoutMs ?? 10_000;
  const deadline = Date.now() + timeoutMs;
  try {
    while (!signal.aborted && Date.now() < deadline) {
      const order = await fetchCurrent(signal);
      if (order.status === "CANCELLED" || order.status === "FILLED") return order.status;
      await abortableDelay(intervalMs, signal);
    }
  } catch (error) {
    if (!signal.aborted) throw error;
  }
  return null;
}
```

`abortableDelay`는 abort listener를 호출 후 제거해 listener 누적을 막는다.

- [ ] **Step 5: AuthPanel RED 테스트**

API module을 mock하고 다음 문구와 lifecycle을 검증한다.

- DELETE 성공 직후: `취소 요청 접수됨`.
- polling CANCELLED: `취소 완료` + `onRefresh`.
- polling FILLED: `취소 전에 체결 완료` + 실패 메시지 없음.
- timeout: `취소 요청 접수됨 · 처리 중` 유지.
- rerender로 token을 null로 만들거나 unmount: controller abort, 이후 state update 없음.

- [ ] **Step 6: AuthPanel 구현**

component 최상단에 `useRef<AbortController | null>`과 cleanup `useEffect`를 둔다. hook은 `if (token && user)` 아래에서 호출하지 않는다.

`handleCancelOrder` 순서:

1. 기존 polling abort.
2. `cancelOrder` 호출.
3. `취소 요청 접수됨` 표시, `onRefresh()`.
4. 새 controller로 `pollCancelOutcome` await.
5. CANCELLED/FILLED/null에 맞는 문구 설정.
6. abort 또는 unauthorized면 state update 금지/기존 auth-expired 처리.

- [ ] **Step 7: 프런트 unit gate**

```powershell
npm test -- --run src/lib/api.test.ts src/lib/cancelPolling.test.ts src/components/trading/AuthPanel.test.tsx
npm run lint
npm run build
```

Expected: `undefined undefined 반환 완료` 문자열이 코드와 테스트 어디에도 없고 모든 명령 PASS.

- [ ] **Step 8: 프런트 Task 8 커밋**

프런트 저장소에서만 stage한다.

권장 subject: `feat(trading): 비동기 주문 취소 상태를 polling으로 표시`

---

### Task 9: E2E와 k6를 202 계약으로 전환

**Files:**
- Modify (front): `tests/e2e/exchange.spec.ts`
- Modify (back): `_workspace/loadtest/sli-classify.js`
- Modify (back): `_workspace/loadtest/sli-classify.selftest.js`
- Modify (back): `_workspace/loadtest/order-spike-availability.js`
- Modify (back): `loadtest/order-spike-single-symbol.js`

**Interfaces:**
- Consumes: Task 4 HTTP 202, Task 8 UI polling
- Produces: 200·202 success / 404·409 excluded / 그 외 infra_fail 분류

- [ ] **Step 1: k6 classifier RED selftest**

```js
assert(classifyCancelResponse(200) === 'success', '취소 legacy 200 → success');
assert(classifyCancelResponse(202) === 'success', '취소 202 → success');
assert(classifyCancelResponse(404) === 'excluded', '취소 404 → excluded');
assert(classifyCancelResponse(409) === 'excluded', '취소 409 → excluded');
```

Run: `k6 run _workspace/loadtest/sli-classify.selftest.js`

Expected: FAIL — 202가 infra_fail이다.

- [ ] **Step 2: k6 스크립트 GREEN**

`classifyCancelResponse`는 `status === 200 || status === 202`를 success로 분류한다. 두 workload의 `expectedStatuses`에 202를 추가하고 custom cancel success 조건도 200/202로 맞춘다. 34번 artifact 복사본인 `_workspace/buy-order-index-remeasurement/`는 수정하지 않는다.

Run:

```powershell
k6 run _workspace/loadtest/sli-classify.selftest.js
rg -n "expectedStatuses\(200, 202, 404, 409\)|status === 200 \|\|.*status === 202" _workspace/loadtest loadtest/order-spike-single-symbol.js
```

- [ ] **Step 3: E2E 응답 타입과 eventual assertion 변경**

`CancelOrderResponse`는 ACCEPTED 형태로 바꾸고 helper는 status 202를 명시적으로 단언한다.
취소 뒤 wallet/order 단언 앞에는 모두
`waitForOrderStatus(request, token, orderID, "CANCELLED")`를 둔다. 부분 체결 테스트는 202
body에서 release 금액을 읽지 않고 eventual wallet/ledger 결과로만 검증한다.

중복 테스트는 두 창을 분리한다.

1. backend 통합 테스트에서 PROCESSED-before-settlement 재요청이 기존 ID 202임을 고정(Task 7).
2. E2E에서는 CANCELLED 도달 뒤 재요청 409와 wallet release 1회를 검증.

UI E2E는 클릭 직후 `취소 요청 접수됨` 또는 이미 polling으로 바뀐 `취소 완료`가 보이는 것을 허용하되, 최종적으로 `취소 완료`, locked 0, open order 0을 단언한다.

- [ ] **Step 4: E2E 실행**

backend/test DB/frontend를 실행한 상태에서:

```powershell
npm run test:e2e -- --grep "cancel|취소|partially filled"
```

Expected: 즉시 CANCELLED/release body 의존 없이 eventual assertions PASS.

- [ ] **Step 5: Task 9 커밋 두 개**

백엔드 권장 subject: `test(load): 취소 202 응답을 SLI 성공으로 분류`

프런트 권장 subject: `test(e2e): 비동기 취소 완료를 eventual assertion으로 검증`

---

### Task 10: 전체 검증, 새 기준선, 문서화

**Files:**
- Create: `docs/benchmarks/35-2026-08-19-cancel-command-outbox.md`
- Modify: `README.md`
- Modify: `docs/refactor/README.md`
- Modify: `docs/ENGINEERING-SUMMARY.md`

**Interfaces:**
- Consumes: 완성된 backend/front/k6
- Produces: 사전 등록 gate 결과와 다음 변경의 latency baseline

- [ ] **Step 1: 로컬 전체 검증**

Backend:

```powershell
$env:GOEXCHANGE_TEST_DATABASE_DSN='host=localhost user=goexchange_test password=goexchange_test_password dbname=goexchange_test port=55432 sslmode=disable'
go test -run Integration -v -p 1 ./internal/dbmigration ./internal/repository ./internal/service ./internal/handler
Remove-Item Env:GOEXCHANGE_TEST_DATABASE_DSN
go test ./... -race
go vet ./...
$buildOutput = Join-Path $env:TEMP 'goexchange-cancel-outbox.exe'
go build -trimpath -o $buildOutput ./cmd
Remove-Item -LiteralPath $buildOutput -ErrorAction SilentlyContinue
git diff --check
```

Frontend:

```powershell
npm test
npm run lint
npm run build
git diff --check
```

Expected: 전부 exit 0.

- [ ] **Step 2: 유료 GCP 실행 전 승인과 preflight**

500 VU baseline은 비용이 발생하므로 사용자 승인 뒤 실행한다. 34번과 같은 4 VM topology, k6 v2.1.0, 두 load-generator, settlement partitions=10/concurrency=8을 사용한다. 새 backend image에는 migration 007이 포함되고 goose 7·`cancel_commands_order_unique`·`cancel_commands_pending`을 확인한다. load-gen dev token은 값 출력 없이 container와 `cmp`한다.

- [ ] **Step 3: 사전 등록한 500 VU 10분 1회 실행**

phase 이름은 `canceloutbox500r1`, 각 load-gen 250 VU, ramp 30초, hold 10분이다. 실행 전 backend stop → DB VM restart → 5개 workload table과 `cancel_commands` truncate → backend start → bootstrap loaded=0 → collector start 순서를 지킨다.

게이트는 다음 두 개뿐이다.

| 지표 | PASS |
|---|---|
| hold 구간 `sli_cancel_success` | **100.00000%** |
| `custom_cancel_fail` 및 status 0/5xx 취소 | **0건** |

404/409는 정상 경쟁으로 분모에서 제외한다. 주문 SLI, 정합성, reconciliation은 기존 hard gate로 함께 보고하되 이번 취소 계약의 별도 합격선을 새로 만들지 않는다.

- [ ] **Step 4: 새 latency 기준선 기록**

hold 시작/끝을 고정해 다음 PromQL을 같은 구간에서 계산한다.

```promql
histogram_quantile(0.50, sum by (le) (increase(cancel_command_latency_seconds_bucket[10m])))
histogram_quantile(0.95, sum by (le) (increase(cancel_command_latency_seconds_bucket[10m])))
histogram_quantile(0.99, sum by (le) (increase(cancel_command_latency_seconds_bucket[10m])))
histogram_quantile(0.95, sum by (le) (increase(http_request_duration_seconds_bucket{method="DELETE",path="/orders/:id",status="202"}[10m])))
```

`cancel_command_awaiting_outbox_deadline_total` 증가량도 기록한다. 이 값들과 HTTP p95는 **PASS/FAIL이 아니라 최초 기준선**이다. 34번의 취소 p95와 수치 비교 문장을 쓰지 않는다.

- [ ] **Step 5: VM 종료를 결과 분석보다 먼저 완료**

```powershell
gcloud compute instances stop @instances --zone=$zone
gcloud compute instances list --filter="name:goexchange-stress-*" --format='table(name,zone.basename(),machineType.basename(),status)'
```

Expected: server, DB, load-gen A/B 모두 `TERMINATED`.

- [ ] **Step 6: 35번 보고서와 문서 갱신**

35번 문서는 다음 순서를 사용한다.

1. 기준 SHA와 backend/frontend SHA, migration 007 카탈로그.
2. 202 의미 변화와 추가 체결 가능 창.
3. crash-window 1–3, 동시 100회, 4b, in-flight 8d/8e 결과.
4. 500 VU runtime/workload/checksum과 acceptance/infra gate.
5. HTTP p95, command p50/p95/p99, awaiting 경고 증가량의 새 기준선.
6. 34번은 참고만이며 직접 성능 비교하지 않았다는 제한.
7. 후속 `maxConsecutiveCancels` 재튜닝이 아직 남았다는 결론.

`README.md`의 DELETE 설명은 “취소 완료”가 아니라 “취소 command 접수(202)”로 바꾸고, `docs/refactor/README.md`와 `docs/ENGINEERING-SUMMARY.md`에는 B 정확성 1번 완료와 실제 측정 결과만 기록한다.

- [ ] **Step 7: 최종 placeholder·diff·CI 검증**

```powershell
rg -n 'TODO|TBD|FIXME|미정' docs/benchmarks/35-2026-08-19-cancel-command-outbox.md README.md docs/refactor/README.md docs/ENGINEERING-SUMMARY.md
git diff --check
git status --short
```

Expected: placeholder 0, 관련 파일 외 변경 없음.

- [ ] **Step 8: 문서 커밋·push·CI**

backend와 frontend 각각 `commit-message` 검토를 거친 뒤 push한다. backend 최종 SHA의 `Backend CI`, frontend 최종 SHA의 CI를 모두 `--exit-status`로 기다린다. CI 실패는 수정·재검증 후 새 커밋으로 해결하며 force-push하지 않는다.

권장 backend subject: `docs(refactor): 취소 command outbox 검증 결과 기록`

---

## Plan Completion Gate

- 설계 §6의 15개 검증이 테스트 이름과 원본 출력으로 추적된다.
- command commit 전 202 없음, command당 ORDER_RELEASE 최대 1회, outbox/PROCESSED 원자성이 실증된다.
- `awaiting_outbox`는 deadline 뒤에도 재투입되지 않고 DB terminal status로만 해제된다.
- startup drain 전 HTTP가 열리지 않고 shutdown에서 worker가 engine보다 먼저 끝난다.
- frontend는 undefined release 문구가 없고 CANCELLED/FILLED/timeout/logout 네 종료 의미를 구분한다.
- k6 500 VU gate는 acceptance 100%, infra failure 0으로 판정되며 latency는 합격선 없이 기준선으로 남는다.
- VM 4대가 TERMINATED이고 backend/frontend CI가 모두 PASS다.
