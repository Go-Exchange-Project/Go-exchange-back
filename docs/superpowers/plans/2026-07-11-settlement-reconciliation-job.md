# 정산 리컨실리에이션 잡 (A-6) 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 원장(`ledger_entries`)-지갑(`wallets`) 정합성, 자산별 총량 보존, 오래된 시장가 주문 잔존을 주기적으로 실제 검사하고 위반을 내구 기록 + 메트릭으로 보고하는 `ReconciliationWorker`를 구현한다.

**Architecture:** `internal/repository/reconciliation_repository.go`가 3개 검사의 raw SQL과 위반 영속화를 담당하고, `internal/service/reconciliation_worker.go`가 순수 분류 로직 + 오케스트레이션(페이지네이션, 메트릭 갱신, 위반 기록)을 담당한다. `cmd/main.go`가 `SettlementRetryWorker`와 나란히 기동한다.

**Tech Stack:** Go, GORM(`db.Raw(...).Scan(&dest)`으로 raw SQL 실행), PostgreSQL 16, `shopspring/decimal`, `prometheus/client_golang`.

**스펙 문서:** `docs/superpowers/specs/2026-07-11-settlement-reconciliation-job-design.md` (3차 리뷰 반영 완료, `263ee87`)

## Global Constraints

- 검사 1(`ledger_wallet`)은 `WHERE w.id > $lastID ORDER BY w.id LIMIT $pageSize`의 keyset 페이지네이션을 쓴다. 페이지 크기는 500(`reconciliationPageSize = 500`).
- 검사 3(`stale_market_order`)의 임계값은 5분(`staleMarketOrderThreshold = 5 * time.Minute`), 하드코딩(환경변수 아님).
- `legacy_mismatch` 분류 조건: `available_gap == implied_initial_available(NULL이면 0으로 취급) AND locked_gap == 0`. 그 외 위반은 전부 `ledger_wallet`.
- 인터벌: 환경변수 `GOEXCHANGE_RECONCILIATION_INTERVAL`(초), 미설정/비정상 시 기본 3600.
- 게이지 `reconciliation_violations{check=...}`는 매 실행마다 성공한 검사의 라벨 전부를 그 회차 위반 건수로 `Set`한다(0건도 명시적으로 set). 검사 쿼리가 에러로 실패하면 해당 라벨은 갱신하지 않고 `reconciliation_check_errors_total{check=...}`(카운터)를 올린다.
- `ReconciliationViolation`은 위반이 있을 때만 insert한다(0건이면 아무것도 안 씀).
- `decimal.Decimal` 비교는 전부 정확히 일치(`Equal`), 오차 허용치 없음.
- 자동 교정 없음 — 탐지/기록/메트릭까지만.
- 공유 테스트 DB를 쓰는 통합 테스트는 전역 "위반 0건" 단언을 하지 않는다 — 자기 테스트가 만든 user_id/coin_symbol/order_id로 결과를 필터링해서만 단언한다(스펙의 "검증 방법" 절 참조).

---

### Task 1: `ReconciliationViolation` 모델 + AutoMigrate 등록 + 인터벌 설정

**Files:**
- Create: `internal/model/reconciliation_violation.go`
- Modify: `cmd/main.go:48-58` (AutoMigrate 목록)
- Modify: `internal/testdb/integration.go:31` (AutoMigrate 목록)
- Modify: `config/runtime.go` (인터벌 env 함수 추가)
- Test: `config/runtime_test.go` (신규 또는 기존 파일에 추가 — 아래 확인 후 결정)

**Interfaces:**
- Produces: `model.ReconciliationViolation{ID, CheckName, SubjectKey, Detail, DetectedAt}` — Task 2/3/4/6이 이 구조체로 위반을 만든다.
- Produces: `config.ReconciliationIntervalFromEnv() time.Duration` — Task 6이 `cmd/main.go`에서 `ReconciliationWorker.Interval`에 이 값을 넣는다.

- [ ] **Step 1: `config/runtime_test.go`가 있는지 확인**

Run: `ls config/*_test.go`

없다면 새로 만든다. 있다면 기존 파일에 아래 테스트 함수를 추가한다.

- [ ] **Step 2: 실패하는 테스트 작성**

`config/runtime_test.go`:

```go
package config

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestReconciliationIntervalFromEnvDefaultsTo3600Seconds(t *testing.T) {
	t.Setenv(EnvGOExchangeReconciliationInterval, "")
	assert.Equal(t, 3600*time.Second, ReconciliationIntervalFromEnv())
}

func TestReconciliationIntervalFromEnvUsesOverride(t *testing.T) {
	t.Setenv(EnvGOExchangeReconciliationInterval, "120")
	assert.Equal(t, 120*time.Second, ReconciliationIntervalFromEnv())
}

func TestReconciliationIntervalFromEnvFallsBackOnInvalidValue(t *testing.T) {
	t.Setenv(EnvGOExchangeReconciliationInterval, "not-a-number")
	assert.Equal(t, 3600*time.Second, ReconciliationIntervalFromEnv())
}
```

- [ ] **Step 3: 테스트가 컴파일 실패로 fail하는지 확인**

Run: `go test ./config/... -run TestReconciliationIntervalFromEnv -v`
Expected: FAIL — `undefined: EnvGOExchangeReconciliationInterval` 또는 `undefined: ReconciliationIntervalFromEnv`

- [ ] **Step 4: `config/runtime.go`에 인터벌 함수 추가**

`config/runtime.go`의 const 블록과 함수를 아래처럼 수정한다(기존 `EnvGOExchangeSettlementWorkers`/`defaultSettlementWorkers`/`SettlementWorkersFromEnv` 옆에 나란히 추가, 기존 코드는 그대로 둔다):

```go
package config

import (
	"os"
	"strings"
	"time"
)

const (
	EnvGOExchangeEnableDevTools         = "GOEXCHANGE_ENABLE_DEV_TOOLS"
	EnvGOExchangeDevToolsToken          = "GOEXCHANGE_DEV_TOOLS_TOKEN"
	EnvGOExchangeEnableUpbit            = "GOEXCHANGE_ENABLE_UPBIT"
	EnvGOExchangeCORSOrigins            = "GOEXCHANGE_CORS_ALLOWED_ORIGINS"
	EnvGOExchangeEnablePprof            = "GOEXCHANGE_ENABLE_PPROF"
	EnvGOExchangeSettlementWorkers      = "GOEXCHANGE_SETTLEMENT_WORKERS"
	EnvGOExchangeReconciliationInterval = "GOEXCHANGE_RECONCILIATION_INTERVAL"
)

const defaultSettlementWorkers = 10
const defaultReconciliationIntervalSeconds = 3600
```

(`"time"` import 추가, 나머지 기존 내용은 그대로.) 파일 맨 끝에 함수를 추가한다:

```go
func ReconciliationIntervalFromEnv() time.Duration {
	seconds := parsePositiveIntEnv(EnvGOExchangeReconciliationInterval, defaultReconciliationIntervalSeconds)
	return time.Duration(seconds) * time.Second
}
```

- [ ] **Step 5: 테스트 통과 확인**

Run: `go test ./config/... -run TestReconciliationIntervalFromEnv -v`
Expected: PASS (3 tests)

- [ ] **Step 6: 모델 파일 작성**

`internal/model/reconciliation_violation.go`:

