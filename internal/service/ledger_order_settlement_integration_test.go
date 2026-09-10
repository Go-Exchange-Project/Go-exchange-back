package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// TestIntegrationHoldBatchFindsLedgerFundedBalance는 HoldCoordinator.HoldBatch가
// 원장에 지급된 잔액을 실제로 찾는지 확인한다.
//
// accountIDByKey를 포인터 필드(OwnerUserID *uint)가 있는 repository.AccountSpec으로
// 키를 잡으면, Go의 구조체 비교가 포인터를 주소로 비교해 조회가 항상 실패한다 — 그러면
// 잔고와 무관하게 모든 주문이 "insufficient available balance"로 거절된다. 이 테스트는
// 그 회귀를 잡는다: 첫 주문은 충분한 잔고로 통과해야 하고, 잔고를 다 쓴 두 번째 주문은
// 여전히 정확히 거절돼야 한다(조회 버그가 아니라 실제 잔고 부족으로).
func TestIntegrationHoldBatchFindsLedgerFundedBalance(t *testing.T) {
	db := openServiceIntegrationDB(t)
	buyerID := serviceTestUserID(960)
	defer cleanupServiceUsers(t, db, buyerID)

	devService := NewDevWalletService(db)
	_, err := devService.FundWallet(FundWalletInput{
		UserID: buyerID, CoinSymbol: model.KRWAssetSymbol, Amount: "150",
		RequestKey: fmt.Sprintf("fund-hb-%d", buyerID),
	})
	require.NoError(t, err)

	orderRepo := repository.NewOrderRepository(db)
	coordinator := &HoldCoordinator{DB: db, OrderRepo: orderRepo, Ledger: NewLedgerService(db)}

	// 100*1*1.0005 = 100.05씩 필요하다. 150에서 첫 주문은 통과하고(잔여 49.95),
	// 같은 배치의 두 번째 주문은 잔여로 감당할 수 없어 거절돼야 한다.
	ok := &model.Order{UserID: buyerID, CoinSymbol: "BTC", Side: model.OrderSideBuy, OrderType: model.OrderTypeLimit, Price: decimal.NewFromInt(100), Amount: decimal.NewFromInt(1), Status: model.OrderStatusPending}
	tooMuch := &model.Order{UserID: buyerID, CoinSymbol: "BTC", Side: model.OrderSideBuy, OrderType: model.OrderTypeLimit, Price: decimal.NewFromInt(100), Amount: decimal.NewFromInt(1), Status: model.OrderStatusPending}

	results, err := coordinator.HoldBatch([]holdRequest{{order: ok}, {order: tooMuch}})
	require.NoError(t, err)
	require.Len(t, results, 2)

	require.NoError(t, results[0].Err, "충분한 잔고인데도 hold가 거절됐다 — 계정 조회 회귀")
	require.NotZero(t, results[0].Order.ID)

	require.Error(t, results[1].Err, "잔고를 초과했는데 두 번째 주문이 통과했다")
	kind, ok2 := DomainErrorKind(results[1].Err)
	require.True(t, ok2)
	assert.Equal(t, ErrorKindConflict, kind)
}

// journalPostingSumByAsset은 멱등성 키로 찾은 분개의 전기를 자산별로 합산한다.
// LedgerService.Record가 저장 시점에 이미 합 0을 강제하므로, 이 값이 0이 아니면
// 저장 자체가 실패했어야 한다 — 여기서는 tradePostings가 만든 구조가 실제로
// 그 계약을 만족하는지 명시적으로 고정한다.
func journalPostingSumByAsset(t *testing.T, db *gorm.DB, idempotencyKey string, asset string) decimal.Decimal {
	t.Helper()

	var journal model.JournalEntry
	require.NoError(t, db.Where("idempotency_key = ?", idempotencyKey).First(&journal).Error)

	var postings []model.Posting
	require.NoError(t, db.Where("journal_id = ? AND asset = ?", journal.ID, asset).Find(&postings).Error)

	sum := decimal.Zero
	for _, posting := range postings {
		sum = sum.Add(posting.Amount)
	}
	return sum
}

