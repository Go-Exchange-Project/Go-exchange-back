package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/testdb"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// TestAllEventsPassReconciliation은 개발용 지급·입금·출금·잠금·해제·체결·수수료를
// 각 1회씩 거친 뒤 검산 1~4가 전부 위반 0건인지 본다(T4). §5의 표 전체를 한 번에
// 덮는다 — 사건마다 따로 테스트하지 않는다.
func TestAllEventsPassReconciliation(t *testing.T) {
	db := openServiceIntegrationDB(t)
	buyerID := serviceTestUserID(1000)
	sellerID := serviceTestUserID(1001)
	defer cleanupServiceUsers(t, db, buyerID, sellerID)

	coinSymbol := fmt.Sprintf("XFER%d", time.Now().UnixNano()%1_000_000_000)
	processor := NewFakeTransferProcessor()
	transferSvc := NewTransferService(db, processor)

	// 개발용 지급.
	seedLedgerFunds(t, db, sellerID, coinSymbol, decimal.NewFromInt(10))

	// 가짜 은행 입금.
	deposit, err := transferSvc.RequestDeposit(DepositInput{
		UserID: buyerID, Rail: model.TransferRailBank, Asset: model.KRWAssetSymbol,
		Amount: "1000000", ClientRequestKey: fmt.Sprintf("t4-deposit-%d", buyerID),
	})
	require.NoError(t, err)
	require.Equal(t, model.TransferStatusProcessing, deposit.Status)
	require.NotNil(t, deposit.ExternalRef)
	require.NoError(t, transferSvc.ResolveTransfer(ResolveInput{
		TransferRequestID: deposit.ID, Source: model.TransferEventSourceCallback,
		EventKey: fmt.Sprintf("callback:%s:t4-deposit-evt-%d", deposit.Rail, deposit.ID),
		Outcome:  model.TransferOutcomeSuccess,
	}))
	assertLedgerBalances(t, db, buyerID, model.KRWAssetSymbol, decimal.NewFromInt(1000000), decimal.Zero)

	// 가짜 은행 출금 — 완료까지.
	withdrawal, err := transferSvc.RequestWithdrawal(WithdrawalInput{
		UserID: buyerID, Rail: model.TransferRailBank, Asset: model.KRWAssetSymbol,
		Amount: "100000", ClientRequestKey: fmt.Sprintf("t4-withdraw-%d", buyerID),
	})
	require.NoError(t, err)
	require.NotNil(t, withdrawal.ExternalRef)
	require.NoError(t, transferSvc.ResolveTransfer(ResolveInput{
		TransferRequestID: withdrawal.ID, Source: model.TransferEventSourceCallback,
		EventKey: fmt.Sprintf("callback:%s:t4-withdraw-evt-%d", withdrawal.Rail, withdrawal.ID),
		Outcome:  model.TransferOutcomeSuccess,
	}))
	assertLedgerBalances(t, db, buyerID, model.KRWAssetSymbol, decimal.NewFromInt(900000), decimal.Zero)

	// 매수·매도 잠금 → 체결(수수료 포함) → 매수자 잔여 해제.
	orderRepo := repository.NewOrderRepository(db)
	ledger := NewLedgerService(db)
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

	settlementService := NewSettlementService(db, orderRepo)
	trade := &model.Trade{
		CoinSymbol: coinSymbol, Price: decimal.NewFromInt(1000), Quantity: decimal.NewFromInt(6),
		TradedAt: time.Now(), BuyOrderID: buyOrder.ID, SellOrderID: sellOrder.ID,
	}
	result, err := settlementService.SettleTrade(trade, 0)
	require.NoError(t, err)
	require.True(t, result.Applied)

	var persistedBuy model.Order
	require.NoError(t, db.First(&persistedBuy, buyOrder.ID).Error)
	remaining := persistedBuy.Amount.Sub(persistedBuy.FilledAmount)
	require.True(t, remaining.Equal(decimal.NewFromInt(4)))
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return releaseOrderHold(ledger, tx, &persistedBuy, remaining)
	}))

	// 검산 1~4 전부 위반 0건.
	recon := repository.NewLedgerReconciliationRepository(db)
	unbalanced, err := recon.CheckUnbalancedJournals(0, 1000)
	require.NoError(t, err)
	require.Empty(t, unbalanced, "자산별 합이 0이 아닌 분개가 있다")
	drift, err := recon.CheckBalanceCacheDrift(0, 1000)
	require.NoError(t, err)
	require.Empty(t, drift, "잔액 캐시가 전기 합과 어긋난다")
	totals, err := recon.CheckAssetTotals()
	require.NoError(t, err)
	require.Empty(t, totals, "자산 전체 합이 0이 아니다")
	negative, err := recon.CheckNegativeAccounts(0, 1000)
	require.NoError(t, err)
	require.Empty(t, negative, "음수가 되면 안 되는 계정이 음수다")
}

