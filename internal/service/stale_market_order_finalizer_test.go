package service

import (
	"errors"
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeStaleMarketOrderSource struct {
	orders []model.Order
	err    error
}

func (f *fakeStaleMarketOrderSource) FindOpenMarketOrders() ([]model.Order, error) {
	return f.orders, f.err
}

type fakeStaleMarketCompleter struct {
	inputs []CompleteMarketOrderInput
	errs   map[uint]error
}

func (f *fakeStaleMarketCompleter) CompleteMarketOrder(input CompleteMarketOrderInput) error {
	f.inputs = append(f.inputs, input)
	return f.errs[input.OrderID]
}

type fakeStaleFailureRecorder struct {
	calls int
}

func (f *fakeStaleFailureRecorder) RecordFailure(CompleteMarketOrderInput, string, error) (*model.FailedMarketCompletion, error) {
	f.calls++
	return &model.FailedMarketCompletion{ID: 1}, nil
}

func TestStaleMarketOrderFinalizerCompletesBuyWithRemainingQuote(t *testing.T) {
	source := &fakeStaleMarketOrderSource{orders: []model.Order{{
		ID:                7,
		Side:              model.OrderSideBuy,
		OrderType:         model.OrderTypeMarket,
		CoinSymbol:        "BTC",
		QuoteAmount:       decimal.NewFromInt(100_000),
		FilledQuoteAmount: decimal.NewFromInt(40_000),
		FilledAmount:      decimal.NewFromInt(1),
	}}}
	completer := &fakeStaleMarketCompleter{}
	finalizer := &StaleMarketOrderFinalizer{Orders: source, Completer: completer, Logger: discardServiceLogger()}

	result, err := finalizer.FinalizeAll()
	require.NoError(t, err)
	assert.Equal(t, StaleMarketOrderFinalizeResult{Finalized: 1}, result)
	require.Len(t, completer.inputs, 1)
	assert.Equal(t, uint(7), completer.inputs[0].OrderID)
	assert.True(t, completer.inputs[0].FilledAmount.Equal(decimal.NewFromInt(1)))
	assert.True(t, completer.inputs[0].RemainingQuoteAmount.Equal(decimal.NewFromInt(60_000)),
		"매수 잔여 예산이 상태 결정(CANCELLED)에 쓰이도록 전달돼야 한다")
}

func TestStaleMarketOrderFinalizerSellPassesZeroRemainingQuote(t *testing.T) {
	source := &fakeStaleMarketOrderSource{orders: []model.Order{{
		ID:           8,
		Side:         model.OrderSideSell,
		OrderType:    model.OrderTypeMarket,
		CoinSymbol:   "BTC",
		Amount:       decimal.NewFromInt(5),
		FilledAmount: decimal.NewFromInt(2),
	}}}
	completer := &fakeStaleMarketCompleter{}
	finalizer := &StaleMarketOrderFinalizer{Orders: source, Completer: completer, Logger: discardServiceLogger()}

	result, err := finalizer.FinalizeAll()
	require.NoError(t, err)
	assert.Equal(t, 1, result.Finalized)
	require.Len(t, completer.inputs, 1)
	assert.True(t, completer.inputs[0].RemainingQuoteAmount.IsZero(),
		"매도 완료는 잔여를 DB에서 자체 계산하므로 0을 전달한다")
}

func TestStaleMarketOrderFinalizerRecordsCompletionFailure(t *testing.T) {
	source := &fakeStaleMarketOrderSource{orders: []model.Order{
		{ID: 9, Side: model.OrderSideBuy, OrderType: model.OrderTypeMarket, CoinSymbol: "BTC", QuoteAmount: decimal.NewFromInt(1000)},
		{ID: 10, Side: model.OrderSideBuy, OrderType: model.OrderTypeMarket, CoinSymbol: "BTC", QuoteAmount: decimal.NewFromInt(1000)},
	}}
	completer := &fakeStaleMarketCompleter{errs: map[uint]error{9: errors.New("boom")}}
	recorder := &fakeStaleFailureRecorder{}
	finalizer := &StaleMarketOrderFinalizer{Orders: source, Completer: completer, FailureRecorder: recorder, Logger: discardServiceLogger()}

	result, err := finalizer.FinalizeAll()
	require.NoError(t, err)
	assert.Equal(t, StaleMarketOrderFinalizeResult{Finalized: 1, Failed: 1}, result)
	assert.Equal(t, 1, recorder.calls, "실패는 내구 기록으로 재시도 워커에 위임돼야 한다")
}

func TestStaleMarketOrderFinalizerPropagatesQueryError(t *testing.T) {
	finalizer := &StaleMarketOrderFinalizer{
		Orders:    &fakeStaleMarketOrderSource{err: errors.New("db unavailable")},
		Completer: &fakeStaleMarketCompleter{},
		Logger:    discardServiceLogger(),
	}
	_, err := finalizer.FinalizeAll()
	require.Error(t, err)
}