```go
package model

import "time"

// ReconciliationViolation은 ReconciliationWorker가 발견한 정합성 위반을 기록합니다.
// 위반이 없으면 아무 행도 만들지 않으므로, 존재 자체가 "그 시점에 위반이 있었다"는
// 증거입니다. 같은 위반이 해소될 때까지 매 실행마다 새 행이 쌓입니다(dedup 안 함 —
// 위반 지속 이력 자체가 유용한 데이터).
type ReconciliationViolation struct {
	ID         uint      `gorm:"primaryKey"`
	CheckName  string    `gorm:"not null"` // ledger_wallet | asset_conservation | stale_market_order | legacy_mismatch
	SubjectKey string    `gorm:"not null;index"` // 예: "wallet:123", "coin:BTC", "order:42"
	Detail     string    `gorm:"type:text;not null"`
	DetectedAt time.Time `gorm:"not null"`
}
```

- [ ] **Step 7: `cmd/main.go`의 AutoMigrate 목록에 추가**

`cmd/main.go:48-58`을 아래처럼 수정한다(기존 항목은 그대로, `&model.ReconciliationViolation{}`만 추가):

```go
	if err := config.DB.AutoMigrate(
		&model.User{},
		&model.Order{},
		&model.Wallet{},
		&model.Trade{},
		&model.FailedSettlement{},
		&model.FailedMarketCompletion{},
		&model.LedgerEntry{},
		&model.ReconciliationViolation{},
	); err != nil {
		log.Fatal("auto migrate failed: ", err)
	}
```

- [ ] **Step 8: `internal/testdb/integration.go`의 AutoMigrate 목록에 추가**

`internal/testdb/integration.go:31`을 아래처럼 수정한다:

```go
	fatalIfErr(t, db.AutoMigrate(&model.User{}, &model.Order{}, &model.Wallet{}, &model.Trade{}, &model.FailedSettlement{}, &model.FailedMarketCompletion{}, &model.LedgerEntry{}, &model.ReconciliationViolation{}))
```

- [ ] **Step 9: 빌드 확인**

Run: `go build ./...`
Expected: 성공, 에러 없음

- [ ] **Step 10: 전체 config 테스트 확인**

Run: `go test ./config/... -v`
Expected: PASS (기존 테스트 포함 전부)

- [ ] **Step 11: Commit**

```bash
git add internal/model/reconciliation_violation.go cmd/main.go internal/testdb/integration.go config/runtime.go config/runtime_test.go
git commit -m "feat(reconciliation): ReconciliationViolation 모델과 인터벌 설정 추가"
```

---

### Task 2: 검사 1 — 원장-지갑 일치 리포지토리 (`ledger_wallet`)

**Files:**
- Create: `internal/repository/reconciliation_repository.go`
- Create: `internal/repository/reconciliation_repository_integration_test.go`

**Interfaces:**
- Consumes: `model.Wallet`, `model.LedgerEntry`(Task 1 이전부터 존재), `repositoryTestUserID`/`openRepositoryIntegrationDB`/`cleanupRepositoryUsers`(`internal/repository/wallet_repository_integration_test.go`에 이미 정의됨).
- Produces: `repository.LedgerWalletRow{WalletID, UserID, CoinSymbol, AvailableBalance, LockedBalance, LedgerAvailableSum, LedgerLockedSum, ImpliedInitialAvailable}`, `repository.NewReconciliationRepository(db) *ReconciliationRepository`, `(*ReconciliationRepository).CheckLedgerWalletPage(afterWalletID uint, limit int) ([]LedgerWalletRow, error)` — Task 6(`ReconciliationWorker`)이 이 타입/메서드를 그대로 쓴다.

**중요:** `ImpliedInitialAvailable`은 **반드시 `decimal.NullDecimal`**(포인터 `*decimal.Decimal`이 아님)로 선언해야 한다. Go의 `database/sql`은 스캔 대상이 `sql.Scanner`를 구현해야 커스텀 타입을 처리하는데, 필드 타입이 `*decimal.Decimal`이면 GORM이 실제로 넘기는 값은 `**decimal.Decimal`이 되어 `decimal.Decimal`의 `Scan`(포인터 리시버) 메서드 셋에 걸리지 않아 조용히 스캔 실패한다. `decimal.NullDecimal`(값 타입, 내부에 `Valid bool`)을 쓰면 한 단계 포인터로 정확히 매칭된다.

- [ ] **Step 1: 실패하는 통합 테스트 작성**

`internal/repository/reconciliation_repository_integration_test.go`:

```go
package repository

import (
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func cleanupReconciliationViolations(t *testing.T, db *gorm.DB, subjectKeys ...string) {
	t.Helper()

	if len(subjectKeys) == 0 {
		return
	}
	require.NoError(t, db.Where("subject_key IN ?", subjectKeys).Delete(&model.ReconciliationViolation{}).Error)
}

func TestIntegrationCheckLedgerWalletPageFindsNoViolationWhenLedgerMatchesWallet(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(800)
	defer cleanupRepositoryUsers(t, db, userID)

	wallet := model.Wallet{
		UserID:           userID,
		CoinSymbol:       model.KRWAssetSymbol,
		KRW:              decimal.NewFromInt(1000),
		AvailableBalance: decimal.NewFromInt(1000),
		LockedBalance:    decimal.Zero,
	}
	require.NoError(t, db.Create(&wallet).Error)
	require.NoError(t, db.Create(&model.LedgerEntry{
		UserID:                userID,
		CoinSymbol:            model.KRWAssetSymbol,
		EntryType:             model.LedgerEntryTypeDevFund,
		AvailableDelta:        decimal.NewFromInt(1000),
		LockedDelta:           decimal.Zero,
		AvailableBalanceAfter: decimal.NewFromInt(1000),
		LockedBalanceAfter:    decimal.Zero,
		ReferenceType:         model.LedgerReferenceTypeDevFund,
		ReferenceID:           0,
	}).Error)

	repo := NewReconciliationRepository(db)
	rows, err := repo.CheckLedgerWalletPage(0, 500)
	require.NoError(t, err)

	row := findLedgerWalletRow(rows, wallet.ID)
	require.NotNil(t, row, "expected a row for the seeded wallet")
	assert.True(t, row.AvailableBalance.Equal(decimal.NewFromInt(1000)))
	assert.True(t, row.LedgerAvailableSum.Equal(decimal.NewFromInt(1000)))
	assert.True(t, row.LockedBalance.Equal(row.LedgerLockedSum))
	require.True(t, row.ImpliedInitialAvailable.Valid)
	assert.True(t, row.ImpliedInitialAvailable.Decimal.IsZero(), "first entry's implied initial balance should be 0 for a wallet that started at 0")
}

func TestIntegrationCheckLedgerWalletPageFindsGapWhenWalletDivergesFromLedger(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(801)
	defer cleanupRepositoryUsers(t, db, userID)

	// 원장은 500만큼 델타를 기록했는데 지갑은 700으로 직접 조작된 상황(진짜 버그 시뮬레이션).
	wallet := model.Wallet{
		UserID:           userID,
		CoinSymbol:       model.KRWAssetSymbol,
		KRW:              decimal.NewFromInt(700),
		AvailableBalance: decimal.NewFromInt(700),
		LockedBalance:    decimal.Zero,
	}
	require.NoError(t, db.Create(&wallet).Error)
	require.NoError(t, db.Create(&model.LedgerEntry{
		UserID:                userID,
		CoinSymbol:            model.KRWAssetSymbol,
		EntryType:             model.LedgerEntryTypeDevFund,
		AvailableDelta:        decimal.NewFromInt(500),
		LockedDelta:           decimal.Zero,
		AvailableBalanceAfter: decimal.NewFromInt(500),
		LockedBalanceAfter:    decimal.Zero,
		ReferenceType:         model.LedgerReferenceTypeDevFund,
		ReferenceID:           0,
	}).Error)

	repo := NewReconciliationRepository(db)
	rows, err := repo.CheckLedgerWalletPage(0, 500)
	require.NoError(t, err)

	row := findLedgerWalletRow(rows, wallet.ID)
	require.NotNil(t, row)
	assert.True(t, row.AvailableBalance.Sub(row.LedgerAvailableSum).Equal(decimal.NewFromInt(200)), "gap should be 700-500=200")
}

func TestIntegrationCheckLedgerWalletPageReturnsNullImpliedForWalletWithNoLedgerEntries(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(802)
	defer cleanupRepositoryUsers(t, db, userID)

	wallet := model.Wallet{
		UserID:           userID,
		CoinSymbol:       model.KRWAssetSymbol,
		KRW:              decimal.NewFromInt(50),
		AvailableBalance: decimal.NewFromInt(50),
		LockedBalance:    decimal.Zero,
	}
	require.NoError(t, db.Create(&wallet).Error)

	repo := NewReconciliationRepository(db)
	rows, err := repo.CheckLedgerWalletPage(0, 500)
	require.NoError(t, err)

	row := findLedgerWalletRow(rows, wallet.ID)
	require.NotNil(t, row)
	assert.False(t, row.ImpliedInitialAvailable.Valid, "wallet with no ledger entries must report NULL, not 0, for implied initial balance")
	assert.True(t, row.LedgerAvailableSum.IsZero())
}

func TestIntegrationCheckLedgerWalletPagePaginatesByWalletID(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(803)
	defer cleanupRepositoryUsers(t, db, userID)

	wallet := model.Wallet{
		UserID:           userID,
		CoinSymbol:       model.KRWAssetSymbol,
		KRW:              decimal.NewFromInt(1),
		AvailableBalance: decimal.NewFromInt(1),
		LockedBalance:    decimal.Zero,
	}
	require.NoError(t, db.Create(&wallet).Error)

	repo := NewReconciliationRepository(db)
	// wallet.ID보다 큰 커서로 조회하면 이 지갑은 절대 나오지 않아야 한다.
	rows, err := repo.CheckLedgerWalletPage(wallet.ID, 500)
	require.NoError(t, err)
	assert.Nil(t, findLedgerWalletRow(rows, wallet.ID))
}

func findLedgerWalletRow(rows []LedgerWalletRow, walletID uint) *LedgerWalletRow {
	for i := range rows {
		if rows[i].WalletID == walletID {
			return &rows[i]
		}
	}
	return nil
}
```