// TestWithdrawalHoldBlocksReuseOfSameFunds는 출금 접수가 잠근 돈이 실제로 이중
// 사용을 막는지, 그리고 접수·제출 양쪽의 멱등성이 지켜지는지 본다(T5).
func TestWithdrawalHoldBlocksReuseOfSameFunds(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(1010)
	defer cleanupServiceUsers(t, db, userID)

	seedLedgerFunds(t, db, userID, model.KRWAssetSymbol, decimal.NewFromInt(1000))

	processor := NewFakeTransferProcessor()
	transferSvc := NewTransferService(db, processor)

	key := fmt.Sprintf("t5-withdraw-%d", userID)
	first, err := transferSvc.RequestWithdrawal(WithdrawalInput{
		UserID: userID, Rail: model.TransferRailBank, Asset: model.KRWAssetSymbol,
		Amount: "1000", ClientRequestKey: key,
	})
	require.NoError(t, err)
	require.NotNil(t, first.ExternalRef)

	// ① 출금 접수 후 같은 돈으로 주문 시도 → 잔액 부족으로 거절.
	orderRepo := repository.NewOrderRepository(db)
	ledger := NewLedgerService(db)
	blockedOrder := &model.Order{
		UserID: userID, CoinSymbol: "BTC", Side: model.OrderSideBuy,
		OrderType: model.OrderTypeLimit, Status: model.OrderStatusPending,
		Price: decimal.NewFromInt(1), Amount: decimal.NewFromInt(1),
	}
	holdErr := persistAndHold(db, orderRepo, ledger, blockedOrder)
	require.Error(t, holdErr, "출금으로 잠긴 돈으로 주문이 통과했다")
	kind, ok := DomainErrorKind(holdErr)
	require.True(t, ok)
	assert.Equal(t, ErrorKindConflict, kind)

	// ② 동일 출금 재시도(같은 client_request_key·같은 본문) → 같은 transfer id,
	// 잠금 분개 1건, 잔액 1회만 잠김.
	second, err := transferSvc.RequestWithdrawal(WithdrawalInput{
		UserID: userID, Rail: model.TransferRailBank, Asset: model.KRWAssetSymbol,
		Amount: "1000", ClientRequestKey: key,
	})
	require.NoError(t, err)
	assert.Equal(t, first.ID, second.ID)

	var holdJournalCount int64
	require.NoError(t, db.Model(&model.JournalEntry{}).
		Where("idempotency_key = ?", fmt.Sprintf("withdraw-hold:%d", first.ID)).
		Count(&holdJournalCount).Error)
	assert.EqualValues(t, 1, holdJournalCount, "잠금 분개가 두 번 생겼다")
	assertLedgerBalances(t, db, userID, model.KRWAssetSymbol, decimal.Zero, decimal.NewFromInt(1000))

	// ③ 같은 키·다른 금액 → 409, 추가 잠금 없음.
	_, err = transferSvc.RequestWithdrawal(WithdrawalInput{
		UserID: userID, Rail: model.TransferRailBank, Asset: model.KRWAssetSymbol,
		Amount: "1", ClientRequestKey: key,
	})
	require.Error(t, err)
	kind, ok = DomainErrorKind(err)
	require.True(t, ok)
	assert.Equal(t, ErrorKindConflict, kind)
	assertLedgerBalances(t, db, userID, model.KRWAssetSymbol, decimal.Zero, decimal.NewFromInt(1000))

	// ④ 같은 dispatch key로 재제출 → 외부 송금이 새로 생기지 않고 같은 external_ref.
	dispatchKey := fmt.Sprintf("transfer:%d", first.ID)
	var reloaded model.TransferRequest
	require.NoError(t, db.First(&reloaded, first.ID).Error)
	refAgain, err := processor.Submit(dispatchKey, reloaded)
	require.NoError(t, err)
	assert.Equal(t, *first.ExternalRef, refAgain, "같은 dispatch key인데 다른 external_ref가 나왔다")

	// 재기동 흉내: 메모리가 빈 새 FakeTransferProcessor 인스턴스로 같은 dispatch
	// key를 제출해도 같은 external_ref가 나와야 한다 — Submit의 멱등 계약이
	// 실제로 의미 있는 크래시 상황이다.
	restarted := NewFakeTransferProcessor()
	refAfterRestart, err := restarted.Submit(dispatchKey, reloaded)
	require.NoError(t, err)
	assert.Equal(t, *first.ExternalRef, refAfterRestart, "재기동 후 같은 dispatch key인데 다른 external_ref가 나왔다")
}

// pinnedIntegrationDB는 커넥션 풀을 1개로 고정한 *gorm.DB를 연다. T6은 이 위에서
// SELECT pg_backend_pid()로 얻은 pid가 그 뒤의 모든 쿼리에서 그대로 재사용된다는
// 것을 전제로 한다 — 풀에서 매번 빌리면 잠근 세션과 pid를 물어본 세션이 달라진다.
func pinnedIntegrationDB(t *testing.T) (*gorm.DB, int) {
	t.Helper()

	db := openServiceIntegrationDB(t)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	var pid int
	require.NoError(t, db.Raw("SELECT pg_backend_pid()").Scan(&pid).Error)
	return db, pid
}

