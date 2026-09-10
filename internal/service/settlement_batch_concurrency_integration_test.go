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

	// 매수자: 두 체결분 KRW를 한 번에 잠근다(원장 계정은 (user,asset) 단위라 주문별로
	// 나눌 필요가 없다) — 그리고 나눗셈이 개입하는 초기 평단가를 미리 심는다.
	seedLockedBalance(t, db, buyer, model.KRWAssetSymbol, totalLocked, buy1.ID)
	seedLedgerFunds(t, db, buyer, "BTC", crossBatchInitialQty)
	setAvgBuyPrice(t, db, buyer, "BTC", crossBatchInitialAvg)

	// 매도자: 각자 팔 BTC를 잠근다. 받을 KRW 계정은 정산이 온디맨드로 만든다.
	seedLockedBalance(t, db, seller1, "BTC", crossBatchQty1, sell1.ID)
	seedLockedBalance(t, db, seller2, "BTC", crossBatchQty2, sell2.ID)

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

// setAvgBuyPrice는 user_asset_stats에 평단가를 직접 심는다. applyAvgBuyPrice와
// 같은 UPSERT 모양을 쓴다 — 고정 소수점 나눗셈이 개입하는 초기 상태를 만들 때만
// 쓴다(정상 경로는 항상 applyAvgBuyPrice를 거친다).
func setAvgBuyPrice(t *testing.T, db *gorm.DB, userID uint, asset string, avg decimal.Decimal) {
	t.Helper()
	require.NoError(t, db.Exec(`
		INSERT INTO user_asset_stats (user_id, asset, avg_buy_price, updated_at)
		VALUES (?, ?, ?, now())
		ON CONFLICT (user_id, asset) DO UPDATE SET avg_buy_price = EXCLUDED.avg_buy_price, updated_at = EXCLUDED.updated_at`,
		userID, asset, avg).Error)
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

type assetBalance struct {
	available   decimal.Decimal
	locked      decimal.Decimal
	avgBuyPrice decimal.Decimal
}

func userAssetBalances(t *testing.T, db *gorm.DB, userID uint) map[string]assetBalance {
	t.Helper()

	var rows []struct {
		Asset       string
		Available   decimal.Decimal
		Locked      decimal.Decimal
		AvgBuyPrice decimal.Decimal
	}
	require.NoError(t, db.Raw(`
		SELECT
			a.asset AS asset,
			COALESCE(SUM(b.balance) FILTER (WHERE a.account_type = 'USER_AVAILABLE'), 0) AS available,
			COALESCE(SUM(b.balance) FILTER (WHERE a.account_type = 'USER_LOCKED'), 0)    AS locked,
			COALESCE(MAX(s.avg_buy_price), 0)                                            AS avg_buy_price
		FROM accounts a
		JOIN account_balances b ON b.account_id = a.id
		LEFT JOIN user_asset_stats s ON s.user_id = a.owner_user_id AND s.asset = a.asset
		WHERE a.owner_user_id = ?
		GROUP BY a.asset`, userID).Scan(&rows).Error)

	balances := make(map[string]assetBalance, len(rows))
	for _, row := range rows {
		balances[row.Asset] = assetBalance{available: row.Available, locked: row.Locked, avgBuyPrice: row.AvgBuyPrice}
	}
	return balances
}

// assertBalancesMatchWithAvgBuyPriceTolerance는 available·locked는 정확히 같아야
// 하고(덧셈뿐이라 순서 무관), avg_buy_price만 tolerance 이내로 본다 — 나눗셈이
// 개입해 배치 커밋 순서에 따라 유한 정밀도 마지막 자리가 달라질 수 있기 때문이다.
func assertBalancesMatchWithAvgBuyPriceTolerance(t *testing.T, db *gorm.DB, leftUserID uint, rightUserID uint) {
	t.Helper()

	left := userAssetBalances(t, db, leftUserID)
	right := userAssetBalances(t, db, rightUserID)
	require.NotEmpty(t, left, "잔액을 하나도 읽지 못했다 user %d", leftUserID)
	require.Equal(t, len(right), len(left), "asset count mismatch user %d vs %d", leftUserID, rightUserID)

	for asset, lb := range left {
		rb, ok := right[asset]
		require.True(t, ok, "asset %s missing for user %d", asset, rightUserID)
		assert.True(t, lb.available.Equal(rb.available), "available user %d/%d asset %s: %s vs %s", leftUserID, rightUserID, asset, lb.available, rb.available)
		assert.True(t, lb.locked.Equal(rb.locked), "locked user %d/%d asset %s: %s vs %s", leftUserID, rightUserID, asset, lb.locked, rb.locked)

		diff := lb.avgBuyPrice.Sub(rb.avgBuyPrice).Abs()
		assert.True(t, diff.LessThanOrEqual(crossBatchAvgBuyPriceTolerance),
			"AvgBuyPrice 오차 %s가 허용치 %s 초과: left=%s right=%s (user %d/%d asset %s)",
			diff, crossBatchAvgBuyPriceTolerance, lb.avgBuyPrice, rb.avgBuyPrice, leftUserID, rightUserID, asset)
	}
}

type postingTotals struct {
	count          int64
	availableDelta decimal.Decimal
	lockedDelta    decimal.Decimal
}

func postingTotalsByAsset(t *testing.T, db *gorm.DB, userID uint) map[string]postingTotals {
	t.Helper()

	var rows []struct {
		Asset       string
		AccountType string
		Amount      decimal.Decimal
	}
	require.NoError(t, db.Raw(`
		SELECT p.asset, a.account_type, p.amount
		FROM postings p
		JOIN accounts a ON a.id = p.account_id
		WHERE a.owner_user_id = ?`, userID).Scan(&rows).Error)

	totals := make(map[string]postingTotals, 2)
	for _, row := range rows {
		cur := totals[row.Asset]
		cur.count++
		switch model.AccountType(row.AccountType) {
		case model.AccountUserAvailable:
			cur.availableDelta = cur.availableDelta.Add(row.Amount)
		case model.AccountUserLocked:
			cur.lockedDelta = cur.lockedDelta.Add(row.Amount)
		}
		totals[row.Asset] = cur
	}
	return totals
}

// 원장은 전기 개수와 자산별 delta 합계만 비교한다. 행별 잔액 캐시는 두 배치 중
// 어느 쪽이 먼저 커밋되느냐에 따라 중간 값이 달라지므로(순서 의존) 동일 단언
// 대상이 아니다.
func assertPostingTotalsMatch(t *testing.T, db *gorm.DB, leftUserID uint, rightUserID uint) {
	t.Helper()

	left := postingTotalsByAsset(t, db, leftUserID)
	right := postingTotalsByAsset(t, db, rightUserID)
	// 양쪽 다 0행이면 "합계가 같다"는 자명하게 참이므로 비교가 무의미하다.
	require.NotEmpty(t, left, "postings missing for user %d", leftUserID)
	require.Equal(t, len(right), len(left), "posting asset count user %d vs %d", leftUserID, rightUserID)
	for asset, lt := range left {
		rt, ok := right[asset]
		require.True(t, ok, "postings missing for asset %s user %d", asset, rightUserID)
		require.Positive(t, lt.count, "posting count 0 user %d asset %s", leftUserID, asset)
		assert.Equal(t, rt.count, lt.count, "posting count user %d/%d asset %s", leftUserID, rightUserID, asset)
		assert.True(t, lt.availableDelta.Equal(rt.availableDelta), "available delta sum user %d/%d asset %s: %s vs %s", leftUserID, rightUserID, asset, lt.availableDelta, rt.availableDelta)
		assert.True(t, lt.lockedDelta.Equal(rt.lockedDelta), "locked delta sum user %d/%d asset %s: %s vs %s", leftUserID, rightUserID, asset, lt.lockedDelta, rt.lockedDelta)
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
func assertCrossBatchAssetConservation(t *testing.T, db *gorm.DB, f crossBatchFixture) {
	t.Helper()

	krw, btc := decimal.Zero, decimal.Zero
	for _, id := range f.userIDs {
		balances, err := repository.NewAccountRepository(db).ListUserBalances(id)
		require.NoError(t, err)
		for _, b := range balances {
			total := b.Available.Add(b.Locked)
			if b.Asset == model.KRWAssetSymbol {
				krw = krw.Add(total)
			} else {
				btc = btc.Add(total)
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
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db))

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
	forwardCoin := userAssetBalances(t, db, forward.buyerID)["BTC"]
	forwardQuantity := forwardCoin.available.Add(forwardCoin.locked)
	assert.True(t, forwardQuantity.Equal(crossBatchExpectedQuantity),
		"정순 Quantity %s != 기대값 %s", forwardQuantity, crossBatchExpectedQuantity)
	assert.True(t, forwardCoin.avgBuyPrice.Equal(crossBatchExpectedForwardAvgBuyPrice),
		"정순 AvgBuyPrice %s != 기대값 %s", forwardCoin.avgBuyPrice, crossBatchExpectedForwardAvgBuyPrice)
	assertCrossBatchAssetConservation(t, db, forward)

	// 2단계: 역순·동시가 그 기준선과 tolerance 이내로 같은가.
	orderIDs := append([]uint{}, forward.orderIDs...)
	for _, other := range []crossBatchFixture{reverse, concurrent} {
		for idx := range forward.userIDs {
			assertBalancesMatchWithAvgBuyPriceTolerance(t, db, forward.userIDs[idx], other.userIDs[idx])
			assertPostingTotalsMatch(t, db, forward.userIDs[idx], other.userIDs[idx])
		}
		for idx := range forward.orderIDs {
			assertOrdersMatch(t, db, forward.orderIDs[idx], other.orderIDs[idx])
		}
		assertCrossBatchAssetConservation(t, db, other)
		orderIDs = append(orderIDs, other.orderIDs...)

		otherCoin := userAssetBalances(t, db, other.buyerID)["BTC"]
		t.Logf("매수자 AvgBuyPrice: 정순=%s 비교=%s 차이=%s",
			forwardCoin.avgBuyPrice, otherCoin.avgBuyPrice,
			forwardCoin.avgBuyPrice.Sub(otherCoin.avgBuyPrice).Abs())
	}

	assertNoSettlementFailures(t, db, orderIDs)
}
