package service

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// failingOutboxRepo는 failing이 true인 동안 호출 수를 세고 sentinel error를
// 반환한다. false가 되면 inner(실제 TradeOutboxRepository)로 위임하고, 성공한
// 호출 수를 센다. 기존 blockableOutboxRepo.block()은 대기만 하고 해제 후
// 곧바로 성공해 오류를 한 번도 반환하지 않으므로, OutboxWriter.flushAndForward의
// 무한 재시도 경로를 지나지 않는다 — 그래서 이 decorator를 새로 둔다.
type failingOutboxRepo struct {
	inner *repository.TradeOutboxRepository

	failing   atomic.Bool
	failed    atomic.Int64 // 오류를 반환한 호출 수
	succeeded atomic.Int64 // inner 호출이 성공한 수
}

var errFailingOutboxRepoInjected = fmt.Errorf("failingOutboxRepo: injected failure")

func (r *failingOutboxRepo) InsertBatchAndMarkCancelCommands(events []*model.TradeOutboxEvent, commandIDs []uint64) error {
	// failing.Load() 확인과 failed.Add(1) 사이에 테스트가 failing = false로
	// 바꿀 수 있다 — 해제 직후 failed가 한 번 더 늘어도 정상이다. failed의
	// 해제 후 불변은 단언하지 않는다.
	if r.failing.Load() {
		r.failed.Add(1)
		return errFailingOutboxRepoInjected
	}
	if err := r.inner.InsertBatchAndMarkCancelCommands(events, commandIDs); err != nil {
		return err
	}
	r.succeeded.Add(1)
	return nil
}

