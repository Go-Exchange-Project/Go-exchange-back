package service

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/config"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func testDatabaseDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("GOEXCHANGE_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("GOEXCHANGE_TEST_DATABASE_DSN is not set; skipping Postgres integration test")
	}
	return dsn
}

// 설계 §4.3: 배치가 57014로 실패하면 단건 폴백을 하지 않는다 — 배치의 모든
// 요청에 unavailable 계열 오류를 돌려준다(HTTP 503 매핑은 D7·D12가 맡는다).
// 55P03·그 밖의 오류는 기존 폴백을 유지한다(TestIntegrationHoldCoordinatorFallsBackOnBatchError, 변경 없음).
//
// 재현: account_balances 두 행(USER_AVAILABLE·USER_LOCKED)을 별도 커넥션에서
// FOR UPDATE로 잡아두고 커밋하지 않는다. 코디네이터 풀은 statement_timeout을
// 짧게(150ms), lock_timeout은 훨씬 길게(5s) 둬 lock_timeout(55P03)이 아니라
// statement_timeout(57014)이 먼저 걸리게 한다.
func TestIntegrationHoldCoordinatorSkipsFallbackOnStatementTimeout(t *testing.T) {
	db := openServiceIntegrationDB(t)
	dsn := testDatabaseDSN(t)

	buyerID := serviceTestUserID(760)
	defer cleanupServiceUsers(t, db, buyerID)

	accountRepo := repository.NewAccountRepository(db)
	accounts, err := accountRepo.EnsureAccounts([]repository.AccountSpec{
		{AccountType: model.AccountUserAvailable, OwnerUserID: &buyerID, Asset: model.KRWAssetSymbol},
		{AccountType: model.AccountUserLocked, OwnerUserID: &buyerID, Asset: model.KRWAssetSymbol},
	})
	require.NoError(t, err)
	require.Len(t, accounts, 2)

	seedLedgerFunds(t, db, buyerID, model.KRWAssetSymbol, decimal.NewFromInt(10_000))

	// 잠금 커넥션: 두 계정의 잔액 캐시 행을 FOR UPDATE로 잡고 테스트가 끝날 때까지
	// 커밋·롤백하지 않는다.
	sqlDB, err := db.DB()
	require.NoError(t, err)
	ctx := context.Background()
	lockerConn, err := sqlDB.Conn(ctx)
	require.NoError(t, err)
	defer lockerConn.Close()
	lockerTx, err := lockerConn.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = lockerTx.Rollback() }()
	_, err = lockerTx.ExecContext(ctx, fmt.Sprintf(
		"SELECT * FROM account_balances WHERE account_id IN (%d, %d) FOR UPDATE", accounts[0].ID, accounts[1].ID))
	require.NoError(t, err)

	shortDB, shortSQLDB, err := config.OpenDBWithProfile(dsn, config.DBTimeoutProfile{
		Name:                            "test-hold-statement-timeout",
		StatementTimeout:                150 * time.Millisecond,
		LockTimeout:                     5 * time.Second,
		IdleInTransactionSessionTimeout: 0,
		MaxOpenConns:                    2,
		MaxIdleConns:                    2,
	})
	require.NoError(t, err)
	defer shortSQLDB.Close()

	coordinator := NewHoldCoordinator(shortDB, repository.NewOrderRepository(shortDB), NewLedgerService(shortDB),
		repository.NewOrderIdempotencyRepository(shortDB), 1)
	go coordinator.Run()
	defer coordinator.Shutdown()

	beforeFallbacks := testutil.ToFloat64(metrics.HoldBatchFallbacksTotal)
	beforeTimeout := testutil.ToFloat64(metrics.DBTimeoutTotal.WithLabelValues("57014", "hold_batch"))

	order := &model.Order{
		UserID: buyerID, CoinSymbol: "BTC", Side: model.OrderSideBuy, OrderType: model.OrderTypeLimit,
		Price: decimal.NewFromInt(100), Amount: decimal.NewFromInt(1), Status: model.OrderStatusPending,
	}
	_, submitErr := coordinator.Submit(order)
	require.Error(t, submitErr)

	kind, ok := DomainErrorKind(submitErr)
	require.True(t, ok, "unavailable 계열 DomainError여야 한다: %v", submitErr)
	require.Equal(t, ErrorKindUnavailable, kind)

	afterFallbacks := testutil.ToFloat64(metrics.HoldBatchFallbacksTotal)
	require.Equal(t, beforeFallbacks, afterFallbacks, "57014는 단건 폴백(fallbackPerRequest)을 타지 않아야 한다")

	// P1-2(설계 §4.2 계측 경로 행렬): hold 배치가 즉시 unavailable로 응답하는
	// 지점에서도 goexchange_db_timeout_total{sqlstate="57014",path="hold_batch"}가
	// 늘어야 한다.
	afterTimeout := testutil.ToFloat64(metrics.DBTimeoutTotal.WithLabelValues("57014", "hold_batch"))
	require.Equal(t, beforeTimeout+1, afterTimeout, "hold_batch 라벨이 늘어야 한다")
}
