package service

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 정산 worker pool은 서로 다른 배치를 동시에 SettleTradeBatch로 커밋한다. 같은 배치
// 안에서는 산술이 순차라 결정론적이므로(settlement_batch.go "7. 순차 산술"), 순서
// 의존성은 배치 경계를 넘어야만 드러난다 — 그래서 이 픽스처는 같은 매수자의 코인
// 지갑이 서로 다른 두 배치에서 각각 매수 체결을 받도록 만든다.
type crossBatchFixture struct {
	userIDs     []uint
	buyerID     uint
	buy1ID      uint
	sell1ID     uint
	buy2ID      uint
	sell2ID     uint
	orderIDs    []uint
	initialQty  decimal.Decimal
	totalLocked decimal.Decimal
}

// 나눗셈 결과가 유한 자리에서 잘리도록 일부러 나누어떨어지지 않는 수량·가격을 쓴다.
// (0.7 → 0.81 → 0.94 vs 0.7 → 0.83 → 0.94)
var (
	crossBatchInitialQty = decimal.RequireFromString("0.7")
	crossBatchInitialAvg = decimal.RequireFromString("49999999.9999999999999999")
	crossBatchPrice1     = decimal.RequireFromString("50000009.09")
	crossBatchQty1       = decimal.RequireFromString("0.11")
	crossBatchPrice2     = decimal.RequireFromString("50000031.17")
	crossBatchQty2       = decimal.RequireFromString("0.13")

	// avg_buy_price_permutation_test.go와 같은 허용치. AvgBuyPrice만 나눗셈이 개입해
	// 순서 의존이고, 나머지 잔고 필드는 덧셈뿐이라 반올림이 없다.
	crossBatchAvgBuyPriceTolerance = decimal.RequireFromString("0.000001")
)

func seedCrossBatchFixture(t *testing.T, db *gorm.DB, offsetBase uint) crossBatchFixture {
	t.Helper()

	buyer := serviceTestUserID(offsetBase)
	seller1 := serviceTestUserID(offsetBase + 1)
	seller2 := serviceTestUserID(offsetBase + 2)

	reserved1 := quoteAmountWithTradingFee(crossBatchPrice1.Mul(crossBatchQty1))
	reserved2 := quoteAmountWithTradingFee(crossBatchPrice2.Mul(crossBatchQty2))
	totalLocked := reserved1.Add(reserved2)

	wallets := []model.Wallet{
		{UserID: buyer, CoinSymbol: model.KRWAssetSymbol, KRW: totalLocked, AvailableBalance: decimal.Zero, LockedBalance: totalLocked},
		{UserID: buyer, CoinSymbol: "BTC", Quantity: crossBatchInitialQty, AvailableBalance: crossBatchInitialQty, LockedBalance: decimal.Zero, AvgBuyPrice: crossBatchInitialAvg},
		{UserID: seller1, CoinSymbol: "BTC", Quantity: crossBatchQty1, AvailableBalance: decimal.Zero, LockedBalance: crossBatchQty1},
		{UserID: seller1, CoinSymbol: model.KRWAssetSymbol, KRW: decimal.Zero, AvailableBalance: decimal.Zero, LockedBalance: decimal.Zero},
		{UserID: seller2, CoinSymbol: "BTC", Quantity: crossBatchQty2, AvailableBalance: decimal.Zero, LockedBalance: crossBatchQty2},
		{UserID: seller2, CoinSymbol: model.KRWAssetSymbol, KRW: decimal.Zero, AvailableBalance: decimal.Zero, LockedBalance: decimal.Zero},
	}
	require.NoError(t, db.Create(&wallets).Error)

	mkOrder := func(userID uint, side model.OrderSide, price decimal.Decimal, amount decimal.Decimal) model.Order {
		order := model.Order{
			UserID:       userID,
			CoinSymbol:   "BTC",
			Side:         side,
			OrderType:    model.OrderTypeLimit,
			Price:        price,
			Amount:       amount,
			Status:       model.OrderStatusPending,
			FilledAmount: decimal.Zero,
		}
		require.NoError(t, db.Create(&order).Error)
		return order
	}

	buy1 := mkOrder(buyer, model.OrderSideBuy, crossBatchPrice1, crossBatchQty1)
	sell1 := mkOrder(seller1, model.OrderSideSell, crossBatchPrice1, crossBatchQty1)
	buy2 := mkOrder(buyer, model.OrderSideBuy, crossBatchPrice2, crossBatchQty2)
	sell2 := mkOrder(seller2, model.OrderSideSell, crossBatchPrice2, crossBatchQty2)

	return crossBatchFixture{
		userIDs:     []uint{buyer, seller1, seller2},
		buyerID:     buyer,
		buy1ID:      buy1.ID,
		sell1ID:     sell1.ID,
		buy2ID:      buy2.ID,
		sell2ID:     sell2.ID,
		orderIDs:    []uint{buy1.ID, sell1.ID, buy2.ID, sell2.ID},
		initialQty:  crossBatchInitialQty,
		totalLocked: totalLocked,
	}
}

