//go:build quantumharness

package matching

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 묶음 측정 계약의 focused 검증. 측정 자료가 판정에 쓰이기 전에
// 무엇이 거부되는지를 고정한다.

// 고정 UserID로 sweep당 정확히 5,000 trades, 묶음 총 작업량이 맞아야 한다.
// 대조군은 경계를 만나지 않으므로 yield가 0이어야 한다.
func TestBatchControlHasExactWorkloadAndNoYields(t *testing.T) {
	const batch = 4
	got := runSweepBatch(t, "control", 1, controlConfig(), batch)

	require.False(t, got.MeasurementInvalid, "invalid: %s", got.InvalidReason)
	require.False(t, got.Censored)
	require.Equal(t, batch*batchSweepTrades, got.ExpectedTradesTotal)
	require.Equal(t, got.ExpectedTradesTotal, got.OrderDoneTradesTotal, "OrderDone 합계")
	require.Equal(t, got.ExpectedTradesTotal, got.MeasuredTradeEmitsTotal, "EmitTrade 합계")
	require.Equal(t, "0", got.RemainingAmountTotal)
	require.Zero(t, got.QuantumYields, "대조군은 quantum 경계를 만나면 안 된다")
	require.Greater(t, got.SweepBatchTotalNs, int64(0))
	require.Greater(t, got.SweepBatchMeanNs, int64(0))
	require.Equal(t, measurementSchemaVersion, got.MeasurementSchemaVersion)
}

// 조각화가 실제로 발동하는 후보는 yield가 0이 아니어야 한다.
// 그래야 대조군/후보가 서로 다른 상태를 재고 있다는 것이 관측으로 확인된다.
func TestBatchCandidateYieldsAndKeepsWorkload(t *testing.T) {
	const batch = 4
	got := runSweepBatch(t, "candidate", 1, QuantumConfig{MaxMatchesPerTurn: 16, MaxConsecutiveCancels: 8}, batch)

	require.False(t, got.MeasurementInvalid, "invalid: %s", got.InvalidReason)
	require.Equal(t, got.ExpectedTradesTotal, got.OrderDoneTradesTotal)
	require.Equal(t, got.ExpectedTradesTotal, got.MeasuredTradeEmitsTotal)
	require.Equal(t, "0", got.RemainingAmountTotal)
	require.Greater(t, got.QuantumYields, int64(0), "조각화 후보는 양보해야 한다")
}

// 작업량 불일치·0ns·이전 schema는 measurement_invalid다.
// censored(watchdog 초과)와 합치지 않는다.
func TestValidateBatchRejectsContractViolations(t *testing.T) {
	good := batchResult{
		MeasurementSchemaVersion: measurementSchemaVersion,
		SweepBatchSize:           64,
		SweepBatchTotalNs:        1000,
		SweepBatchMeanNs:         15,
		ExpectedTradesTotal:      64 * batchSweepTrades,
		OrderDoneTradesTotal:     64 * batchSweepTrades,
		MeasuredTradeEmitsTotal:  64 * batchSweepTrades,
		RemainingAmountTotal:     "0",
	}
	invalid, reason := validateBatch(good)
	require.False(t, invalid, reason)

	cases := []struct {
		name   string
		mutate func(*batchResult)
		want   string
	}{
		{"이전 schema", func(r *batchResult) { r.MeasurementSchemaVersion = 1 }, "schema version"},
		{"schema 필드 없음", func(r *batchResult) { r.MeasurementSchemaVersion = 0 }, "schema version"},
		{"작업량 총계 불일치", func(r *batchResult) { r.ExpectedTradesTotal = 64*batchSweepTrades - 1 }, "expected_trades_total"},
		{"OrderDone 합계 불일치", func(r *batchResult) { r.OrderDoneTradesTotal-- }, "order_done_trades_total"},
		{"EmitTrade 합계 불일치", func(r *batchResult) { r.MeasuredTradeEmitsTotal-- }, "measured_trade_emits_total"},
		{"잔량 남음", func(r *batchResult) { r.RemainingAmountTotal = "1" }, "remaining_amount_total"},
		{"총 시간 0ns", func(r *batchResult) { r.SweepBatchTotalNs = 0 }, "sweep_batch_total_ns"},
		{"평균 0ns", func(r *batchResult) { r.SweepBatchMeanNs = 0 }, "sweep_batch_mean_ns"},
		{"묶음 크기 0", func(r *batchResult) { r.SweepBatchSize = 0 }, "sweep_batch_size"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := good
			tc.mutate(&r)
			invalid, reason := validateBatch(r)
			require.True(t, invalid, "거부돼야 한다")
			require.Contains(t, reason, tc.want)
		})
	}
}
