package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 부분체결 주문에 ProcessOrderCancellation을 적용하면 잔여 hold만 정확히 해제되고
// CANCELLED가 커밋되며 원장 기록이 남는다(A-4 정산 파이프라인 핵심 경로). 선행 체결
// 정산은 이 테스트 범위 밖이므로 UpdateOrderExecution으로 "이미 정산된 FilledAmount"
// 상태만 시뮬레이션한다 — ProcessOrderCancellation은 정확히 이 값만 신뢰한다.
func TestIntegrationProcessOrderCancellationReleasesRemainingHoldAndCommitsCancelled(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(95)
	defer cleanupServiceUsers(t, db, userID)

	require.NoError(t, db.Create(&model.Wallet{
		UserID:           userID,
		CoinSymbol:       model.KRWAssetSymbol,
		KRW:              decimal.NewFromInt(1_000_000),
		AvailableBalance: decimal.NewFromInt(1_000_000),
		LockedBalance:    decimal.Zero,
	}).Error)

	orderService := newIntegrationOrderService(db, matching.NewMatchingEngine())
	order, err := orderService.CreateOrder(CreateOrderInput{
		UserID:     userID,
		CoinSymbol: "BTC",
		Side:       "BUY",
		Price:      "100",
		Amount:     "10",
	})
	require.NoError(t, err)

	orderRepo := repository.NewOrderRepository(db)
	require.NoError(t, orderRepo.UpdateOrderExecution(order.ID, decimal.NewFromInt(4), decimal.NewFromInt(400), model.OrderStatusPartial))

	err = orderService.ProcessOrderCancellation(matching.OrderCancelled{
		OrderID:    order.ID,
		CoinSymbol: order.CoinSymbol,
		Side:       order.Side,
	})
	require.NoError(t, err)

	var persisted model.Order
	require.NoError(t, db.First(&persisted, order.ID).Error)
	assert.Equal(t, model.OrderStatusCancelled, persisted.Status)

	walletRepo := repository.NewWalletRepository(db)
	wallet, err := walletRepo.FindKRWWalletByUserID(userID)
	require.NoError(t, err)
	// hold(10주, 1000.5) 중 이미 체결된 4주 몫은 남고, 잔여 6주 몫(600.3)만 해제된다.
	assert.True(t, wallet.AvailableBalance.Equal(decimal.RequireFromString("999599.8")), "available=%s", wallet.AvailableBalance.String())
	assert.True(t, wallet.LockedBalance.Equal(decimal.RequireFromString("400.2")), "locked=%s", wallet.LockedBalance.String())

	entries := requireLedgerEntries(t, db, userID, model.LedgerEntryTypeOrderRelease, model.LedgerReferenceTypeOrder, order.ID)
	require.Len(t, entries, 1)
	assertLedgerDelta(t, entries[0], model.KRWAssetSymbol, "600.3", "-600.3", "999599.8", "400.2")

	assertNoReconciliationViolationsForWallet(t, db, userID, model.KRWAssetSymbol)
}

// 같은 OrderCancelled 이벤트가 리플레이 등으로 두 번 처리돼도 두 번째 호출은 no-op —
// hold가 이중 해제되지 않고 원장도 중복 기록되지 않는다.
func TestIntegrationProcessOrderCancellationIsIdempotent(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(96)
	defer cleanupServiceUsers(t, db, userID)

	require.NoError(t, db.Create(&model.Wallet{
		UserID:           userID,
		CoinSymbol:       model.KRWAssetSymbol,
		KRW:              decimal.NewFromInt(1000),
		AvailableBalance: decimal.NewFromInt(1000),
		LockedBalance:    decimal.Zero,
	}).Error)

	orderService := newIntegrationOrderService(db, matching.NewMatchingEngine())
	order, err := orderService.CreateOrder(CreateOrderInput{
		UserID:     userID,
		CoinSymbol: "BTC",
		Side:       "BUY",
		Price:      "100",
		Amount:     "5",
	})
	require.NoError(t, err)

	event := matching.OrderCancelled{OrderID: order.ID, CoinSymbol: order.CoinSymbol, Side: order.Side}
	require.NoError(t, orderService.ProcessOrderCancellation(event))
	require.NoError(t, orderService.ProcessOrderCancellation(event))

	var persisted model.Order
	require.NoError(t, db.First(&persisted, order.ID).Error)
	assert.Equal(t, model.OrderStatusCancelled, persisted.Status)

	walletRepo := repository.NewWalletRepository(db)
	wallet, err := walletRepo.FindKRWWalletByUserID(userID)
	require.NoError(t, err)
	assert.True(t, wallet.AvailableBalance.Equal(decimal.NewFromInt(1000)), "hold 전액이 정확히 원복돼야 한다")
	assert.True(t, wallet.LockedBalance.IsZero())

	entries := requireLedgerEntries(t, db, userID, model.LedgerEntryTypeOrderRelease, model.LedgerReferenceTypeOrder, order.ID)
	require.Len(t, entries, 1, "두 번째 호출은 no-op이어야 하고 원장이 중복 기록되면 안 된다")

	assertNoReconciliationViolationsForWallet(t, db, userID, model.KRWAssetSymbol)
}

