package service

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// TestSettlementBatchHoldBatchLockOrderIsGlobal은 배치 정산과 배치 잠금이 서로
// 반대되는 사용자 순서로 같은 계정 집합을 건드려도 둘 다 성공하는지 본다.
//
// LedgerService.Record는 자기 호출 안에서만 계정을 정렬한다. 체결마다 Record를
// 부르면 각 호출은 정렬돼 있어도 트랜잭션 전체의 획득 순서는 정렬되지 않는다 —
// 앞 체결이 큰 ID를 쥔 채 다음 체결이 작은 ID를 요구하고, 오름차순으로 잠그는
// HoldBatch와 만나면 순환 대기가 되어 40P01로 한쪽이 희생된다.
//
// 결정적으로 재현하려고 두 지점에 장벽을 둔다. 순서가 핵심이다:
//
//  1. HoldBatch를 먼저 띄워 잠금 직전에 세운다(아직 아무것도 안 쥔 상태).
//  2. 정산 배치를 띄워 첫 체결을 반영한 직후에 세운다.
//  3. HoldBatch를 풀어 준다 — 정산 배치가 쥔 계정을 기다리게 된다.
//  4. 기다리는 것을 pg_blocking_pids로 확인한다. 확인되지 않으면 두 트랜잭션이
//     겹치지 않았다는 뜻이라 이 테스트는 아무것도 증명하지 못한다.
//  5. 정산 배치를 풀어 준다. 잠금 순서가 전역으로 일관되면 둘 다 통과하고,
//     아니면 교착상태가 된다.
func TestSettlementBatchHoldBatchLockOrderIsGlobal(t *testing.T) {
	db := openServiceIntegrationDB(t)

	// 계정 ID는 만든 순서대로 커진다. low가 먼저 생겨야 두 경로의 사용자 순서가
	// 실제로 반대가 된다.
	lowID := serviceTestUserID(970)
	highID := serviceTestUserID(971)
	sellerID := serviceTestUserID(972)
	defer cleanupServiceUsers(t, db, lowID, highID, sellerID)

	asset := fmt.Sprintf("LOCK%d", time.Now().UnixNano()%1_000_000_000)
	devService := NewDevWalletService(db)
	fund := func(userID uint, coin string, amount string) {
		t.Helper()
		_, err := devService.FundWallet(FundWalletInput{
			UserID: userID, CoinSymbol: coin, Amount: amount,
			RequestKey: fmt.Sprintf("lockorder-%d-%s-%d", userID, coin, time.Now().UnixNano()),
		})
		require.NoError(t, err)
	}
	fund(lowID, asset, "10")
	fund(highID, asset, "10")
	fund(lowID, model.KRWAssetSymbol, "1000")
	fund(highID, model.KRWAssetSymbol, "1000")
	fund(sellerID, asset, "10")

	orderRepo := repository.NewOrderRepository(db)
	ledger := NewLedgerService(db)
	mkOrder := func(userID uint, side model.OrderSide, amount int64) *model.Order {
		t.Helper()
		order := &model.Order{
			UserID: userID, CoinSymbol: asset, Side: side,
			OrderType: model.OrderTypeLimit, Status: model.OrderStatusPending,
			Price: decimal.NewFromInt(100), Amount: decimal.NewFromInt(amount),
		}
		require.NoError(t, persistAndHold(db, orderRepo, ledger, order))
		return order
	}
	buyHigh := mkOrder(highID, model.OrderSideBuy, 1)
	buyLow := mkOrder(lowID, model.OrderSideBuy, 1)
	sellOrder := mkOrder(sellerID, model.OrderSideSell, 2)

	mkTrade := func(buyOrder *model.Order) *model.Trade {
		return &model.Trade{
			CoinSymbol: asset, Price: decimal.NewFromInt(100), Quantity: decimal.NewFromInt(1),
			TradedAt: time.Now(), BuyOrderID: buyOrder.ID, SellOrderID: sellOrder.ID,
		}
	}

	// 장벽 A: 첫 체결의 평단가 갱신 직후. 이 시점에 정산 배치는 트랜잭션 안에서
	// 계정을 쥐고 있다.
	var settleArmed atomic.Bool
	settleReached := make(chan struct{}, 1)
	settleResume := make(chan struct{})
	var settleOnce sync.Once
	releaseSettle := func() { settleOnce.Do(func() { close(settleResume) }) }
	defer releaseSettle()

	require.NoError(t, db.Callback().Raw().After("gorm:raw").Register("test_pause_after_first_trade",
		func(tx *gorm.DB) {
			if !settleArmed.CompareAndSwap(true, false) {
				return
			}
			if !strings.Contains(tx.Statement.SQL.String(), "INSERT INTO user_asset_stats") {
				settleArmed.Store(true)
				return
			}
			settleReached <- struct{}{}
			<-settleResume
		}))
	defer db.Callback().Raw().Remove("test_pause_after_first_trade")

	// 장벽 B: HoldBatch가 계정을 잠그기 직전. 아직 아무것도 쥐지 않았다.
	var holdArmed atomic.Bool
	holdPIDCh := make(chan int, 1)
	holdResume := make(chan struct{})
	var holdOnce sync.Once
	releaseHold := func() { holdOnce.Do(func() { close(holdResume) }) }
	defer releaseHold()

	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("test_pause_before_hold_lock",
		func(tx *gorm.DB) {
			if !holdArmed.CompareAndSwap(true, false) {
				return
			}
			if tx.Statement.Table != "account_balances" {
				holdArmed.Store(true)
				return
			}
			if _, locking := tx.Statement.Clauses["FOR"]; !locking {
				holdArmed.Store(true)
				return
			}
			// 같은 커넥션(=같은 트랜잭션)에서 읽어야 잠글 백엔드의 pid가 나온다.
			var pid int
			if err := tx.Session(&gorm.Session{NewDB: true}).
				Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
				panic(err)
			}
			holdPIDCh <- pid
			<-holdResume
		}))
	defer db.Callback().Query().Remove("test_pause_before_hold_lock")

	// 1. HoldBatch를 먼저 띄운다. 정산 배치보다 앞서 장벽 B를 소비해야 두 경로가
	//    각자 의도한 지점에서 선다.
	holdArmed.Store(true)
	holdDone := make(chan error, 1)
	go func() {
		coordinator := &HoldCoordinator{DB: db, OrderRepo: repository.NewOrderRepository(db), Ledger: NewLedgerService(db)}
		sell := func(userID uint) *model.Order {
			return &model.Order{
				UserID: userID, CoinSymbol: asset, Side: model.OrderSideSell,
				OrderType: model.OrderTypeLimit, Status: model.OrderStatusPending,
				Price: decimal.NewFromInt(100), Amount: decimal.NewFromInt(1),
			}
		}
		// 오름차순: low 다음 high.
		_, err := coordinator.HoldBatch([]holdRequest{{order: sell(lowID)}, {order: sell(highID)}})
		holdDone <- err
	}()

	var holdPID int
	select {
	case holdPID = <-holdPIDCh:
	case err := <-holdDone:
		t.Fatalf("HoldBatch가 잠금 장벽에 닿지 못했다: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("HoldBatch 잠금 장벽 대기 시간 초과")
	}

	// 2. 정산 배치를 띄운다. 내림차순: high 다음 low — HoldBatch와 반대다.
	settleArmed.Store(true)
	settleDone := make(chan error, 1)
	go func() {
		settlementService := NewSettlementService(db, repository.NewOrderRepository(db))
		_, err := settlementService.SettleTradeBatch([]TradeBatchItem{
			{Trade: mkTrade(buyHigh)}, {Trade: mkTrade(buyLow)},
		})
		settleDone <- err
	}()

	select {
	case <-settleReached:
	case err := <-settleDone:
		t.Fatalf("정산 배치가 첫 체결 장벽에 닿지 못했다: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("정산 배치 장벽 대기 시간 초과")
	}

	// 3~4. HoldBatch를 풀고, 정산 배치가 쥔 계정을 기다리는지 확인한다.
	releaseHold()
	blocked := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		require.NoError(t, db.Raw(`
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE pid = ? AND cardinality(pg_blocking_pids(pid)) > 0
			)`, holdPID).Scan(&blocked).Error)
		if blocked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 5. 정산 배치를 푼다.
	releaseSettle()
	settleErr := <-settleDone
	holdErr := <-holdDone

	require.True(t, blocked,
		"HoldBatch가 정산 배치의 계정 잠금을 기다리지 않았다 — 두 트랜잭션이 겹치지 않아 이 테스트가 아무것도 증명하지 못한다")
	require.NoError(t, settleErr, "정산 배치가 실패했다 — 배치 전체의 잠금 순서가 전역으로 일관되지 않다")
	require.NoError(t, holdErr, "동시 실행된 HoldBatch가 실패했다 — 교착상태 회귀")
}

// lockAccountBalanceNoWait은 계정 하나의 잔액 행을 NOWAIT으로 잠가 본다.
// 이미 다른 트랜잭션이 쥐고 있으면 즉시 오류(55P03)가 나고, 아니면 nil이다.
// 기다리지 않으므로 "지금 이 순간 잠겨 있는가"를 결정적으로 판정할 수 있다.
//
// 계정이 아직 없으면 0행이라 오류가 나지 않는다 — 그것도 "잠겨 있지 않다"가 맞다.
func lockAccountBalanceNoWait(db *gorm.DB, accountType model.AccountType, userID uint, asset string) error {
	var found int
	return db.Raw(`
		SELECT 1
		FROM account_balances b
		JOIN accounts a ON a.id = b.account_id
		WHERE a.account_type = ? AND a.owner_user_id = ? AND a.asset = ?
		FOR UPDATE OF b NOWAIT`, accountType, userID, asset).Scan(&found).Error
}

// TestHoldBatchPreLocksAvailableAndLocked는 HoldBatch가 잠금 분개를 만들기 전에
// available과 locked를 **둘 다** 미리 잠그는지 본다.
//
// 잠금 분개는 available에서 빼고 locked에 더하므로 Record는 두 계정을 모두 잠근다.
// 사전 잠금이 available만 덮으면 locked는 체결 루프 중간에 처음 잠기게 되어,
// 트랜잭션 전체의 획득 순서가 오름차순에서 벗어난다.
//
// TestSettlementBatchHoldBatchLockOrderIsGlobal은 HoldBatch를 아직 아무것도 잡지
// 않은 시점에 세우므로 이 성질을 보지 못한다 — 그 테스트는 정산 배치 쪽 회귀
// 테스트로 두고, HoldBatch 쪽은 여기서 따로 고정한다.
//
// 판정은 두 겹이다:
//   - NOWAIT 탐침으로 available·locked가 지금 잠겨 있는지 각각 즉시 확인한다.
//   - 실제로 기다리는 트랜잭션을 하나 붙여 pg_blocking_pids로 대기 관계를 확인한다.
//     겹치지 않아서 통과하는 경우를 배제하기 위해서다.
func TestHoldBatchPreLocksAvailableAndLocked(t *testing.T) {
	db := openServiceIntegrationDB(t)

	userID := serviceTestUserID(977)
	defer cleanupServiceUsers(t, db, userID)

	asset := fmt.Sprintf("PRE%d", time.Now().UnixNano()%1_000_000_000)
	orderRepo := repository.NewOrderRepository(db)
	ledger := NewLedgerService(db)

	_, err := NewDevWalletService(db).FundWallet(FundWalletInput{
		UserID: userID, CoinSymbol: asset, Amount: "10",
		RequestKey: fmt.Sprintf("prelock-%d-%d", userID, time.Now().UnixNano()),
	})
	require.NoError(t, err)

	// locked 계정을 **커밋된 상태로** 미리 만든다. 같은 트랜잭션이 방금 INSERT한
	// 행은 다른 세션에 보이지 않으므로(MVCC), 미리 커밋해 두지 않으면 아래 탐침이
	// "잠겨 있지 않다"와 "아직 없다"를 구분하지 못한다.
	seed := &model.Order{
		UserID: userID, CoinSymbol: asset, Side: model.OrderSideSell,
		OrderType: model.OrderTypeLimit, Status: model.OrderStatusPending,
		Price: decimal.NewFromInt(100), Amount: decimal.NewFromInt(1),
	}
	require.NoError(t, persistAndHold(db, orderRepo, ledger, seed))
	require.NoError(t, lockAccountBalanceNoWait(db, model.AccountUserLocked, userID, asset),
		"사전 조건이 깨졌다 — 아무도 잠그지 않은 locked 계정이 잠겨 있다")

	// 장벽: HoldBatch의 사전 잠금 질의가 끝난 직후. 이 시점에 HoldBatch는 사전
	// 잠금 집합을 쥐고 있고 아직 Record 루프에 들어가지 않았다.
	var armed atomic.Bool
	reached := make(chan int, 1)
	resume := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(resume) }) }
	defer release()

	require.NoError(t, db.Callback().Query().After("gorm:query").Register("test_pause_after_hold_prelock",
		func(tx *gorm.DB) {
			if !armed.CompareAndSwap(true, false) {
				return
			}
			if tx.Statement.Table != "account_balances" {
				armed.Store(true)
				return
			}
			if _, locking := tx.Statement.Clauses["FOR"]; !locking {
				armed.Store(true)
				return
			}
			var pid int
			if err := tx.Session(&gorm.Session{NewDB: true}).
				Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
				panic(err)
			}
			reached <- pid
			<-resume
		}))
	defer db.Callback().Query().Remove("test_pause_after_hold_prelock")

	armed.Store(true)
	holdDone := make(chan error, 1)
	go func() {
		coordinator := &HoldCoordinator{DB: db, OrderRepo: repository.NewOrderRepository(db), Ledger: NewLedgerService(db)}
		sell := func() *model.Order {
			return &model.Order{
				UserID: userID, CoinSymbol: asset, Side: model.OrderSideSell,
				OrderType: model.OrderTypeLimit, Status: model.OrderStatusPending,
				Price: decimal.NewFromInt(100), Amount: decimal.NewFromInt(1),
			}
		}
		_, err := coordinator.HoldBatch([]holdRequest{{order: sell()}, {order: sell()}})
		holdDone <- err
	}()

	var holdPID int
	select {
	case holdPID = <-reached:
	case err := <-holdDone:
		t.Fatalf("HoldBatch가 사전 잠금 장벽에 닿지 못했다: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("HoldBatch 사전 잠금 장벽 대기 시간 초과")
	}

	// 판정 1: 두 계정이 지금 잠겨 있어야 한다.
	require.Error(t, lockAccountBalanceNoWait(db, model.AccountUserAvailable, userID, asset),
		"HoldBatch가 available 계정을 사전 잠금하지 않았다")
	require.Error(t, lockAccountBalanceNoWait(db, model.AccountUserLocked, userID, asset),
		"HoldBatch가 locked 계정을 사전 잠금하지 않았다 — Record 루프 중간에 처음 잠기면 트랜잭션 전체의 획득 순서가 오름차순에서 벗어난다")

	// 판정 2: 실제로 기다리는 트랜잭션이 HoldBatch를 기다려야 한다.
	waiterPIDCh := make(chan int, 1)
	waiterDone := make(chan error, 1)
	go func() {
		waiterDone <- db.Transaction(func(tx *gorm.DB) error {
			var pid int
			if err := tx.Raw("SELECT pg_backend_pid()").Scan(&pid).Error; err != nil {
				return err
			}
			waiterPIDCh <- pid

			var found int
			return tx.Raw(`
				SELECT 1
				FROM account_balances b
				JOIN accounts a ON a.id = b.account_id
				WHERE a.account_type = ? AND a.owner_user_id = ? AND a.asset = ?
				FOR UPDATE OF b`, model.AccountUserLocked, userID, asset).Scan(&found).Error
		})
	}()

	var waiterPID int
	select {
	case waiterPID = <-waiterPIDCh:
	case err := <-waiterDone:
		t.Fatalf("경쟁 트랜잭션이 시작되지 못했다: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("경쟁 트랜잭션 시작 대기 시간 초과")
	}

	blockedByHold := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		require.NoError(t, db.Raw(
			"SELECT ? = ANY(pg_blocking_pids(?))", holdPID, waiterPID).Scan(&blockedByHold).Error)
		if blockedByHold {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	release()
	holdErr := <-holdDone
	waiterErr := <-waiterDone

	require.True(t, blockedByHold,
		"locked 계정을 요구한 트랜잭션이 HoldBatch를 기다리지 않았다 — HoldBatch가 그 계정을 쥐고 있지 않다는 뜻이다")
	require.NoError(t, holdErr, "HoldBatch가 실패했다")
	require.NoError(t, waiterErr, "경쟁 트랜잭션이 실패했다")
}

// normalizedPosting은 전기 한 줄을 사용자 ID·계정 ID·분개 ID 없이 표현한다.
// 서로 다른 사용자로 돌린 두 세계를 비교하려면 그 값들을 역할로 바꿔야 한다.
type normalizedPosting struct {
	TradeIndex  int
	Role        string
	AccountType string
	Asset       string
	Amount      string
}

// normalizedPostingsOf는 주어진 체결들의 분개를 체결 순서대로 읽어 정규화한다.
// 같은 분개 안의 전기 순서는 DB가 정하므로 정렬해 비교 가능한 모양으로 만든다.
//
// 분개의 머리(사건 종류·참조)도 함께 확인한다. 전기 금액만 보면 엉뚱한 참조를 단
// 분개가 그대로 통과한다.
func normalizedPostingsOf(
	t *testing.T, db *gorm.DB, trades []*model.Trade, buyerID uint, sellerID uint, asset string,
) []normalizedPosting {
	t.Helper()

	role := func(ownerUserID *uint) string {
		switch {
		case ownerUserID == nil:
			return "system"
		case *ownerUserID == buyerID:
			return "buyer"
		case *ownerUserID == sellerID:
			return "seller"
		default:
			return fmt.Sprintf("other:%d", *ownerUserID)
		}
	}
	assetName := func(value string) string {
		if value == asset {
			return "coin"
		}
		return value
	}

	normalized := make([]normalizedPosting, 0, len(trades)*6)
	for tradeIndex, trade := range trades {
		key := "trade:" + trade.IdempotencyKey

		var persisted model.Trade
		require.NoError(t, db.Where("idempotency_key = ?", trade.IdempotencyKey).First(&persisted).Error)

		var journal model.JournalEntry
		require.NoError(t, db.Where("idempotency_key = ?", key).First(&journal).Error)
		require.Equal(t, model.JournalEventTrade, journal.EventType,
			"분개 %s의 사건 종류가 체결이 아니다", key)
		require.Equal(t, model.JournalReferenceTrade, journal.ReferenceType,
			"분개 %s의 참조 종류가 체결이 아니다", key)
		require.Equal(t, persisted.ID, journal.ReferenceID,
			"분개 %s가 가리키는 체결이 저장된 체결과 다르다", key)

		var rows []struct {
			AccountType string
			OwnerUserID *uint
			Asset       string
			Amount      decimal.Decimal
		}
		require.NoError(t, db.Raw(`
			SELECT a.account_type, a.owner_user_id, p.asset, p.amount
			FROM postings p
			JOIN accounts a ON a.id = p.account_id
			WHERE p.journal_id = ?`, journal.ID).Scan(&rows).Error)
		require.NotEmpty(t, rows, "분개 %s에 전기가 없다", key)

		group := make([]normalizedPosting, 0, len(rows))
		for _, row := range rows {
			group = append(group, normalizedPosting{
				TradeIndex:  tradeIndex,
				Role:        role(row.OwnerUserID),
				AccountType: row.AccountType,
				Asset:       assetName(row.Asset),
				Amount:      row.Amount.String(),
			})
		}
		sort.Slice(group, func(i, j int) bool {
			if group[i].Role != group[j].Role {
				return group[i].Role < group[j].Role
			}
			if group[i].AccountType != group[j].AccountType {
				return group[i].AccountType < group[j].AccountType
			}
			if group[i].Asset != group[j].Asset {
				return group[i].Asset < group[j].Asset
			}
			return group[i].Amount < group[j].Amount
		})
		normalized = append(normalized, group...)
	}
	return normalized
}

// roleBalances는 사용자의 계정별 잔액을 (계정종류, 자산) 기준으로 읽는다.
func roleBalances(t *testing.T, db *gorm.DB, userID uint, asset string) map[string]string {
	t.Helper()

	var rows []struct {
		AccountType string
		Asset       string
		Balance     decimal.Decimal
	}
	require.NoError(t, db.Raw(`
		SELECT a.account_type, a.asset, b.balance
		FROM accounts a
		JOIN account_balances b ON b.account_id = a.id
		WHERE a.owner_user_id = ?`, userID).Scan(&rows).Error)

	balances := make(map[string]string, len(rows))
	for _, row := range rows {
		name := row.Asset
		if name == asset {
			name = "coin"
		}
		balances[row.AccountType+"/"+name] = row.Balance.String()
	}
	return balances
}

// TestSettlementBatchMatchesSingleSettlements는 settlement_batch.go의 등가성
// 불변식을 원장 기준으로 고정한다: 같은 체결 묶음을 SettleTrade로 N회 처리한
// 결과와 SettleTradeBatch로 1회 처리한 결과가 같아야 한다.
//
// 자산별 합이 0인지만 보면 부족하다 — 두 경로가 서로 다른 계정에 서로 다른
// 금액을 넣고도 합만 0이면 통과하기 때문이다. 그래서 계정별 잔액, 주문 상태,
// 평균매수가, 체결 수수료, FEE_INCOME 증가분, 정규화한 전기 전체를 비교한다.
func TestSettlementBatchMatchesSingleSettlements(t *testing.T) {
	db := openServiceIntegrationDB(t)

	singleBuyer := serviceTestUserID(973)
	singleSeller := serviceTestUserID(974)
	batchBuyer := serviceTestUserID(975)
	batchSeller := serviceTestUserID(976)
	defer cleanupServiceUsers(t, db, singleBuyer, singleSeller, batchBuyer, batchSeller)

	singleAsset := fmt.Sprintf("EQS%d", time.Now().UnixNano()%1_000_000_000)
	batchAsset := fmt.Sprintf("EQB%d", time.Now().UnixNano()%1_000_000_000)

	orderRepo := repository.NewOrderRepository(db)
	ledger := NewLedgerService(db)
	devService := NewDevWalletService(db)
	settlementService := NewSettlementService(db, orderRepo)

	fund := func(userID uint, coin string, amount string) {
		t.Helper()
		_, err := devService.FundWallet(FundWalletInput{
			UserID: userID, CoinSymbol: coin, Amount: amount,
			RequestKey: fmt.Sprintf("equiv-%d-%s-%d", userID, coin, time.Now().UnixNano()),
		})
		require.NoError(t, err)
	}

	// 두 세계를 같은 모양으로 만든다. 매수 한도가는 체결가보다 높아서 잠금액이
	// 체결액보다 크고, 그래서 refund 줄까지 전기에 등장한다.
	setup := func(buyerID, sellerID uint, asset string) (*model.Order, *model.Order) {
		fund(buyerID, model.KRWAssetSymbol, "1000000")
		fund(sellerID, asset, "100")
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
	singleBuy, singleSell := setup(singleBuyer, singleSeller, singleAsset)
	batchBuy, batchSell := setup(batchBuyer, batchSeller, batchAsset)

	// 체결가가 서로 달라야 평단가와 refund가 산술로만 맞아떨어지지 않는다.
	prices := []int64{900, 1100, 1000}
	quantities := []int64{5, 7, 3}
	mkTrades := func(buy, sell *model.Order, asset string) []*model.Trade {
		trades := make([]*model.Trade, len(prices))
		for i := range prices {
			trades[i] = &model.Trade{
				CoinSymbol: asset,
				Price:      decimal.NewFromInt(prices[i]),
				Quantity:   decimal.NewFromInt(quantities[i]),
				TradedAt:   time.Now(),
				BuyOrderID: buy.ID, SellOrderID: sell.ID,
			}
		}
		return trades
	}
	singleTrades := mkTrades(singleBuy, singleSell, singleAsset)
	batchTrades := mkTrades(batchBuy, batchSell, batchAsset)

	feeIncomeStart := feeIncomeBalance(t, db, model.KRWAssetSymbol)
	for _, trade := range singleTrades {
		result, err := settlementService.SettleTrade(trade, 0)
		require.NoError(t, err)
		require.True(t, result.Applied)
	}
	feeIncomeAfterSingle := feeIncomeBalance(t, db, model.KRWAssetSymbol)

	// 이 시점에 매수자의 코인 계정과 매도자의 KRW 계정은 아직 없다(setup이 KRW/자산을
	// 교차로만 지급했다). 배치 정산이 그 신규 계정 경로를 지나면서 등가성까지
	// 지키는지 여기서 함께 증명한다 — 사전 확보(EnsureAccounts)가 잠금(LockBalances)
	// 보다 먼저 일어나야 성립한다. 순서가 뒤집히면 아직 없는 행을 잠그려다
	// "balance lock expected N rows"로 실패한다.
	batchBuyerCoinAvail, batchBuyerCoinLocked := ledgerBalances(t, db, batchBuyer, batchAsset)
	require.True(t, batchBuyerCoinAvail.IsZero() && batchBuyerCoinLocked.IsZero(),
		"매수자 코인 계정이 이미 있으면 이 테스트가 신규 계정 경로를 지나지 않는다")
	batchSellerKRWAvail, batchSellerKRWLocked := ledgerBalances(t, db, batchSeller, model.KRWAssetSymbol)
	require.True(t, batchSellerKRWAvail.IsZero() && batchSellerKRWLocked.IsZero(),
		"매도자 KRW 계정이 이미 있으면 이 테스트가 신규 계정 경로를 지나지 않는다")

	batchItems := make([]TradeBatchItem, len(batchTrades))
	for i, trade := range batchTrades {
		batchItems[i] = TradeBatchItem{Trade: trade}
	}
	results, err := settlementService.SettleTradeBatch(batchItems)
	require.NoError(t, err)
	require.Len(t, results, len(batchItems))
	for i, result := range results {
		require.True(t, result.Applied, "배치 %d번 체결이 반영되지 않았다", i)
	}
	feeIncomeAfterBatch := feeIncomeBalance(t, db, model.KRWAssetSymbol)

	// 1. FEE_INCOME 증가분이 같다.
	require.True(t,
		feeIncomeAfterSingle.Sub(feeIncomeStart).Equal(feeIncomeAfterBatch.Sub(feeIncomeAfterSingle)),
		"FEE_INCOME 증가분: 단건 %s, 배치 %s",
		feeIncomeAfterSingle.Sub(feeIncomeStart), feeIncomeAfterBatch.Sub(feeIncomeAfterSingle))

	// 2. 계정별 잔액이 역할 단위로 같다. 빈 map끼리 비교하면 무엇도 증명하지
	//    못하므로 먼저 읽힌 것이 있는지 본다.
	singleBuyerBalances := roleBalances(t, db, singleBuyer, singleAsset)
	singleSellerBalances := roleBalances(t, db, singleSeller, singleAsset)
	require.NotEmpty(t, singleBuyerBalances, "매수자 잔액을 하나도 읽지 못했다")
	require.NotEmpty(t, singleSellerBalances, "매도자 잔액을 하나도 읽지 못했다")
	require.Equal(t, singleBuyerBalances, roleBalances(t, db, batchBuyer, batchAsset),
		"매수자의 계정별 잔액이 단건과 배치에서 다르다")
	require.Equal(t, singleSellerBalances, roleBalances(t, db, batchSeller, batchAsset),
		"매도자의 계정별 잔액이 단건과 배치에서 다르다")

	// 3. 주문 체결 상태가 같다.
	orderStateOf := func(orderID uint) string {
		var order model.Order
		require.NoError(t, db.First(&order, orderID).Error)
		return fmt.Sprintf("%s|%s|%s",
			order.Status, order.FilledAmount.String(), order.FilledQuoteAmount.String())
	}
	require.Equal(t, orderStateOf(singleBuy.ID), orderStateOf(batchBuy.ID), "매수 주문 상태가 다르다")
	require.Equal(t, orderStateOf(singleSell.ID), orderStateOf(batchSell.ID), "매도 주문 상태가 다르다")

	// 4. 평균매수가가 같다.
	singleAvg := avgBuyPriceOf(t, db, singleBuyer, singleAsset)
	batchAvg := avgBuyPriceOf(t, db, batchBuyer, batchAsset)
	require.True(t, singleAvg.IsPositive(), "단건 평단가가 0이다 — 비교가 무의미하다")
	require.True(t, singleAvg.Equal(batchAvg), "평단가: 단건 %s, 배치 %s", singleAvg, batchAvg)

	// 5. 체결 수수료가 같다 — 금액뿐 아니라 수수료 자산까지 같아야 한다.
	feesOf := func(trades []*model.Trade) []string {
		fees := make([]string, len(trades))
		for i, trade := range trades {
			var persisted model.Trade
			require.NoError(t, db.Where("idempotency_key = ?", trade.IdempotencyKey).First(&persisted).Error)
			fees[i] = fmt.Sprintf("%s|%s|%s|%s|%s",
				persisted.FeeRate.String(),
				persisted.BuyerFee.String(), persisted.BuyerFeeAsset,
				persisted.SellerFee.String(), persisted.SellerFeeAsset)
		}
		return fees
	}
	require.Equal(t, feesOf(singleTrades), feesOf(batchTrades), "체결 수수료가 단건과 배치에서 다르다")

	// 6. 정규화한 전기 전체가 같다 — 어느 계정에 얼마가 갔는지까지 같아야 한다.
	require.Equal(t,
		normalizedPostingsOf(t, db, singleTrades, singleBuyer, singleSeller, singleAsset),
		normalizedPostingsOf(t, db, batchTrades, batchBuyer, batchSeller, batchAsset),
		"정규화한 전기가 단건과 배치에서 다르다")
}
