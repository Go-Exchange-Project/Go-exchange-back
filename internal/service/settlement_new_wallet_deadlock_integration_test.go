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
// 정산한다. 두 트랜잭션 모두 정확히 같은 신규 지갑 키 집합(A:BTC, B:KRW)을
// missingCreatable로 생성해야 하는데, 그 목록이 map 순회 순서(무작위)를 그대로
// 쓰면 두 트랜잭션의 배치 INSERT 행 순서가 반대로 나올 확률이 있고, 그러면
// PostgreSQL이 튜플 락 순환으로 데드락(40P01)을 감지한다. 재현이 확률적이라
// 라운드를 충분히 돌려 안정적으로 잡히게 한다 — 로컬에서 30라운드면 매번 최소
// 1건 이상 재현됨을 확인했다(정렬 수정 전 기준).
//
// 기존 TestIntegrationConcurrentReversedSettlementsDoNotDeadlock과 달리 이 테스트는
// 매 라운드 신규 유저 + 미리 4개가 아닌 2개 지갑만 심어, CreateZeroBalanceWallets
// (missingCreatable) 경로를 반드시 타게 만든다 — 그쪽은 지갑이 이미 다 있어 이
// 버그를 재현하지 못한다.
func TestIntegrationConcurrentNewWalletCreationDoesNotDeadlock(t *testing.T) {
	db := openServiceIntegrationDB(t)
	settlementService := NewSettlementService(db, repository.NewOrderRepository(db), repository.NewWalletRepository(db))

	const rounds = 30
	testRunID := time.Now().UnixNano()
	var userIDs []uint
	t.Cleanup(func() { cleanupServiceUsers(t, db, userIDs...) })
	errs := make(chan error, rounds*2)

	for round := 0; round < rounds; round++ {
		userA := serviceTestUserID(uint(1000 + round*2))
		userB := serviceTestUserID(uint(1000 + round*2 + 1))
		userIDs = append(userIDs, userA, userB)

		seedNewWalletDeadlockFunding(t, db, userA, userB)
		buy1, sell1 := seedDeadlockOrderPair(t, db, userA, userB)
		buy2, sell2 := seedDeadlockOrderPair(t, db, userA, userB)

		var wg sync.WaitGroup
		wg.Add(2)
		go func(round int) {
			defer wg.Done()
			items := []TradeBatchItem{{Trade: deadlockTestTrade(buy1.ID, sell1.ID, testRunID, round, "newwallet-t1")}}
			_, err := settlementService.SettleTradeBatch(items)
			errs <- err
		}(round)
		go func(round int) {
			defer wg.Done()
			items := []TradeBatchItem{{Trade: deadlockTestTrade(buy2.ID, sell2.ID, testRunID, round, "newwallet-t2")}}
			_, err := settlementService.SettleTradeBatch(items)
			errs <- err
		}(round)
		wg.Wait()
	}
	close(errs)

	for err := range errs {
		require.NoError(t, err, "동시 신규 지갑 생성이 데드락 없이 끝나야 한다")
	}
}

func seedNewWalletDeadlockFunding(t *testing.T, db *gorm.DB, userA uint, userB uint) {
	t.Helper()

	lockedKRW := decimal.NewFromInt(1_000_000)
	lockedBTC := decimal.NewFromInt(1_000)
	// A는 KRW만, B는 BTC만 미리 보유 — 매 라운드 반대편 자산 지갑이 반드시
	// 새로 생성돼야 한다(crossing-flood.js 픽스처와 동일한 자금 구성).
	wallets := []model.Wallet{
		{UserID: userA, CoinSymbol: model.KRWAssetSymbol, KRW: lockedKRW, AvailableBalance: decimal.Zero, LockedBalance: lockedKRW},
		{UserID: userB, CoinSymbol: "BTC", Quantity: lockedBTC, AvailableBalance: decimal.Zero, LockedBalance: lockedBTC},
	}
	require.NoError(t, db.Create(&wallets).Error)
}