// TestConcurrentTerminalObservations는 T6이다. 설계 §11.1대로 실제 동시 실행
// 장벽(afterLock)을 써서 A가 행을 잠근 채 멈춘 상태에서 B가 같은 행에서 실제로
// 막히는지 pg_blocking_pids로 확인한 뒤에만 A를 풀어준다. 승자는 항상 A다.
func TestConcurrentTerminalObservations(t *testing.T) {
	t.Run("같은 결과: A SUCCESS ∥ B SUCCESS", func(t *testing.T) {
		runTerminalObservationRace(t, func(svcB *TransferService, id uint) error {
			return svcB.ResolveTransfer(ResolveInput{
				TransferRequestID: id, Source: model.TransferEventSourcePoll,
				EventKey: fmt.Sprintf("poll:%d:1", id), Outcome: model.TransferOutcomeSuccess,
			})
		}, "")
	})
	t.Run("반대 결과: A SUCCESS ∥ B FAILURE", func(t *testing.T) {
		runTerminalObservationRace(t, func(svcB *TransferService, id uint) error {
			return svcB.ResolveTransfer(ResolveInput{
				TransferRequestID: id, Source: model.TransferEventSourcePoll,
				EventKey: fmt.Sprintf("poll:%d:1", id), Outcome: model.TransferOutcomeFailure,
			})
		}, model.ReviewReasonConflictingTerminalOutcome)
	})
	t.Run("확정 ∥ 미확정: A ResolveTransfer(SUCCESS) ∥ B RecordObservation(PENDING)", func(t *testing.T) {
		runTerminalObservationRace(t, func(svcB *TransferService, id uint) error {
			return svcB.RecordObservation(ObservationInput{
				TransferRequestID: id,
				EventKey:          fmt.Sprintf("poll:%d:1", id), Outcome: model.TransferOutcomePending,
			})
		}, "")
	})
}

func runTerminalObservationRace(t *testing.T, callB func(svcB *TransferService, id uint) error, expectReviewReason string) {
	t.Helper()

	setupDB := openServiceIntegrationDB(t)
	userID := serviceTestUserID(1020)
	defer cleanupServiceUsers(t, setupDB, userID)

	seedLedgerFunds(t, setupDB, userID, model.KRWAssetSymbol, decimal.NewFromInt(100000))

	processor := NewFakeTransferProcessor()
	setupSvc := NewTransferService(setupDB, processor)
	request, err := setupSvc.RequestWithdrawal(WithdrawalInput{
		UserID: userID, Rail: model.TransferRailBank, Asset: model.KRWAssetSymbol,
		Amount: "50000", ClientRequestKey: fmt.Sprintf("t6-%d-%d", userID, time.Now().UnixNano()),
	})
	require.NoError(t, err)
	require.Equal(t, model.TransferStatusProcessing, request.Status, "T6 픽스처는 PROCESSING 상태여야 한다")
	require.NotNil(t, request.ExternalRef)
	confirmKey := fmt.Sprintf("withdraw-settle:%s", *request.ExternalRef)

	dbA, pidA := pinnedIntegrationDB(t)
	dbB, pidB := pinnedIntegrationDB(t)
	observerDB := openServiceIntegrationDB(t)

	svcA := NewTransferService(dbA, processor)
	svcB := NewTransferService(dbB, processor)

	release := make(chan struct{})
	locked := make(chan struct{})
	svcA.afterLock = func() {
		close(locked)
		<-release
	}

	doneA := make(chan error, 1)
	go func() {
		doneA <- svcA.ResolveTransfer(ResolveInput{
			TransferRequestID: request.ID, Source: model.TransferEventSourceCallback,
			EventKey: fmt.Sprintf("callback:%s:t6-evt-%d", request.Rail, request.ID),
			Outcome:  model.TransferOutcomeSuccess,
		})
	}()

	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		t.Fatal("A가 FOR UPDATE를 잠그지 못했다 — 관측 1 실패")
	}

	doneB := make(chan error, 1)
	go func() {
		doneB <- callB(svcB, request.ID)
	}()

	require.Eventually(t, func() bool {
		var blocked bool
		require.NoError(t, observerDB.Raw(
			"SELECT ? = ANY(pg_blocking_pids(?))", pidA, pidB).Scan(&blocked).Error)
		return blocked
	}, 5*time.Second, 20*time.Millisecond, "B가 A 때문에 막히지 않았다 — 관측 2 실패")

	close(release)

	require.NoError(t, <-doneA, "A(ResolveTransfer SUCCESS)가 실패했다")
	require.NoError(t, <-doneB, "B 호출이 실패했다")

	var final model.TransferRequest
	require.NoError(t, observerDB.First(&final, request.ID).Error)
	assert.Equal(t, model.TransferStatusCompleted, final.Status, "승자는 항상 A다")
	assert.Nil(t, final.NextCheckAt, "확정된 요청의 조회 일정이 되살아났다")
	if expectReviewReason == "" {
		assert.Nil(t, final.ReviewRequiredAt)
		assert.Nil(t, final.ReviewReason)
	} else {
		require.NotNil(t, final.ReviewReason)
		assert.Equal(t, expectReviewReason, *final.ReviewReason)
	}

	var confirmJournalCount int64
	require.NoError(t, observerDB.Model(&model.JournalEntry{}).
		Where("idempotency_key = ?", confirmKey).Count(&confirmJournalCount).Error)
	assert.EqualValues(t, 1, confirmJournalCount, "확정 분개가 정확히 1건이어야 한다")

	var eventCount int64
	require.NoError(t, observerDB.Model(&model.TransferStatusEvent{}).
		Where("transfer_request_id = ?", request.ID).Count(&eventCount).Error)
	assert.EqualValues(t, 2, eventCount, "A·B 각자의 사건이 2줄 남아야 한다")
}