// 설계 §9.2 테스트 12 — outbox 저장 실패 → 회복. OutboxWriter가 무한 재시도에
// 갇혀 ExecutionCh를 소비하지 못하는 동안, 엔진은 조각 시작 조건 부족으로
// park하면서도 취소 quota(=maxConsecutiveCancels) 안의 취소는 계속 처리한다.
func TestExecutionCapacityOutboxFailureRecovers(t *testing.T) {
	db := openServiceIntegrationDB(t)
	symbol := harnessSymbol(t)
	cleanupHarnessOutbox(t, db, symbol)

	const makerCount = 30 // 1(outbox 보류) + 16(채널) + 조각 예산(5) 이상
	price := decimal.NewFromInt(100)

	makerIDs := make([]uint, makerCount)
	for i := range makerIDs {
		makerIDs[i] = serviceTestUserID(uint(900 + i))
	}
	takerID := serviceTestUserID(950)
	victimID := serviceTestUserID(951)
	cleanupIDs := append(append([]uint{}, makerIDs...), takerID, victimID)
	defer cleanupServiceUsers(t, db, cleanupIDs...)

	engine, err := matching.NewMatchingEngineWithQuantum(matching.QuantumConfig{MaxMatchesPerTurn: 4, MaxConsecutiveCancels: 2})
	require.NoError(t, err)
	engine.ExecutionCh = make(chan matching.ExecutionEvent, 16) // Start() 전(§2.3)

	var parkStarted, cancelBackpressured atomic.Int64
	engine.Observers = matching.EngineObservers{
		ParkStarted:         func() { parkStarted.Add(1) },
		CancelBackpressured: func() { cancelBackpressured.Add(1) },
	}

	orderRepo := repository.NewOrderRepository(db)
	orderService := NewOrderService(orderRepo, engine)
	commandRepo := repository.NewCancelCommandRepository(db)
	orderService.CancelCommandRepository = commandRepo
	settlement := NewSettlementService(db, orderRepo)

	outboxRepo := &failingOutboxRepo{inner: repository.NewTradeOutboxRepository(db)}
	outboxRepo.failing.Store(true)

	forwarded := make(chan OutboxEvent, 4096)
	writer := &OutboxWriter{
		Repo:           outboxRepo,
		Source:         engine.ExecutionCh,
		Forward:        func(e OutboxEvent) { forwarded <- e },
		BatchSize:      1,
		RetryBaseDelay: 5 * time.Millisecond,
	}
	writerDone := make(chan struct{})

	scopedStore := &symbolScopedCancelCommandStore{inner: commandRepo, symbol: symbol}
	worker := NewCancelCommandWorker(scopedStore, orderRepo, engine)
	worker.PollInterval = 10 * time.Millisecond
	orderService.CancelCommandWake = worker.Wake
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	workerDone := make(chan struct{})

	engine.Start()
	go func() { writer.Run(); close(writerDone) }()
	go func() { worker.Run(workerCtx); close(workerDone) }()

	// 본문 마지막에 파이프라인을 명시적으로 멈추므로(아래), 여기 도달할 때는
	// 이미 정지된 뒤라 사실상 no-op다(Stop 멱등, 닫힌 채널 대기는 즉시 반환) —
	// 본문이 중간에 t.Fatal로 끝나는 경로를 위해 안전망으로 남긴다.
	t.Cleanup(func() {
		cancelWorker()
		select {
		case <-workerDone:
		case <-time.After(5 * time.Second):
			t.Error("cancel worker가 정지하지 않았다")
		}
		engine.Stop()
		select {
		case <-engine.Done():
		case <-time.After(5 * time.Second):
			t.Error("엔진이 드레인되지 않았다")
		}
		select {
		case <-writerDone:
		case <-time.After(5 * time.Second):
			t.Error("outbox writer가 종료되지 않았다")
		}
	})

	// maker N개(매도, price) + taker 1개(매수, price, amount=N) — sweep이
	// 조각 시작 조건 부족으로 park하게 만든다.
	makers := make([]model.Order, makerCount)
	for i, userID := range makerIDs {
		makers[i] = seedCancelOrderRows(t, db, cancelOrderSeed{
			UserID: userID, CoinSymbol: symbol, Side: model.OrderSideSell,
			Status: model.OrderStatusPending, Price: price, Amount: decimal.NewFromInt(1),
			FilledAmount: decimal.Zero, LockedBalance: decimal.NewFromInt(1),
		})
		submitIntegrationEngineOrder(t, engine, makers[i], decimal.NewFromInt(1))
	}

	// victim 3건은 taker(sweep)보다 먼저 admit돼야 한다 — active sweep이 있는
	// 동안은 admission phase 자체가 건너뛰어지므로(§3.2 5단계, hadActive), sweep
	// 시작 후에 넣으면 book에 오르지 못한 채 OrderCh에 걸린다. sweep 가격(=price)과
	// 겹치지 않도록 훨씬 높은 가격에 매도로 둔다.
	victimPrice := price.Add(decimal.NewFromInt(1_000_000))
	victims := make([]model.Order, 3)
	for i := range victims {
		victims[i] = seedCancelOrderRows(t, db, cancelOrderSeed{
			UserID: victimID, CoinSymbol: symbol, Side: model.OrderSideSell,
			Status: model.OrderStatusPending, Price: victimPrice, Amount: decimal.NewFromInt(1),
			FilledAmount: decimal.Zero, LockedBalance: decimal.NewFromInt(1),
		})
		submitIntegrationEngineOrder(t, engine, victims[i], decimal.NewFromInt(1))
	}

	taker := seedCancelOrderRows(t, db, cancelOrderSeed{
		UserID: takerID, CoinSymbol: symbol, Side: model.OrderSideBuy,
		Status: model.OrderStatusPending, Price: price, Amount: decimal.NewFromInt(makerCount),
		FilledAmount:  decimal.Zero,
		LockedBalance: price.Mul(decimal.NewFromInt(makerCount)).Mul(decimal.RequireFromString("1.0005")),
	})
	submitIntegrationEngineOrder(t, engine, taker, decimal.NewFromInt(makerCount))

	// 장벽 1: OutboxWriter가 적어도 한 번 오류를 받고 재시도 루프에 들어갔다.
	require.Eventually(t, func() bool { return outboxRepo.failed.Load() >= 1 }, 5*time.Second, 5*time.Millisecond,
		"outbox 저장 실패가 관측되지 않았다")

	// 장벽 2: 엔진이 park했다(OutboxWriter가 꺼낸 1건 + 채널 16칸이 찼다).
	require.Eventually(t, func() bool { return parkStarted.Load() >= 1 }, 10*time.Second, 5*time.Millisecond,
		"엔진이 park하지 않았다")

	// C+1(=3)건 취소를 실제 OrderService.CancelOrder로 요청한다. worker가
	// 이미 돌고 있으므로 PENDING command를 곧 집어 엔진에 디스패치한다.
	cancelResults := make([]*CancelOrderResult, len(victims))
	for i, victim := range victims {
		result, err := orderService.CancelOrder(CancelOrderInput{UserID: victimID, OrderID: victim.ID})
		require.NoError(t, err)
		cancelResults[i] = result
	}
	commandIDs := make([]uint64, len(cancelResults))
	for i, r := range cancelResults {
		commandIDs[i] = r.CommandID
	}
	defer cleanupServiceCancelCommands(t, db, commandIDs...)

	// 장벽 3: DB에서 직접 확인한다(observer 횟수만으로는 세 건 모두 잘못
	// 거절한 구현도 통과한다) — AttemptCount>0인 command가 정확히 1개,
	// 나머지 2개는 0, 세 command 모두 아직 PENDING.
	require.Eventually(t, func() bool {
		var commands []model.CancelCommand
		if err := db.Where("id IN ?", commandIDs).Find(&commands).Error; err != nil || len(commands) != 3 {
			return false
		}
		attempted := 0
		allPending := true
		for _, c := range commands {
			if c.AttemptCount > 0 {
				attempted++
			}
			if c.Status != model.CancelCommandStatusPending {
				allPending = false
			}
		}
		return attempted == 1 && allPending
	}, 10*time.Second, 10*time.Millisecond,
		"AttemptCount>0인 command가 정확히 1개, 나머지는 PENDING·0이어야 한다")

	// 회복: failed >= 1을 이미 확인한 상태에서 failing = false.
	outboxRepo.failing.Store(false)

	require.Eventually(t, func() bool { return outboxRepo.succeeded.Load() >= 1 }, 10*time.Second, 5*time.Millisecond,
		"회복 후에도 outbox 저장이 성공하지 않았다")
	require.Eventually(t, func() bool {
		var count int64
		require.NoError(t, db.Model(&model.TradeOutboxEvent{}).Where("coin_symbol = ?", symbol).Count(&count).Error)
		return count > 0
	}, 10*time.Second, 10*time.Millisecond, "회복 후에도 이 심볼의 outbox 행이 생성되지 않았다")

	// release → 정산을 settleForwarded 방식으로 끝까지 흘린다.
	// 기대 이벤트 수: Trade makerCount건 + OrderCancelled 3건. 루프·이후 drain
	// 양쪽에서 같은 처리를 쓰므로 클로저로 묶는다.
	outboxRepoDirect := repository.NewTradeOutboxRepository(db)
	var tradeCount, cancelledCount int
	processForwardedEvent := func(event OutboxEvent) {
		switch {
		case event.Event.Trade != nil:
			_, err := settlement.SettleTrade(event.Event.Trade, event.OutboxID)
			require.NoError(t, err)
			tradeCount++
		case event.Event.OrderCancelled != nil:
			require.NoError(t, orderService.ProcessOrderCancellation(*event.Event.OrderCancelled))
			cancelledCount++
		}
		require.NoError(t, outboxRepoDirect.MarkProcessed(event.OutboxID))
	}

	deadline := time.After(30 * time.Second)
	for tradeCount < makerCount || cancelledCount < 3 {
		select {
		case event := <-forwarded:
			processForwardedEvent(event)
		case <-deadline:
			t.Fatalf("이벤트를 기다리다 시간 초과(trade=%d/%d, cancelled=%d/3)", tradeCount, makerCount, cancelledCount)
		}
	}

	// 기대 개수 이후에 더 붙는 이벤트(중복·유실)를 놓치지 않으려면, 위 카운트
	// 도달 즉시 채널을 버려두는 대신 파이프라인을 순서대로 멈추고 그 안에 남은
	// 것까지 전부 비운다. cleanupServiceUsers·cleanupServiceCancelCommands의
	// defer가 (이 함수 자신의 defer 스택이라) t.Cleanup보다 먼저 실행되므로,
	// worker·engine·writer가 아직 도는 채로 픽스처를 지우면 안 된다 — 여기서
	// 명시적으로 먼저 멈춘다.
	cancelWorker()
	select {
	case <-workerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel worker가 정지하지 않았다")
	}

	engine.Stop()
	select {
	case <-engine.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("엔진이 드레인되지 않았다")
	}

	// writerDone을 기다리는 동안에도 forwarded를 계속 읽는다 — writer가
	// Forward에서 막혀 교착되지 않게 한다.
	writerStopDeadline := time.After(10 * time.Second)
	for waiting := true; waiting; {
		select {
		case event := <-forwarded:
			processForwardedEvent(event)
		case <-writerDone:
			waiting = false
		case <-writerStopDeadline:
			t.Fatal("outbox writer가 종료되지 않았다")
		}
	}

	// writer 종료 후에는 더 이상 생산자가 없다 — forwarded에 남은 것을
	// 논블로킹으로 전부 비운다.
drain:
	for {
		select {
		case event := <-forwarded:
			processForwardedEvent(event)
		default:
			break drain
		}
	}

	// 최종 단언.
	require.Eventually(t, func() bool {
		var commands []model.CancelCommand
		require.NoError(t, db.Where("id IN ?", commandIDs).Find(&commands).Error)
		for _, c := range commands {
			if c.Status != model.CancelCommandStatusProcessed {
				return false
			}
		}
		return len(commands) == 3
	}, 10*time.Second, 10*time.Millisecond, "세 취소 command 모두 PROCESSED여야 한다")

	require.Equal(t, makerCount, tradeCount, "체결 수가 기대값과 정확히 일치해야 한다(기대 개수 이후 추가 이벤트가 없어야 한다)")
	require.Equal(t, 3, cancelledCount, "취소 이벤트가 정확히 3건이어야 한다(기대 개수 이후 추가 이벤트가 없어야 한다)")

	var totalOutboxRows int64
	require.NoError(t, db.Model(&model.TradeOutboxEvent{}).Where("coin_symbol = ?", symbol).Count(&totalOutboxRows).Error)
	require.EqualValues(t, makerCount+3, totalOutboxRows, "이 심볼 outbox 총 행 수가 33(=maker 30 + 취소 3)이어야 한다")

	var stillPendingOutboxRows int64
	require.NoError(t, db.Model(&model.TradeOutboxEvent{}).
		Where("coin_symbol = ? AND status = ?", symbol, model.TradeOutboxStatusPending).
		Count(&stillPendingOutboxRows).Error)
	require.EqualValues(t, 0, stillPendingOutboxRows, "이 심볼 outbox에 PENDING이 남지 않아야 한다")

	for _, victim := range victims {
		var order model.Order
		require.NoError(t, db.First(&order, victim.ID).Error)
		require.Equal(t, model.OrderStatusCancelled, order.Status, "victim 주문 %d는 CANCELLED여야 한다", victim.ID)

		var releaseCount int64
		require.NoError(t, db.Model(&model.JournalEntry{}).
			Where("reference_type = ? AND reference_id = ? AND event_type = ?",
				model.JournalReferenceOrder, victim.ID, model.JournalEventOrderRelease).
			Count(&releaseCount).Error)
		require.Equal(t, int64(1), releaseCount, "victim 주문 %d는 release 분개가 정확히 1건이어야 한다", victim.ID)
	}

	assertNoDBReconciliationViolations(t, db, symbol, append(append([]uint{}, makerIDs...), takerID, victimID), "KRW", symbol)
}