// 이미 FILLED인 주문에 뒤늦게 OrderCancelled가 도착해도(엔진 FIFO상 정상적으로는
// 발생하지 않지만 리플레이 중복 등 방어적으로) 상태를 덮어쓰지 않고 no-op이다.
func TestIntegrationProcessOrderCancellationOnAlreadyFilledOrderIsNoop(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(97)
	defer cleanupServiceUsers(t, db, userID)

	order := seedCancelOrderRows(t, db, cancelOrderSeed{
		UserID:        userID,
		CoinSymbol:    "BTC",
		Side:          model.OrderSideBuy,
		Status:        model.OrderStatusFilled,
		Price:         decimal.NewFromInt(100),
		Amount:        decimal.NewFromInt(5),
		FilledAmount:  decimal.NewFromInt(5),
		LockedBalance: decimal.Zero,
	})

	orderService := newIntegrationOrderService(db, nil)
	err := orderService.ProcessOrderCancellation(matching.OrderCancelled{
		OrderID:    order.ID,
		CoinSymbol: order.CoinSymbol,
		Side:       order.Side,
	})
	require.NoError(t, err)

	var persisted model.Order
	require.NoError(t, db.First(&persisted, order.ID).Error)
	assert.Equal(t, model.OrderStatusFilled, persisted.Status)
	assertLedgerCount(t, db, userID, 0)
}

