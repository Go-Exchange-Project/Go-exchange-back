# 정산 지갑 갱신 배치화 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 정산 트랜잭션당 4번의 개별 지갑 UPDATE를 하나의 다중 행 UPDATE로 묶어서, 인프라 증설이 아니라 코드 레벨에서 Postgres 쿼리 왕복 횟수를 줄인다.

**Architecture:** `internal/repository`에 `WalletBatchUpdate` 타입과 `WalletRepository.BatchUpdateBalances([]WalletBatchUpdate) error`를 추가한다 — `UPDATE wallets AS w SET ... FROM (VALUES ...) AS v(...) WHERE w.id = v.id` 형태의 단일 raw SQL을 실행한다. `internal/service/settlement_service.go`의 4번의 개별 `walletRepo.UpdateBalances`/`UpdateBalancesAndAvgBuyPrice` 호출을 이 배치 호출 하나로 교체한다.

**Tech Stack:** GORM(`*gorm.DB.Exec`), PostgreSQL 다중 행 UPDATE(`UPDATE ... FROM (VALUES ...)`), `github.com/shopspring/decimal`.

## Global Constraints

- 정산 결과(지갑 최종 상태)는 기존과 완전히 동일해야 한다 — 이 변경은 순수 내부 구현 교체이며 관측 가능한 동작을 바꾸지 않는다.
- 기존 정산 통합 테스트(`internal/service/service_integration_test.go`의 `TestIntegrationSettleTrade...` 7개)가 전부 그대로 통과해야 한다.
- `internal/repository/wallet_reporsitory.go`, `internal/service/settlement_service.go`, 그리고 각각의 테스트 파일만 수정한다 — 주문 조회/갱신, 지갑 조회를 합치는 건 이번 스코프가 아니다.

---

### Task 1: `WalletRepository.BatchUpdateBalances` 추가

**Files:**
- Modify: `internal/repository/wallet_reporsitory.go`
- Modify: `internal/repository/wallet_repository_integration_test.go`

**Interfaces:**
- Produces: `type WalletBatchUpdate struct { WalletID uint; AvailableBalance, LockedBalance, KRW, Quantity, AvgBuyPrice decimal.Decimal }`, `func (r *WalletRepository) BatchUpdateBalances(updates []WalletBatchUpdate) error` — Task 2에서 `internal/service/settlement_service.go`가 이 정확한 타입/시그니처로 호출한다.

- [ ] **Step 1: 실패하는 통합 테스트 작성**

`internal/repository/wallet_repository_integration_test.go` 파일 끝에 추가:

```go
func TestIntegrationBatchUpdateBalancesUpdatesMultipleWallets(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	userA := repositoryTestUserID(20)
	userB := repositoryTestUserID(21)
	defer cleanupRepositoryUsers(t, db, userA, userB)

	repo := NewWalletRepository(db)
	walletA := model.Wallet{UserID: userA, CoinSymbol: "KRW", KRW: decimal.NewFromInt(1000), AvailableBalance: decimal.NewFromInt(1000), LockedBalance: decimal.Zero}
	walletB := model.Wallet{UserID: userB, CoinSymbol: "BTC", Quantity: decimal.NewFromInt(5), AvailableBalance: decimal.NewFromInt(5), LockedBalance: decimal.Zero, AvgBuyPrice: decimal.NewFromInt(90)}
	require.NoError(t, db.Create(&walletA).Error)
	require.NoError(t, db.Create(&walletB).Error)

	err := repo.BatchUpdateBalances([]WalletBatchUpdate{
		{WalletID: walletA.ID, AvailableBalance: decimal.NewFromInt(400), LockedBalance: decimal.NewFromInt(600), KRW: decimal.NewFromInt(1000), Quantity: decimal.Zero, AvgBuyPrice: decimal.Zero},
		{WalletID: walletB.ID, AvailableBalance: decimal.NewFromInt(2), LockedBalance: decimal.NewFromInt(3), KRW: decimal.Zero, Quantity: decimal.NewFromInt(5), AvgBuyPrice: decimal.NewFromInt(100)},
	})
	require.NoError(t, err)

	updatedA, err := repo.FindByUserIDAndCoinSymbol(userA, "KRW")
	require.NoError(t, err)
	assert.True(t, updatedA.AvailableBalance.Equal(decimal.NewFromInt(400)))
	assert.True(t, updatedA.LockedBalance.Equal(decimal.NewFromInt(600)))
	assert.True(t, updatedA.KRW.Equal(decimal.NewFromInt(1000)))

	updatedB, err := repo.FindByUserIDAndCoinSymbol(userB, "BTC")
	require.NoError(t, err)
	assert.True(t, updatedB.AvailableBalance.Equal(decimal.NewFromInt(2)))
	assert.True(t, updatedB.LockedBalance.Equal(decimal.NewFromInt(3)))
	assert.True(t, updatedB.Quantity.Equal(decimal.NewFromInt(5)))
	assert.True(t, updatedB.AvgBuyPrice.Equal(decimal.NewFromInt(100)))
}

func TestIntegrationBatchUpdateBalancesEmptySliceIsNoop(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewWalletRepository(db)

	require.NoError(t, repo.BatchUpdateBalances(nil))
}

func TestIntegrationBatchUpdateBalancesMissingWalletReturnsError(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewWalletRepository(db)

	err := repo.BatchUpdateBalances([]WalletBatchUpdate{
		{WalletID: 999999999, AvailableBalance: decimal.NewFromInt(1), LockedBalance: decimal.Zero, KRW: decimal.NewFromInt(1), Quantity: decimal.Zero, AvgBuyPrice: decimal.Zero},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected")
}
```

