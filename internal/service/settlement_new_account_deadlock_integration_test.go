package service

import (
	"sync"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 두 유저(A: KRW만 보유, B: 코인만 보유)가 매 라운드 신규로 등장해, 두 독립된
// 주문쌍(같은 A-매수/B-매도 역할)을 서로 다른 SettleTradeBatch 트랜잭션에서 동시
// 정산한다. 두 트랜잭션 모두 정확히 같은 신규 계정 집합(A의 BTC 계정, B의 KRW 계정)을
// EnsureAccounts로 만들어야 하는데, 그 목록이 입력 순서를 그대로 쓰면 두 트랜잭션의
// 배치 INSERT 행 순서가 반대로 나올 확률이 있고, 그러면 PostgreSQL이 튜플 락 순환으로
// 데드락(40P01)을 감지한다. 재현이 확률적이라 라운드를 충분히 돌려 안정적으로 잡히게
// 한다 — 로컬에서 30라운드면 매번 최소 1건 이상 재현됨을 확인했다(정렬 수정 전 기준).
//
// EnsureAccounts의 계정 생성은 INSERT 한 문장이라 실행 도중에 장벽을 끼울 지점이 없어
// 이 경쟁을 결정적으로 재현하는 단위 테스트를 따로 만들 수 없다 — 대신 정렬 계약 자체는
// TestEnsureAccountsCreatesInDeterministicOrder(internal/repository)가 결정적으로 고정한다.
// 이 테스트는 그 계약이 실제로 동시 실행에서 데드락을 막는지를 확률적으로 재확인한다.
//
// 기존 TestIntegrationConcurrentReversedSettlementsDoNotDeadlock과 달리 이 테스트는
// 매 라운드 신규 유저 + 자산 하나씩만 지급해, EnsureAccounts의 신규 계정 생성 경로를
// 반드시 타게 만든다 — 그쪽은 계정이 이미 다 있어 이 버그를 재현하지 못한다.
func TestIntegrationConcurrentNewAccountCreationDoesNotDeadlock(t *testing.T) {
	db := openServiceIntegrationDB(t)
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db))

	const rounds = 30
	testRunID := time.Now().UnixNano()
	var userIDs []uint
	t.Cleanup(func() { cleanupServiceUsers(t, db, userIDs...) })
	errs := make(chan error, rounds*2)

	for round := 0; round < rounds; round++ {
		userA := serviceTestUserID(uint(1000 + round*2))
		userB := serviceTestUserID(uint(1000 + round*2 + 1))
		userIDs = append(userIDs, userA, userB)

		buy1, sell1 := seedDeadlockOrderPair(t, db, userA, userB)
		buy2, sell2 := seedDeadlockOrderPair(t, db, userA, userB)
		seedNewAccountDeadlockFunding(t, db, userA, userB, buy1.ID, sell1.ID)

		var wg sync.WaitGroup
		wg.Add(2)
		go func(round int) {
			defer wg.Done()
			items := []TradeBatchItem{{Trade: deadlockTestTrade(buy1.ID, sell1.ID, testRunID, round, "newaccount-t1")}}
			_, err := settlementService.SettleTradeBatch(items)
			errs <- err
		}(round)
		go func(round int) {
			defer wg.Done()
			items := []TradeBatchItem{{Trade: deadlockTestTrade(buy2.ID, sell2.ID, testRunID, round, "newaccount-t2")}}
			_, err := settlementService.SettleTradeBatch(items)
			errs <- err
		}(round)
		wg.Wait()
	}
	close(errs)

	for err := range errs {
		require.NoError(t, err, "동시 신규 계정 생성이 데드락 없이 끝나야 한다")
	}
}

// seedNewAccountDeadlockFunding은 A에게 KRW만, B에게 BTC만 원장으로 지급하고
// 잠근다. 반대편 자산(A의 BTC, B의 KRW)은 지급하지 않는다 — 정산이 그 계정을
// 매 라운드 새로 만들어야 이 테스트가 재현하려는 경쟁이 성립한다. 잠긴 잔액은
// (user, asset) 단위 계정 하나로 buy1·buy2(또는 sell1·sell2) 양쪽 주문을 함께
// 감당한다 — orderID는 멱등성 키 참조일 뿐, 특정 주문에 귀속되지 않는다.
func seedNewAccountDeadlockFunding(t *testing.T, db *gorm.DB, userA uint, userB uint, orderRefA uint, orderRefB uint) {
	t.Helper()

	lockedKRW := decimal.NewFromInt(1_000_000)
	lockedBTC := decimal.NewFromInt(1_000)
	seedLockedBalance(t, db, userA, model.KRWAssetSymbol, lockedKRW, orderRefA)
	seedLockedBalance(t, db, userB, "BTC", lockedBTC, orderRefB)
}
