package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/testdb"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// D8·D9(실제 DB): 즉시 재시도가 전부 57014로 실패하면 failed_settlements에
// STATEMENT_TIMEOUT으로 내구 기록되고(D8), SettlementRetryWorker가 그 기록을
// 선택해 재처리해 성공하면 resolved된다(D9).
func TestIntegrationStatementTimeoutSettlementFailureRecoversViaRetryWorker(t *testing.T) {
	withFastTransientRetries(t)
	db := testdb.OpenIntegrationDB(t)

	failedRepo := repository.NewFailedSettlementRepository(db)
	failedService := service.NewFailedSettlementService(failedRepo)

	trade := &model.Trade{
		EngineSequence: time.Now().UnixNano(),
		EngineEventID:  fmt.Sprintf("engine-statement-timeout-%d", time.Now().UnixNano()),
		CoinSymbol:     "BTC",
		Price:          decimal.NewFromInt(90),
		Quantity:       decimal.NewFromInt(5),
		BuyOrderID:     900001,
		SellOrderID:    900002,
	}

	// D8: 즉시 재시도가 전부 57014로 실패한다.
	failingSettler := &fakeTradeSettler{err: statementTimeoutError()}
	handled, markedInTx := processTradeSettlement(trade, 0, failingSettler, failedService, func(string, []byte) {}, discardLogger())
	require.True(t, handled, "실패 기록까지는 내구적으로 확정돼야 한다")
	require.False(t, markedInTx)
	require.Equal(t, 1+len(transientRetryDelays), failingSettler.calls, "즉시 재시도가 실제로 수행됐다")

	var failure model.FailedSettlement
	require.NoError(t, db.Where("trade_idempotency_key = ?", trade.IdempotencyKey).First(&failure).Error)
	t.Cleanup(func() {
		require.NoError(t, db.Unscoped().Where("id = ?", failure.ID).Delete(&model.FailedSettlement{}).Error)
	})

	category := service.ClassifyFailedSettlement(&failure)
	require.Equal(t, service.FailedSettlementCategoryStatementTimeout, category)
	require.True(t, service.IsTransientFailedSettlementCategory(category))
	require.Equal(t, model.FailedSettlementStatusOpen, failure.Status)

	// D9: SettlementRetryWorker가 이 기록을 선택해 재처리하고, 성공하면 resolved.
	// ListOpenFailures는 공유 테스트 DB의 다른 OPEN 실패도 함께 돌려줄 수 있으므로
	// (전역 스캔이라 이 테스트의 실패로 범위를 좁히지 못한다) 호출 횟수는 최소
	// 1회 이상으로만 확인하고, 우리 실패가 실제로 resolved됐는지를 본다.
	succeedingSettler := &fakeTradeSettler{result: service.SettlementResult{Applied: true, TradeID: 1}}
	worker := &service.SettlementRetryWorker{
		Settler:           succeedingSettler,
		FailedSettlements: failedService,
	}
	worker.RunOnce()

	require.GreaterOrEqual(t, succeedingSettler.calls, 1, "retry worker가 실제로 재정산을 호출해야 한다")

	var resolved model.FailedSettlement
	require.NoError(t, db.First(&resolved, failure.ID).Error)
	require.Equal(t, model.FailedSettlementStatusResolved, resolved.Status)
}