// crossBatchTrades는 서로 다른 배치에 들어갈 체결 2건을 만든다. 둘 다 같은 매수자의
// BTC 지갑에 매수 체결을 적재하므로 AvgBuyPrice가 배치 경계를 넘어 재계산된다.
func crossBatchTrades(f crossBatchFixture, runTag string) []*model.Trade {
	mk := func(buyOrderID uint, sellOrderID uint, price decimal.Decimal, quantity decimal.Decimal, tag string) *model.Trade {
		return &model.Trade{
			EngineSequence: 1,
			EngineEventID:  fmt.Sprintf("batch-conc-%s-%s-%d", runTag, tag, time.Now().UnixNano()),
			CoinSymbol:     "BTC",
			Price:          price,
			Quantity:       quantity,
			TradedAt:       time.Now().UTC(),
			BuyOrderID:     buyOrderID,
			SellOrderID:    sellOrderID,
		}
	}
	return []*model.Trade{
		mk(f.buy1ID, f.sell1ID, crossBatchPrice1, crossBatchQty1, "b1"),
		mk(f.buy2ID, f.sell2ID, crossBatchPrice2, crossBatchQty2, "b2"),
	}
}

func settleSingleTradeBatch(s *SettlementService, trade *model.Trade) error {
	results, err := s.SettleTradeBatch([]TradeBatchItem{{Trade: trade}})
	if err != nil {
		return err
	}
	if len(results) != 1 || !results[0].Applied {
		return fmt.Errorf("batch not applied: %+v", results)
	}
	return nil
}

// assertWalletsMatchWithAvgBuyPriceTolerance는 settlement_batch_integration_test.go의
// assertWalletsMatch와 달리 AvgBuyPrice를 Equal()로 비교하지 않는다 — 나눗셈이 개입해
// 배치 커밋 순서에 따라 유한 정밀도 마지막 자리가 달라질 수 있기 때문이다. 나머지
// 4필드는 덧셈뿐이라 순서와 무관하게 정확히 같아야 한다.
func assertWalletsMatchWithAvgBuyPriceTolerance(t *testing.T, walletRepo *repository.WalletRepository, leftUserID uint, rightUserID uint) {
	t.Helper()

	left, err := walletRepo.ListByUserID(leftUserID)
	require.NoError(t, err)
	right, err := walletRepo.ListByUserID(rightUserID)
	require.NoError(t, err)
	require.Equal(t, len(right), len(left), "wallet count mismatch user %d vs %d", leftUserID, rightUserID)
	for idx := range left {
		lw, rw := left[idx], right[idx]
		assert.Equal(t, rw.CoinSymbol, lw.CoinSymbol)
		assert.True(t, lw.AvailableBalance.Equal(rw.AvailableBalance), "AvailableBalance user %d/%d coin %s: %s vs %s", leftUserID, rightUserID, lw.CoinSymbol, lw.AvailableBalance, rw.AvailableBalance)
		assert.True(t, lw.LockedBalance.Equal(rw.LockedBalance), "LockedBalance user %d/%d coin %s: %s vs %s", leftUserID, rightUserID, lw.CoinSymbol, lw.LockedBalance, rw.LockedBalance)
		assert.True(t, lw.KRW.Equal(rw.KRW), "KRW user %d/%d coin %s: %s vs %s", leftUserID, rightUserID, lw.CoinSymbol, lw.KRW, rw.KRW)
		assert.True(t, lw.Quantity.Equal(rw.Quantity), "Quantity user %d/%d coin %s: %s vs %s", leftUserID, rightUserID, lw.CoinSymbol, lw.Quantity, rw.Quantity)

		diff := lw.AvgBuyPrice.Sub(rw.AvgBuyPrice).Abs()
		assert.True(t, diff.LessThanOrEqual(crossBatchAvgBuyPriceTolerance),
			"AvgBuyPrice 오차 %s가 허용치 %s 초과: serial=%s concurrent=%s (user %d/%d coin %s)",
			diff, crossBatchAvgBuyPriceTolerance, lw.AvgBuyPrice, rw.AvgBuyPrice, leftUserID, rightUserID, lw.CoinSymbol)
	}
}