// TestFailureCallbackRefundsLockedFunds는 T7이다. 실패 알림이 오면 잠근 돈이
// available로 정확히 되돌아가고, 원장의 순변화가 0인지 본다.
func TestFailureCallbackRefundsLockedFunds(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(1030)
	defer cleanupServiceUsers(t, db, userID)

	seedLedgerFunds(t, db, userID, model.KRWAssetSymbol, decimal.NewFromInt(200000))

	processor := NewFakeTransferProcessor()
	transferSvc := NewTransferService(db, processor)

	request, err := transferSvc.RequestWithdrawal(WithdrawalInput{
		UserID: userID, Rail: model.TransferRailBank, Asset: model.KRWAssetSymbol,
		Amount: "200000", ClientRequestKey: fmt.Sprintf("t7-%d", userID),
	})
	require.NoError(t, err)
	require.NotNil(t, request.ExternalRef)
	assertLedgerBalances(t, db, userID, model.KRWAssetSymbol, decimal.Zero, decimal.NewFromInt(200000))

	require.NoError(t, transferSvc.ResolveTransfer(ResolveInput{
		TransferRequestID: request.ID, Source: model.TransferEventSourceCallback,
		EventKey: fmt.Sprintf("callback:%s:t7-evt-%d", request.Rail, request.ID),
		Outcome:  model.TransferOutcomeFailure,
		Payload:  map[string]any{"reason": "ACCOUNT_FROZEN"},
	}))

	assertLedgerBalances(t, db, userID, model.KRWAssetSymbol, decimal.NewFromInt(200000), decimal.Zero)

	var final model.TransferRequest
	require.NoError(t, db.First(&final, request.ID).Error)
	assert.Equal(t, model.TransferStatusFailed, final.Status)
	require.NotNil(t, final.ResolutionJournalID, "출금 실패는 돈을 푼 분개가 있어야 한다")
	// item 4: 콜백 경로의 FAILURE 확정도 payload["reason"]을 failure_reason에
	// 저장해야 한다.
	assert.Equal(t, "ACCOUNT_FROZEN", final.FailureReason)
}

// TestPollFailureRecordsFailureReason은 item 4다. 조회(poll) 경로로 들어온
// FAILURE도 콜백 경로와 똑같이 payload["reason"]을 failure_reason에 저장해야
// 한다 — 콜백만 저장하면 조회로 실패를 알게 되는 경우 사유가 빈 채로 남는다.
func TestPollFailureRecordsFailureReason(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(1080)
	defer cleanupServiceUsers(t, db, userID)

	seedLedgerFunds(t, db, userID, model.KRWAssetSymbol, decimal.NewFromInt(100000))

	processor := NewFakeTransferProcessor()
	transferSvc := NewTransferService(db, processor)

	request, err := transferSvc.RequestWithdrawal(WithdrawalInput{
		UserID: userID, Rail: model.TransferRailBank, Asset: model.KRWAssetSymbol,
		Amount: "100000", ClientRequestKey: fmt.Sprintf("t-failreason-poll-%d", userID),
	})
	require.NoError(t, err)
	require.NotNil(t, request.ExternalRef)

	require.NoError(t, transferSvc.ResolveTransfer(ResolveInput{
		TransferRequestID: request.ID, Source: model.TransferEventSourcePoll,
		EventKey: fmt.Sprintf("poll:%d:1", request.ID),
		Outcome:  model.TransferOutcomeFailure,
		Payload:  map[string]any{"reason": "INSUFFICIENT_EXTERNAL_BALANCE"},
	}))

	var final model.TransferRequest
	require.NoError(t, db.First(&final, request.ID).Error)
	assert.Equal(t, model.TransferStatusFailed, final.Status)
	assert.Equal(t, "INSUFFICIENT_EXTERNAL_BALANCE", final.FailureReason)
}

// TestSuccessConfirmationNeverWritesFailureReason은 item 4다. SUCCESS 확정은
// payload에 "reason" 키가 섞여 있어도 failure_reason을 절대 쓰지 않아야 한다 —
// 성공한 이전이 실패 사유를 갖는 것은 말이 안 된다.
func TestSuccessConfirmationNeverWritesFailureReason(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(1081)
	defer cleanupServiceUsers(t, db, userID)

	seedLedgerFunds(t, db, userID, model.KRWAssetSymbol, decimal.NewFromInt(100000))

	processor := NewFakeTransferProcessor()
	transferSvc := NewTransferService(db, processor)

	request, err := transferSvc.RequestWithdrawal(WithdrawalInput{
		UserID: userID, Rail: model.TransferRailBank, Asset: model.KRWAssetSymbol,
		Amount: "100000", ClientRequestKey: fmt.Sprintf("t-failreason-success-%d", userID),
	})
	require.NoError(t, err)
	require.NotNil(t, request.ExternalRef)

	require.NoError(t, transferSvc.ResolveTransfer(ResolveInput{
		TransferRequestID: request.ID, Source: model.TransferEventSourcePoll,
		EventKey: fmt.Sprintf("poll:%d:1", request.ID),
		Outcome:  model.TransferOutcomeSuccess,
		Payload:  map[string]any{"reason": "should-never-be-stored"},
	}))

	var final model.TransferRequest
	require.NoError(t, db.First(&final, request.ID).Error)
	assert.Equal(t, model.TransferStatusCompleted, final.Status)
	assert.Empty(t, final.FailureReason)
}