- [ ] **Step 2: 컴파일/실행해서 실패 확인**

Run: `go test ./internal/repository/... -run TestIntegrationCheckLedgerWalletPage -v`
Expected: FAIL — `undefined: NewReconciliationRepository` (GOEXCHANGE_TEST_DATABASE_DSN이 없으면 SKIP으로 뜨는데, 그 경우 로컬 Postgres를 세팅하거나 CI 환경에서 이 스텝을 확인한다 — 최소한 `go build ./...`로 컴파일 에러는 잡는다)

Run: `go build ./...`
Expected: FAIL — `undefined: NewReconciliationRepository` 등

- [ ] **Step 3: 리포지토리 구현**

`internal/repository/reconciliation_repository.go`:

```go
package repository

import (
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

type ReconciliationRepository struct {
	DB *gorm.DB
}

func NewReconciliationRepository(db *gorm.DB) *ReconciliationRepository {
	return &ReconciliationRepository{DB: db}
}

// LedgerWalletRow는 검사 1(ledger_wallet)의 한 지갑에 대한 원장-지갑 비교 결과입니다.
type LedgerWalletRow struct {
	WalletID                 uint
	UserID                   uint
	CoinSymbol               string
	AvailableBalance         decimal.Decimal
	LockedBalance            decimal.Decimal
	LedgerAvailableSum       decimal.Decimal
	LedgerLockedSum          decimal.Decimal
	ImpliedInitialAvailable  decimal.NullDecimal
}

// CheckLedgerWalletPage는 지갑 ID 기준 keyset 페이지네이션으로 지갑별 원장 델타 합계와
// 레거시 초기 잔액 후보(가장 이른 원장 항목의 available_balance_after - available_delta)를
// 단일 SQL(하나의 스냅샷)로 조회합니다. 지갑 읽기와 원장 집계를 별도 쿼리로 하면 그 사이에
// 정산이 끼어들어 가짜 위반이 뜰 수 있습니다(tolerance 0 설계라 위양성이 그대로 알람이 됨).
func (r *ReconciliationRepository) CheckLedgerWalletPage(afterWalletID uint, limit int) ([]LedgerWalletRow, error) {
	var rows []LedgerWalletRow
	err := r.DB.Raw(`
		SELECT
			w.id AS wallet_id, w.user_id, w.coin_symbol,
			w.available_balance, w.locked_balance,
			COALESCE(agg.available_delta_sum, 0) AS ledger_available_sum,
			COALESCE(agg.locked_delta_sum, 0) AS ledger_locked_sum,
			first_entry.implied_initial_available
		FROM wallets w
		LEFT JOIN LATERAL (
			SELECT SUM(available_delta) AS available_delta_sum,
			       SUM(locked_delta)    AS locked_delta_sum
			FROM ledger_entries l
			WHERE l.user_id = w.user_id AND l.coin_symbol = w.coin_symbol
		) agg ON true
		LEFT JOIN LATERAL (
			SELECT available_balance_after - available_delta AS implied_initial_available
			FROM ledger_entries l
			WHERE l.user_id = w.user_id AND l.coin_symbol = w.coin_symbol
			ORDER BY l.id ASC
			LIMIT 1
		) first_entry ON true
		WHERE w.id > ?
		ORDER BY w.id
		LIMIT ?
	`, afterWalletID, limit).Scan(&rows).Error
	return rows, err
}
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go build ./...`
Expected: 성공

Run: `go test ./internal/repository/... -run TestIntegrationCheckLedgerWalletPage -v`
Expected: PASS (4 tests) — `GOEXCHANGE_TEST_DATABASE_DSN`이 로컬/CI에 설정되어 있어야 실제로 돈다(설정 없으면 SKIP으로 표시됨, 그 자체는 실패가 아님).

- [ ] **Step 5: Commit**

```bash
git add internal/repository/reconciliation_repository.go internal/repository/reconciliation_repository_integration_test.go
git commit -m "feat(reconciliation): 검사 1(원장-지갑 일치) 리포지토리 추가"
```

---

### Task 3: 검사 2 — 자산별 총량 보존 리포지토리 (`asset_conservation`)

**Files:**
- Modify: `internal/repository/reconciliation_repository.go`
- Modify: `internal/repository/reconciliation_repository_integration_test.go`

**Interfaces:**
- Consumes: `model.Trade{BuyerFee, SellerFee}`, `model.LedgerEntryTypeDevFund`(이미 존재).
- Produces: `repository.AssetConservationRow{CoinSymbol, WalletTotal, FeeTotal, FundedTotal}`, `(*ReconciliationRepository).CheckAssetConservation() ([]AssetConservationRow, error)` — Task 6이 그대로 쓴다.

