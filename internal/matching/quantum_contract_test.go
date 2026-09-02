package matching

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// 조각화의 **결정적 계수 계약**. 시계에 의존하지 않는다.
//
// 로컬 wall-clock C3(처리량 보존)는 판정 불가로 종료됐다(설계 §8.4).
// 여기서 세는 조각 수와 yield 수는 **처리량을 증명하지 않는다.** sweep 하나를
// 몇 조각으로 나눴고 스케줄러 제어점으로 몇 번 더 돌아왔는지만 뜻한다.
// 실제 처리량은 최종 통합 GCP 실행에서 확인한다.

// 측정 워크로드와 겹치지 않는 고정 UserID. 자기 주문 제외(isSelfTrade)를 0으로
// 만들어 작업량을 정확히 고정한다 — 무작위 UserID는 회차마다 1~6건을 건너뛰었다.
const (
	contractMakerUser  uint = 700001
	contractTakerUser  uint = 700002
	contractVictimUser uint = 700003
)

func contractOrder(id, user uint, side model.OrderSide, price, amount int64) *Order {
	return &Order{
		ID:         id,
		UserID:     user,
		CoinSymbol: "BTC",
		Side:       side,
		Price:      decimal.NewFromInt(price),
		Amount:     decimal.NewFromInt(amount),
		OrderType:  model.OrderTypeLimit,
		CreatedAt:  time.Now(),
		EnqueuedAt: time.Now(),
	}
}

func TestExpectedSlicesAndYields(t *testing.T) {
	cases := []struct {
		trades, budget, slices, yields int
	}{
		{5000, 16, 313, 312},  // 격자 m16
		{5000, 64, 79, 78},    // 격자 m64
		{5000, 128, 40, 39},   // 격자 m128 — 선택값
		{5000, 100, 50, 49},   // 나누어떨어지는 경우
		{5000, 5000, 1, 0},    // 예산이 정확히 작업량
		{5000, 6000, 1, 0},    // 예산이 더 큼
		{5000, 0, 1, 0},       // 무제한 sentinel
		{5000, -1, 1, 0},      // 방어
		{0, 128, 1, 0},        // 체결 없음도 조각 1개
		{1, 1, 1, 0},          // 최소
		{2, 1, 2, 1},          //
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("trades=%d,budget=%d", tc.trades, tc.budget), func(t *testing.T) {
			require.Equal(t, tc.slices, ExpectedSlices(tc.trades, tc.budget))
			require.Equal(t, tc.yields, ExpectedYields(tc.trades, tc.budget))
		})
	}
}

// 선택값 m128-c8에서 5,000 체결 sweep의 실제 yield가 계산값과 정확히 같아야 한다.
// 작업량도 함께 고정한다 — 체결 수가 흔들리면 yield 수도 흔들려 계약이 무의미해진다.
func TestSelectedConfigYieldsMatchExpectation(t *testing.T) {
	const trades = 5000
	cfg := QuantumConfig{
		MaxMatchesPerTurn:     defaultMaxMatchesPerTurn,
		MaxConsecutiveCancels: defaultMaxConsecutiveCancels,
	}
	require.Equal(t, 128, cfg.MaxMatchesPerTurn, "선택값이 바뀌면 이 테스트의 기대값도 바뀐다")
	require.Equal(t, 8, cfg.MaxConsecutiveCancels)

	me := newTestEngine()
	me.maxMatchesPerTurn = cfg.MaxMatchesPerTurn
	me.maxConsecutiveCancels = cfg.MaxConsecutiveCancels

	var yields, emits atomic.Int64
	doneTrades := make(chan int, 1<<16)
	admitted := make(chan struct{}, 1<<16)
	me.Observers = EngineObservers{
		Yield:         func() { yields.Add(1) },
		OrderAdmitted: func(time.Duration) { admitted <- struct{}{} },
		OrderDone:     func(n int) { doneTrades <- n },
		EmitBlock: func(k EmitKind, _ time.Duration) {
			if k == EmitTrade {
				emits.Add(1)
			}
		},
	}
	drainAll(me)
	me.Start()

	for i := 0; i < trades; i++ {
		me.OrderCh <- contractOrder(uint(i+1), contractMakerUser, model.OrderSideSell, 50000, 1)
	}
	for i := 0; i < trades; i++ {
		<-admitted
		<-doneTrades
	}
	yields.Store(0)
	emits.Store(0)

	taker := contractOrder(uint(trades+1), contractTakerUser, model.OrderSideBuy, 50000, trades)
	me.OrderCh <- taker
	<-admitted
	takerTrades := <-doneTrades

	me.Stop()
	waitEngineDone(t, me)

	require.Equal(t, trades, takerTrades, "sweep이 정확히 5,000건을 체결해야 한다")
	require.Equal(t, int64(trades), emits.Load(), "EmitTrade 수")
	require.True(t, taker.Amount.IsZero(), "잔량 0, got %s", taker.Amount)
	require.Equal(t, int64(ExpectedYields(trades, cfg.MaxMatchesPerTurn)), yields.Load(),
		"관측 yield가 계산값과 달라졌다 — 조각화 계약이 깨졌다")
	require.Equal(t, int64(39), yields.Load(), "m128에서 5,000 체결은 39회 양보")
}

// 취소 상한의 결정적 계약. 선택값 c=8에서 progress 사이 연속 취소가 8건을
// 넘지 않는다. 32 설정도 함께 확인해 상한이 실제로 값에 따라 움직이는지 본다.
func TestCancelBoundIsDeterministicPerConfig(t *testing.T) {
	for _, limit := range []int{8, 32} {
		t.Run(fmt.Sprintf("maxConsecutiveCancels=%d", limit), func(t *testing.T) {
			me := newTestEngine()
			me.maxConsecutiveCancels = limit
			rec := newSchedRecorder()
			rec.install(me)
			drainAll(me)

			for i := 0; i < limit*10; i++ {
				me.CancelCh <- CancelOrderCommand{
					CoinSymbol: "BTC", OrderID: uint(9000 + i),
					Side: model.OrderSideSell, Price: decimal.NewFromInt(70000),
					EnqueuedAt: time.Now(),
				}
			}
			const orders = 10
			for i := 0; i < orders; i++ {
				o := contractOrder(uint(i+1), contractMakerUser, model.OrderSideSell, int64(50000+i), 1)
				me.OrderCh <- o
			}
			me.Start()
			waitN(t, rec.admit, orders, "주문 admit")

			seq := rec.seq()
			lastOrder := -1
			firstOrder := -1
			for i, e := range seq {
				if e == "order" {
					if firstOrder < 0 {
						firstOrder = i
					}
					lastOrder = i
				}
			}
			require.GreaterOrEqual(t, firstOrder, 0)
			require.LessOrEqual(t, firstOrder, limit,
				"첫 주문이 취소 %d건 뒤에야 admit됐다", firstOrder)

			run := 0
			for i, e := range seq[:lastOrder+1] {
				if e == "cancel" {
					run++
					require.LessOrEqual(t, run, limit,
						"인덱스 %d: progress 사이 연속 취소 %d건이 상한 %d을 넘었다", i, run, limit)
					continue
				}
				run = 0 // "order"(P-b) 또는 "slice"(P-a) — 둘 다 progress다
			}

			me.Stop()
			waitEngineDone(t, me)
		})
	}
}