- [ ] **Step 2: 테스트 실행해서 실패 확인**

Run: `go test ./internal/repository/... -run TestIntegrationBatchUpdateBalances -v`
Expected: FAIL — `WalletBatchUpdate`/`BatchUpdateBalances`가 아직 정의되지 않아 컴파일 에러 (`undefined: WalletBatchUpdate`).

(테스트 DB가 없는 환경이면 `GOEXCHANGE_TEST_DATABASE_DSN` 미설정으로 스킵될 수 있다 — 이 경우 최소한 `go build ./internal/repository/...`로 컴파일 에러를 확인한다.)

- [ ] **Step 3: `internal/repository/wallet_reporsitory.go`에 구현 추가**

파일 상단 import 블록, 현재:

```go
import (
	"errors"
	"fmt"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)
```

다음과 같이 수정한다:

```go
import (
	"errors"
	"fmt"
	"strings"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)
```

파일 끝(`requireRowsAffected` 함수 다음)에 추가:

```go

type WalletBatchUpdate struct {
	WalletID         uint
	AvailableBalance decimal.Decimal
	LockedBalance    decimal.Decimal
	KRW              decimal.Decimal
	Quantity         decimal.Decimal
	AvgBuyPrice      decimal.Decimal
}

func (r *WalletRepository) BatchUpdateBalances(updates []WalletBatchUpdate) error {
	if len(updates) == 0 {
		return nil
	}

	rows := make([]string, 0, len(updates))
	args := make([]interface{}, 0, len(updates)*6)
	for i, u := range updates {
		base := i * 6
		rows = append(rows, fmt.Sprintf(
			"($%d::bigint, $%d::numeric, $%d::numeric, $%d::numeric, $%d::numeric, $%d::numeric)",
			base+1, base+2, base+3, base+4, base+5, base+6,
		))
		args = append(args, u.WalletID, u.AvailableBalance, u.LockedBalance, u.KRW, u.Quantity, u.AvgBuyPrice)
	}

	sql := fmt.Sprintf(`
		UPDATE wallets AS w
		SET
			available_balance = v.available_balance,
			locked_balance = v.locked_balance,
			krw = v.krw,
			quantity = v.quantity,
			avg_buy_price = v.avg_buy_price
		FROM (VALUES %s) AS v(id, available_balance, locked_balance, krw, quantity, avg_buy_price)
		WHERE w.id = v.id`,
		strings.Join(rows, ", "),
	)

	result := r.DB.Exec(sql, args...)
	if result.Error != nil {
		return result.Error
	}
	if int(result.RowsAffected) != len(updates) {
		return fmt.Errorf("wallet batch update affected %d rows, expected %d", result.RowsAffected, len(updates))
	}
	return nil
}
```

- [ ] **Step 4: 테스트 실행해서 통과 확인**

Run: `go test ./internal/repository/... -run TestIntegrationBatchUpdateBalances -v`
Expected: PASS (테스트 DB가 없으면 스킵 — 그 경우 Step 5의 빌드 확인으로 대체한다).

- [ ] **Step 5: 빌드 및 전체 회귀 확인**

```bash
go build ./...
go vet ./...
go test ./internal/repository/... -v
```

Expected: 전부 에러 없이 종료, `internal/repository` 패키지의 모든 테스트 PASS(또는 DB 없어서 스킵).

- [ ] **Step 6: 커밋**

```bash
git add internal/repository/wallet_reporsitory.go internal/repository/wallet_repository_integration_test.go
git commit -m "$(cat <<'MSG'
feat(repository): 지갑 잔액 배치 갱신 메서드 추가

정산 트랜잭션당 지갑 UPDATE 4회를 다중 행 UPDATE 1회로 묶기 위해,
WalletBatchUpdate 타입과 BatchUpdateBalances 메서드를 추가한다.
UPDATE ... FROM (VALUES ...) 패턴으로 단일 SQL 왕복만 발생시킨다.
MSG
)"
```

---

### Task 2: `settlement_service.go`에서 배치 갱신 사용

**Files:**
- Modify: `internal/service/settlement_service.go`

**Interfaces:**
- Consumes: `repository.WalletBatchUpdate`, `walletRepo.BatchUpdateBalances([]repository.WalletBatchUpdate) error` (Task 1에서 정의).