// TestReversalNetsToZeroPerAccount는 T8이다. LedgerService.Reverse는 Task 1·2에서
// 이미 구현됐으므로(§ 계획서 "이미 있는 것"), 원본 분개를 하나 만들고 되돌려서
// 계정별 합이 0이 되는지만 확인한다.
func TestReversalNetsToZeroPerAccount(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(1040)
	defer cleanupServiceUsers(t, db, userID)

	asset := fmt.Sprintf("REV%d", time.Now().UnixNano()%1_000_000_000)
	ledger := NewLedgerService(db)
	owner := userID

	var original *model.JournalEntry
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		entry, _, recordErr := ledger.Record(tx, JournalInput{
			EventType:      model.JournalEventDevFund,
			IdempotencyKey: fmt.Sprintf("t8-original-%d", userID),
			ReferenceType:  model.JournalReferenceDevFund,
			ReferenceID:    userID,
			Postings: []PostingInput{
				{AccountType: model.AccountDevMint, Asset: asset, Amount: decimal.NewFromInt(-500)},
				{AccountType: model.AccountUserAvailable, OwnerUserID: &owner, Asset: asset, Amount: decimal.NewFromInt(500)},
			},
		})
		original = entry
		return recordErr
	}))

	var reversal *model.JournalEntry
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		entry, reverseErr := ledger.Reverse(tx, original.ID)
		reversal = entry
		return reverseErr
	}))
	require.NotNil(t, reversal)
	assert.NotEqual(t, original.ID, reversal.ID)

	var rows []struct {
		AccountID uint
		Sum       decimal.Decimal
	}
	require.NoError(t, db.Raw(`
		SELECT account_id, SUM(amount) AS sum
		FROM postings WHERE journal_id IN (?, ?)
		GROUP BY account_id`, original.ID, reversal.ID).Scan(&rows).Error)
	require.Len(t, rows, 2, "두 계정(DEV_MINT, USER_AVAILABLE) 각각 원본+역분개 두 행씩 있어야 한다")
	for _, row := range rows {
		assert.True(t, row.Sum.IsZero(), "계정 %d의 원본+역분개 합이 0이 아니다: %s", row.AccountID, row.Sum.String())
	}
}

// TestUnknownKeepsLockThenSuccessCompletes는 T10이다. 2단계 시나리오: ① 알림
// 유실 뒤 조회가 UNKNOWN이면 분개 없이 잠금이 그대로 남고 확인 표시만 켜진다.
// ② 이어서 SUCCESS가 오면 완료 분개가 정확히 1회 생기고 확인 표시가 해제된다.
func TestUnknownKeepsLockThenSuccessCompletes(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(1050)
	defer cleanupServiceUsers(t, db, userID)

	seedLedgerFunds(t, db, userID, model.KRWAssetSymbol, decimal.NewFromInt(100000))

	processor := NewFakeTransferProcessor()
	transferSvc := NewTransferService(db, processor)

	request, err := transferSvc.RequestWithdrawal(WithdrawalInput{
		UserID: userID, Rail: model.TransferRailBank, Asset: model.KRWAssetSymbol,
		Amount: "100000", ClientRequestKey: fmt.Sprintf("t10-%d", userID),
	})
	require.NoError(t, err)
	require.NotNil(t, request.ExternalRef)
	confirmKey := fmt.Sprintf("withdraw-settle:%s", *request.ExternalRef)

	// ① 조회 UNKNOWN.
	require.NoError(t, transferSvc.RecordObservation(ObservationInput{
		TransferRequestID: request.ID,
		EventKey:          fmt.Sprintf("poll:%d:1", request.ID),
		Outcome:           model.TransferOutcomeUnknown,
	}))

	var afterUnknown model.TransferRequest
	require.NoError(t, db.First(&afterUnknown, request.ID).Error)
	assert.Equal(t, model.TransferStatusProcessing, afterUnknown.Status)
	require.NotNil(t, afterUnknown.ReviewRequiredAt)
	require.NotNil(t, afterUnknown.ReviewReason)
	assert.Equal(t, model.ReviewReasonExternalUnknown, *afterUnknown.ReviewReason)
	assertLedgerBalances(t, db, userID, model.KRWAssetSymbol, decimal.Zero, decimal.NewFromInt(100000))

	var journalCountAfterUnknown int64
	require.NoError(t, db.Model(&model.JournalEntry{}).
		Where("idempotency_key = ?", confirmKey).Count(&journalCountAfterUnknown).Error)
	assert.Zero(t, journalCountAfterUnknown, "UNKNOWN 관측이 분개를 만들었다")

	// ② 이어서 SUCCESS.
	require.NoError(t, transferSvc.ResolveTransfer(ResolveInput{
		TransferRequestID: request.ID, Source: model.TransferEventSourcePoll,
		EventKey: fmt.Sprintf("poll:%d:2", request.ID),
		Outcome:  model.TransferOutcomeSuccess,
	}))

	var final model.TransferRequest
	require.NoError(t, db.First(&final, request.ID).Error)
	assert.Equal(t, model.TransferStatusCompleted, final.Status)
	assert.Nil(t, final.NextCheckAt)
	assert.Nil(t, final.ReviewRequiredAt, "확인 표시가 해제돼야 한다")
	assert.Nil(t, final.ReviewReason)

	var journalCountFinal int64
	require.NoError(t, db.Model(&model.JournalEntry{}).
		Where("idempotency_key = ?", confirmKey).Count(&journalCountFinal).Error)
	assert.EqualValues(t, 1, journalCountFinal, "완료 분개가 정확히 1회여야 한다")

	assertLedgerBalances(t, db, userID, model.KRWAssetSymbol, decimal.Zero, decimal.Zero)
}

