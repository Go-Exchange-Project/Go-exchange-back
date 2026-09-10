package repository

import (
	"sort"
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/stretchr/testify/require"
)

// TestEnsureAccountsCreatesInDeterministicOrder는 EnsureAccounts가 신규 계정을
// (account_type, owner_user_id, asset) 오름차순으로 INSERT하는지 고정한다.
//
// 계정 ID는 INSERT 순서대로 커지므로, "ID 오름차순"이 "(종류,소유자,자산) 오름차순"과
// 같은 배열이 되는지가 관측 가능한 대리값이다. 이 정렬이 없으면, 같은 계정 집합을
// 서로 반대 순서로 만들려는 두 동시 트랜잭션이 서로 기다리다 데드락(40P01)이 될 수
// 있다(TestIntegrationConcurrentNewAccountCreationDoesNotDeadlock이 그 경쟁을 확률적으로
// 재현한다 — 이 테스트는 그 원인이 되는 정렬 계약 자체를 결정적으로 고정한다).
//
// 세 비교축(종류·소유자·자산)을 모두 섞는다. 소유자만 바뀌는 입력이면 account_type이나
// asset 비교를 지워도 통과한다:
//   - 계정 종류 혼합: USER_AVAILABLE과 USER_LOCKED, 그리고 시스템 계정 FEE_INCOME
//     (OwnerUserID가 nil이라 ownerValue가 0으로 접힌다)
//   - 같은 소유자(userA)의 두 자산: "BTC"와 "KRW" — type·owner가 같을 때 asset
//     비교가 없으면 순서가 입력 순서로 남는다
//   - owner가 낮을수록 오히려 type이 높은 쌍(userA/USER_LOCKED vs userB/USER_AVAILABLE)
//     — type 비교가 없으면 owner만으로 정렬해 순서가 뒤집힌다
//
// 입력은 목표 순서의 완전한 역순으로 준다. account_repository.go의 sort.Slice에서
// account_type 비교를 지우면 이 테스트가 실패하는지, asset 비교를 지우면 실패하는지
// 각각 확인한 뒤 되돌렸다.
func TestEnsureAccountsCreatesInDeterministicOrder(t *testing.T) {
	db := openRepositoryIntegrationDB(t)
	repo := NewAccountRepository(db)

	base := repositoryTestUserID(9000)
	userA, userB := base+1, base+2
	defer cleanupRepositoryUsers(t, db, userA, userB)

	// 목표 순서(오름차순): FEE_INCOME/-/BTC < USER_AVAILABLE/userA/BTC <
	// USER_AVAILABLE/userA/KRW < USER_AVAILABLE/userB/BTC < USER_LOCKED/userA/BTC.
	// 입력은 이 역순으로 넣는다.
	assertEnsureAccountsOrdersById(t, repo, []AccountSpec{
		{AccountType: model.AccountUserLocked, OwnerUserID: &userA, Asset: "BTC"},
		{AccountType: model.AccountUserAvailable, OwnerUserID: &userB, Asset: "BTC"},
		{AccountType: model.AccountUserAvailable, OwnerUserID: &userA, Asset: "KRW"},
		{AccountType: model.AccountUserAvailable, OwnerUserID: &userA, Asset: "BTC"},
		{AccountType: model.AccountFeeIncome, OwnerUserID: nil, Asset: "BTC"},
	})
}

func assertEnsureAccountsOrdersById(t *testing.T, repo *AccountRepository, specs []AccountSpec) {
	t.Helper()

	accounts, err := repo.EnsureAccounts(specs)
	require.NoError(t, err)
	require.Len(t, accounts, len(specs))

	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })

	for i := 1; i < len(accounts); i++ {
		prev, cur := accounts[i-1], accounts[i]
		prevOwner, curOwner := ownerValue(prev.OwnerUserID), ownerValue(cur.OwnerUserID)

		inOrder := prev.AccountType < cur.AccountType ||
			(prev.AccountType == cur.AccountType && prevOwner < curOwner) ||
			(prev.AccountType == cur.AccountType && prevOwner == curOwner && prev.Asset < cur.Asset)
		require.True(t, inOrder,
			"ID 오름차순(계정 %d → %d)이 (종류,소유자,자산) 오름차순과 어긋난다: %+v 다음에 %+v",
			prev.ID, cur.ID, prev, cur)
	}
}
