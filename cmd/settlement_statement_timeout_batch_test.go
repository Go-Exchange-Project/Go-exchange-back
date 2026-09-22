package main

import (
	"errors"
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubHandoffRecorder는 설계 §4.3 원자적 배치 인계(RecordBatchFailuresAndMarkOutboxProcessed)의
// 결과(commit/rollback)를 흉내 낸다.
type stubHandoffRecorder struct {
	err   error
	calls int
	items []service.TradeOutboxFailureItem
}

func (s *stubHandoffRecorder) RecordBatchFailuresAndMarkOutboxProcessed(items []service.TradeOutboxFailureItem, settlementErr error) error {
	s.calls++
	s.items = items
	return s.err
}

type stubCountingSettler struct{ calls int }

func (s *stubCountingSettler) SettleTrade(*model.Trade, uint64) (service.SettlementResult, error) {
	s.calls++
	return service.SettlementResult{Applied: true}, nil
}

type stubCountingFailureRecorder struct{ calls int }

func (s *stubCountingFailureRecorder) RecordFailure(*model.Trade, error) (*model.FailedSettlement, error) {
	s.calls++
	return &model.FailedSettlement{}, nil
}

var errHandoffFailed = errors.New("handoff failed")

// D10(정산): 배치가 57014로 실패하면 단건 폴백을 타지 않고(settler·failureRecorder
// 미호출) 원자적 인계로 넘어간다. 인계가 commit되면 undurable이 비어야 한다.
func TestSettleTradeBatchWithFallbackHandsOffStatementTimeoutBatchAtomically(t *testing.T) {
	batch := []service.OutboxEvent{
		tradeOutboxEventForOrders(1, 10, 20),
		tradeOutboxEventForOrders(2, 30, 40),
	}
	settler := &stubCountingSettler{}
	recorder := &stubCountingFailureRecorder{}
	handoff := &stubHandoffRecorder{}

	before := testutil.ToFloat64(metrics.SettlementBatchFallbacksTotal)

	undurable := settleTradeBatchWithFallback(
		batch,
		stubBatchSettler{err: statementTimeoutError()},
		settler,
		recorder,
		handoff,
		nil, nil, nil, nil, nil,
		func(string, []byte) {},
		stubOutboxMarker{},
		discardLogger(),
	)

	assert.Empty(t, undurable, "인계가 commit되면 undurable이 없어야 한다")
	assert.Equal(t, 1, handoff.calls, "원자적 인계가 정확히 1번 호출돼야 한다")
	assert.Len(t, handoff.items, 2)
	assert.Zero(t, settler.calls, "단건 폴백(settler.SettleTrade)을 타면 안 된다")
	assert.Zero(t, recorder.calls, "단건 실패 기록(RecordFailure) 경로를 타면 안 된다")

	after := testutil.ToFloat64(metrics.SettlementBatchFallbacksTotal)
	assert.Equal(t, before, after, "57014 인계는 기존 배치 폴백 카운터를 늘리면 안 된다")
}

// D10: 인계 자체(원자적 배치 upsert+마킹)가 실패하면(rollback) 배치 전체의
// maker·taker 주문 ID가 undurable로 돌아온다 — outbox는 PENDING 그대로라
// 다음 부팅 replay가 소유한다.
func TestSettleTradeBatchWithFallbackReportsWholeBatchUndurableWhenHandoffFails(t *testing.T) {
	batch := []service.OutboxEvent{
		tradeOutboxEventForOrders(1, 10, 20),
		tradeOutboxEventForOrders(2, 30, 40),
	}
	handoff := &stubHandoffRecorder{err: errHandoffFailed}

	undurable := settleTradeBatchWithFallback(
		batch,
		stubBatchSettler{err: statementTimeoutError()},
		&stubCountingSettler{},
		&stubCountingFailureRecorder{},
		handoff,
		nil, nil, nil, nil, nil,
		func(string, []byte) {},
		stubOutboxMarker{},
		discardLogger(),
	)

	assert.ElementsMatch(t, []uint{10, 20, 30, 40}, undurable)
}

// 대조군: 55P03 등 다른 배치 오류는 기존 단건 폴백을 그대로 탄다(handoff 미호출).
func TestSettleTradeBatchWithFallbackStillFallsBackOnNonStatementTimeoutError(t *testing.T) {
	batch := []service.OutboxEvent{tradeOutboxEventForOrders(1, 10, 20)}
	handoff := &stubHandoffRecorder{}
	settler := &stubCountingSettler{}

	settleTradeBatchWithFallback(
		batch,
		stubBatchSettler{err: deadlockError()},
		settler,
		&stubCountingFailureRecorder{},
		handoff,
		nil, nil, nil, nil, nil,
		func(string, []byte) {},
		stubOutboxMarker{},
		discardLogger(),
	)

	assert.Zero(t, handoff.calls, "57014가 아니면 원자적 인계를 타면 안 된다")
	assert.Equal(t, 1, settler.calls, "기존처럼 단건 폴백(settler.SettleTrade)을 타야 한다")
}

// P1-2(설계 §4.2 계측 경로 행렬): 배치 정산이 57014로 실패해 원자적 인계로
// 넘어가는 지점에서도 goexchange_db_timeout_total{sqlstate="57014",path="settlement_batch"}가
// 늘어야 한다 — 기존 recordDBTimeoutMetric은 단건 재시도 세 곳(settlement·
// market_completion·cancellation)뿐이라 이 배치 경로는 계측되지 않았다.
func TestDBTimeoutMetricIncrementsOnSettlementBatchHandoff(t *testing.T) {
	batch := []service.OutboxEvent{tradeOutboxEventForOrders(1, 10, 20)}
	handoff := &stubHandoffRecorder{}

	before := counterVecValue(t, metrics.DBTimeoutTotal, "57014", "settlement_batch")

	settleTradeBatchWithFallback(
		batch,
		stubBatchSettler{err: statementTimeoutError()},
		&stubCountingSettler{},
		&stubCountingFailureRecorder{},
		handoff,
		nil, nil, nil, nil, nil,
		func(string, []byte) {},
		stubOutboxMarker{},
		discardLogger(),
	)

	after := counterVecValue(t, metrics.DBTimeoutTotal, "57014", "settlement_batch")
	assert.Equal(t, before+1, after, "배치 정산이 57014로 실패하면 settlement_batch 라벨이 늘어야 한다")
}

// P1(2차 리뷰): 55P03도 settlement_batch 라벨을 늘려야 한다 — 배치가 55P03으로
// 실패해도(§3.3의 배포 전 게이트가 세는 대상) 단건 폴백 자체는 그대로 유지된다.
// 계측만 추가하고 제어 흐름(핸드오프로 안 빠짐)은 바꾸지 않는다.
func TestDBTimeoutMetricIncrementsOnSettlementBatchLockTimeout(t *testing.T) {
	batch := []service.OutboxEvent{tradeOutboxEventForOrders(1, 10, 20)}
	settler := &stubCountingSettler{}

	before := counterVecValue(t, metrics.DBTimeoutTotal, "55P03", "settlement_batch")

	undurable := settleTradeBatchWithFallback(
		batch,
		stubBatchSettler{err: lockTimeoutError()},
		settler,
		&stubCountingFailureRecorder{},
		&stubHandoffRecorder{},
		nil, nil, nil, nil, nil,
		func(string, []byte) {},
		stubOutboxMarker{},
		discardLogger(),
	)

	after := counterVecValue(t, metrics.DBTimeoutTotal, "55P03", "settlement_batch")
	assert.Equal(t, before+1, after, "55P03도 settlement_batch 라벨이 늘어야 한다")
	assert.Equal(t, 1, settler.calls, "55P03은 기존처럼 단건 폴백을 타야 한다(제어 흐름 불변)")
	assert.Empty(t, undurable, "단건 폴백이 성공했으므로 undurable이 없어야 한다")
}

// 대조군: 57014가 아닌 배치 오류(단건 폴백 경로)는 settlement_batch 라벨을 늘리면 안 된다.
func TestDBTimeoutMetricDoesNotIncrementSettlementBatchOnNonStatementTimeout(t *testing.T) {
	batch := []service.OutboxEvent{tradeOutboxEventForOrders(1, 10, 20)}

	before := counterVecValue(t, metrics.DBTimeoutTotal, "40P01", "settlement_batch")

	settleTradeBatchWithFallback(
		batch,
		stubBatchSettler{err: deadlockError()},
		&stubCountingSettler{},
		&stubCountingFailureRecorder{},
		&stubHandoffRecorder{},
		nil, nil, nil, nil, nil,
		func(string, []byte) {},
		stubOutboxMarker{},
		discardLogger(),
	)

	after := counterVecValue(t, metrics.DBTimeoutTotal, "40P01", "settlement_batch")
	assert.Equal(t, before, after)
}

// D14①: 배치 인계가 rollback되면(원자적 인계 자체 실패) 배치 전체가 dispatcher의
// 기존 quarantine 경로를 타 terminal이 dispatch되지 않는다 — outbox는 PENDING
// 그대로 남는다(TestDispatcherSkipsTerminalForQuarantinedOrder와 같은 메커니즘을
// 실제 settleTradeBatchWithFallback 호출로 잇는다).
func TestDispatcherSkipsTerminalWhenStatementTimeoutHandoffRollsBack(t *testing.T) {
	queue := make(chan service.OutboxEvent, 4)
	jobs := make(chan settlementJob, 4)

	queue <- tradeOutboxEventForOrders(1, 10, 20)
	queue <- cancelOutboxEvent(2, 10)
	close(queue)

	handoff := &stubHandoffRecorder{err: errHandoffFailed}

	done := make(chan struct{})
	go func() {
		runPartitionDispatcher("0", queue, jobs, 4, 32, func(string, []byte) {})
		close(done)
	}()

	first := <-jobs
	require.Equal(t, jobKindTrade, first.kind)
	undurable := settleTradeBatchWithFallback(
		first.batch,
		stubBatchSettler{err: statementTimeoutError()},
		&stubCountingSettler{},
		&stubCountingFailureRecorder{},
		handoff,
		nil, nil, nil, nil, nil,
		func(string, []byte) {},
		stubOutboxMarker{},
		discardLogger(),
	)
	first.done <- settlementResult{kind: jobKindTrade, id: first.id, seq: first.seq, undurableOrderIDs: undurable}

	select {
	case job := <-jobs:
		t.Fatalf("quarantine된 주문의 terminal이 dispatch되면 안 된다: %+v", job)
	case <-done:
		// 정상 종료 — terminal은 실행되지 않고 outbox는 PENDING으로 남는다.
	}
	assert.Equal(t, 1, handoff.calls)
}