// erroringTransferProcessor는 GetTransferStatus가 항상 실패하는 처리기다 — 실제
// 외부 처리기가 진짜로 응답하지 않는 상황(EXTERNAL_UNREACHABLE)을 재현한다.
// Submit은 FakeTransferProcessor에 그대로 위임한다.
type erroringTransferProcessor struct {
	*FakeTransferProcessor
}

func (p *erroringTransferProcessor) GetTransferStatus(string) (model.TransferOutcome, map[string]any, error) {
	return "", nil, fmt.Errorf("external system unreachable")
}

// TestStatusCheckFailureAdvancesScheduleThenFlagsReview는 GetTransferStatus
// 자체가 실패했을 때도 §8.6 백오프가 돌고, 임계(30분)를 넘기면 확인 표시가
// EXTERNAL_UNREACHABLE로 켜지는지 본다. 분개는 만들지 않는다 — 시간이나 조회
// 실패가 돈을 움직이지 않는다. poller의 checkStatus를 직접 불러 DueForCheck의
// 전역 스캔(테스트 격리 위반)을 피한다.
func TestStatusCheckFailureAdvancesScheduleThenFlagsReview(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(1060)
	defer cleanupServiceUsers(t, db, userID)

	seedLedgerFunds(t, db, userID, model.KRWAssetSymbol, decimal.NewFromInt(100000))

	processor := &erroringTransferProcessor{FakeTransferProcessor: NewFakeTransferProcessor()}
	fixedNow := time.Now()
	transferSvc := NewTransferService(db, processor)
	transferSvc.Now = func() time.Time { return fixedNow }

	request, err := transferSvc.RequestWithdrawal(WithdrawalInput{
		UserID: userID, Rail: model.TransferRailBank, Asset: model.KRWAssetSymbol,
		Amount: "100000", ClientRequestKey: fmt.Sprintf("t-unreachable-%d", userID),
	})
	require.NoError(t, err)
	require.Equal(t, model.TransferStatusProcessing, request.Status)
	require.NotNil(t, request.ExternalRef)
	confirmKey := fmt.Sprintf("withdraw-settle:%s", *request.ExternalRef)

	poller := &TransferStatusPoller{Transfers: repository.NewTransferRepository(db), Service: transferSvc}

	// 1차 조회 — 실패, 임계 전.
	poller.checkStatus(*request)

	var afterFirst model.TransferRequest
	require.NoError(t, db.First(&afterFirst, request.ID).Error)
	assert.Equal(t, model.TransferStatusProcessing, afterFirst.Status)
	assert.EqualValues(t, 1, afterFirst.CheckAttempts, "check_attempts가 오르지 않았다")
	require.NotNil(t, afterFirst.NextCheckAt)
	assert.True(t, afterFirst.NextCheckAt.After(fixedNow), "next_check_at이 전진하지 않았다 — 조회 폭주")
	assert.Nil(t, afterFirst.ReviewRequiredAt, "임계 전인데 확인 표시가 켜졌다")
	assertLedgerBalances(t, db, userID, model.KRWAssetSymbol, decimal.Zero, decimal.NewFromInt(100000))

	var journalCount int64
	require.NoError(t, db.Model(&model.JournalEntry{}).
		Where("idempotency_key = ?", confirmKey).Count(&journalCount).Error)
	assert.Zero(t, journalCount, "조회 실패가 분개를 만들었다 — 시간(이나 실패)이 돈을 움직이면 안 된다")

	var eventCount int64
	require.NoError(t, db.Model(&model.TransferStatusEvent{}).
		Where("transfer_request_id = ?", request.ID).Count(&eventCount).Error)
	assert.Zero(t, eventCount, "외부로부터 알게 된 사실이 없는데 사건이 남았다")

	// 2차 조회 — 임계(30분)를 넘긴 뒤.
	fixedNow = fixedNow.Add(31 * time.Minute)
	poller.checkStatus(afterFirst)

	var afterSecond model.TransferRequest
	require.NoError(t, db.First(&afterSecond, request.ID).Error)
	assert.Equal(t, model.TransferStatusProcessing, afterSecond.Status)
	assert.EqualValues(t, 2, afterSecond.CheckAttempts)
	require.NotNil(t, afterSecond.ReviewRequiredAt)
	require.NotNil(t, afterSecond.ReviewReason)
	assert.Equal(t, model.ReviewReasonExternalUnreachable, *afterSecond.ReviewReason)
	assertLedgerBalances(t, db, userID, model.KRWAssetSymbol, decimal.Zero, decimal.NewFromInt(100000))

	// 3차 조회 — 임계를 이미 넘긴 채로 시간이 더 지나도 review_required_at은
	// 최초 표시 시각 그대로여야 한다. 매 조회마다 갱신되면 5시간 전에 걸린
	// 출금도 방금 걸린 것처럼 보여 "가장 오래 멈춘 것부터" 정렬이 뒤집힌다.
	fixedNow = fixedNow.Add(1 * time.Hour)
	poller.checkStatus(afterSecond)

	var afterThird model.TransferRequest
	require.NoError(t, db.First(&afterThird, request.ID).Error)
	require.NotNil(t, afterThird.ReviewRequiredAt)
	assert.True(t, afterSecond.ReviewRequiredAt.Equal(*afterThird.ReviewRequiredAt),
		"review_required_at이 최초 표시 시각에서 밀렸다: %s -> %s",
		afterSecond.ReviewRequiredAt, afterThird.ReviewRequiredAt)
}

