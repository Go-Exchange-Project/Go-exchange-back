package service

import (
	"errors"
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

// D13: settlement phase의 첫 57014 이후 남은 settlement·completion·cancellation
// 호출 수가 0이어야 한다 — 원래 카테고리가 DEADLOCK(transientOpenFailure)이어도
// "이번 시도"가 57014면 같은 중단이다(설계 §4.4, "저장된 카테고리가 아니라
// 이번 시도의 반환 오류"가 판정 기준).
func TestRetryWorkerStopsRunOnceOnStatementTimeoutInSettlementPhase(t *testing.T) {
	settler := &fakeRetrySettler{err: statementTimeoutPgError()}
	settlementStore := &fakeFailedSettlementStore{open: []model.FailedSettlement{transientOpenFailure(3, 1)}}
	completer := &fakeRetryCompleter{}
	completionStore := &fakeFailedCompletionStore{open: []model.FailedMarketCompletion{{ID: 5, OrderID: 100, RetryCount: 1}}}
	processor := &fakeCancelProcessor{}
	cancellationStore := &fakeFailedCancellationStore{open: []model.FailedOrderCancellation{{ID: 7, OrderID: 200, RetryCount: 1}}}

	worker := &SettlementRetryWorker{
		Settler: settler, FailedSettlements: settlementStore,
		MarketCompleter: completer, FailedCompletions: completionStore,
		CancelProcessor: processor, FailedCancellations: cancellationStore,
		Logger: discardServiceLogger(),
	}

	worker.RunOnce()

	assert.Equal(t, 1, settler.calls, "settlement 첫 시도(재시도 없이 1회)에서 멈춰야 한다")
	assert.Equal(t, 1, settlementStore.recorded, "갱신을 시도해야 한다")
	assert.Zero(t, completer.calls, "completion phase가 아예 시작되면 안 된다")
	assert.Zero(t, processor.calls, "cancellation phase가 아예 시작되면 안 된다")
}

// completion phase에서 57014가 나면 남은 completion과 cancellation을 건너뛴다.
// settlement phase 자체는 정상 진행(멈추지 않음)한다.
func TestRetryWorkerStopsRunOnceOnStatementTimeoutInCompletionPhase(t *testing.T) {
	settler := &fakeRetrySettler{}
	settlementStore := &fakeFailedSettlementStore{}
	completer := &fakeRetryCompleter{err: statementTimeoutPgError()}
	completionStore := &fakeFailedCompletionStore{open: []model.FailedMarketCompletion{{ID: 5, OrderID: 100, RetryCount: 1}}}
	processor := &fakeCancelProcessor{}
	cancellationStore := &fakeFailedCancellationStore{open: []model.FailedOrderCancellation{{ID: 7, OrderID: 200, RetryCount: 1}}}

	worker := &SettlementRetryWorker{
		Settler: settler, FailedSettlements: settlementStore,
		MarketCompleter: completer, FailedCompletions: completionStore,
		CancelProcessor: processor, FailedCancellations: cancellationStore,
		Logger: discardServiceLogger(),
	}

	worker.RunOnce()

	assert.Equal(t, 1, completer.calls)
	assert.Equal(t, 1, completionStore.recorded, "갱신을 시도해야 한다")
	assert.Zero(t, processor.calls, "cancellation phase가 건너뛰어져야 한다")
}

// cancellation phase에서 57014가 나면(마지막 phase라 건너뛸 대상이 없다) 갱신을
// 시도한 뒤 조용히 끝난다 — 패닉·추가 항목 처리 없음.
func TestRetryWorkerRecordsThenStopsOnStatementTimeoutInCancellationPhase(t *testing.T) {
	processor := &fakeCancelProcessor{err: statementTimeoutPgError()}
	cancellationStore := &fakeFailedCancellationStore{open: []model.FailedOrderCancellation{
		{ID: 7, OrderID: 200, RetryCount: 1},
		{ID: 8, OrderID: 201, RetryCount: 1},
	}}
	worker := &SettlementRetryWorker{
		CancelProcessor: processor, FailedCancellations: cancellationStore,
		FailedSettlements: &fakeFailedSettlementStore{},
		Logger:            discardServiceLogger(),
	}

	worker.RunOnce()

	assert.Equal(t, 1, processor.calls, "첫 항목에서 멈춰야 한다(두 번째 항목은 처리되지 않는다)")
	assert.Equal(t, 1, cancellationStore.recordCalls)
}

// 갱신(RecordFailure) 자체가 실패해도 같은 중단 결과다 — 기록 DB가 timeout인
// 상황에서 다음 항목을 계속 두드리면 안 된다.
func TestRetryWorkerStopsEvenWhenRecordFailureItselfFails(t *testing.T) {
	settler := &fakeRetrySettler{err: statementTimeoutPgError()}
	settlementStore := &fakeFailedSettlementStore{
		open:      []model.FailedSettlement{transientOpenFailure(3, 1), transientOpenFailure(4, 1)},
		recordErr: errors.New("record also times out"),
	}
	completer := &fakeRetryCompleter{}
	completionStore := &fakeFailedCompletionStore{open: []model.FailedMarketCompletion{{ID: 5, OrderID: 100, RetryCount: 1}}}

	worker := &SettlementRetryWorker{
		Settler: settler, FailedSettlements: settlementStore,
		MarketCompleter: completer, FailedCompletions: completionStore,
		Logger: discardServiceLogger(),
	}

	worker.RunOnce()

	assert.Equal(t, 1, settler.calls, "첫 failure에서 멈춰 두 번째(id=4)는 처리되지 않는다")
	assert.Zero(t, completer.calls, "completion phase도 건너뛰어야 한다")
}

// P1-2(설계 §4.2 계측 경로 행렬): retry worker가 57014로 RunOnce를 중단할 때도
// goexchange_db_timeout_total{sqlstate="57014",path="retry_worker"}가 늘어야
// 한다 — 세 phase(settlement·completion·cancellation) 중 어디서 멈췄든 같은
// 라벨을 쓴다(리뷰 지시: 라벨을 phase별로 더 쪼개지 않는다).
func TestDBTimeoutMetricIncrementsOnRetryWorkerStopInSettlementPhase(t *testing.T) {
	before := testutil.ToFloat64(metrics.DBTimeoutTotal.WithLabelValues("57014", "retry_worker"))

	settler := &fakeRetrySettler{err: statementTimeoutPgError()}
	settlementStore := &fakeFailedSettlementStore{open: []model.FailedSettlement{transientOpenFailure(3, 1)}}
	worker := &SettlementRetryWorker{Settler: settler, FailedSettlements: settlementStore, Logger: discardServiceLogger()}

	worker.RunOnce()

	after := testutil.ToFloat64(metrics.DBTimeoutTotal.WithLabelValues("57014", "retry_worker"))
	assert.Equal(t, before+1, after)
}

func TestDBTimeoutMetricIncrementsOnRetryWorkerStopInCompletionPhase(t *testing.T) {
	before := testutil.ToFloat64(metrics.DBTimeoutTotal.WithLabelValues("57014", "retry_worker"))

	completer := &fakeRetryCompleter{err: statementTimeoutPgError()}
	completionStore := &fakeFailedCompletionStore{open: []model.FailedMarketCompletion{{ID: 5, OrderID: 100, RetryCount: 1}}}
	worker := &SettlementRetryWorker{
		FailedSettlements: &fakeFailedSettlementStore{},
		MarketCompleter:   completer, FailedCompletions: completionStore,
		Logger: discardServiceLogger(),
	}

	worker.RunOnce()

	after := testutil.ToFloat64(metrics.DBTimeoutTotal.WithLabelValues("57014", "retry_worker"))
	assert.Equal(t, before+1, after)
}

func TestDBTimeoutMetricIncrementsOnRetryWorkerStopInCancellationPhase(t *testing.T) {
	before := testutil.ToFloat64(metrics.DBTimeoutTotal.WithLabelValues("57014", "retry_worker"))

	processor := &fakeCancelProcessor{err: statementTimeoutPgError()}
	cancellationStore := &fakeFailedCancellationStore{open: []model.FailedOrderCancellation{{ID: 7, OrderID: 200, RetryCount: 1}}}
	worker := &SettlementRetryWorker{
		FailedSettlements:   &fakeFailedSettlementStore{},
		CancelProcessor:     processor,
		FailedCancellations: cancellationStore,
		Logger:              discardServiceLogger(),
	}

	worker.RunOnce()

	after := testutil.ToFloat64(metrics.DBTimeoutTotal.WithLabelValues("57014", "retry_worker"))
	assert.Equal(t, before+1, after)
}

// 대조군: 57014가 아니면 retry_worker 라벨이 늘면 안 된다.
func TestDBTimeoutMetricDoesNotIncrementRetryWorkerOnNonStatementTimeout(t *testing.T) {
	before := testutil.ToFloat64(metrics.DBTimeoutTotal.WithLabelValues("40P01", "retry_worker"))

	settler := &fakeRetrySettler{err: errors.New("boom")}
	settlementStore := &fakeFailedSettlementStore{open: []model.FailedSettlement{transientOpenFailure(3, 1)}}
	worker := &SettlementRetryWorker{Settler: settler, FailedSettlements: settlementStore, Logger: discardServiceLogger()}

	worker.RunOnce()

	after := testutil.ToFloat64(metrics.DBTimeoutTotal.WithLabelValues("40P01", "retry_worker"))
	assert.Equal(t, before, after)
}

// 대조군: 57014가 아닌 transient 오류(기존 deadlock 등)는 다음 항목으로 계속
// 진행한다(기존 동작 그대로) — 회귀 방지.
func TestRetryWorkerContinuesOnNonStatementTimeoutTransientError(t *testing.T) {
	settler := &fakeRetrySettler{err: errors.New("boom")} // 분류상 UNKNOWN category지만 저장된 카테고리는 DEADLOCK
	settlementStore := &fakeFailedSettlementStore{open: []model.FailedSettlement{
		transientOpenFailure(3, 1),
		transientOpenFailure(4, 1),
	}}
	completer := &fakeRetryCompleter{}
	completionStore := &fakeFailedCompletionStore{open: []model.FailedMarketCompletion{{ID: 5, OrderID: 100, RetryCount: 1}}}

	worker := &SettlementRetryWorker{
		Settler: settler, FailedSettlements: settlementStore,
		MarketCompleter: completer, FailedCompletions: completionStore,
		Logger: discardServiceLogger(),
	}

	worker.RunOnce()

	assert.Equal(t, 2, settler.calls, "57014가 아니면 두 항목 모두 시도해야 한다")
	assert.Equal(t, 1, completer.calls, "completion phase가 정상 진행해야 한다")
}