// TestIntegrationCancelDuringInFlightPartialFillProducesNoFailedSettlements는
// 버그 발견 문서(docs/refactor/bug-findings-2026-07-17-cancel-fill-race-and-market-buy-overspend.md,
// 버그 1)가 실측한 취소-체결 레이스를 실제 파이프라인(엔진 → OutboxWriter(실
// DB 커밋) → 정산 디스패치, 모킹 없음)으로 결정론적으로 재현하고, A-4 수정
// 후 failed_settlements가 0임을 증명한다(Task 1E Step 3, 이 태스크의 핵심).
//
// 재현하는 "레이스 창": 매도 주문이 매수 주문의 일부를 체결시켜 trade가 엔진
// ExecutionCh → OutboxWriter를 거쳐 DB(outbox 테이블)에 커밋됐지만, 아직
// 정산(SettleTrade)이 그 trade를 처리하지 않아 order.FilledAmount가 DB에는
// 여전히 0으로 남아 있는 바로 그 순간에 CancelOrder를 호출한다.
//
// 수정 전(Task 1E 이전) 코드였다면: CancelOrder가 그 순간의 DB 스냅샷
// (FilledAmount=0)을 기준으로 remaining=전량을 계산해 hold 전액을 해제하고
// CANCELLED를 즉시 커밋했을 것이다(order_service.go의 옛 CancelOrder는 검증
// 트랜잭션 안에서 releaseOrderHold + UpdateOrderExecution을 곧바로 커밋했다).
// 그 직후 이 테스트의 3단계에서 방금 체결된 trade를 정산하려 하면
// "buy order N status CANCELLED cannot be settled"로 실패해 failed_settlements가
// 기록됐을 것이다 — 발견 문서 실측치와 정확히 같은 실패 모드(24건 중 21건이
// 이 사유, 나머지는 같은 레이스의 "insufficient locked KRW" 변종).
//
// 수정 후(현재 코드): CancelOrder는 DB를 건드리지 않고 엔진에 접수만
// 요청한다. 엔진은 잔여분을 오더북에서 제거하고 OrderCancelled 이벤트를 그
// 주문의 선행 trade "뒤"에 FIFO로 방출한다(engine.go processCancel/
// emitOrderCancelled, Task 1B). 정산 파이프라인이 이 순서대로(trade 먼저,
// cancel 다음) 처리하면 trade 정산 시점엔 주문이 아직 CANCELLED가 아니라
// 정상 정산되고, 뒤이은 ProcessOrderCancellation은 정산 후 최신
// FilledAmount를 기준으로 정확히 잔여분만 해제한다 — failed_settlements가
// 발생할 여지가 없다.
func TestIntegrationCancelDuringInFlightPartialFillProducesNoFailedSettlements(t *testing.T) {
	db := openServiceIntegrationDB(t)
	buyerID := serviceTestUserID(98)
	sellerID := serviceTestUserID(99)
	defer cleanupServiceUsers(t, db, buyerID, sellerID)

	require.NoError(t, db.Create(&model.Wallet{
		UserID:           buyerID,
		CoinSymbol:       model.KRWAssetSymbol,
		KRW:              decimal.NewFromInt(2_000_000),
		AvailableBalance: decimal.NewFromInt(2_000_000),
		LockedBalance:    decimal.Zero,
	}).Error)
	require.NoError(t, db.Create(&model.Wallet{
		UserID:           sellerID,
		CoinSymbol:       "BTC",
		Quantity:         decimal.NewFromInt(10),
		AvailableBalance: decimal.NewFromInt(10),
		LockedBalance:    decimal.Zero,
	}).Error)

	me := matching.NewMatchingEngine()
	me.Start()
	orderService := newIntegrationOrderService(db, me)
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db), repository.NewWalletRepository(db))

	// 실제 outbox 파이프라인: 엔진 ExecutionCh → (커밋) DB outbox 테이블 →
	// forward 콜백. OutboxWriter가 이 코드베이스에서 ExecutionCh의 유일한
	// 소비자(A-3 write-ahead 게이트)이므로, "in-flight" 창을 만들려면 실제로
	// 이 writer를 거쳐야 한다 — 모킹하면 이 테스트가 증명하려는 레이스 자체가
	// 사라진다.
	outboxRepo := repository.NewTradeOutboxRepository(db)
	forwarded := make(chan OutboxEvent, 16)
	writer := &OutboxWriter{
		Repo:          outboxRepo,
		Source:        me.ExecutionCh,
		Forward:       func(event OutboxEvent) { forwarded <- event },
		BatchSize:     1,
		FlushInterval: 5 * time.Millisecond,
		Logger:        discardServiceLogger(),
	}
	writerDone := make(chan struct{})
	go func() {
		writer.Run()
		close(writerDone)
	}()

	// 매수 주문(10주, 지정가 100) 오더북 대기.
	buyOrder, err := orderService.CreateOrder(CreateOrderInput{
		UserID:     buyerID,
		CoinSymbol: "BTC",
		Side:       "BUY",
		Price:      "100",
		Amount:     "10",
	})
	require.NoError(t, err)

	// 매도 주문(4주)이 매수 주문의 일부만 체결 — trade 1건, 잔여 6주는
	// 오더북에 남는다(부분 체결).
	_, err = orderService.CreateOrder(CreateOrderInput{
		UserID:     sellerID,
		CoinSymbol: "BTC",
		Side:       "SELL",
		Price:      "100",
		Amount:     "4",
	})
	require.NoError(t, err)

	// 1) trade가 outbox에 커밋되어 정산 파이프라인으로 전달될 때까지 대기 —
	// 이 시점이 곧 "in-flight" 창의 시작이다.
	tradeOutboxEvent := requireForwardedOutboxEvent(t, forwarded)
	require.NotNil(t, tradeOutboxEvent.Event.Trade, "첫 이벤트는 trade여야 한다")
	require.Equal(t, buyOrder.ID, tradeOutboxEvent.Event.Trade.BuyOrderID)

	// in-flight 확인: outbox에는 이미 있지만 정산 전이라 DB의 FilledAmount는
	// 아직 0이다 — 바로 이 간극이 구버전 코드가 잘못 읽던 "취소 가능 여부"였다.
	var preSettle model.Order
	require.NoError(t, db.First(&preSettle, buyOrder.ID).Error)
	assert.True(t, preSettle.FilledAmount.IsZero(), "정산 전이므로 FilledAmount는 아직 0이어야 한다(in-flight 증명)")

	// 2) 바로 이 창에서 취소 요청 — 레이스의 핵심 순간.
	cancelResult, err := orderService.CancelOrder(CancelOrderInput{UserID: buyerID, OrderID: buyOrder.ID})
	require.NoError(t, err, "취소 접수는 500이 아니라 성공해야 한다(발견 문서의 21/154건 500 버그)")
	assert.True(t, cancelResult.EngineRemoved)

	// Step 1 계약 재확인: CancelOrder 단독 호출은 DB를 건드리지 않는다.
	var afterCancelCall model.Order
	require.NoError(t, db.First(&afterCancelCall, buyOrder.ID).Error)
	assert.NotEqual(t, model.OrderStatusCancelled, afterCancelCall.Status, "CancelOrder 단독으로는 CANCELLED가 커밋되면 안 된다")

	cancelOutboxEvent := requireForwardedOutboxEvent(t, forwarded)
	require.NotNil(t, cancelOutboxEvent.Event.OrderCancelled, "trade 다음으로 방출되는 이벤트는 OrderCancelled여야 한다(FIFO)")
	assert.Equal(t, buyOrder.ID, cancelOutboxEvent.Event.OrderCancelled.OrderID)
	assert.Less(t, tradeOutboxEvent.OutboxID, cancelOutboxEvent.OutboxID, "outbox ID 순서로도 trade가 cancel보다 먼저 커밋됐어야 한다")

	// 3) 파이프라인이 실제로 처리하는 순서 그대로 확정한다: trade 정산 먼저.
	// 레이스가 안 닫혔다면(구버전) 여기서 "status CANCELLED cannot be settled"로
	// 실패했을 지점이다.
	settleResult, err := settlementService.SettleTrade(tradeOutboxEvent.Event.Trade, tradeOutboxEvent.OutboxID)
	require.NoError(t, err, "in-flight 체결 정산은 레이스가 닫혔다면 실패하면 안 된다")
	assert.True(t, settleResult.Applied)

	// 4) 그 다음 취소 확정 — 이제 FilledAmount가 4로 갱신된 뒤이므로 잔여
	// 6주만 정확히 해제된다.
	require.NoError(t, orderService.ProcessOrderCancellation(*cancelOutboxEvent.Event.OrderCancelled))

	var final model.Order
	require.NoError(t, db.First(&final, buyOrder.ID).Error)
	assert.Equal(t, model.OrderStatusCancelled, final.Status)
	assert.True(t, final.FilledAmount.Equal(decimal.NewFromInt(4)), "filled=%s", final.FilledAmount.String())

	// 5) 레이스 해소의 핵심 단언: failed_settlements 0건.
	var failedCount int64
	require.NoError(t, db.Model(&model.FailedSettlement{}).
		Where("buy_order_id = ?", buyOrder.ID).Count(&failedCount).Error)
	assert.Equal(t, int64(0), failedCount, "레이스가 닫혔다면 failed_settlements가 발생하면 안 된다")

	// 6) 자금 정합성: 매수자는 4주 매수분(400 + 수수료)만 소진하고 나머지
	// hold(잔여 6주 몫)는 취소로 해제돼야 한다.
	buyerWallet, err := repository.NewWalletRepository(db).FindKRWWalletByUserID(buyerID)
	require.NoError(t, err)
	assert.True(t, buyerWallet.LockedBalance.IsZero(), "취소 확정 후 매수자 KRW hold가 전부 정리돼야 한다(locked=%s)", buyerWallet.LockedBalance.String())

	assertNoReconciliationViolationsForWallet(t, db, buyerID, model.KRWAssetSymbol)

	me.Stop()
	<-me.Done()
	<-writerDone
}