**격리 전략:** 이 검사는 전역 SUM 집계라 유저 필터링으로는 격리가 안 된다. 테스트에서 반드시 유니크 심볼(`fmt.Sprintf("RCN%d", time.Now().UnixNano())`)을 써서 다른 테스트/시드 데이터와 절대 겹치지 않는 `coin_symbol`로 결과를 필터링해서 단언한다.

- [ ] **Step 1: 실패하는 통합 테스트 추가**

`internal/repository/reconciliation_repository_integration_test.go`에 아래 함수들을 추가한다. 파일 상단 import 블록에 `"fmt"`와 `"time"`을 추가한다:

```go
import (
	"fmt"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)
```

```go
func TestIntegrationCheckAssetConservationBalancesForDevFundedSymbol(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(810)
	defer cleanupRepositoryUsers(t, db, userID)

	symbol := fmt.Sprintf("RCN%d", time.Now().UnixNano())
	wallet := model.Wallet{
		UserID:           userID,
		CoinSymbol:       symbol,
		Quantity:         decimal.NewFromInt(10),
		AvailableBalance: decimal.NewFromInt(10),
		LockedBalance:    decimal.Zero,
	}
	require.NoError(t, db.Create(&wallet).Error)
	require.NoError(t, db.Create(&model.LedgerEntry{
		UserID:                userID,
		CoinSymbol:            symbol,
		EntryType:             model.LedgerEntryTypeDevFund,
		AvailableDelta:        decimal.NewFromInt(10),
		LockedDelta:           decimal.Zero,
		AvailableBalanceAfter: decimal.NewFromInt(10),
		LockedBalanceAfter:    decimal.Zero,
		ReferenceType:         model.LedgerReferenceTypeDevFund,
		ReferenceID:           0,
	}).Error)

	repo := NewReconciliationRepository(db)
	rows, err := repo.CheckAssetConservation()
	require.NoError(t, err)

	row := findAssetConservationRow(rows, symbol)
	require.NotNil(t, row, "expected a row for the isolated test symbol")
	assert.True(t, row.WalletTotal.Equal(decimal.NewFromInt(10)))
	assert.True(t, row.FundedTotal.Equal(decimal.NewFromInt(10)))
	assert.True(t, row.FeeTotal.IsZero(), "non-KRW symbol should have zero fee total")
	assert.True(t, row.WalletTotal.Add(row.FeeTotal).Equal(row.FundedTotal))
}

func findAssetConservationRow(rows []AssetConservationRow, coinSymbol string) *AssetConservationRow {
	for i := range rows {
		if rows[i].CoinSymbol == coinSymbol {
			return &rows[i]
		}
	}
	return nil
}
```

- [ ] **Step 2: 실패 확인**

Run: `go build ./...`
Expected: FAIL — `undefined: AssetConservationRow` / `.CheckAssetConservation undefined`

- [ ] **Step 3: 리포지토리에 메서드 추가**

`internal/repository/reconciliation_repository.go`에 아래를 추가한다(기존 `LedgerWalletRow`/`CheckLedgerWalletPage` 밑에):

```go
// AssetConservationRow는 검사 2(asset_conservation)의 자산 하나에 대한 총량 보존 결과입니다.
type AssetConservationRow struct {
	CoinSymbol  string
	WalletTotal decimal.Decimal
	FeeTotal    decimal.Decimal
	FundedTotal decimal.Decimal
}

// CheckAssetConservation은 자산(coin_symbol)별로 Σ(available+locked) + (KRW일 때만 누적 수수료)
// == Σ(DEV_FUND delta)인지 확인합니다. 수수료는 internal/service/fee.go에서 항상 KRW로만
// 부과되므로 코인 자산은 fee_total이 0이 되어 동일한 쿼리 형태로 일반화됩니다. 지갑 배치
// 순회와 무관하게 매 실행 1회만 수행합니다.
func (r *ReconciliationRepository) CheckAssetConservation() ([]AssetConservationRow, error) {
	var rows []AssetConservationRow
	err := r.DB.Raw(`
		WITH wallet_totals AS (
			SELECT coin_symbol, SUM(available_balance + locked_balance) AS total
			FROM wallets
			GROUP BY coin_symbol
		),
		fee_totals AS (
			SELECT 'KRW' AS coin_symbol,
			       COALESCE(SUM(buyer_fee), 0) + COALESCE(SUM(seller_fee), 0) AS total
			FROM trades
		),
		funded_totals AS (
			SELECT coin_symbol, SUM(available_delta) AS total
			FROM ledger_entries
			WHERE entry_type = 'DEV_FUND'
			GROUP BY coin_symbol
		)
		SELECT
			COALESCE(w.coin_symbol, f.coin_symbol) AS coin_symbol,
			COALESCE(w.total, 0) AS wallet_total,
			COALESCE(fee.total, 0) AS fee_total,
			COALESCE(f.total, 0) AS funded_total
		FROM wallet_totals w
		FULL OUTER JOIN funded_totals f ON f.coin_symbol = w.coin_symbol
		LEFT JOIN fee_totals fee ON fee.coin_symbol = COALESCE(w.coin_symbol, f.coin_symbol)
	`).Scan(&rows).Error
	return rows, err
}
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go build ./...`
Expected: 성공

Run: `go test ./internal/repository/... -run TestIntegrationCheckAssetConservation -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/repository/reconciliation_repository.go internal/repository/reconciliation_repository_integration_test.go
git commit -m "feat(reconciliation): 검사 2(자산별 총량 보존) 리포지토리 추가"
```

---

### Task 4: 검사 3 — 오래된 시장가 주문 리포지토리 + 위반 영속화

**Files:**
- Modify: `internal/repository/reconciliation_repository.go`
- Modify: `internal/repository/reconciliation_repository_integration_test.go`

**Interfaces:**
- Consumes: `model.OrderTypeMarket`, `model.OrderStatusPending`, `model.OrderStatusPartial`(이미 존재), `model.ReconciliationViolation`(Task 1).
- Produces: `repository.StaleMarketOrderRow{OrderID, UserID, CoinSymbol, Status, CreatedAt}`, `(*ReconciliationRepository).CheckStaleMarketOrders(staleAfter time.Duration) ([]StaleMarketOrderRow, error)`, `(*ReconciliationRepository).CreateViolations(violations []model.ReconciliationViolation) error` — Task 6이 그대로 쓴다.

- [ ] **Step 1: 실패하는 통합 테스트 추가**

`internal/repository/reconciliation_repository_integration_test.go`에 추가:

