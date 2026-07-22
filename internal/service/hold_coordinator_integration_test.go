package service

import (
	"sync"
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func newHoldTestRepos(db *gorm.DB) (*repository.OrderRepository, *repository.WalletRepository, *repository.LedgerRepository) {
	return repository.NewOrderRepository(db), repository.NewWalletRepository(db), repository.NewLedgerRepository(db)
}

func seedHoldWallets(t *testing.T, db *gorm.DB, buyerID uint, sellerID uint) {
	t.Helper()

	wallets := []model.Wallet{
		{UserID: buyerID, CoinSymbol: model.KRWAssetSymbol, KRW: decimal.NewFromInt(1000), AvailableBalance: decimal.NewFromInt(1000), LockedBalance: decimal.Zero},
		{UserID: buyerID, CoinSymbol: "BTC", Quantity: decimal.Zero, AvailableBalance: decimal.Zero, LockedBalance: decimal.Zero},
		{UserID: sellerID, CoinSymbol: "BTC", Quantity: decimal.NewFromInt(10), AvailableBalance: decimal.NewFromInt(10), LockedBalance: decimal.Zero},
		{UserID: sellerID, CoinSymbol: model.KRWAssetSymbol, KRW: decimal.Zero, AvailableBalance: decimal.Zero, LockedBalance: decimal.Zero},
	}
	require.NoError(t, db.Create(&wallets).Error)
}

func holdEquivalenceOrders(buyerID uint, sellerID uint) []*model.Order {
	price := decimal.NewFromInt(100)
	return []*model.Order{
		{UserID: buyerID, CoinSymbol: "BTC", Side: model.OrderSideBuy, OrderType: model.OrderTypeLimit, Price: price, Amount: decimal.NewFromInt(2), Status: model.OrderStatusPending},
		{UserID: sellerID, CoinSymbol: "BTC", Side: model.OrderSideSell, OrderType: model.OrderTypeLimit, Price: price, Amount: decimal.NewFromInt(3), Status: model.OrderStatusPending},
		// 같은 유저(buyerID)의 두 번째 매수 — 배치 내 fold(첫 주문이 깎은 잔고를 보는지) 검증.
		{UserID: buyerID, CoinSymbol: "BTC", Side: model.OrderSideBuy, OrderType: model.OrderTypeLimit, Price: price, Amount: decimal.NewFromInt(1), Status: model.OrderStatusPending},
	}
}

// 등가성: 같은 주문 시퀀스를 HoldBatch(batch 유저) vs persistAndHold 순차(seq 유저)로
// 처리 → 지갑 잔고·원장 엔트리·주문 상태가 사용자 대응되게 동일. 같은 유저 다중 주문
// (fold, batchBuyer가 buy를 2번) 포함.
func TestIntegrationHoldBatchMatchesSequentialSingleHold(t *testing.T) {
	db := openServiceIntegrationDB(t)
	orderRepo, walletRepo, ledgerRepo := newHoldTestRepos(db)

	batchBuyer := serviceTestUserID(700)
	batchSeller := serviceTestUserID(701)
	seqBuyer := serviceTestUserID(702)
	seqSeller := serviceTestUserID(703)
	defer cleanupServiceUsers(t, db, batchBuyer, batchSeller, seqBuyer, seqSeller)

	seedHoldWallets(t, db, batchBuyer, batchSeller)
	seedHoldWallets(t, db, seqBuyer, seqSeller)

	batchOrders := holdEquivalenceOrders(batchBuyer, batchSeller)
	seqOrders := holdEquivalenceOrders(seqBuyer, seqSeller)

	coordinator := &HoldCoordinator{DB: db, OrderRepo: orderRepo, WalletRepo: walletRepo, LedgerRepo: ledgerRepo}
	results, err := coordinator.HoldBatch(batchOrders)
	require.NoError(t, err)
	require.Len(t, results, 3)
	for i, r := range results {
		require.NoError(t, r.Err, "order %d should hold successfully", i)
		require.NotZero(t, r.Order.ID)
	}

	for _, o := range seqOrders {
		require.NoError(t, persistAndHold(db, orderRepo, walletRepo, ledgerRepo, o))
	}

	assertWalletsMatch(t, walletRepo, batchBuyer, seqBuyer)
	assertWalletsMatch(t, walletRepo, batchSeller, seqSeller)
	assertLedgerSequencesMatch(t, db, batchBuyer, seqBuyer)
	assertLedgerSequencesMatch(t, db, batchSeller, seqSeller)

	for i := range batchOrders {
		assertOrdersMatch(t, db, batchOrders[i].ID, seqOrders[i].ID)
		assert.Equal(t, model.OrderStatusPending, batchOrders[i].Status)
	}

	// ReferenceID는 배치 INSERT로 채워진 실제 order.ID를 가리켜야 한다(fold-후-INSERT 순서 계약).
	holdEntries := requireLedgerEntries(t, db, batchBuyer, model.LedgerEntryTypeOrderHold, model.LedgerReferenceTypeOrder, batchOrders[0].ID)
	require.Len(t, holdEntries, 1)
	holdEntries2 := requireLedgerEntries(t, db, batchBuyer, model.LedgerEntryTypeOrderHold, model.LedgerReferenceTypeOrder, batchOrders[2].ID)
	require.Len(t, holdEntries2, 1)
}

// 개별 격리: 배치에 잔고 충분 2건 + 부족 1건 → 충분분은 홀드·주문 PENDING, 부족분은
// holdResult.Err=ConflictError·주문 행 0. 통과분 지갑/원장만 반영.
func TestIntegrationHoldBatchIsolatesInsufficientFunds(t *testing.T) {
	db := openServiceIntegrationDB(t)
	orderRepo, walletRepo, ledgerRepo := newHoldTestRepos(db)

	okBuyer := serviceTestUserID(704)
	okSeller := serviceTestUserID(705)
	poorBuyer := serviceTestUserID(706)
	defer cleanupServiceUsers(t, db, okBuyer, okSeller, poorBuyer)

	seedHoldWallets(t, db, okBuyer, okSeller)
	require.NoError(t, db.Create(&model.Wallet{
		UserID: poorBuyer, CoinSymbol: model.KRWAssetSymbol,
		KRW: decimal.NewFromInt(10), AvailableBalance: decimal.NewFromInt(10), LockedBalance: decimal.Zero,
	}).Error)

	price := decimal.NewFromInt(100)
	orders := []*model.Order{
		{UserID: okBuyer, CoinSymbol: "BTC", Side: model.OrderSideBuy, OrderType: model.OrderTypeLimit, Price: price, Amount: decimal.NewFromInt(2), Status: model.OrderStatusPending},
		{UserID: okSeller, CoinSymbol: "BTC", Side: model.OrderSideSell, OrderType: model.OrderTypeLimit, Price: price, Amount: decimal.NewFromInt(3), Status: model.OrderStatusPending},
		// KRW 10으로는 (5*100)*1.0005 = 500.25 를 감당할 수 없다.
		{UserID: poorBuyer, CoinSymbol: "BTC", Side: model.OrderSideBuy, OrderType: model.OrderTypeLimit, Price: price, Amount: decimal.NewFromInt(5), Status: model.OrderStatusPending},
	}

	coordinator := &HoldCoordinator{DB: db, OrderRepo: orderRepo, WalletRepo: walletRepo, LedgerRepo: ledgerRepo}
	results, err := coordinator.HoldBatch(orders)
	require.NoError(t, err, "부분 실패는 배치 자체를 실패시키지 않는다")
	require.Len(t, results, 3)

	require.NoError(t, results[0].Err)
	require.NotZero(t, results[0].Order.ID)
	require.NoError(t, results[1].Err)
	require.NotZero(t, results[1].Order.ID)

	require.Error(t, results[2].Err)
	assert.Nil(t, results[2].Order)
	kind, ok := DomainErrorKind(results[2].Err)
	require.True(t, ok)
	assert.Equal(t, ErrorKindConflict, kind)
	assert.Equal(t, uint(0), orders[2].ID, "잔고부족 주문은 INSERT되지 않아 ID가 채워지지 않아야 한다")

	var okBuyerOrderCount, poorBuyerOrderCount int64
	require.NoError(t, db.Model(&model.Order{}).Where("user_id = ?", okBuyer).Count(&okBuyerOrderCount).Error)
	require.NoError(t, db.Model(&model.Order{}).Where("user_id = ?", poorBuyer).Count(&poorBuyerOrderCount).Error)
	assert.Equal(t, int64(1), okBuyerOrderCount)
	assert.Equal(t, int64(0), poorBuyerOrderCount, "잔고부족 주문 행은 0건이어야 한다")

	var persistedOK model.Order
	require.NoError(t, db.First(&persistedOK, results[0].Order.ID).Error)
	assert.Equal(t, model.OrderStatusPending, persistedOK.Status)

	poorWallet, err := walletRepo.FindKRWWalletByUserID(poorBuyer)
	require.NoError(t, err)
	assert.True(t, poorWallet.AvailableBalance.Equal(decimal.NewFromInt(10)), "잔고부족 유저 지갑은 무변화여야 한다")
	assert.True(t, poorWallet.LockedBalance.Equal(decimal.Zero))
	assertLedgerCount(t, db, poorBuyer, 0)

	okBuyerWallet, err := walletRepo.FindKRWWalletByUserID(okBuyer)
	require.NoError(t, err)
	assert.True(t, okBuyerWallet.LockedBalance.Equal(quoteAmountWithTradingFee(price.Mul(decimal.NewFromInt(2)))))
	assertLedgerCount(t, db, okBuyer, 1)
}

// 같은 유저 fold: 잔고 100인 유저가 한 배치에 60+50 두 매수 → 첫째 통과(잔고 차감),
// 둘째 부족(ConflictError). overspend 방지 = 단건 순차와 동일.
func TestIntegrationHoldBatchFoldsSameUserBalance(t *testing.T) {
	db := openServiceIntegrationDB(t)
	orderRepo, walletRepo, ledgerRepo := newHoldTestRepos(db)

	batchUser := serviceTestUserID(707)
	seqUser := serviceTestUserID(708)
	defer cleanupServiceUsers(t, db, batchUser, seqUser)

	seedFoldWallet := func(userID uint) {
		require.NoError(t, db.Create(&model.Wallet{
			UserID: userID, CoinSymbol: model.KRWAssetSymbol,
			KRW: decimal.NewFromInt(100), AvailableBalance: decimal.NewFromInt(100), LockedBalance: decimal.Zero,
		}).Error)
	}
	seedFoldWallet(batchUser)
	seedFoldWallet(seqUser)

	mkOrders := func(userID uint) []*model.Order {
		price := decimal.NewFromInt(1)
		return []*model.Order{
			{UserID: userID, CoinSymbol: "BTC", Side: model.OrderSideBuy, OrderType: model.OrderTypeLimit, Price: price, Amount: decimal.NewFromInt(60), Status: model.OrderStatusPending},
			{UserID: userID, CoinSymbol: "BTC", Side: model.OrderSideBuy, OrderType: model.OrderTypeLimit, Price: price, Amount: decimal.NewFromInt(50), Status: model.OrderStatusPending},
		}
	}

	batchOrders := mkOrders(batchUser)
	coordinator := &HoldCoordinator{DB: db, OrderRepo: orderRepo, WalletRepo: walletRepo, LedgerRepo: ledgerRepo}
	results, err := coordinator.HoldBatch(batchOrders)
	require.NoError(t, err)
	require.Len(t, results, 2)

	require.NoError(t, results[0].Err)
	require.NotZero(t, results[0].Order.ID)

	require.Error(t, results[1].Err, "60을 먼저 홀드하면 잔여 39.97로는 50을 못 담는다")
	kind, ok := DomainErrorKind(results[1].Err)
	require.True(t, ok)
	assert.Equal(t, ErrorKindConflict, kind)
	assert.Equal(t, uint(0), batchOrders[1].ID)

	// 단건 순차 기준선: 첫 주문은 persistAndHold로 통과, 둘째는 각자 트랜잭션에서 실패.
	seqOrders := mkOrders(seqUser)
	require.NoError(t, persistAndHold(db, orderRepo, walletRepo, ledgerRepo, seqOrders[0]))
	seqErr := persistAndHold(db, orderRepo, walletRepo, ledgerRepo, seqOrders[1])
	require.Error(t, seqErr)
	seqKind, seqOK := DomainErrorKind(seqErr)
	require.True(t, seqOK)
	assert.Equal(t, ErrorKindConflict, seqKind)

	assertWalletsMatch(t, walletRepo, batchUser, seqUser)

	var batchOrderCount, seqOrderCount int64
	require.NoError(t, db.Model(&model.Order{}).Where("user_id = ?", batchUser).Count(&batchOrderCount).Error)
	require.NoError(t, db.Model(&model.Order{}).Where("user_id = ?", seqUser).Count(&seqOrderCount).Error)
	assert.Equal(t, int64(1), batchOrderCount)
	assert.Equal(t, int64(1), seqOrderCount)

	assertLedgerSequencesMatch(t, db, batchUser, seqUser)
}

// Submit→Run 왕복: 코디네이터를 기동한 뒤 동시에 여러 주문을 제출하면 각자 홀드된
// 주문(ID 채워짐)이 돌아온다.
func TestIntegrationHoldCoordinatorSubmitHolds(t *testing.T) {
	db := openServiceIntegrationDB(t)
	orderRepo, walletRepo, ledgerRepo := newHoldTestRepos(db)

	buyerID := serviceTestUserID(709)
	sellerID := serviceTestUserID(710)
	defer cleanupServiceUsers(t, db, buyerID, sellerID)
	seedHoldWallets(t, db, buyerID, sellerID)

	coordinator := NewHoldCoordinator(db, orderRepo, walletRepo, ledgerRepo, 0)
	go coordinator.Run()
	defer coordinator.Shutdown()

	orders := holdEquivalenceOrders(buyerID, sellerID)
	results := make([]*model.Order, len(orders))
	errs := make([]error, len(orders))
	var wg sync.WaitGroup
	for i, o := range orders {
		wg.Add(1)
		go func(i int, o *model.Order) {
			defer wg.Done()
			results[i], errs[i] = coordinator.Submit(o)
		}(i, o)
	}
	wg.Wait()

	for i := range orders {
		require.NoError(t, errs[i], "order %d should hold successfully", i)
		require.NotZero(t, results[i].ID)
	}
}

// 입력 만석: 코디네이터를 기동하지 않은(input을 아무도 비우지 않는) 채로 작은
// 용량의 input을 직접 채운 뒤 Submit하면 즉시 503(Unavailable)이 반환돼야 한다.
func TestHoldCoordinatorSubmitReturnsUnavailableWhenInputFull(t *testing.T) {
	c := &HoldCoordinator{input: make(chan holdRequest, 2), done: make(chan struct{})}
	c.input <- holdRequest{order: &model.Order{}, resultCh: make(chan holdResult, 1)}
	c.input <- holdRequest{order: &model.Order{}, resultCh: make(chan holdResult, 1)}

	before := testutil.ToFloat64(metrics.OrdersAdmissionRejectedTotal.WithLabelValues("coordinator"))

	_, err := c.Submit(&model.Order{})

	require.Error(t, err)
	kind, ok := DomainErrorKind(err)
	require.True(t, ok)
	assert.Equal(t, ErrorKindUnavailable, kind)

	after := testutil.ToFloat64(metrics.OrdersAdmissionRejectedTotal.WithLabelValues("coordinator"))
	assert.Equal(t, before+1, after)
}

// 폴백: DB 커넥션을 닫아 HoldBatch(트랜잭션)를 강제로 실패시키면, 코디네이터는
// 배치 전체를 에러로 돌려주는 대신 각 요청을 persistAndHold 폴백으로 넘긴다(같은
// 커넥션이 죽어있으므로 폴백도 실패하지만, 폴백 경로 자체가 호출됐음은 fallbacks_total
// 증가로 확인한다). 요청마다 정확히 하나의 배치이므로 카운터는 제출 수만큼 증가한다.
func TestIntegrationHoldCoordinatorFallsBackOnBatchError(t *testing.T) {
	db := openServiceIntegrationDB(t)
	orderRepo, walletRepo, ledgerRepo := newHoldTestRepos(db)

	buyerID := serviceTestUserID(711)
	// cleanup 없음 — DB 커넥션을 아래에서 닫으므로 아무 것도 실제로 persist되지
	// 않는다(정리할 행이 없고, 닫힌 커넥션으로 정리 쿼리를 시도하면 그 자체가 실패한다).

	sqlDB, err := db.DB()
	require.NoError(t, err)

	coordinator := NewHoldCoordinator(db, orderRepo, walletRepo, ledgerRepo, 2)
	require.NoError(t, sqlDB.Close(), "DB 커넥션을 닫아 배치·폴백 모두 실패하게 만든다")

	before := testutil.ToFloat64(metrics.HoldBatchFallbacksTotal)
	go coordinator.Run()
	defer coordinator.Shutdown()

	mkOrder := func() *model.Order {
		return &model.Order{
			UserID: buyerID, CoinSymbol: "BTC", Side: model.OrderSideBuy, OrderType: model.OrderTypeLimit,
			Price: decimal.NewFromInt(100), Amount: decimal.NewFromInt(1), Status: model.OrderStatusPending,
		}
	}

	// 순차 제출 — Submit은 결과가 시그널될 때까지 블로킹하므로, 두 번째 제출은
	// 첫 번째가 이미 자신만의 배치로 처리된 뒤에 일어난다(배치 2개 = fallback 2회).
	_, err1 := coordinator.Submit(mkOrder())
	_, err2 := coordinator.Submit(mkOrder())

	require.Error(t, err1, "닫힌 DB에서는 폴백도 실패해야 한다")
	require.Error(t, err2)

	after := testutil.ToFloat64(metrics.HoldBatchFallbacksTotal)
	assert.Equal(t, before+2, after, "배치 실패마다 fallbacks_total이 증가해야 한다")
}

// 종료 drain: input에 다수 요청이 이미 쌓인 상태에서 Shutdown(input 닫기)을
// 호출해도, Run은 잔여분을 전부 처리(persist+hold)한 뒤에 반환해야 한다 — 유실 0.
func TestIntegrationHoldCoordinatorShutdownDrains(t *testing.T) {
	db := openServiceIntegrationDB(t)
	orderRepo, walletRepo, ledgerRepo := newHoldTestRepos(db)

	buyerID := serviceTestUserID(712)
	defer cleanupServiceUsers(t, db, buyerID)

	require.NoError(t, db.Create(&model.Wallet{
		UserID: buyerID, CoinSymbol: model.KRWAssetSymbol,
		KRW: decimal.NewFromInt(1_000_000), AvailableBalance: decimal.NewFromInt(1_000_000), LockedBalance: decimal.Zero,
	}).Error)
	require.NoError(t, db.Create(&model.Wallet{
		UserID: buyerID, CoinSymbol: "BTC", Quantity: decimal.Zero, AvailableBalance: decimal.Zero, LockedBalance: decimal.Zero,
	}).Error)

	coordinator := NewHoldCoordinator(db, orderRepo, walletRepo, ledgerRepo, 4)

	const n = 20
	reqs := make([]holdRequest, n)
	for i := 0; i < n; i++ {
		order := &model.Order{
			UserID: buyerID, CoinSymbol: "BTC", Side: model.OrderSideBuy, OrderType: model.OrderTypeLimit,
			Price: decimal.NewFromInt(1), Amount: decimal.NewFromInt(1), Status: model.OrderStatusPending,
		}
		reqs[i] = holdRequest{order: order, resultCh: make(chan holdResult, 1)}
		coordinator.input <- reqs[i] // Run 기동 전에 input을 직접 채워 "다수 제출 후 Shutdown" 상태를 재현한다.
	}

	go coordinator.Run()
	coordinator.Shutdown() // Run이 남은 20건을 전부 drain하고 반환할 때까지 블로킹.

	for i, r := range reqs {
		select {
		case res := <-r.resultCh:
			assert.NoError(t, res.Err, "request %d should hold successfully", i)
			assert.NotZero(t, res.Order.ID, "request %d should be persisted", i)
		default:
			t.Fatalf("request %d result not signaled — drain lost a request", i)
		}
	}
}