// avgBuyPriceOf는 user_asset_stats에서 평균매수가를 읽는다. 통계 행이 없으면 0이다.
func avgBuyPriceOf(t *testing.T, db *gorm.DB, userID uint, asset string) decimal.Decimal {
	t.Helper()

	var avg decimal.Decimal
	err := db.Raw(`
		SELECT COALESCE(avg_buy_price, 0) FROM user_asset_stats
		WHERE user_id = ? AND asset = ?`, userID, asset).Scan(&avg).Error
	require.NoError(t, err)
	return avg
}

// feeIncomeBalance는 FEE_INCOME 계정의 현재 잔액 캐시를 읽는다. 계정이 아직 없으면
// (이 자산으로 수수료를 받은 적이 없으면) 0이다 — 공유 테스트 DB에 걸쳐 누적되는
// 값이므로 호출자는 절대값이 아니라 전후 델타를 비교해야 한다.
func feeIncomeBalance(t *testing.T, db *gorm.DB, asset string) decimal.Decimal {
	t.Helper()

	var balance decimal.Decimal
	err := db.Raw(`
		SELECT COALESCE(b.balance, 0)
		FROM accounts a
		JOIN account_balances b ON b.account_id = a.id
		WHERE a.account_type = 'FEE_INCOME' AND a.owner_user_id IS NULL AND a.asset = ?`,
		asset).Scan(&balance).Error
	require.NoError(t, err)
	return balance
}