```go
func TestIntegrationCheckStaleMarketOrdersFindsOldPendingMarketOrder(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userID := repositoryTestUserID(820)
	defer cleanupRepositoryUsers(t, db, userID)

	staleOrder := model.Order{
		UserID:       userID,
		CoinSymbol:   "BTC",
		Side:         model.OrderSideBuy,
		OrderType:    model.OrderTypeMarket,
		Status:       model.OrderStatusPending,
		Price:        decimal.Zero,
		Amount:       decimal.NewFromInt(1),
		FilledAmount: decimal.Zero,
		CreatedAt:    time.Now().UTC().Add(-10 * time.Minute),
	}
	require.NoError(t, db.Create(&staleOrder).Error)

	freshOrder := model.Order{
		UserID:       userID,
		CoinSymbol:   "BTC",
		Side:         model.OrderSideBuy,
		OrderType:    model.OrderTypeMarket,
		Status:       model.OrderStatusPending,
		Price:        decimal.Zero,
		Amount:       decimal.NewFromInt(1),
		FilledAmount: decimal.Zero,
		CreatedAt:    time.Now().UTC(),
	}
	require.NoError(t, db.Create(&freshOrder).Error)

	repo := NewReconciliationRepository(db)
	rows, err := repo.CheckStaleMarketOrders(5 * time.Minute)
	require.NoError(t, err)

	assert.NotNil(t, findStaleMarketOrderRow(rows, staleOrder.ID), "10-minute-old pending market order must be flagged")
	assert.Nil(t, findStaleMarketOrderRow(rows, freshOrder.ID), "fresh order must not be flagged")
}

func findStaleMarketOrderRow(rows []StaleMarketOrderRow, orderID uint) *StaleMarketOrderRow {
	for i := range rows {
		if rows[i].OrderID == orderID {
			return &rows[i]
		}
	}
	return nil
}

func TestIntegrationCreateViolationsInsertsOnlyWhenNonEmpty(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewReconciliationRepository(db)

	require.NoError(t, repo.CreateViolations(nil))

	subjectKey := fmt.Sprintf("wallet:test-%d", time.Now().UnixNano())
	defer cleanupReconciliationViolations(t, db, subjectKey)

	violations := []model.ReconciliationViolation{{
		CheckName:  "ledger_wallet",
		SubjectKey: subjectKey,
		Detail:     "test detail",
		DetectedAt: time.Now().UTC(),
	}}
	require.NoError(t, repo.CreateViolations(violations))

	var count int64
	require.NoError(t, db.Model(&model.ReconciliationViolation{}).Where("subject_key = ?", subjectKey).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}
```

- [ ] **Step 2: 실패 확인**

Run: `go build ./...`
Expected: FAIL — `undefined: StaleMarketOrderRow` / `.CheckStaleMarketOrders undefined` / `.CreateViolations undefined`

- [ ] **Step 3: 리포지토리에 메서드 추가**

`internal/repository/reconciliation_repository.go`에 추가(`"time"` import 추가):

```go
// StaleMarketOrderRow는 검사 3(stale_market_order)에서 발견된, 완료되지 않고 오래 남은
// 시장가 주문입니다.
type StaleMarketOrderRow struct {
	OrderID    uint
	UserID     uint
	CoinSymbol string
	Status     string
	CreatedAt  time.Time
}

// CheckStaleMarketOrders는 5분 넘게 PENDING/PARTIAL 상태로 남은 시장가 주문을 찾습니다.
// 시장가는 오더북에 rest하지 않으므로 이 상태가 지속되면 완료(MarketOrderDone 처리 또는
// 그 재시도)가 어딘가에서 유실됐다는 뜻입니다. SettlementRetryWorker의 RetryCount 소진 등으로
// 놓친 케이스의 최종 안전망입니다.
func (r *ReconciliationRepository) CheckStaleMarketOrders(staleAfter time.Duration) ([]StaleMarketOrderRow, error) {
	var rows []StaleMarketOrderRow
	err := r.DB.Raw(`
		SELECT id AS order_id, user_id, coin_symbol, status, created_at
		FROM orders
		WHERE order_type = 'MARKET'
		  AND status IN ('PENDING', 'PARTIAL')
		  AND created_at < ?
	`, time.Now().UTC().Add(-staleAfter)).Scan(&rows).Error
	return rows, err
}

// CreateViolations는 발견된 위반만 일괄 insert합니다. 위반이 없으면 아무것도 쓰지 않습니다.
func (r *ReconciliationRepository) CreateViolations(violations []model.ReconciliationViolation) error {
	if len(violations) == 0 {
		return nil
	}
	return r.DB.Create(&violations).Error
}
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go build ./...`
Expected: 성공

Run: `go test ./internal/repository/... -run "TestIntegrationCheckStaleMarketOrders|TestIntegrationCreateViolations" -v`
Expected: PASS

- [ ] **Step 5: 전체 리포지토리 패키지 회귀 확인**

Run: `go test ./internal/repository/... -v`
Expected: PASS (신규 + 기존 전부)

- [ ] **Step 6: Commit**

```bash
git add internal/repository/reconciliation_repository.go internal/repository/reconciliation_repository_integration_test.go
git commit -m "feat(reconciliation): 검사 3(오래된 시장가 주문) + 위반 영속화 리포지토리 추가"
```

---

### Task 5: 리컨실리에이션 메트릭

**Files:**
- Modify: `internal/metrics/metrics.go`
- Test: `internal/metrics/reconciliation_test.go`

**Interfaces:**
- Produces: `metrics.ReconciliationViolations *prometheus.GaugeVec`(라벨 `check`), `metrics.ReconciliationLastRunTimestamp prometheus.Gauge`, `metrics.ReconciliationCheckErrorsTotal *prometheus.CounterVec`(라벨 `check`) — Task 6의 `ReconciliationWorker`가 그대로 쓴다.

- [ ] **Step 1: 실패하는 테스트 작성**

`internal/metrics/reconciliation_test.go`:

```go
package metrics_test

import (
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

func TestReconciliationViolationsGaugeVecSetsPerCheckLabel(t *testing.T) {
	metrics.ReconciliationViolations.WithLabelValues("ledger_wallet").Set(3)
	metrics.ReconciliationViolations.WithLabelValues("asset_conservation").Set(0)

	assert.Equal(t, float64(3), testutil.ToFloat64(metrics.ReconciliationViolations.WithLabelValues("ledger_wallet")))
	assert.Equal(t, float64(0), testutil.ToFloat64(metrics.ReconciliationViolations.WithLabelValues("asset_conservation")))
}

func TestReconciliationLastRunTimestampGaugeIsSettable(t *testing.T) {
	metrics.ReconciliationLastRunTimestamp.Set(1234567890)
	assert.Equal(t, float64(1234567890), testutil.ToFloat64(metrics.ReconciliationLastRunTimestamp))
}

func TestReconciliationCheckErrorsTotalCounterIncrementsPerCheckLabel(t *testing.T) {
	before := testutil.ToFloat64(metrics.ReconciliationCheckErrorsTotal.WithLabelValues("stale_market_order"))
	metrics.ReconciliationCheckErrorsTotal.WithLabelValues("stale_market_order").Inc()
	after := testutil.ToFloat64(metrics.ReconciliationCheckErrorsTotal.WithLabelValues("stale_market_order"))
	assert.Equal(t, before+1, after)
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/metrics/... -run TestReconciliation -v`
Expected: FAIL — `undefined: metrics.ReconciliationViolations` 등

- [ ] **Step 3: 메트릭 추가**

`internal/metrics/metrics.go`의 `var (...)` 블록 마지막(`OrderSettlementDuration` 뒤)에 추가:

```go
	ReconciliationViolations = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "reconciliation_violations",
		Help: "Violation count from the most recent reconciliation run, labeled by check name.",
	}, []string{"check"})

	ReconciliationLastRunTimestamp = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "reconciliation_last_run_timestamp_seconds",
		Help: "Unix timestamp of the most recent reconciliation run.",
	})

	ReconciliationCheckErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "reconciliation_check_errors_total",
		Help: "Total number of reconciliation check queries that failed to execute, labeled by check name.",
	}, []string{"check"})
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go test ./internal/metrics/... -v`
Expected: PASS (신규 3개 + 기존 전부)

- [ ] **Step 5: Commit**