// assertNoDBReconciliationViolations는 검산 4종을 이 테스트가 만든 계정·journal
// 범위로 한정해 리포지토리 Check를 직접 호출해 확인한다(TestAllEventsPassReconciliation의
// 선례, transfer_integration_test.go). ReconciliationWorker.RunOnce()를 거치는
// 이전 버전은 subject_key가 "account:%d"인 검사(balance_cache_drift·negative_account)만
// 걸러내고, "journal:%d"인 unbalanced_journal과 "asset:%s"인 asset_totals는 전혀
// 걸러지지 않아 이 함수로는 그 두 검사가 통과한 것처럼 보였다 — 판별력 구멍이었다.
func assertNoDBReconciliationViolations(t *testing.T, db *gorm.DB, symbol string, userIDs []uint, assets ...string) {
	t.Helper()

	var accountIDs []uint
	require.NoError(t, db.Raw(`
		SELECT id FROM accounts
		WHERE owner_user_id IN ? AND asset IN ? AND account_type IN ('USER_AVAILABLE','USER_LOCKED')`,
		userIDs, assets).Scan(&accountIDs).Error)
	require.NotEmpty(t, accountIDs, "계정이 아직 없다")

	// FEE_INCOME은 사용자 소유가 아니라 자산별 전역 계정이다 — 존재하는 것만 범위에 넣는다.
	var feeAccountIDs []uint
	require.NoError(t, db.Raw(`
		SELECT id FROM accounts WHERE asset IN ? AND account_type = 'FEE_INCOME'`, assets).Scan(&feeAccountIDs).Error)
	scopeAccountIDs := append(append([]uint{}, accountIDs...), feeAccountIDs...)
	scopeSet := make(map[uint]bool, len(scopeAccountIDs))
	for _, id := range scopeAccountIDs {
		scopeSet[id] = true
	}

	recon := repository.NewLedgerReconciliationRepository(db)

	// 검사 1(CheckUnbalancedJournals): 범위 계정에 posting이 있는 journal ID
	// 집합을 postings에서 먼저 구하고, 전체 결과 중 그 집합에 속하는 행이
	// 0건인지 본다. 종료 판정은 reconciliation_worker.go의 runUnbalancedJournalCheck와
	// 같다 — 페이지 안의 서로 다른 journal 수로 판정한다(한 journal이 자산
	// 여러 종의 불균형 행을 가질 수 있어 행 수로는 안 된다).
	var scopedJournalIDs []uint
	require.NoError(t, db.Raw(`SELECT DISTINCT journal_id FROM postings WHERE account_id IN ?`, scopeAccountIDs).Scan(&scopedJournalIDs).Error)
	journalScope := make(map[uint]bool, len(scopedJournalIDs))
	for _, id := range scopedJournalIDs {
		journalScope[id] = true
	}
	const pageSize = 1000
	var unbalancedInScope []repository.UnbalancedJournalRow
	var afterJournalID uint
	for {
		rows, err := recon.CheckUnbalancedJournals(afterJournalID, pageSize)
		require.NoError(t, err)
		if len(rows) == 0 {
			break
		}
		journalCount := 0
		var lastJournalID uint
		for i, row := range rows {
			if i == 0 || row.JournalID != lastJournalID {
				journalCount++
				lastJournalID = row.JournalID
			}
			if journalScope[row.JournalID] {
				unbalancedInScope = append(unbalancedInScope, row)
			}
			afterJournalID = row.JournalID
		}
		if journalCount < pageSize {
			break
		}
	}
	require.Empty(t, unbalancedInScope, "검산 1(unbalanced_journal) 위반이 없어야 한다(심볼 %s): %+v", symbol, unbalancedInScope)

	// 검사 2(CheckBalanceCacheDrift): 범위 계정만.
	var driftInScope []repository.BalanceDriftRow
	var afterAccountID uint
	for {
		rows, err := recon.CheckBalanceCacheDrift(afterAccountID, pageSize)
		require.NoError(t, err)
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			if scopeSet[row.AccountID] {
				driftInScope = append(driftInScope, row)
			}
			afterAccountID = row.AccountID
		}
		if len(rows) < pageSize {
			break
		}
	}
	require.Empty(t, driftInScope, "검산 2(balance_cache_drift) 위반이 없어야 한다(심볼 %s): %+v", symbol, driftInScope)

	// 검사 3(CheckAssetTotals): 이 심볼만 본다. KRW는 전역 자산이라 이 테스트
	// 밖의 다른 테스트가 남긴 잔여물의 영향을 받을 수 있어 여기서는 단언하지
	// 않는다 — KRW 쪽 불균형이 있다면 그 journal은 범위 밖(우리 계정과
	// 무관)이거나, 우리 계정과 관련됐다면 이미 검사 1(범위 journal)이 잡는다.
	totals, err := recon.CheckAssetTotals()
	require.NoError(t, err)
	var symbolTotal []repository.AssetTotalRow
	for _, row := range totals {
		if row.Asset == symbol {
			symbolTotal = append(symbolTotal, row)
		}
	}
	require.Empty(t, symbolTotal, "검산 3(asset_totals) 위반이 없어야 한다(심볼 %s): %+v", symbol, symbolTotal)

	// 검사 4(CheckNegativeAccounts): 범위 계정만.
	var negativeInScope []repository.NegativeAccountRow
	var afterNegativeID uint
	for {
		rows, err := recon.CheckNegativeAccounts(afterNegativeID, pageSize)
		require.NoError(t, err)
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			if scopeSet[row.AccountID] {
				negativeInScope = append(negativeInScope, row)
			}
			afterNegativeID = row.AccountID
		}
		if len(rows) < pageSize {
			break
		}
	}
	require.Empty(t, negativeInScope, "검산 4(negative_account) 위반이 없어야 한다(심볼 %s): %+v", symbol, negativeInScope)
}

