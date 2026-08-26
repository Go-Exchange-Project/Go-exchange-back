package service

import (
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 지문 비교는 레코드에 저장된 버전의 규칙으로 해야 한다. 이 서버가 모르는 버전이면
// 비교 자체가 불가능하므로 409로 단정하면 안 된다 — 정상 재시도일 수 있다.
func TestIntegrationCreateOrderReplayHonorsStoredFingerprintVersion(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(784)
	defer cleanupServiceUsers(t, db, userID)
	defer cleanupIdemKeys(t, db, userID)
	seedIdemBuyerWallet(t, db, userID, 10000)

	// 더 새 서버가 v99로 저장해 둔 레코드를 흉내 낸다.
	require.NoError(t, db.Create(&model.OrderIdempotencyKey{
		UserID: userID, IdempotencyKey: "future-version-key",
		Fingerprint: "fingerprint-computed-by-v99", FingerprintVersion: 99,
		Outcome: model.OrderIdempotencyOutcomePending,
	}).Error)

	engine := &countingAcceptanceEngine{
		fakeAcceptanceEngine: fakeAcceptanceEngine{admissible: true, submitSucceeds: true},
	}
	orderService := NewOrderService(
		repository.NewOrderRepository(db), repository.NewWalletRepository(db), engine)

	result, err := orderService.CreateOrder(idemOrderInput(userID, "future-version-key", "1"))
	require.Error(t, err)
	assert.Nil(t, result)

	kind, ok := DomainErrorKind(err)
	require.True(t, ok)
	assert.Equal(t, ErrorKindUnavailable, kind,
		"모르는 버전을 현재 버전 규칙으로 비교해 409로 단정했다 — 배포만으로 기존 키가 깨진다")

	assert.EqualValues(t, 0, countOrders(t, db, userID))
	assert.EqualValues(t, 0, engine.submits.Load())
}

// 유입 게이트가 닫혀도 이미 결정된 요청의 결과는 돌려줘야 한다. 포화 여부가 멱등성
// 계약을 덮어쓰면 ACCEPTED된 요청의 재시도가 503을, 다른 지문이 409 대신 503을 받는다.
func TestIntegrationCreateOrderReplaysWhileIntakeSaturated(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(785)
	defer cleanupServiceUsers(t, db, userID)
	defer cleanupIdemKeys(t, db, userID)
	seedIdemBuyerWallet(t, db, userID, 10000)

	engine := &countingAcceptanceEngine{
		fakeAcceptanceEngine: fakeAcceptanceEngine{admissible: true, submitSucceeds: true},
	}
	orderService := NewOrderService(
		repository.NewOrderRepository(db), repository.NewWalletRepository(db), engine)

	first, err := orderService.CreateOrder(idemOrderInput(userID, "gated-key", "1"))
	require.NoError(t, err)

	engine.admissible = false // 이제 유입이 포화다

	replay, err := orderService.CreateOrder(idemOrderInput(userID, "gated-key", "1"))
	require.NoError(t, err, "게이트가 닫혔다고 결정된 요청의 재시도가 503이 됐다")
	assert.True(t, replay.Replay)
	assert.Equal(t, first.Order.ID, replay.Order.ID)
	assert.Equal(t, model.OrderIdempotencyOutcomeAccepted, replay.Outcome)

	conflict, err := orderService.CreateOrder(idemOrderInput(userID, "gated-key", "2"))
	require.Error(t, err)
	assert.Nil(t, conflict)
	kind, ok := DomainErrorKind(err)
	require.True(t, ok)
	assert.Equal(t, ErrorKindConflict, kind, "다른 지문이 409 대신 503을 받았다")

	// 새 키는 그대로 거절된다 — 게이트의 본래 목적이다.
	fresh, err := orderService.CreateOrder(idemOrderInput(userID, "fresh-key", "1"))
	require.Error(t, err)
	assert.Nil(t, fresh)
	kind, ok = DomainErrorKind(err)
	require.True(t, ok)
	assert.Equal(t, ErrorKindUnavailable, kind)

	assert.EqualValues(t, 1, countOrders(t, db, userID))
	assert.EqualValues(t, 1, engine.submits.Load())
}

// 엔진이 없으면 주문은 영속화·홀드까지만 됐다. DB의 outcome이 PENDING인데 ACCEPTED를
// 돌려주면 응답과 저장된 상태가 어긋나고, 재시도가 200과 202를 오간다.
func TestIntegrationCreateOrderWithoutEngineReportsPending(t *testing.T) {
	db := openServiceIntegrationDB(t)
	userID := serviceTestUserID(786)
	defer cleanupServiceUsers(t, db, userID)
	defer cleanupIdemKeys(t, db, userID)
	seedIdemBuyerWallet(t, db, userID, 10000)

	orderService := NewOrderService(
		repository.NewOrderRepository(db), repository.NewWalletRepository(db), nil)

	result, err := orderService.CreateOrder(idemOrderInput(userID, "no-engine-key", "1"))
	require.NoError(t, err)
	assert.Equal(t, model.OrderIdempotencyOutcomePending, result.Outcome)

	var record model.OrderIdempotencyKey
	require.NoError(t, db.Where("user_id = ?", userID).First(&record).Error)
	assert.Equal(t, model.OrderIdempotencyOutcomePending, record.Outcome,
		"응답 outcome과 저장된 outcome이 어긋났다")
}