```bash
git add internal/metrics/metrics.go internal/metrics/reconciliation_test.go
git commit -m "feat(reconciliation): 리컨실리에이션 게이지/카운터 메트릭 추가"
```

---

### Task 6: `ReconciliationWorker` (분류 로직 + 오케스트레이션) + `cmd/main.go` 기동

**Files:**
- Create: `internal/service/reconciliation_worker.go`
- Create: `internal/service/reconciliation_worker_test.go`
- Modify: `cmd/main.go`

**Interfaces:**
- Consumes: `repository.LedgerWalletRow`, `repository.AssetConservationRow`, `repository.StaleMarketOrderRow`(Task 2/3/4), `repository.NewReconciliationRepository`, `model.ReconciliationViolation`(Task 1), `metrics.ReconciliationViolations`/`ReconciliationLastRunTimestamp`/`ReconciliationCheckErrorsTotal`(Task 5), `config.ReconciliationIntervalFromEnv()`(Task 1).
- Produces: `service.ReconciliationWorker{Repository, Interval, Logger}`, `(*ReconciliationWorker).Run(ctx context.Context)`, `(*ReconciliationWorker).RunOnce()` — `cmd/main.go`가 `go reconciliationWorker.Run(context.Background())`로 기동한다.

이 태스크는 DB 없이 페이크 리포지토리로 전부 테스트한다(리포지토리 자체의 SQL 정확성은 Task 2/3/4의 통합 테스트가 이미 검증했다).

- [ ] **Step 1: 실패하는 유닛 테스트 작성**

`internal/service/reconciliation_worker_test.go`:

```go
package service

import (
	"errors"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// classifyLedgerWalletRow

func TestClassifyLedgerWalletRowNoGapIsNotViolated(t *testing.T) {
	row := repository.LedgerWalletRow{
		AvailableBalance:   decimal.NewFromInt(100),
		LockedBalance:      decimal.Zero,
		LedgerAvailableSum: decimal.NewFromInt(100),
		LedgerLockedSum:    decimal.Zero,
	}
	checkName, violated := classifyLedgerWalletRow(row)
	assert.False(t, violated)
	assert.Empty(t, checkName)
}

func TestClassifyLedgerWalletRowLegacyGapExplainedByImpliedInitial(t *testing.T) {
	row := repository.LedgerWalletRow{
		AvailableBalance:        decimal.NewFromInt(700),
		LockedBalance:           decimal.Zero,
		LedgerAvailableSum:      decimal.NewFromInt(500),
		LedgerLockedSum:         decimal.Zero,
		ImpliedInitialAvailable: decimal.NewNullDecimal(decimal.NewFromInt(200)),
	}
	checkName, violated := classifyLedgerWalletRow(row)
	assert.True(t, violated)
	assert.Equal(t, "legacy_mismatch", checkName)
}

func TestClassifyLedgerWalletRowLockedGapAlwaysMeansRealViolation(t *testing.T) {
	row := repository.LedgerWalletRow{
		AvailableBalance:        decimal.NewFromInt(700),
		LockedBalance:           decimal.NewFromInt(50),
		LedgerAvailableSum:      decimal.NewFromInt(500),
		LedgerLockedSum:         decimal.NewFromInt(10),
		ImpliedInitialAvailable: decimal.NewNullDecimal(decimal.NewFromInt(200)),
	}
	checkName, violated := classifyLedgerWalletRow(row)
	assert.True(t, violated)
	assert.Equal(t, "ledger_wallet", checkName, "available gap matches legacy but locked gap is non-zero, so this is a real bug overlapping legacy data")
}

func TestClassifyLedgerWalletRowGapNotExplainedByImpliedInitialIsRealViolation(t *testing.T) {
	row := repository.LedgerWalletRow{
		AvailableBalance:        decimal.NewFromInt(700),
		LockedBalance:           decimal.Zero,
		LedgerAvailableSum:      decimal.NewFromInt(500),
		LedgerLockedSum:         decimal.Zero,
		ImpliedInitialAvailable: decimal.NewNullDecimal(decimal.NewFromInt(100)),
	}
	checkName, violated := classifyLedgerWalletRow(row)
	assert.True(t, violated)
	assert.Equal(t, "ledger_wallet", checkName)
}

func TestClassifyLedgerWalletRowNullImpliedInitialTreatedAsZero(t *testing.T) {
	row := repository.LedgerWalletRow{
		AvailableBalance:   decimal.NewFromInt(50),
		LockedBalance:      decimal.Zero,
		LedgerAvailableSum: decimal.Zero,
		LedgerLockedSum:    decimal.Zero,
		// ImpliedInitialAvailable left zero-value: Valid=false (NULL)
	}
	checkName, violated := classifyLedgerWalletRow(row)
	assert.True(t, violated)
	assert.Equal(t, "ledger_wallet", checkName, "a wallet with no ledger history but a nonzero balance cannot be distinguished from a bug, so it must NOT be classified as legacy")
}

// classifyAssetConservationRow

func TestClassifyAssetConservationRowBalancedIsNotViolated(t *testing.T) {
	row := repository.AssetConservationRow{
		WalletTotal: decimal.NewFromInt(100),
		FeeTotal:    decimal.NewFromInt(5),
		FundedTotal: decimal.NewFromInt(105),
	}
	assert.False(t, classifyAssetConservationRow(row))
}

func TestClassifyAssetConservationRowImbalancedIsViolated(t *testing.T) {
	row := repository.AssetConservationRow{
		WalletTotal: decimal.NewFromInt(100),
		FeeTotal:    decimal.NewFromInt(5),
		FundedTotal: decimal.NewFromInt(200),
	}
	assert.True(t, classifyAssetConservationRow(row))
}

// RunOnce orchestration with fakes

type fakeReconciliationRepository struct {
	ledgerWalletRows       []repository.LedgerWalletRow
	ledgerWalletErr        error
	assetConservationRows  []repository.AssetConservationRow
	assetConservationErr   error
	staleMarketOrderRows   []repository.StaleMarketOrderRow
	staleMarketOrderErr    error
	createViolationsCalls  [][]model.ReconciliationViolation
	createViolationsErr    error
	ledgerWalletPageCalls  []uint
}

func (f *fakeReconciliationRepository) CheckLedgerWalletPage(afterWalletID uint, limit int) ([]repository.LedgerWalletRow, error) {
	f.ledgerWalletPageCalls = append(f.ledgerWalletPageCalls, afterWalletID)
	if f.ledgerWalletErr != nil {
		return nil, f.ledgerWalletErr
	}
	if len(f.ledgerWalletPageCalls) > 1 {
		return nil, nil // second page is always empty in these tests
	}
	return f.ledgerWalletRows, nil
}

func (f *fakeReconciliationRepository) CheckAssetConservation() ([]repository.AssetConservationRow, error) {
	return f.assetConservationRows, f.assetConservationErr
}

func (f *fakeReconciliationRepository) CheckStaleMarketOrders(time.Duration) ([]repository.StaleMarketOrderRow, error) {
	return f.staleMarketOrderRows, f.staleMarketOrderErr
}

func (f *fakeReconciliationRepository) CreateViolations(violations []model.ReconciliationViolation) error {
	f.createViolationsCalls = append(f.createViolationsCalls, violations)
	return f.createViolationsErr
}

func TestRunOnceRecordsLedgerWalletViolationAndSetsGauges(t *testing.T) {
	repo := &fakeReconciliationRepository{
		ledgerWalletRows: []repository.LedgerWalletRow{{
			WalletID:           42,
			UserID:              7,
			CoinSymbol:          "BTC",
			AvailableBalance:    decimal.NewFromInt(700),
			LockedBalance:       decimal.Zero,
			LedgerAvailableSum:  decimal.NewFromInt(500),
			LedgerLockedSum:     decimal.Zero,
		}},
	}
	worker := &ReconciliationWorker{Repository: repo, Logger: discardServiceLogger()}

	worker.RunOnce()

	require.Len(t, repo.createViolationsCalls, 1)
	require.Len(t, repo.createViolationsCalls[0], 1)
	assert.Equal(t, "ledger_wallet", repo.createViolationsCalls[0][0].CheckName)
	assert.Equal(t, "wallet:42", repo.createViolationsCalls[0][0].SubjectKey)
	assert.Equal(t, float64(1), testutil.ToFloat64(metrics.ReconciliationViolations.WithLabelValues("ledger_wallet")))
	assert.Equal(t, float64(0), testutil.ToFloat64(metrics.ReconciliationViolations.WithLabelValues("legacy_mismatch")))
}

func TestRunOnceSetsZeroGaugeWhenNoViolations(t *testing.T) {
	repo := &fakeReconciliationRepository{}
	worker := &ReconciliationWorker{Repository: repo, Logger: discardServiceLogger()}

	worker.RunOnce()

	assert.Empty(t, repo.createViolationsCalls, "no violations found, so CreateViolations must not be called with an empty slice inside RunOnce's own persist step")
	assert.Equal(t, float64(0), testutil.ToFloat64(metrics.ReconciliationViolations.WithLabelValues("ledger_wallet")))
	assert.Equal(t, float64(0), testutil.ToFloat64(metrics.ReconciliationViolations.WithLabelValues("asset_conservation")))
	assert.Equal(t, float64(0), testutil.ToFloat64(metrics.ReconciliationViolations.WithLabelValues("stale_market_order")))
}

func TestRunOnceIncrementsErrorCounterAndSkipsGaugeOnQueryFailure(t *testing.T) {
	repo := &fakeReconciliationRepository{assetConservationErr: errors.New("db unavailable")}
	worker := &ReconciliationWorker{Repository: repo, Logger: discardServiceLogger()}

	before := testutil.ToFloat64(metrics.ReconciliationCheckErrorsTotal.WithLabelValues("asset_conservation"))
	beforeGauge := testutil.ToFloat64(metrics.ReconciliationViolations.WithLabelValues("asset_conservation"))

	worker.RunOnce()

	after := testutil.ToFloat64(metrics.ReconciliationCheckErrorsTotal.WithLabelValues("asset_conservation"))
	afterGauge := testutil.ToFloat64(metrics.ReconciliationViolations.WithLabelValues("asset_conservation"))
	assert.Equal(t, before+1, after)
	assert.Equal(t, beforeGauge, afterGauge, "gauge must not be overwritten when the check itself failed")
}

func TestRunOncePaginatesLedgerWalletCheckUntilPageIsShort(t *testing.T) {
	fullPage := make([]repository.LedgerWalletRow, reconciliationPageSize)
	for i := range fullPage {
		fullPage[i] = repository.LedgerWalletRow{WalletID: uint(i + 1)}
	}
	repo := &fakeReconciliationRepository{ledgerWalletRows: fullPage}
	worker := &ReconciliationWorker{Repository: repo, Logger: discardServiceLogger()}

	worker.RunOnce()

	require.Len(t, repo.ledgerWalletPageCalls, 2, "a full first page must trigger a second page request")
	assert.Equal(t, uint(0), repo.ledgerWalletPageCalls[0])
	assert.Equal(t, uint(reconciliationPageSize), repo.ledgerWalletPageCalls[1])
}
```