// ===== Task 8: 테스트 13 — durable prefix + undurable suffix 복구 =====

// seedTaskEightOrder는 seedCancelOrderRows와 같은 모양이지만 CreatedAt을
// 명시적으로 받는다 — bootstrap 순서(created_at ASC, id ASC)를 고정하려면
// 필요하다. ID는 auto-increment 그대로 둔다: 공유 테스트 DB에서 명시적 ID를
// 강제하면 동시에 도는 다른 테스트의 시퀀스 값과 충돌할 위험이 있다. 우리 자신의
// 순차 INSERT라 id도 이 순서와 자연히 같은 방향으로 늘어난다.
func seedTaskEightOrder(t *testing.T, db *gorm.DB, userID uint, symbol string, side model.OrderSide, price, amount, lockedBalance decimal.Decimal, createdAt time.Time) model.Order {
	t.Helper()

	order := model.Order{
		UserID: userID, CoinSymbol: symbol, Side: side, OrderType: model.OrderTypeLimit,
		Price: price, Amount: amount, Status: model.OrderStatusPending,
		FilledAmount: decimal.Zero, CreatedAt: createdAt,
	}
	require.NoError(t, db.Create(&order).Error)

	asset := model.KRWAssetSymbol
	if side == model.OrderSideSell {
		asset = symbol
	}
	seedLockedBalance(t, db, userID, asset, lockedBalance, order.ID)
	return order
}

