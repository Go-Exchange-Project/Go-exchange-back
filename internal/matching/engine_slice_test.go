package matching

import (
	"fmt"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

type comparableTrade struct {
	Price       string
	Quantity    string
	BuyOrderID  uint
	SellOrderID uint
}

// 동적 필드(trade ID, 시각, 시퀀스, engine ID)는 실행마다 달라지므로 비교에서
// 뺀다. 구조체 리터럴로 비교해 새 필드가 추가되면 컴파일이 깨지게 한다.
func collectTrades(events chan ExecutionEvent) []comparableTrade {
	close(events)
	var out []comparableTrade
	for e := range events {
		if e.Trade == nil {
			continue
		}
		out = append(out, comparableTrade{
			Price:       e.Trade.Price.String(),
			Quantity:    e.Trade.Quantity.String(),
			BuyOrderID:  e.Trade.BuyOrderID,
			SellOrderID: e.Trade.SellOrderID,
		})
	}
	return out
}

func runToCompletion(budget, makers int) []comparableTrade {
	me := NewMatchingEngine()
	events := make(chan ExecutionEvent, 8192)
	me.ExecutionCh = events
	book := me.GetOrderBook("BTC")
	for i := 0; i < makers; i++ {
		book.AddOrder(testOrder(uint(i+1), "BTC", model.OrderSideSell, int64(50000+i), 1))
	}
	taker := testOrder(9999, "BTC", model.OrderSideBuy, int64(50000+makers-1), int64(makers))
	taker.OrderType = model.OrderTypeLimit
	for {
		if _, done := me.matchSlice(book, taker, budget); done {
			me.finishOrder(book, taker)
			break
		}
	}
	return collectTrades(events)
}

// 조각화가 체결열을 바꾸지 않는다. budget=1(최대 조각화)과 budget=0(무제한),
// 그리고 예산 경계를 직접 비교한다. 하드코딩된 기대값만 보면 "조각화해도
// 같다"를 증명하지 못한다.
func TestSlicingPreservesTradeSequence(t *testing.T) {
	for _, makers := range []int{1, 2, 3, 17, 64} {
		t.Run(fmt.Sprintf("makers-%d", makers), func(t *testing.T) {
			unlimited := runToCompletion(0, makers)
			require.Len(t, unlimited, makers)
			require.Equal(t, unlimited, runToCompletion(1, makers), "budget=1")
			for _, b := range []int{makers - 1, makers, makers + 1} {
				if b < 1 {
					continue
				}
				require.Equal(t, unlimited, runToCompletion(b, makers), "budget=%d", b)
			}
		})
	}
}

// 예산 경계에서 정확히 전량 체결되면 같은 조각에서 done=true여야 한다.
// 예산 검사가 루프 조건보다 앞에 있으면 빈 조각이 하나 더 생긴다.
func TestSliceBudgetBoundaryDoneInSameSlice(t *testing.T) {
	me := NewMatchingEngine()
	me.ExecutionCh = make(chan ExecutionEvent, 16)
	book := me.GetOrderBook("BTC")
	book.AddOrder(testOrder(1, "BTC", model.OrderSideSell, 50000, 5))

	taker := testOrder(2, "BTC", model.OrderSideBuy, 50000, 5)
	taker.OrderType = model.OrderTypeLimit
	trades, done := me.matchSlice(book, taker, 1)
	require.Equal(t, 1, trades)
	require.True(t, done, "전량 체결됐으므로 같은 조각에서 done=true")
}

// 예산 소진은 완료가 아니다.
func TestSliceBudgetExhaustionIsNotDone(t *testing.T) {
	me := NewMatchingEngine()
	me.ExecutionCh = make(chan ExecutionEvent, 16)
	book := me.GetOrderBook("BTC")
	book.AddOrder(testOrder(1, "BTC", model.OrderSideSell, 50000, 2))
	book.AddOrder(testOrder(2, "BTC", model.OrderSideSell, 50001, 3))

	taker := testOrder(3, "BTC", model.OrderSideBuy, 50001, 5)
	taker.OrderType = model.OrderTypeLimit

	trades, done := me.matchSlice(book, taker, 1)
	require.Equal(t, 1, trades)
	require.False(t, done, "예산 소진이지 완료가 아니다")

	trades, done = me.matchSlice(book, taker, 1)
	require.Equal(t, 1, trades)
	require.True(t, done)
}

// 시장가는 마지막 조각에서만 MarketOrderDone을 정확히 1회 낸다.
func TestMarketOrderDoneExactlyOnceAcrossSlices(t *testing.T) {
	me := NewMatchingEngine()
	events := make(chan ExecutionEvent, 100)
	me.ExecutionCh = events
	book := me.GetOrderBook("BTC")
	for i := 0; i < 4; i++ {
		book.AddOrder(testOrder(uint(i+1), "BTC", model.OrderSideSell, 50000, 1))
	}
	taker := &Order{
		ID: 99, CoinSymbol: "BTC", Side: model.OrderSideBuy,
		QuoteAmount: decimal.NewFromInt(200000), OrderType: model.OrderTypeMarket,
	}
	for {
		if _, done := me.matchSlice(book, taker, 1); done {
			me.finishOrder(book, taker)
			break
		}
	}
	close(events)
	doneCount, tradeCount := 0, 0
	for e := range events {
		if e.MarketOrderDone != nil {
			doneCount++
		}
		if e.Trade != nil {
			tradeCount++
		}
	}
	require.Equal(t, 1, doneCount, "MarketOrderDone은 정확히 1회")
	require.Equal(t, 4, tradeCount)
}

// 즉시 완료 경로는 슬롯을 만들지 않고, book을 바꾸지 않고, terminal event를
// 내지 않고, dirty도 찍지 않는다.
func TestImmediateCompletionPathsDoNotMutate(t *testing.T) {
	cases := []struct {
		name  string
		order *Order
	}{
		{"limit-zero-amount", &Order{
			ID: 1, CoinSymbol: "BTC", Side: model.OrderSideBuy,
			Amount: decimal.Zero, Price: decimal.NewFromInt(50000),
			OrderType: model.OrderTypeLimit,
		}},
		{"market-sell-zero-amount", &Order{
			ID: 2, CoinSymbol: "BTC", Side: model.OrderSideSell,
			Amount: decimal.Zero, OrderType: model.OrderTypeMarket,
		}},
		{"unknown-side", &Order{
			ID: 3, CoinSymbol: "BTC", Side: model.OrderSide("SIDEWAYS"),
			Amount: decimal.NewFromInt(1), Price: decimal.NewFromInt(50000),
			OrderType: model.OrderTypeLimit,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			me := NewMatchingEngine()
			events := make(chan ExecutionEvent, 10)
			me.ExecutionCh = events
			observed := 0
			me.MatchLatencyObserver = func(time.Duration) { observed++ }
			tc.order.EnqueuedAt = time.Now()

			// 엔진을 Start하지 않으므로 book 직접 조회가 안전하다.
			require.Nil(t, me.admitOrder(tc.order), "슬롯을 만들지 않는다")
			require.Equal(t, 1, observed, "observer는 정확히 1회")
			require.Empty(t, events, "terminal event 없음")
			require.Empty(t, me.dirtySymbols, "dirty 없음")
			snapshot := me.GetOrderBookSnapshot("BTC")
			require.Empty(t, snapshot.Bids)
			require.Empty(t, snapshot.Asks)
		})
	}
}

func TestNilOrderDoesNotObserve(t *testing.T) {
	me := NewMatchingEngine()
	observed := 0
	me.MatchLatencyObserver = func(time.Duration) { observed++ }
	require.Nil(t, me.admitOrder(nil))
	require.Equal(t, 0, observed, "nil 주문은 observer를 부르지 않는다")
}