- [ ] **Step 2: 실패 확인**

Run: `go build ./...`
Expected: FAIL — `undefined: ReconciliationWorker` / `classifyLedgerWalletRow` 등

- [ ] **Step 3: `ReconciliationWorker` 구현**

`internal/service/reconciliation_worker.go`:

```go
package service

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
)

const (
	defaultReconciliationInterval  = time.Hour
	reconciliationPageSize         = 500
	staleMarketOrderThreshold      = 5 * time.Minute
	maxReconciliationDetailLength  = 2048
)

type reconciliationRepository interface {
	CheckLedgerWalletPage(afterWalletID uint, limit int) ([]repository.LedgerWalletRow, error)
	CheckAssetConservation() ([]repository.AssetConservationRow, error)
	CheckStaleMarketOrders(staleAfter time.Duration) ([]repository.StaleMarketOrderRow, error)
	CreateViolations(violations []model.ReconciliationViolation) error
}

// ReconciliationWorker는 원장-지갑 정합성, 자산별 총량 보존, 오래된 시장가 주문 잔존을
// 주기적으로 검사하고 위반을 내구 기록 + 메트릭으로 보고합니다. 자동 교정은 하지 않습니다 —
// 탐지/보고만 합니다.
type ReconciliationWorker struct {
	Repository reconciliationRepository
	Interval   time.Duration
	Logger     *log.Logger
}

func (w *ReconciliationWorker) Run(ctx context.Context) {
	w.RunOnce() // 기동 직후 1회 — 배포/재시작 직후가 정합성이 가장 의심스러운 시점
	ticker := time.NewTicker(w.interval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.RunOnce()
		}
	}
}

func (w *ReconciliationWorker) RunOnce() {
	w.runLedgerWalletCheck()
	w.runAssetConservationCheck()
	w.runStaleMarketOrderCheck()
	metrics.ReconciliationLastRunTimestamp.Set(float64(time.Now().UTC().Unix()))
}

func (w *ReconciliationWorker) runLedgerWalletCheck() {
	var violations []model.ReconciliationViolation
	var lastWalletID uint

	for {
		rows, err := w.Repository.CheckLedgerWalletPage(lastWalletID, reconciliationPageSize)
		if err != nil {
			w.logf("reconciliation: ledger_wallet check failed: %v", err)
			metrics.ReconciliationCheckErrorsTotal.WithLabelValues("ledger_wallet").Inc()
			return
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			if checkName, violated := classifyLedgerWalletRow(row); violated {
				violations = append(violations, model.ReconciliationViolation{
					CheckName:  checkName,
					SubjectKey: fmt.Sprintf("wallet:%d", row.WalletID),
					Detail:     ledgerWalletViolationDetail(row),
					DetectedAt: time.Now().UTC(),
				})
			}
			lastWalletID = row.WalletID
		}
		if len(rows) < reconciliationPageSize {
			break
		}
	}

	w.persist(violations)
	ledgerWalletCount := 0
	legacyMismatchCount := 0
	for _, v := range violations {
		if v.CheckName == "legacy_mismatch" {
			legacyMismatchCount++
		} else {
			ledgerWalletCount++
		}
	}
	metrics.ReconciliationViolations.WithLabelValues("ledger_wallet").Set(float64(ledgerWalletCount))
	metrics.ReconciliationViolations.WithLabelValues("legacy_mismatch").Set(float64(legacyMismatchCount))
}

func (w *ReconciliationWorker) runAssetConservationCheck() {
	rows, err := w.Repository.CheckAssetConservation()
	if err != nil {
		w.logf("reconciliation: asset_conservation check failed: %v", err)
		metrics.ReconciliationCheckErrorsTotal.WithLabelValues("asset_conservation").Inc()
		return
	}

	var violations []model.ReconciliationViolation
	for _, row := range rows {
		if classifyAssetConservationRow(row) {
			violations = append(violations, model.ReconciliationViolation{
				CheckName:  "asset_conservation",
				SubjectKey: fmt.Sprintf("coin:%s", row.CoinSymbol),
				Detail:     assetConservationViolationDetail(row),
				DetectedAt: time.Now().UTC(),
			})
		}
	}

	w.persist(violations)
	metrics.ReconciliationViolations.WithLabelValues("asset_conservation").Set(float64(len(violations)))
}

func (w *ReconciliationWorker) runStaleMarketOrderCheck() {
	rows, err := w.Repository.CheckStaleMarketOrders(staleMarketOrderThreshold)
	if err != nil {
		w.logf("reconciliation: stale_market_order check failed: %v", err)
		metrics.ReconciliationCheckErrorsTotal.WithLabelValues("stale_market_order").Inc()
		return
	}

	violations := make([]model.ReconciliationViolation, 0, len(rows))
	for _, row := range rows {
		violations = append(violations, model.ReconciliationViolation{
			CheckName:  "stale_market_order",
			SubjectKey: fmt.Sprintf("order:%d", row.OrderID),
			Detail:     staleMarketOrderViolationDetail(row),
			DetectedAt: time.Now().UTC(),
		})
	}

	w.persist(violations)
	metrics.ReconciliationViolations.WithLabelValues("stale_market_order").Set(float64(len(violations)))
}

func (w *ReconciliationWorker) persist(violations []model.ReconciliationViolation) {
	if len(violations) == 0 {
		return
	}
	if err := w.Repository.CreateViolations(violations); err != nil {
		w.logf("reconciliation: persist violations failed: %v", err)
	}
}

// classifyLedgerWalletRow는 위반이 레거시 데이터로 완전히 설명되는지(legacy_mismatch) 아니면
// 진짜 버그인지(ledger_wallet) 판정합니다. locked_delta는 레거시 구체화의 영향을 받지 않으므로
// (ledger.go의 ledgerEntryFromWalletUpdate가 available만 폴백 기준으로 계산) locked gap이
// 0이 아니면 항상 ledger_wallet입니다. 원장 항목이 하나도 없는 지갑(implied가 NULL)은
// 레거시 패턴과 구분할 근거가 없으므로 0으로 취급해 안전하게 ledger_wallet으로 분류합니다.
func classifyLedgerWalletRow(row repository.LedgerWalletRow) (checkName string, violated bool) {
	availableGap := row.AvailableBalance.Sub(row.LedgerAvailableSum)
	lockedGap := row.LockedBalance.Sub(row.LedgerLockedSum)
	if availableGap.IsZero() && lockedGap.IsZero() {
		return "", false
	}

	implied := decimal.Zero
	if row.ImpliedInitialAvailable.Valid {
		implied = row.ImpliedInitialAvailable.Decimal
	}
	if availableGap.Equal(implied) && lockedGap.IsZero() {
		return "legacy_mismatch", true
	}
	return "ledger_wallet", true
}

func classifyAssetConservationRow(row repository.AssetConservationRow) bool {
	return !row.WalletTotal.Add(row.FeeTotal).Equal(row.FundedTotal)
}

func ledgerWalletViolationDetail(row repository.LedgerWalletRow) string {
	implied := "null"
	if row.ImpliedInitialAvailable.Valid {
		implied = row.ImpliedInitialAvailable.Decimal.String()
	}
	return truncateReconciliationDetail(fmt.Sprintf(
		"wallet_id=%d user_id=%d coin_symbol=%s available_balance=%s locked_balance=%s ledger_available_sum=%s ledger_locked_sum=%s implied_initial_available=%s",
		row.WalletID, row.UserID, row.CoinSymbol,
		row.AvailableBalance.String(), row.LockedBalance.String(),
		row.LedgerAvailableSum.String(), row.LedgerLockedSum.String(), implied,
	))
}

func assetConservationViolationDetail(row repository.AssetConservationRow) string {
	return truncateReconciliationDetail(fmt.Sprintf(
		"coin_symbol=%s wallet_total=%s fee_total=%s funded_total=%s",
		row.CoinSymbol, row.WalletTotal.String(), row.FeeTotal.String(), row.FundedTotal.String(),
	))
}

func staleMarketOrderViolationDetail(row repository.StaleMarketOrderRow) string {
	return truncateReconciliationDetail(fmt.Sprintf(
		"order_id=%d user_id=%d coin_symbol=%s status=%s created_at=%s",
		row.OrderID, row.UserID, row.CoinSymbol, row.Status, row.CreatedAt.Format(time.RFC3339),
	))
}

func truncateReconciliationDetail(s string) string {
	if len(s) <= maxReconciliationDetailLength {
		return s
	}
	return s[:maxReconciliationDetailLength]
}

func (w *ReconciliationWorker) interval() time.Duration {
	if w.Interval > 0 {
		return w.Interval
	}
	return defaultReconciliationInterval
}

func (w *ReconciliationWorker) logf(format string, args ...interface{}) {
	logger := w.Logger
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf(format, args...)
}
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go build ./...`
Expected: 성공