type ledgerTotals struct {
	count          int64
	availableDelta decimal.Decimal
	lockedDelta    decimal.Decimal
}

func ledgerTotalsByCoin(t *testing.T, db *gorm.DB, userID uint) map[string]ledgerTotals {
	t.Helper()

	var entries []model.LedgerEntry
	require.NoError(t, db.Where("user_id = ?", userID).Find(&entries).Error)
	totals := make(map[string]ledgerTotals, 2)
	for _, e := range entries {
		cur := totals[e.CoinSymbol]
		cur.count++
		cur.availableDelta = cur.availableDelta.Add(e.AvailableDelta)
		cur.lockedDelta = cur.lockedDelta.Add(e.LockedDelta)
		totals[e.CoinSymbol] = cur
	}
	return totals
}

// 원장은 행 개수와 자산별 delta 합계만 비교한다. 행별 AvailableBalanceAfter/
// LockedBalanceAfter는 두 배치 중 어느 쪽이 먼저 커밋되느냐에 따라 중간 잔액이
// 달라지므로(순서 의존) 동일 단언 대상이 아니다.
func assertLedgerTotalsMatch(t *testing.T, db *gorm.DB, leftUserID uint, rightUserID uint) {
	t.Helper()

	left := ledgerTotalsByCoin(t, db, leftUserID)
	right := ledgerTotalsByCoin(t, db, rightUserID)
	require.Equal(t, len(right), len(left), "ledger asset count user %d vs %d", leftUserID, rightUserID)
	for coin, lt := range left {
		rt, ok := right[coin]
		require.True(t, ok, "ledger entries missing for coin %s user %d", coin, rightUserID)
		assert.Equal(t, rt.count, lt.count, "ledger entry count user %d/%d coin %s", leftUserID, rightUserID, coin)
		assert.True(t, lt.availableDelta.Equal(rt.availableDelta), "AvailableDelta sum user %d/%d coin %s: %s vs %s", leftUserID, rightUserID, coin, lt.availableDelta, rt.availableDelta)
		assert.True(t, lt.lockedDelta.Equal(rt.lockedDelta), "LockedDelta sum user %d/%d coin %s: %s vs %s", leftUserID, rightUserID, coin, lt.lockedDelta, rt.lockedDelta)
	}
}

func assertNoSettlementFailures(t *testing.T, db *gorm.DB, orderIDs []uint) {
	t.Helper()

	var failed int64
	require.NoError(t, db.Model(&model.FailedSettlement{}).
		Where("buy_order_id IN ? OR sell_order_id IN ?", orderIDs, orderIDs).Count(&failed).Error)
	assert.Equal(t, int64(0), failed, "failed_settlements 신규 행이 있으면 안 된다")

	var marketFailed int64
	require.NoError(t, db.Model(&model.FailedMarketCompletion{}).
		Where("order_id IN ?", orderIDs).Count(&marketFailed).Error)
	assert.Equal(t, int64(0), marketFailed, "failed_market_completions 신규 행이 있으면 안 된다")
}