// failingSubmitTransferProcessor는 Submit이 항상 실패하는 처리기다 — 외부 제출이
// 영구적으로 거절되는 상황(예: 계좌 정지)을 재현한다. GetTransferStatus는 이
// 테스트가 쓰지 않으므로 FakeTransferProcessor에 그대로 위임한다.
type failingSubmitTransferProcessor struct {
	*FakeTransferProcessor
}

func (p *failingSubmitTransferProcessor) Submit(string, model.TransferRequest) (string, error) {
	return "", fmt.Errorf("external submit permanently failing")
}

// TestDueForCheckSchedulesAroundPermanentlyFailingLowIDRequests는 item 2의
// 스타베이션 회귀다. next_check_at으로 RECEIVED를 게이트하기 전에는 DueForCheck가
// "status = RECEIVED OR (...)"·"ORDER BY id"였다 — id가 낮은 영구 제출 실패
// 요청들이 매 틱 배치를 가득 채워 뒤의 다른 요청이 굶었다.
//
// 이 테스트는 실제 DueForCheck SQL·LIMIT·정렬을 그대로 쓴다(poller.RunOnce를
// 두 번 부른다). 결과를 사후에 걸러내는 데코레이터를 쓰면 LIMIT을 남의 행이
// 먼저 차지해도 테스트가 조용히 다른 것을 검증하게 된다.
//
// testdb.OpenIsolatedSchemaDB로 전용 임시 스키마를 쓴다 — 공유 스키마에는 이
// 테스트가 만들지 않은, 정리되지 않는 transfer_requests 행이 있을 수 있고
// (예: TestLedgerSchemaIntegration의 제약 검증 픽스처), next_check_at이 NULL인
// 행은 DueForCheck에서 항상 즉시 due로 취급돼 이 테스트의 배치를 나눠 가진다.
// 격리된 스키마에서는 그런 행이 존재할 수 없으므로, 아래 전제 단언(픽스처
// 생성 전 due 0건)은 항상 참이어야 한다 — 실패하면 격리 자체가 깨졌다는
// 뜻이다. 이 테스트를 파일의 어디에 두든(다른 테스트보다 앞이든 뒤든, 심지어
// -shuffle=on으로 실행 순서가 섞여도) 결과가 같아야 한다 — 격리된 스키마는
// 다른 테스트의 실행 순서·타이밍과 무관하기 때문이다.
func TestDueForCheckSchedulesAroundPermanentlyFailingLowIDRequests(t *testing.T) {
	db := testdb.OpenIsolatedSchemaDB(t)
	repo := repository.NewTransferRepository(db)

	createdAt := time.Now()
	dueAt := createdAt.Add(pollBaseInterval)
	precheck, err := repo.DueForCheck(dueAt, 1_000_000)
	require.NoError(t, err)
	require.Empty(t, precheck,
		"사전 조건 위반: 격리된 스키마인데 픽스처를 만들기도 전에"+
			" DueForCheck(now+pollBaseInterval, 큰 값)가 %d건을 돌려줬다 — 격리 자체가"+
			" 깨졌다는 뜻이다.", len(precheck))

	userID := serviceTestUserID(1070)
	seedLedgerFunds(t, db, userID, model.KRWAssetSymbol, decimal.NewFromInt(10000))

	processor := &failingSubmitTransferProcessor{FakeTransferProcessor: NewFakeTransferProcessor()}
	transferSvc := NewTransferService(db, processor)
	now := createdAt
	transferSvc.Now = func() time.Time { return now }

	const failingCount = 3
	failingIDs := make([]uint, 0, failingCount)
	for i := 0; i < failingCount; i++ {
		request, reqErr := transferSvc.RequestWithdrawal(WithdrawalInput{
			UserID: userID, Rail: model.TransferRailBank, Asset: model.KRWAssetSymbol,
			Amount: "100", ClientRequestKey: fmt.Sprintf("t-starve-low-%d-%d", userID, i),
		})
		require.NoError(t, reqErr)
		require.Equal(t, model.TransferStatusReceived, request.Status)
		require.EqualValues(t, 1, request.CheckAttempts)
		failingIDs = append(failingIDs, request.ID)
	}

	higherIDRequest, err := transferSvc.RequestWithdrawal(WithdrawalInput{
		UserID: userID, Rail: model.TransferRailBank, Asset: model.KRWAssetSymbol,
		Amount: "100", ClientRequestKey: fmt.Sprintf("t-starve-high-%d", userID),
	})
	require.NoError(t, err)
	require.Equal(t, model.TransferStatusReceived, higherIDRequest.Status)
	for _, failingID := range failingIDs {
		require.Less(t, failingID, higherIDRequest.ID, "픽스처 순서상 실패 요청들의 id가 더 낮아야 한다")
	}

	poller := &TransferStatusPoller{Transfers: repo, Service: transferSvc, Batch: failingCount}

	// 접수 시점의 첫 제출 시도도 실패해 이미 next_check_at = now+10s(백오프 1회차)로
	// 예약돼 있다. 그 시각으로 시계를 옮기면 N+1건이 모두 동시에 due가 된다 — id
	// 말고는 이들을 가를 기준이 없다.
	now = now.Add(pollBaseInterval)

	// 1차 RunOnce: batch가 failingCount와 같으므로, id가 더 낮은 실패 요청
	// 3건만 배치를 채우고 id가 더 높은 요청은 밀려난다(스타베이션 재현).
	poller.RunOnce()

	for _, failingID := range failingIDs {
		var reloaded model.TransferRequest
		require.NoError(t, db.First(&reloaded, failingID).Error)
		assert.EqualValues(t, 2, reloaded.CheckAttempts, "1차 RunOnce에서 재시도됐어야 한다")
		require.NotNil(t, reloaded.NextCheckAt)
		assert.True(t, reloaded.NextCheckAt.After(now),
			"실패 요청의 next_check_at이 전진하지 않았다 — 다음 틱에도 배치를 채운다")
	}

	var afterFirstRun model.TransferRequest
	require.NoError(t, db.First(&afterFirstRun, higherIDRequest.ID).Error)
	assert.EqualValues(t, 1, afterFirstRun.CheckAttempts,
		"1차 RunOnce가 id 낮은 실패 요청들에 밀려 처리되지 않았어야 한다(스타베이션)")
	require.NotNil(t, afterFirstRun.NextCheckAt)
	assert.True(t, !afterFirstRun.NextCheckAt.After(now),
		"id 높은 요청이 여전히 due 상태여야 다음 틱에서 처리될 수 있다")

	// 2차 RunOnce: 시계는 그대로다(now를 옮기지 않는다). 실패 요청 3건은 방금
	// next_check_at이 미래로 밀려 더 이상 due가 아니고, id 높은 요청만 여전히
	// due다 — 배치에 자리가 남으므로 이번에는 처리된다.
	poller.RunOnce()

	for _, failingID := range failingIDs {
		var reloaded model.TransferRequest
		require.NoError(t, db.First(&reloaded, failingID).Error)
		assert.EqualValues(t, 2, reloaded.CheckAttempts,
			"2차 RunOnce에서 재제출되지 않았어야 한다 — next_check_at이 아직 미래다")
	}

	var afterSecondRun model.TransferRequest
	require.NoError(t, db.First(&afterSecondRun, higherIDRequest.ID).Error)
	assert.EqualValues(t, 2, afterSecondRun.CheckAttempts,
		"id 높은 요청이 2차 RunOnce에서 처리됐어야 한다")
}