func requireForwardedOutboxEvent(t *testing.T, forwarded <-chan OutboxEvent) OutboxEvent {
	t.Helper()

	select {
	case event := <-forwarded:
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for outbox writer to forward execution event")
		return OutboxEvent{}
	}
}

// assertNoReconciliationViolationsForWallet은 기존 reconciliation_worker_integration_test.go의
// worker.RunOnce() + subject_key 필터 패턴을 재사용한다(공유 테스트 DB이므로 전역 건수는
// 단언하지 않는다). "legacy_mismatch"는 실패로 세지 않는다 — 이 테스트들은
// seedCancelOrderRows/db.Create로 지갑을 직접 시딩해(펀딩 원장 항목 없음)
// classifyLedgerWalletRow(reconciliation_worker.go)가 정의한 대로 "레거시 초기 잔고"로
// 정상 분류되는 갭이 생긴다(reconciliation_worker_integration_test.go도 동일 패턴을
// legacy_mismatch로 단언). 여기서 잡아야 할 실제 결함은 "ledger_wallet" 분류뿐이다.
func assertNoReconciliationViolationsForWallet(t *testing.T, db *gorm.DB, userID uint, coinSymbol string) {
	t.Helper()

	walletRepo := repository.NewWalletRepository(db)
	var wallet *model.Wallet
	var err error
	if coinSymbol == model.KRWAssetSymbol {
		wallet, err = walletRepo.FindKRWWalletByUserID(userID)
	} else {
		wallet, err = walletRepo.FindByUserIDAndCoinSymbol(userID, coinSymbol)
	}
	require.NoError(t, err)

	subject := fmt.Sprintf("wallet:%d", wallet.ID)
	t.Cleanup(func() {
		require.NoError(t, db.Where("subject_key = ?", subject).Delete(&model.ReconciliationViolation{}).Error)
	})

	worker := &ReconciliationWorker{
		Repository: repository.NewReconciliationRepository(db),
		Logger:     discardServiceLogger(),
	}
	worker.RunOnce()

	var realViolations []model.ReconciliationViolation
	for _, v := range findViolationsBySubject(t, db, []string{subject})[subject] {
		if v.CheckName != "legacy_mismatch" {
			realViolations = append(realViolations, v)
		}
	}
	assert.Empty(t, realViolations, "정산 후 지갑에 실제(legacy_mismatch가 아닌) 리컨실리에이션 위반이 없어야 한다")
}