// 자산 총량 보존: 매수자+매도자 전체의 KRW는 두 체결의 수수료(매수자·매도자 각 1건)
// 만큼만 줄고, BTC는 정확히 보존된다.
func assertCrossBatchAssetConservation(t *testing.T, walletRepo *repository.WalletRepository, f crossBatchFixture) {
	t.Helper()

	krw, btc := decimal.Zero, decimal.Zero
	for _, id := range f.userIDs {
		wallets, err := walletRepo.ListByUserID(id)
		require.NoError(t, err)
		for _, w := range wallets {
			if w.CoinSymbol == model.KRWAssetSymbol {
				krw = krw.Add(w.KRW)
			} else {
				btc = btc.Add(w.Quantity)
			}
		}
	}

	quote1 := crossBatchPrice1.Mul(crossBatchQty1)
	quote2 := crossBatchPrice2.Mul(crossBatchQty2)
	expectedKRW := f.totalLocked.
		Sub(tradingFeeAmount(quote1).Mul(decimal.NewFromInt(2))).
		Sub(tradingFeeAmount(quote2).Mul(decimal.NewFromInt(2)))
	expectedBTC := f.initialQty.Add(crossBatchQty1).Add(crossBatchQty2)

	assert.True(t, krw.Equal(expectedKRW), "KRW 총량 %s != 기대값 %s", krw, expectedKRW)
	assert.True(t, btc.Equal(expectedBTC), "BTC 총량 %s != 기대값 %s", btc, expectedBTC)
}

// 병렬 정산 등가성: 같은 모양의 픽스처 2벌에 대해 한쪽은 배치를 고정 순서로
// (batch1 → batch2, concurrency=1 동등) 정산하고, 다른 쪽은 두 배치를 각각 별도
// goroutine에서 동시에(concurrency=N 동등) 정산한 뒤 최종 상태를 비교한다. 커밋
// 순서는 DB 행 락 경합에 맡긴다 — 순서를 강제하지 않는 게 이 테스트의 요점이다.
func TestIntegrationSettlementBatchConcurrencyMatchesSerialSettlement(t *testing.T) {
	db := openServiceIntegrationDB(t)
	walletRepo := repository.NewWalletRepository(db)
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db), walletRepo)

	serial := seedCrossBatchFixture(t, db, 700)
	concurrent := seedCrossBatchFixture(t, db, 710)
	defer cleanupServiceUsers(t, db, append(append([]uint{}, serial.userIDs...), concurrent.userIDs...)...)

	for _, trade := range crossBatchTrades(serial, "serial") {
		require.NoError(t, settleSingleTradeBatch(settlementService, trade))
	}

	concurrentTrades := crossBatchTrades(concurrent, "concurrent")
	errs := make([]error, len(concurrentTrades))
	var wg sync.WaitGroup
	for i, trade := range concurrentTrades {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = settleSingleTradeBatch(settlementService, trade)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "동시 배치 %d 정산 실패", i)
	}

	for idx := range serial.userIDs {
		assertWalletsMatchWithAvgBuyPriceTolerance(t, walletRepo, serial.userIDs[idx], concurrent.userIDs[idx])
		assertLedgerTotalsMatch(t, db, serial.userIDs[idx], concurrent.userIDs[idx])
	}

	for idx := range serial.orderIDs {
		assertOrdersMatch(t, db, serial.orderIDs[idx], concurrent.orderIDs[idx])
	}

	assertNoSettlementFailures(t, db, append(append([]uint{}, serial.orderIDs...), concurrent.orderIDs...))
	assertCrossBatchAssetConservation(t, walletRepo, serial)
	assertCrossBatchAssetConservation(t, walletRepo, concurrent)

	serialCoin := findWalletForCoin(t, walletRepo, serial.buyerID, "BTC")
	concurrentCoin := findWalletForCoin(t, walletRepo, concurrent.buyerID, "BTC")
	t.Logf("매수자 AvgBuyPrice: 직렬=%s 동시=%s 차이=%s",
		serialCoin.AvgBuyPrice, concurrentCoin.AvgBuyPrice,
		serialCoin.AvgBuyPrice.Sub(concurrentCoin.AvgBuyPrice).Abs())
}

func findWalletForCoin(t *testing.T, walletRepo *repository.WalletRepository, userID uint, coinSymbol string) model.Wallet {
	t.Helper()

	wallets, err := walletRepo.ListByUserID(userID)
	require.NoError(t, err)
	for _, w := range wallets {
		if w.CoinSymbol == coinSymbol {
			return w
		}
	}
	t.Fatalf("wallet %s not found for user %d", coinSymbol, userID)
	return model.Wallet{}
}