- [ ] **Step 1: 4개의 개별 지갑 UPDATE 호출을 배치 호출로 교체**

`internal/service/settlement_service.go`의 현재:

```go
		if err := walletRepo.UpdateBalances(participants.BuyerUserID, model.KRWAssetSymbol, buyerKRWUpdate.AvailableBalance, buyerKRWUpdate.LockedBalance); err != nil {
			return err
		}
		if err := walletRepo.UpdateBalancesAndAvgBuyPrice(participants.BuyerUserID, trade.CoinSymbol, buyerCoinUpdate.AvailableBalance, buyerCoinUpdate.LockedBalance, buyerCoinUpdate.AvgBuyPrice); err != nil {
			return err
		}
		if err := walletRepo.UpdateBalancesAndAvgBuyPrice(participants.SellerUserID, trade.CoinSymbol, sellerCoinUpdate.AvailableBalance, sellerCoinUpdate.LockedBalance, sellerCoinUpdate.AvgBuyPrice); err != nil {
			return err
		}
		if err := walletRepo.UpdateBalances(participants.SellerUserID, model.KRWAssetSymbol, sellerKRWUpdate.AvailableBalance, sellerKRWUpdate.LockedBalance); err != nil {
			return err
		}
```

다음과 같이 수정한다:

```go
		if err := walletRepo.BatchUpdateBalances([]repository.WalletBatchUpdate{
			{WalletID: buyerKRW.ID, AvailableBalance: buyerKRWUpdate.AvailableBalance, LockedBalance: buyerKRWUpdate.LockedBalance, KRW: buyerKRWUpdate.KRW, Quantity: buyerKRWUpdate.Quantity, AvgBuyPrice: buyerKRWUpdate.AvgBuyPrice},
			{WalletID: buyerCoin.ID, AvailableBalance: buyerCoinUpdate.AvailableBalance, LockedBalance: buyerCoinUpdate.LockedBalance, KRW: buyerCoinUpdate.KRW, Quantity: buyerCoinUpdate.Quantity, AvgBuyPrice: buyerCoinUpdate.AvgBuyPrice},
			{WalletID: sellerCoin.ID, AvailableBalance: sellerCoinUpdate.AvailableBalance, LockedBalance: sellerCoinUpdate.LockedBalance, KRW: sellerCoinUpdate.KRW, Quantity: sellerCoinUpdate.Quantity, AvgBuyPrice: sellerCoinUpdate.AvgBuyPrice},
			{WalletID: sellerKRW.ID, AvailableBalance: sellerKRWUpdate.AvailableBalance, LockedBalance: sellerKRWUpdate.LockedBalance, KRW: sellerKRWUpdate.KRW, Quantity: sellerKRWUpdate.Quantity, AvgBuyPrice: sellerKRWUpdate.AvgBuyPrice},
		}); err != nil {
			return err
		}
```

(`buyerKRW`, `buyerCoin`, `sellerCoin`, `sellerKRW`는 이미 이 함수 앞부분에서 `FOR UPDATE`로 조회해 둔 `*model.Wallet`이라 `.ID`를 그대로 쓸 수 있다. `repository` 패키지는 이미 파일 상단에 import되어 있다.)

- [ ] **Step 2: 기존 정산 통합 테스트로 회귀 확인**

```bash
go build ./...
go vet ./...
go test ./internal/service/... -run TestIntegrationSettleTrade -v
```

Expected: `TestIntegrationSettleTradeUpdatesTradeOrdersAndWallets`, `TestIntegrationSettleTradeCreatesMissingDestinationWallets`, `TestIntegrationSettleTradeFailureRollsBackAllWrites`, `TestIntegrationSettleTradeDuplicateIsIdempotent`, `TestIntegrationSettleTradeSameIdempotencyKeyDifferentPayloadReturnsConflict`, `TestIntegrationSettleTradeRejectsCancelledBuyOrder`, `TestIntegrationSettleTradeRejectsCancelledSellOrder` 전부 PASS(또는 테스트 DB 없어서 스킵). 하나라도 실패하면 배치 갱신이 기존 동작과 다르다는 뜻이므로, 원인을 찾아 고치기 전까지 다음 단계로 넘어가지 않는다.

- [ ] **Step 3: 전체 테스트 스위트 확인**

```bash
go test ./... 2>&1 | tail -40
```

Expected: 모든 패키지 PASS.

- [ ] **Step 4: 커밋**

```bash
git add internal/service/settlement_service.go
git commit -m "$(cat <<'MSG'
feat: 정산 지갑 갱신을 배치 UPDATE로 전환

SettleTrade의 4번의 개별 지갑 UPDATE 호출을 BatchUpdateBalances
한 번으로 교체해, 정산 트랜잭션당 쿼리 왕복을 줄인다. 정산 결과는
기존과 동일하며, 기존 정산 통합 테스트로 회귀를 확인했다.
MSG
)"
```