Run: `go test ./internal/service/... -run "TestClassifyLedgerWalletRow|TestClassifyAssetConservationRow|TestRunOnce" -v`
Expected: PASS (전부)

- [ ] **Step 5: `cmd/main.go`에 기동 코드 추가**

`cmd/main.go`의 `settlementRetryWorker` 기동 블록(`go settlementRetryWorker.Run(context.Background())`) 바로 뒤에 추가:

```go
	reconciliationWorker := &service.ReconciliationWorker{
		Repository: repository.NewReconciliationRepository(config.DB),
		Interval:   config.ReconciliationIntervalFromEnv(),
	}
	go reconciliationWorker.Run(context.Background())
```

- [ ] **Step 6: 전체 빌드 + 전체 테스트 확인**

Run: `go build ./...`
Expected: 성공

Run: `go vet ./...`
Expected: 에러 없음

Run: `go test ./... -count=1`
Expected: PASS 전체 (통합 테스트는 `GOEXCHANGE_TEST_DATABASE_DSN` 설정 여부에 따라 SKIP될 수 있음 — SKIP은 실패가 아님)

- [ ] **Step 7: Commit**

```bash
git add internal/service/reconciliation_worker.go internal/service/reconciliation_worker_test.go cmd/main.go
git commit -m "feat(reconciliation): ReconciliationWorker 구현 및 서버 기동에 연결"
```

---

## 완료 후 수동 확인 (스펙의 "검증 방법" 절)

플랜 실행이 끝나면, 스펙 문서가 요구하는 아래 항목은 로컬/스테이징 서버 기동으로 별도 확인한다(자동화된 태스크에는 포함하지 않음):

- 서버를 로컬(스트레스 DB 기준, dev-fund만 사용)로 기동한 뒤 로그에서 `ReconciliationWorker`의 첫 실행이 "위반 0건"을 보고하는지 확인한다. `legacy_mismatch`도 0건이어야 정상이며, 0건이 아니면 원인을 조사한다.

## 범위 밖 (스펙과 동일)

- krw/quantity ↔ available/locked 이중 필드 통합 데이터 마이그레이션.
- 검사 4(미체결 주문별 잔여 hold 재계산) — 2단계.
- 위반 자동 복구/교정.
- 원장 증분 집계/스냅샷 테이블로의 전환.
