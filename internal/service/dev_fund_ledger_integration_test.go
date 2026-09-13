package service

import (
	"fmt"
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// 개발용 지급은 유지하되 잔액을 직접 늘리는 방식은 없앤다. DEV_MINT에서
// 사용자 계정으로 옮기는 정상 분개를 통과해야 한다.
//
// 이 테스트가 고정하는 것은 원장의 계약이 아니라 **dev funding이 원장을
// 거치는가**다. 원장 자체의 계약은 T1~T3·T9가 이미 본다.
func TestDevFundCreatesMintToAvailableJournal(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(910)
	defer cleanupServiceUsers(t, db, userID)

	devService := NewDevWalletService(db)
	requestKey := fmt.Sprintf("devfund-%d", userID)

	balance, err := devService.FundWallet(FundWalletInput{
		UserID:     userID,
		CoinSymbol: model.KRWAssetSymbol,
		Amount:     "1000",
		RequestKey: requestKey,
	})
	require.NoError(t, err)
	require.True(t, balance.Available.Equal(decimal.NewFromInt(1000)), "사용 가능 잔액이 %s다", balance.Available)
	require.True(t, balance.Locked.IsZero())

	// 전기 두 줄이 자산별로 상쇄된다. DEV_MINT가 음수가 되는 것이 정상이다 —
	// "개발용 공급에서 나갔다"는 뜻이다.
	var journal model.JournalEntry
	require.NoError(t, db.
		Where("idempotency_key = ?", fmt.Sprintf("devfund:%d:KRW:%s", userID, requestKey)).
		First(&journal).Error)
	require.Equal(t, model.JournalEventDevFund, journal.EventType)

	var postings []model.Posting
	require.NoError(t, db.Where("journal_id = ?", journal.ID).Find(&postings).Error)
	require.Len(t, postings, 2, "전기가 두 줄이 아니다 — 상대 계정이 빠졌다")

	sum := decimal.Zero
	for _, posting := range postings {
		sum = sum.Add(posting.Amount)
	}
	require.True(t, sum.IsZero(), "전기 합이 %s다", sum)
}

// 같은 요청 번호로 재시도하면 한 번만 지급한다.
//
// 이것은 T2(원장 계층 멱등)와 다른 것을 본다. T2는 Record가 멱등한지 보고,
// 여기서는 dev funding이 요청 키를 실제로 분개 멱등성 키로 넘기는지 본다.
// 넘기지 않으면 매번 새 키가 되어 원장의 멱등이 무의미해진다.
func TestDevFundSameRequestKeyPaysOnce(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(911)
	defer cleanupServiceUsers(t, db, userID)

	devService := NewDevWalletService(db)
	input := FundWalletInput{
		UserID:     userID,
		CoinSymbol: model.KRWAssetSymbol,
		Amount:     "1000",
		RequestKey: fmt.Sprintf("devfund-once-%d", userID),
	}

	first, err := devService.FundWallet(input)
	require.NoError(t, err)
	require.True(t, first.Available.Equal(decimal.NewFromInt(1000)))

	second, err := devService.FundWallet(input)
	require.NoError(t, err, "같은 요청 재시도가 오류가 됐다")
	require.True(t, second.Available.Equal(decimal.NewFromInt(1000)), "두 번 지급됐다: %s", second.Available)

	t.Run("요청 키가 없으면 거부된다", func(t *testing.T) {
		// 키를 서버가 자동 생성하면 재시도가 항상 새 키가 되어 멱등이 무의미해진다.
		noKey := input
		noKey.RequestKey = ""
		_, err := devService.FundWallet(noKey)
		require.Error(t, err)
	})
}