// tradeJournalCount는 특정 주문이 관여한 체결들의 JournalEventTrade 분개 수를
// 센다(JournalReferenceTrade는 ReferenceID로 order가 아니라 trade.ID를 쓴다).
func tradeJournalCount(t *testing.T, db *gorm.DB, orderID uint) int64 {
	t.Helper()
	var count int64
	require.NoError(t, db.Raw(`
		SELECT COUNT(*) FROM journal_entries
		WHERE reference_type = ? AND event_type = ?
		  AND reference_id IN (SELECT id FROM trades WHERE buy_order_id = ? OR sell_order_id = ?)`,
		model.JournalReferenceTrade, model.JournalEventTrade, orderID, orderID).Scan(&count).Error)
	return count
}

func releaseJournalCount(t *testing.T, db *gorm.DB, orderID uint) int64 {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(&model.JournalEntry{}).
		Where("reference_type = ? AND reference_id = ? AND event_type = ?",
			model.JournalReferenceOrder, orderID, model.JournalEventOrderRelease).
		Count(&count).Error)
	return count
}

func TestExecutionCapacityDurablePrefixAndUndurableSuffixRecovery(t *testing.T) {
	db := openServiceIntegrationDB(t)
	symbol := harnessSymbol(t)
	cleanupHarnessOutbox(t, db, symbol)

	const makerCount = 40
	const prefixCount = 8
	price := decimal.NewFromInt(100)
	feeRate := decimal.RequireFromString("0.0005")
	quantity := decimal.NewFromInt(1)

	executionQuote := price.Mul(quantity)                                                       // 100
	perTradeFee := executionQuote.Mul(feeRate)                                                   // 0.05
	sellerQuoteNet := executionQuote.Sub(perTradeFee)                                            // 99.95
	reservedDebitPerTrade := executionQuote.Add(perTradeFee)                                     // 100.05 — taker Price == maker Price라 refund 0
	totalFeeIncome := perTradeFee.Mul(decimal.NewFromInt(2)).Mul(decimal.NewFromInt(makerCount)) // (0.05+0.05)*40 = 4.00

	makerIDs := make([]uint, makerCount)
	for i := range makerIDs {
		makerIDs[i] = serviceTestUserID(uint(2000 + i))
	}
	takerID := serviceTestUserID(2100)
	cleanupIDs := append(append([]uint{}, makerIDs...), takerID)
	defer cleanupServiceUsers(t, db, cleanupIDs...)

	feeIncomeBefore := feeIncomeBalance(t, db, model.KRWAssetSymbol)

	// Step 1: 픽스처. maker 1..40이 taker보다 이르도록 CreatedAt을 명시 시드한다.
	base := time.Now().UTC().Add(-time.Hour)
	makers := make([]model.Order, makerCount)
	for i, userID := range makerIDs {
		makers[i] = seedTaskEightOrder(t, db, userID, symbol, model.OrderSideSell,
			price, quantity, quantity, base.Add(time.Duration(i)*time.Millisecond))
	}
	taker := seedTaskEightOrder(t, db, takerID, symbol, model.OrderSideBuy,
		price, decimal.NewFromInt(makerCount), reservedDebitPerTrade.Mul(decimal.NewFromInt(makerCount)),
		base.Add(time.Duration(makerCount)*time.Millisecond))

	// ---- 런타임 1 ----
	engine1, err := matching.NewMatchingEngineWithQuantum(matching.QuantumConfig{MaxMatchesPerTurn: 4, MaxConsecutiveCancels: 2})
	require.NoError(t, err)
	engine1.ExecutionCh = make(chan matching.ExecutionEvent, 16) // Start() 전(§2.3)

	var started1, finished1 atomic.Int64
	engine1.Observers = matching.EngineObservers{
		ParkStarted:  func() { started1.Add(1) },
		ParkDuration: func(time.Duration) { finished1.Add(1) },
	}

	outboxRepo1 := repository.NewTradeOutboxRepository(db)
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		committed := 0
		for event := range engine1.ExecutionCh {
			row, err := NewTradeOutboxEvent(event)
			if err != nil {
				t.Errorf("outbox row 직렬화 실패: %v", err)
				return
			}
			if err := outboxRepo1.InsertBatchAndMarkCancelCommands([]*model.TradeOutboxEvent{row}, nil); err != nil {
				t.Errorf("outbox 커밋 실패: %v", err)
				return
			}
			committed++
			if committed >= prefixCount {
				return // P건 커밋 후 더 받지 않고 반환 — 채널이 곧 차 엔진이 park한다.
			}
		}
	}()

	// t.Cleanup은 등록의 역순으로 실행된다. 이 콜백을 가장 먼저 등록해 가장
	// 나중에(런타임 2와 모든 단언이 끝난 뒤에) 실행되게 한다 — 그 전까지 엔진1은
	// park한 채로 남아 크래시와 등가다.
	t.Cleanup(func() {
		go func() {
			for range engine1.ExecutionCh {
			}
		}()
		engine1.Stop()
		select {
		case <-engine1.Done():
		case <-time.After(10 * time.Second):
			t.Error("엔진1이 드레인되지 않았다")
		}
	})

	engine1.Start()

	orderRepo1 := repository.NewOrderRepository(db)
	bootstrap1 := NewMatchingBootstrapService(orderRepo1, engine1)
	bootstrapResult1, err := bootstrap1.BootstrapOpenOrders(context.Background())
	require.NoError(t, err)
	require.Equal(t, makerCount+1, bootstrapResult1.Submitted, "런타임 1 bootstrap이 maker 40 + taker 1을 올려야 한다")

	// 장벽: 소비자가 P건을 커밋하고 반환한 뒤, "현재 진행 중인" park를 확인한다
	// (started > finished) — 소비자가 P건을 커밋하는 동안 생겼다가 이미 끝난
	// 과거 park 신호로 통과하면 안 된다.
	select {
	case <-consumerDone:
	case <-time.After(30 * time.Second):
		t.Fatal("소비자가 P건을 커밋하고 반환하지 않았다")
	}
	require.Eventually(t, func() bool { return started1.Load() > finished1.Load() }, 10*time.Second, 5*time.Millisecond,
		"엔진1이 park하지 않았다")

	// 런타임 1 종료 직전 단언: 이 심볼의 outbox가 정확히 P건, 모두 PENDING, suffix 행 0.
	var outboxCount int64
	require.NoError(t, db.Model(&model.TradeOutboxEvent{}).Where("coin_symbol = ?", symbol).Count(&outboxCount).Error)
	require.EqualValues(t, prefixCount, outboxCount, "prefix outbox 행 수가 P와 일치해야 한다")
	var pendingCount int64
	require.NoError(t, db.Model(&model.TradeOutboxEvent{}).
		Where("coin_symbol = ? AND status = ?", symbol, model.TradeOutboxStatusPending).Count(&pendingCount).Error)
	require.EqualValues(t, prefixCount, pendingCount, "prefix outbox 행이 모두 PENDING이어야 한다(suffix 0)")

	// ---- 런타임 2 ----
	orderRepo2 := repository.NewOrderRepository(db)
	settlement2 := NewSettlementService(db, orderRepo2)

	// replay 먼저(main 순서): prefix P건을 실제로 정산해 DB 주문 상태를
	// 갱신한다. 그래야 bootstrap이 이미 체결된 maker를 제외하고 taker 잔량도
	// 정확히 반영한다.
	replayer := &OutboxReplayer{
		Repo: &symbolScopedReplaySource{inner: repository.NewTradeOutboxRepository(db), symbol: symbol},
		Process: func(_ uint64, event matching.ExecutionEvent) bool {
			_, err := settlement2.SettleTrade(event.Trade, 0)
			return err == nil
		},
	}
	replayResult, err := replayer.Replay()
	require.NoError(t, err)
	require.Equal(t, prefixCount, replayResult.Replayed, "replay가 prefix P건을 모두 처리해야 한다")

	engine2, err := matching.NewMatchingEngineWithQuantum(matching.QuantumConfig{MaxMatchesPerTurn: 4, MaxConsecutiveCancels: 2})
	require.NoError(t, err) // 기본 cap 그대로(계획: "기본 cap") — Start() 전 교체 없음.

	outboxRepo2 := repository.NewTradeOutboxRepository(db)
	writer2 := &OutboxWriter{
		Repo:   outboxRepo2,
		Source: engine2.ExecutionCh,
		Forward: func(e OutboxEvent) {
			require.NotNil(t, e.Event.Trade, "이 심볼의 suffix에는 Trade만 있어야 한다")
			// outboxEventID > 0이므로 SettleTrade가 같은 트랜잭션에서 PROCESSED까지 마킹한다.
			_, err := settlement2.SettleTrade(e.Event.Trade, e.OutboxID)
			require.NoError(t, err)
		},
	}
	writer2Done := make(chan struct{})
	go func() { writer2.Run(); close(writer2Done) }()

	engine2.Start()
	t.Cleanup(func() {
		engine2.Stop()
		select {
		case <-engine2.Done():
		case <-time.After(10 * time.Second):
			t.Error("엔진2가 드레인되지 않았다")
		}
		select {
		case <-writer2Done:
		case <-time.After(10 * time.Second):
			t.Error("outbox writer2가 종료되지 않았다")
		}
	})

	bootstrap2 := NewMatchingBootstrapService(orderRepo2, engine2)
	bootstrapResult2, err := bootstrap2.BootstrapOpenOrders(context.Background())
	require.NoError(t, err)
	require.Equal(t, makerCount-prefixCount+1, bootstrapResult2.Submitted,
		"런타임 2 bootstrap이 남은 maker 32 + taker 1을 올려야 한다")

	// 정산 완료 대기: taker가 최종적으로 FILLED.
	require.Eventually(t, func() bool {
		var order model.Order
		if err := db.First(&order, taker.ID).Error; err != nil {
			return false
		}
		return order.Status == model.OrderStatusFilled
	}, 30*time.Second, 20*time.Millisecond, "taker가 최종적으로 FILLED되지 않았다")

	// ---- Step 4: 단언(정확한 기대값) ----

	var tradeStats struct {
		Count    int64
		QtySum   decimal.Decimal
		QuoteSum decimal.Decimal
	}
	require.NoError(t, db.Raw(`
		SELECT COUNT(*) AS count, COALESCE(SUM(quantity),0) AS qty_sum, COALESCE(SUM(price*quantity),0) AS quote_sum
		FROM trades WHERE coin_symbol = ?`, symbol).Scan(&tradeStats).Error)
	require.EqualValues(t, makerCount, tradeStats.Count, "이 심볼 trades 수 == 40이어야 한다")
	require.True(t, tradeStats.QtySum.Equal(decimal.NewFromInt(makerCount)), "수량 합 == 40이어야 한다, got %s", tradeStats.QtySum)
	require.True(t, tradeStats.QuoteSum.Equal(executionQuote.Mul(decimal.NewFromInt(makerCount))),
		"quote 총액 == 40*가격이어야 한다, got %s", tradeStats.QuoteSum)

	// prefix 8 + suffix 32 == 40.
	suffixCount := tradeStats.Count - int64(prefixCount)
	require.EqualValues(t, makerCount-prefixCount, suffixCount, "suffix 재매칭 수가 32여야 한다")

	// maker 40개 각각 정확히 1건 체결·FILLED, release 없음.
	for i, maker := range makers {
		var order model.Order
		require.NoError(t, db.First(&order, maker.ID).Error)
		require.Equal(t, model.OrderStatusFilled, order.Status, "maker %d(주문 %d)는 FILLED여야 한다", i, maker.ID)
		require.True(t, order.FilledAmount.Equal(quantity), "maker %d 체결량이 1이어야 한다", i)

		require.EqualValues(t, 1, tradeJournalCount(t, db, maker.ID), "maker %d의 settlement 분개 수는 1이어야 한다", i)
		require.EqualValues(t, 0, releaseJournalCount(t, db, maker.ID), "maker %d의 release 분개 수는 0이어야 한다", i)

		assertLedgerBalances(t, db, makerIDs[i], model.KRWAssetSymbol, sellerQuoteNet, decimal.Zero)
		assertLedgerBalances(t, db, makerIDs[i], symbol, decimal.Zero, decimal.Zero)
	}

	// taker: FILLED, 잔량 0, settlement 분개 40건, release 0건.
	var takerOrder model.Order
	require.NoError(t, db.First(&takerOrder, taker.ID).Error)
	require.Equal(t, model.OrderStatusFilled, takerOrder.Status)
	require.True(t, takerOrder.FilledAmount.Equal(decimal.NewFromInt(makerCount)), "taker 체결량이 40이어야 한다")
	require.True(t, takerOrder.Amount.Sub(takerOrder.FilledAmount).IsZero(), "taker 잔량이 0이어야 한다")
	require.EqualValues(t, makerCount, tradeJournalCount(t, db, taker.ID), "taker의 settlement 분개 수는 40이어야 한다")
	require.EqualValues(t, 0, releaseJournalCount(t, db, taker.ID), "taker의 release 분개 수는 0이어야 한다")

	assertLedgerBalances(t, db, takerID, model.KRWAssetSymbol, decimal.Zero, decimal.Zero)
	assertLedgerBalances(t, db, takerID, symbol, decimal.NewFromInt(makerCount), decimal.Zero)

	// FEE_INCOME 정확한 델타.
	feeIncomeAfter := feeIncomeBalance(t, db, model.KRWAssetSymbol)
	require.True(t, feeIncomeAfter.Sub(feeIncomeBefore).Equal(totalFeeIncome),
		"FEE_INCOME 증가분이 기대와 일치해야 한다: got %s, want %s", feeIncomeAfter.Sub(feeIncomeBefore), totalFeeIncome)

	// 이 심볼 outbox PENDING 0.
	var stillPending int64
	require.NoError(t, db.Model(&model.TradeOutboxEvent{}).
		Where("coin_symbol = ? AND status = ?", symbol, model.TradeOutboxStatusPending).Count(&stillPending).Error)
	require.EqualValues(t, 0, stillPending, "이 심볼 outbox는 PENDING이 남지 않아야 한다")

	// 원장 검산 4종(보조).
	assertNoDBReconciliationViolations(t, db, symbol, append(append([]uint{}, makerIDs...), takerID), "KRW", symbol)
}
