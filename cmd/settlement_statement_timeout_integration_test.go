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
//
// 격리 스키마(testdb.OpenIsolatedSchemaDB)를 쓴다 — SettlementRetryWorker의
// ListOpenFailures는 전역 스캔이라(운영 동작이므로 좁히지 않는다), 공유 스키마를
// 쓰면 다른 테스트가 남긴 OPEN 실패까지 같이 집어 재정산 호출 수가 흔들린다
// (실제로 공유 스키마에서 1회 기대가 7회로 관측됐다). 격리 스키마는 이 테스트만의
// 빈 DB라 전역 스캔이어도 이 테스트가 만든 실패만 걸린다.
func TestIntegrationStatementTimeoutSettlementFailureRecoversViaRetryWorker(t *testing.T) {
	withFastTransientRetries(t)
	db := testdb.OpenIsolatedSchemaDB(t)

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

	category := service.ClassifyFailedSettlement(&failure)
	require.Equal(t, service.FailedSettlementCategoryStatementTimeout, category)
	require.True(t, service.IsTransientFailedSettlementCategory(category))
	require.Equal(t, model.FailedSettlementStatusOpen, failure.Status)

	// D9: SettlementRetryWorker가 이 기록을 선택해 재처리하고, 성공하면 resolved.
	// 격리 스키마라 ListOpenFailures가 이 테스트가 만든 실패 1건만 돌려준다 —
	// 재정산 호출 수를 정확히 1회로 단언할 수 있다.
	succeedingSettler := &fakeTradeSettler{result: service.SettlementResult{Applied: true, TradeID: 1}}
	worker := &service.SettlementRetryWorker{
		Settler:           succeedingSettler,
		FailedSettlements: failedService,
	}
	worker.RunOnce()

	require.Equal(t, 1, succeedingSettler.calls, "retry worker가 정확히 1회 재정산을 호출해야 한다")

	var resolved model.FailedSettlement
	require.NoError(t, db.First(&resolved, failure.ID).Error)
	require.Equal(t, model.FailedSettlementStatusResolved, resolved.Status)
}