// TestSetDispatchedFirstCheckDueAtPollBaseIntervalBoundary는 item 2의 "첫 조회
// 10초 경계" 테스트다. SetDispatched가 next_check_at을 제출 시각+pollBaseInterval로
// 설정한 뒤, 그 경계 바로 앞에서는 아직 조회 대상이 아니고 경계에 도달해야
// 비로소 조회 대상이 되는지 DueForCheck로 직접 확인한다. 위 스타베이션
// 테스트와 같은 이유로 격리된 스키마를 쓴다.
func TestSetDispatchedFirstCheckDueAtPollBaseIntervalBoundary(t *testing.T) {
	db := testdb.OpenIsolatedSchemaDB(t)
	userID := serviceTestUserID(1071)

	seedLedgerFunds(t, db, userID, model.KRWAssetSymbol, decimal.NewFromInt(100000))

	processor := NewFakeTransferProcessor()
	transferSvc := NewTransferService(db, processor)

	dispatchAt := time.Now()
	transferSvc.Now = func() time.Time { return dispatchAt }

	request, err := transferSvc.RequestWithdrawal(WithdrawalInput{
		UserID: userID, Rail: model.TransferRailBank, Asset: model.KRWAssetSymbol,
		Amount: "100000", ClientRequestKey: fmt.Sprintf("t-boundary-%d", userID),
	})
	require.NoError(t, err)
	require.Equal(t, model.TransferStatusProcessing, request.Status)
	require.NotNil(t, request.NextCheckAt)
	// timestamptz 컬럼은 마이크로초까지만 저장하므로, Go의 나노초 정밀도
	// time.Now()와 DB에서 다시 읽은 값은 정확히 같지 않을 수 있다 — 1ms
	// 허용 오차로 비교한다.
	assert.WithinDuration(t, dispatchAt.Add(pollBaseInterval), *request.NextCheckAt, time.Millisecond,
		"SetDispatched의 next_check_at이 제출 시각+pollBaseInterval이 아니다")

	repo := repository.NewTransferRepository(db)

	beforeBoundary, err := repo.DueForCheck(dispatchAt.Add(pollBaseInterval-time.Millisecond), 1_000_000)
	require.NoError(t, err)
	assert.False(t, containsTransferRequestID(beforeBoundary, request.ID),
		"경계-1ms인데 이미 조회 대상이다")

	atBoundary, err := repo.DueForCheck(dispatchAt.Add(pollBaseInterval), 1_000_000)
	require.NoError(t, err)
	assert.True(t, containsTransferRequestID(atBoundary, request.ID),
		"경계 시각인데 아직 조회 대상이 아니다")
}

func containsTransferRequestID(requests []model.TransferRequest, id uint) bool {
	for _, request := range requests {
		if request.ID == id {
			return true
		}
	}
	return false
}