// TestOrderAndSettlementPreserveAssets는 Task 4의 자산 보존 증거다: 개발용 지급 →
// 지정가 매수·매도 접수(잠금) → 체결 정산 → 매수자 취소분 해제까지 하나의 시나리오를
// 끝까지 돌려, 각 단계의 전기가 자산별로 정확히 상쇄되는지 확인한다.
//
// §1.6이 우려한 "수수료가 사라진다"를 닫는 증거이기도 하다 — FEE_INCOME 잔액의
// 증가분이 정확히 BuyerFee+SellerFee와 같아야 한다.
func TestOrderAndSettlementPreserveAssets(t *testing.T) {
	db := openServiceIntegrationDB(t)
	buyerID := serviceTestUserID(950)
	sellerID := serviceTestUserID(951)
	defer cleanupServiceUsers(t, db, buyerID, sellerID)

	// accounts.asset은 varchar(16)이라 코인 심볼도 그 안에 들어가야 한다.
	coinSymbol := fmt.Sprintf("PRSV%d", time.Now().UnixNano()%1_000_000_000)
	ledger := NewLedgerService(db)
	orderRepo := repository.NewOrderRepository(db)

	// 1. 개발용 지급: 매수자에게 KRW, 매도자에게 코인.
	devService := NewDevWalletService(db)
	_, err := devService.FundWallet(FundWalletInput{
		UserID: buyerID, CoinSymbol: model.KRWAssetSymbol, Amount: "1000000",
		RequestKey: fmt.Sprintf("fund-buyer-%d", buyerID),
	})
	require.NoError(t, err)
	_, err = devService.FundWallet(FundWalletInput{
		UserID: sellerID, CoinSymbol: coinSymbol, Amount: "10",
		RequestKey: fmt.Sprintf("fund-seller-%d", sellerID),
	})
	require.NoError(t, err)

	// 2. 지정가 매수·매도 접수 — hold는 이미 원장 경유다(Task 4 Step 3).
	buyOrder := &model.Order{
		UserID: buyerID, CoinSymbol: coinSymbol, Side: model.OrderSideBuy,
		OrderType: model.OrderTypeLimit, Status: model.OrderStatusPending,
		Price: decimal.NewFromInt(1000), Amount: decimal.NewFromInt(10),
	}
	sellOrder := &model.Order{
		UserID: sellerID, CoinSymbol: coinSymbol, Side: model.OrderSideSell,
		OrderType: model.OrderTypeLimit, Status: model.OrderStatusPending,
		Price: decimal.NewFromInt(1000), Amount: decimal.NewFromInt(10),
	}
	require.NoError(t, persistAndHold(db, orderRepo, ledger, buyOrder))
	require.NoError(t, persistAndHold(db, orderRepo, ledger, sellOrder))

	feeIncomeBefore := feeIncomeBalance(t, db, model.KRWAssetSymbol)

	// 이 시점에 매수자 코인 계정과 매도자 KRW 계정은 아직 없다. 정산이 그것을
	// 만들면서 진행하는 경로를 여기서 함께 지난다 — 사전 확보(EnsureAccounts)가
	// 잠금(LockBalances)보다 먼저 일어나야 성립한다. 순서가 뒤집히면 아직 없는
	// 행을 잠그려다 "balance lock expected N rows"로 실패한다.
	buyerCoinAvail, buyerCoinLocked := ledgerBalances(t, db, buyerID, coinSymbol)
	require.True(t, buyerCoinAvail.IsZero() && buyerCoinLocked.IsZero(),
		"매수자 코인 계정이 이미 있으면 이 테스트가 신규 계정 경로를 지나지 않는다")
	sellerKRWAvail, sellerKRWLocked := ledgerBalances(t, db, sellerID, model.KRWAssetSymbol)
	require.True(t, sellerKRWAvail.IsZero() && sellerKRWLocked.IsZero(),
		"매도자 KRW 계정이 이미 있으면 이 테스트가 신규 계정 경로를 지나지 않는다")

	// 3. 체결 정산 — 주문 10개 중 6개만 체결해, 이후 취소 해제 경로도 함께 본다.
	settlementService := NewSettlementService(db, orderRepo)
	trade := &model.Trade{
		CoinSymbol: coinSymbol, Price: decimal.NewFromInt(1000), Quantity: decimal.NewFromInt(6),
		TradedAt: time.Now(), BuyOrderID: buyOrder.ID, SellOrderID: sellOrder.ID,
	}
	result, err := settlementService.SettleTrade(trade, 0)
	require.NoError(t, err)
	require.True(t, result.Applied)

	// 단언 1·2: 체결 분개의 자산별 전기 합이 0이다.
	require.True(t, journalPostingSumByAsset(t, db, "trade:"+trade.IdempotencyKey, model.KRWAssetSymbol).IsZero(),
		"체결 분개의 KRW 전기 합이 0이 아니다")
	require.True(t, journalPostingSumByAsset(t, db, "trade:"+trade.IdempotencyKey, coinSymbol).IsZero(),
		"체결 분개의 코인 전기 합이 0이 아니다")

	// 단언 3: FEE_INCOME 증가분이 정확히 BuyerFee+SellerFee와 같다 — 수수료가
	// 사라지지 않는다는 증거. 공유 DB에 걸쳐 누적되므로 델타로 비교한다.
	var persistedTrade model.Trade
	require.NoError(t, db.Where("idempotency_key = ?", trade.IdempotencyKey).First(&persistedTrade).Error)
	feeIncomeAfter := feeIncomeBalance(t, db, model.KRWAssetSymbol)
	require.True(t, feeIncomeAfter.Sub(feeIncomeBefore).Equal(persistedTrade.BuyerFee.Add(persistedTrade.SellerFee)),
		"FEE_INCOME 증가분이 %s, 기대값 %s", feeIncomeAfter.Sub(feeIncomeBefore), persistedTrade.BuyerFee.Add(persistedTrade.SellerFee))

	// 4. 매수자 취소분 해제 — 남은 4개.
	var persistedBuy model.Order
	require.NoError(t, db.First(&persistedBuy, buyOrder.ID).Error)
	remaining := persistedBuy.Amount.Sub(persistedBuy.FilledAmount)
	require.True(t, remaining.Equal(decimal.NewFromInt(4)))
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return releaseOrderHold(ledger, tx, &persistedBuy, remaining)
	}))

	// 단언 4: 검산 4종이 전부 위반 0건이다.
	recon := repository.NewLedgerReconciliationRepository(db)
	unbalanced, err := recon.CheckUnbalancedJournals(0, 100)
	require.NoError(t, err)
	require.Empty(t, unbalanced, "자산별 합이 0이 아닌 분개가 있다")
	drift, err := recon.CheckBalanceCacheDrift(0, 100)
	require.NoError(t, err)
	require.Empty(t, drift, "잔액 캐시가 전기 합과 어긋난다")
	totals, err := recon.CheckAssetTotals()
	require.NoError(t, err)
	require.Empty(t, totals, "자산 전체 합이 0이 아니다")
	negative, err := recon.CheckNegativeAccounts(0, 100)
	require.NoError(t, err)
	require.Empty(t, negative, "음수가 되면 안 되는 계정이 음수다")

	// 단언 5(Step 6): avg_buy_price가 단건 정산 N회와 배치 정산 1회에서 같다 —
	// 등가성 불변식이 평균매수가에서도 성립해야 한다.
	t.Run("avg_buy_price가 단건과 배치가 같다", func(t *testing.T) {
		singleBuyer := serviceTestUserID(952)
		singleSeller := serviceTestUserID(953)
		batchBuyer := serviceTestUserID(954)
		batchSeller := serviceTestUserID(955)
		defer cleanupServiceUsers(t, db, singleBuyer, singleSeller, batchBuyer, batchSeller)

		asset := fmt.Sprintf("AVG%d", time.Now().UnixNano()%1_000_000_000)
		fund := func(userID uint, coin string) {
			_, err := devService.FundWallet(FundWalletInput{
				UserID: userID, CoinSymbol: coin, Amount: "1000000",
				RequestKey: fmt.Sprintf("fund-%d-%s", userID, coin),
			})
			require.NoError(t, err)
		}
		fund(singleBuyer, model.KRWAssetSymbol)
		fund(singleSeller, asset)
		fund(batchBuyer, model.KRWAssetSymbol)
		fund(batchSeller, asset)

		// 매수 한도가는 두 체결가보다 높고 매도 한도가는 둘보다 낮아야 한다 — 실제
		// 매칭엔진은 이 범위 밖에서 체결시키지 않는다. 한도가와 같은 값을 쓰면
		// 체결가가 둘 다 같아져 가중평균 계산 자체가 시험되지 않는다.
		mkOrders := func(buyerID, sellerID uint) (*model.Order, *model.Order) {
			buy := &model.Order{
				UserID: buyerID, CoinSymbol: asset, Side: model.OrderSideBuy,
				OrderType: model.OrderTypeLimit, Status: model.OrderStatusPending,
				Price: decimal.NewFromInt(1200), Amount: decimal.NewFromInt(20),
			}
			sell := &model.Order{
				UserID: sellerID, CoinSymbol: asset, Side: model.OrderSideSell,
				OrderType: model.OrderTypeLimit, Status: model.OrderStatusPending,
				Price: decimal.NewFromInt(800), Amount: decimal.NewFromInt(20),
			}
			require.NoError(t, persistAndHold(db, orderRepo, ledger, buy))
			require.NoError(t, persistAndHold(db, orderRepo, ledger, sell))
			return buy, sell
		}
		singleBuy, singleSell := mkOrders(singleBuyer, singleSeller)
		batchBuy, batchSell := mkOrders(batchBuyer, batchSeller)

		// 서로 다른 가격의 체결 두 건이어야 가중평균이 산술로만 맞아떨어지지 않는다.
		single1 := &model.Trade{CoinSymbol: asset, Price: decimal.NewFromInt(900), Quantity: decimal.NewFromInt(5), TradedAt: time.Now(), BuyOrderID: singleBuy.ID, SellOrderID: singleSell.ID}
		single2 := &model.Trade{CoinSymbol: asset, Price: decimal.NewFromInt(1100), Quantity: decimal.NewFromInt(7), TradedAt: time.Now(), BuyOrderID: singleBuy.ID, SellOrderID: singleSell.ID}
		_, err := settlementService.SettleTrade(single1, 0)
		require.NoError(t, err)
		_, err = settlementService.SettleTrade(single2, 0)
		require.NoError(t, err)

		batch1 := &model.Trade{CoinSymbol: asset, Price: decimal.NewFromInt(900), Quantity: decimal.NewFromInt(5), TradedAt: time.Now(), BuyOrderID: batchBuy.ID, SellOrderID: batchSell.ID}
		batch2 := &model.Trade{CoinSymbol: asset, Price: decimal.NewFromInt(1100), Quantity: decimal.NewFromInt(7), TradedAt: time.Now(), BuyOrderID: batchBuy.ID, SellOrderID: batchSell.ID}
		_, err = settlementService.SettleTradeBatch([]TradeBatchItem{{Trade: batch1}, {Trade: batch2}})
		require.NoError(t, err)

		singleAvg := avgBuyPriceOf(t, db, singleBuyer, asset)
		batchAvg := avgBuyPriceOf(t, db, batchBuyer, asset)
		require.True(t, singleAvg.IsPositive())
		require.True(t, singleAvg.Equal(batchAvg), "단건 평단가 %s, 배치 평단가 %s", singleAvg, batchAvg)
	})
}
