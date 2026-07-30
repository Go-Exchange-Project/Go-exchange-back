package main

import (
	"errors"
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/stretchr/testify/assert"
)

type fakeDependencyGuard struct {
	hasOpen bool
	err     error
}

func (g *fakeDependencyGuard) HasOpenFailureForOrder(uint) (bool, error) {
	return g.hasOpen, g.err
}

type fakeCancellationDeferStore struct {
	recordCalls int
	ensureCalls int
	recordErr   error
	ensureErr   error
}

func (s *fakeCancellationDeferStore) RecordFailure(matching.OrderCancelled, uint64, error) (*model.FailedOrderCancellation, error) {
	s.recordCalls++
	if s.recordErr != nil {
		return nil, s.recordErr
	}
	return &model.FailedOrderCancellation{}, nil
}

func (s *fakeCancellationDeferStore) EnsureDeferred(matching.OrderCancelled, uint64, error) (*model.FailedOrderCancellation, error) {
	s.ensureCalls++
	if s.ensureErr != nil {
		return nil, s.ensureErr
	}
	return &model.FailedOrderCancellation{}, nil
}

type fakeCancelProcessor struct {
	err   error
	calls int
}

func (p *fakeCancelProcessor) ProcessOrderCancellation(matching.OrderCancelled) error {
	p.calls++
	return p.err
}

func TestProcessOrderCancellationDefersWhenDependencyOpen(t *testing.T) {
	guard := &fakeDependencyGuard{hasOpen: true}
	store := &fakeCancellationDeferStore{}
	processor := &fakeCancelProcessor{}

	handled := processOrderCancellationEvent(
		&matching.OrderCancelled{OrderID: 42, CoinSymbol: "BTC"},
		77, // sourceOutboxID
		processor, guard, store, discardLogger(),
	)

	assert.True(t, handled, "내구 인계에 성공하면 outbox를 PROCESSED로 닫는다")
	assert.Zero(t, processor.calls, "선행 정산이 미해결이면 취소를 실행하지 않는다")
	assert.Equal(t, 1, store.ensureCalls)
	assert.Zero(t, store.recordCalls, "차단은 실행 실패가 아니다")
}

func TestProcessOrderCancellationRecordsFailureWhenExecutionFails(t *testing.T) {
	guard := &fakeDependencyGuard{}
	store := &fakeCancellationDeferStore{}
	processor := &fakeCancelProcessor{err: errors.New("boom")}

	handled := processOrderCancellationEvent(
		&matching.OrderCancelled{OrderID: 42, CoinSymbol: "BTC"},
		77, processor, guard, store, discardLogger(),
	)

	assert.True(t, handled)
	assert.Equal(t, 1, store.recordCalls, "실제 실행 실패는 RecordFailure다")
	assert.Zero(t, store.ensureCalls)
}

// 기록 자체가 실패하면(defer store가 죽어 있으면) outbox를 PENDING으로 남겨
// 다음 부팅 리플레이가 재시도하게 한다 — 조용히 유실하면 안 된다.
func TestProcessOrderCancellationLeavesPendingWhenRecordFails(t *testing.T) {
	guard := &fakeDependencyGuard{}
	store := &fakeCancellationDeferStore{recordErr: errors.New("db down")}
	processor := &fakeCancelProcessor{err: errors.New("boom")}

	handled := processOrderCancellationEvent(
		&matching.OrderCancelled{OrderID: 42, CoinSymbol: "BTC"},
		77, processor, guard, store, discardLogger(),
	)

	assert.False(t, handled, "실행도 실패하고 기록도 실패하면 outbox를 PENDING으로 남겨야 한다")
	assert.Equal(t, 1, store.recordCalls)
}

// guard 조회 자체가 실패하면(dependency를 확인할 수 없으면) fail-closed로 실행하지
// 않고 defer를 시도한다 — 차단 여부를 모르는 채로 실행하면 안 된다.
func TestProcessOrderCancellationFailsClosedOnGuardError(t *testing.T) {
	guard := &fakeDependencyGuard{err: errors.New("db down")}
	store := &fakeCancellationDeferStore{}
	processor := &fakeCancelProcessor{}

	handled := processOrderCancellationEvent(
		&matching.OrderCancelled{OrderID: 42, CoinSymbol: "BTC"},
		77, processor, guard, store, discardLogger(),
	)

	assert.True(t, handled, "defer 기록에 성공하면 outbox를 닫는다")
	assert.Zero(t, processor.calls, "dependency를 확인할 수 없으면 실행 금지")
	assert.Equal(t, 1, store.ensureCalls, "확인 불가도 차단과 동일하게 defer한다")
	assert.Zero(t, store.recordCalls)
}

// processMarketOrderDone도 취소와 동일한 계약을 따른다: OPEN dependency면 완료를
// 실행하지 않고 EnsureDeferred로 내구 defer한다.
func TestProcessMarketOrderDoneDefersWhenDependencyOpen(t *testing.T) {
	guard := &fakeDependencyGuard{hasOpen: true}
	completer := &fakeMarketCompleter{}
	recorder := &fakeCompletionFailureRecorder{}

	handled := processMarketOrderDone(testMarketOrderDone(), completer, guard, recorder, discardLogger())

	assert.True(t, handled, "내구 인계에 성공하면 outbox를 PROCESSED로 닫는다")
	assert.Zero(t, completer.calls, "선행 정산이 미해결이면 완료를 실행하지 않는다")
	assert.Equal(t, 1, recorder.ensureCalls)
	assert.Zero(t, recorder.calls, "차단은 실행 실패가 아니다")
}
