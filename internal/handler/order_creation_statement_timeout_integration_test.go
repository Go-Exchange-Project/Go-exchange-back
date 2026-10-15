package handler

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/config"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/testdb"
	"github.com/stretchr/testify/require"
)

func testDatabaseDSNForHandler(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("GOEXCHANGE_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("GOEXCHANGE_TEST_DATABASE_DSN is not set; skipping Postgres integration test")
	}
	return dsn
}

// D7: 주문 생성 경로에서 timeout(57014)이 나면 — 이 배치가 통째로 롤백돼
// 멱등키 레코드가 남지 않으므로 — 503을 돌려준다. 응답과 DB 상태가 일치한다
// (레코드가 실제로 없다).
//
// 재현: hold_coordinator_statement_timeout_integration_test.go(internal/service,
// CP A)와 같은 기법이다 — 대상 계정의 account_balances 행을 별도 커넥션에서
// FOR UPDATE로 잡아두고, HoldCoordinator 풀은 statement_timeout을 짧게(150ms),
// lock_timeout은 훨씬 길게(5s) 둬 57014가 먼저 걸리게 한다.
func TestIntegrationCreateOrderHandlerMapsStatementTimeoutTo503WithNoIdempotencyRecord(t *testing.T) {
	db := testdb.OpenIntegrationDB(t)
	dsn := testDatabaseDSNForHandler(t)

	buyerID := uint(time.Now().UnixNano()%1_000_000 + 980_000_000)
	require.NoError(t, db.Create(&model.User{ID: buyerID, Name: fmt.Sprintf("d7-%d", buyerID)}).Error)
	t.Cleanup(func() {
		require.NoError(t, db.Where("user_id = ?", buyerID).Delete(&model.OrderIdempotencyKey{}).Error)
		require.NoError(t, db.Where("user_id = ?", buyerID).Delete(&model.Order{}).Error)
		require.NoError(t, db.Delete(&model.User{}, buyerID).Error)
	})

	accountRepo := repository.NewAccountRepository(db)
	accounts, err := accountRepo.EnsureAccounts([]repository.AccountSpec{
		{AccountType: model.AccountUserAvailable, OwnerUserID: &buyerID, Asset: model.KRWAssetSymbol},
		{AccountType: model.AccountUserLocked, OwnerUserID: &buyerID, Asset: model.KRWAssetSymbol},
	})
	require.NoError(t, err)
	require.Len(t, accounts, 2)
	_, err = service.NewDevWalletService(db).FundWallet(service.FundWalletInput{
		UserID: buyerID, CoinSymbol: model.KRWAssetSymbol, Amount: "100000",
		RequestKey: fmt.Sprintf("d7-fund-%d", buyerID),
	})
	require.NoError(t, err)

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
		Name:                            "test-create-order-statement-timeout",
		StatementTimeout:                150 * time.Millisecond,
		LockTimeout:                     5 * time.Second,
		IdleInTransactionSessionTimeout: 0,
		MaxOpenConns:                    2,
		MaxIdleConns:                    2,
	})
	require.NoError(t, err)
	defer shortSQLDB.Close()

	orderRepo := repository.NewOrderRepository(shortDB)
	orderService := service.NewOrderService(orderRepo, acceptingEngine{})
	holdCoordinator := service.NewHoldCoordinator(shortDB, orderRepo, service.NewLedgerService(shortDB),
		repository.NewOrderIdempotencyRepository(shortDB), 1)
	go holdCoordinator.Run()
	defer holdCoordinator.Shutdown()
	orderService.HoldCoordinator = holdCoordinator

	handler := NewOrderHandler(orderService)
	key := fmt.Sprintf("d7-key-%d", time.Now().UnixNano())
	recorder := postOrder(t, handler, buyerID, key, CreateOrderRequest{
		CoinSymbol: "BTC", Side: "BUY", OrderType: "LIMIT", Price: "100", Amount: "1",
	})

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code, "body=%s", recorder.Body.String())

	var count int64
	require.NoError(t, db.Model(&model.OrderIdempotencyKey{}).
		Where("user_id = ? AND idempotency_key = ?", buyerID, key).Count(&count).Error)
	require.EqualValues(t, 0, count, "57014로 배치가 롤백되면 멱등키 레코드가 남지 않아야 한다")
}
