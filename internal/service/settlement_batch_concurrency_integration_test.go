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

	// 정순(batch1 → batch2) 실행의 절대 기대값. 상대 비교만 하면 세 픽스처가 똑같이
	// 틀려도 통과하므로, 기준이 되는 정순 결과만은 balance.go의 산술
	// ((avg*qty + acquisitionCost) / newQty, DivisionPrecision=16)을 그대로 따라
	// 손으로 계산한 값과 대조한다. acquisitionCost는 수수료 포함 체결대금이다.
	//   0.7  @ 49999999.9999999999999999
	//   + 0.11 (cost 5502751.00039995) → 0.81 @ 49999999.9999999999999999*0.7 … /0.81
	//   + 0.13 (cost 6503254.05412605) → 0.94 @ 50006388.3558787234042553
	// (역순은 마지막 자리만 다른 …2552가 되어 매 실행 결정론적으로 오차를 관통한다.)
	crossBatchExpectedQuantity           = decimal.RequireFromString("0.94")
	crossBatchExpectedForwardAvgBuyPrice = decimal.RequireFromString("50006388.3558787234042553")
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
			"AvgBuyPrice 오차 %s가 허용치 %s 초과: left=%s right=%s (user %d/%d coin %s)",
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
	// 양쪽 다 0행이면 "합계가 같다"는 자명하게 참이므로 비교가 무의미하다.
	require.NotEmpty(t, left, "ledger entries missing for user %d", leftUserID)
	require.Equal(t, len(right), len(left), "ledger asset count user %d vs %d", leftUserID, rightUserID)
	for coin, lt := range left {
		rt, ok := right[coin]
		require.True(t, ok, "ledger entries missing for coin %s user %d", coin, rightUserID)
		require.Positive(t, lt.count, "ledger entry count 0 user %d coin %s", leftUserID, coin)
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

// 병렬 정산 등가성: 같은 모양의 픽스처 3벌을 서로 다른 커밋 순서로 정산한 뒤 최종
// 상태를 비교한다.
//
//	정순 직렬 — batch1 → batch2 고정 순서(concurrency=1 동등). 기준선.
//	역순 직렬 — batch2 → batch1 고정 순서. 매 실행 반드시 순서 의존 오차를
//	           관통시키는 결정론적 경로다(정순과 마지막 자리가 다르다).
//	동시     — 두 배치를 각각 별도 goroutine에서(concurrency=N 동등). 커밋 순서를
//	           DB 행 락 경합에 맡기는 실제 경합 스모크 테스트이지만, 스케줄러가
//	           우연히 정순과 같은 순서로 완료하면 아무것도 관통하지 못하므로
//	           결정론적 검증은 정순-역순 쌍이 담당한다.
//
// 그리고 상대 비교만으로는 세 픽스처가 똑같이 틀려도 통과하므로, 기준선인 정순
// 결과는 절대 기대값(crossBatchExpectedQuantity/…ForwardAvgBuyPrice)과 먼저 대조한다.
func TestIntegrationSettlementBatchConcurrencyMatchesSerialSettlement(t *testing.T) {
	db := openServiceIntegrationDB(t)
	walletRepo := repository.NewWalletRepository(db)
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db), walletRepo)

	forward := seedCrossBatchFixture(t, db, 900)
	defer cleanupServiceUsers(t, db, forward.userIDs...)
	reverse := seedCrossBatchFixture(t, db, 910)
	defer cleanupServiceUsers(t, db, reverse.userIDs...)
	concurrent := seedCrossBatchFixture(t, db, 920)
	defer cleanupServiceUsers(t, db, concurrent.userIDs...)

	for _, trade := range crossBatchTrades(forward, "forward") {
		require.NoError(t, settleSingleTradeBatch(settlementService, trade))
	}

	reverseTrades := crossBatchTrades(reverse, "reverse")
	for i := len(reverseTrades) - 1; i >= 0; i-- {
		require.NoError(t, settleSingleTradeBatch(settlementService, reverseTrades[i]))
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

	// 1단계: 기준선(정순 직렬)이 절대 기대값과 일치하는가.
	forwardCoin := findWalletForCoin(t, walletRepo, forward.buyerID, "BTC")
	assert.True(t, forwardCoin.Quantity.Equal(crossBatchExpectedQuantity),
		"정순 Quantity %s != 기대값 %s", forwardCoin.Quantity, crossBatchExpectedQuantity)
	assert.True(t, forwardCoin.AvgBuyPrice.Equal(crossBatchExpectedForwardAvgBuyPrice),
		"정순 AvgBuyPrice %s != 기대값 %s", forwardCoin.AvgBuyPrice, crossBatchExpectedForwardAvgBuyPrice)
	assertCrossBatchAssetConservation(t, walletRepo, forward)

	// 2단계: 역순·동시가 그 기준선과 tolerance 이내로 같은가.
	orderIDs := append([]uint{}, forward.orderIDs...)
	for _, other := range []crossBatchFixture{reverse, concurrent} {
		for idx := range forward.userIDs {
			assertWalletsMatchWithAvgBuyPriceTolerance(t, walletRepo, forward.userIDs[idx], other.userIDs[idx])
			assertLedgerTotalsMatch(t, db, forward.userIDs[idx], other.userIDs[idx])
		}
		for idx := range forward.orderIDs {
			assertOrdersMatch(t, db, forward.orderIDs[idx], other.orderIDs[idx])
		}
		assertCrossBatchAssetConservation(t, walletRepo, other)
		orderIDs = append(orderIDs, other.orderIDs...)

		otherCoin := findWalletForCoin(t, walletRepo, other.buyerID, "BTC")
		t.Logf("매수자 AvgBuyPrice: 정순=%s 비교=%s 차이=%s",
			forwardCoin.AvgBuyPrice, otherCoin.AvgBuyPrice,
			forwardCoin.AvgBuyPrice.Sub(otherCoin.AvgBuyPrice).Abs())
	}

	assertNoSettlementFailures(t, db, orderIDs)
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
