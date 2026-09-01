package matching

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
)

var (
	ErrCancelOrderNotFound          = errors.New("matching order not found")
	ErrCancelOrderInvalidCommand    = errors.New("invalid matching cancel command")
	ErrCancelOrderEngineUnavailable = errors.New("matching engine is unavailable")
	ErrCancelOrderTimedOut          = errors.New("matching cancel timed out")
	ErrSnapshotEngineUnavailable    = errors.New("matching engine is unavailable")
	ErrSnapshotTimedOut             = errors.New("matching snapshot request timed out")
)

// orderIntakeHighWatermarkRatio: OrderCh가 이 비율 이상 차면 입장 게이트가 거절한다.
// (④가 히스테리시스·env로 정교화)
const orderIntakeHighWatermarkRatio = 0.9

// 0.75 is an operational starting point, not a correctness boundary.
// It reserves 256 slots at the default capacity to absorb execution
// events already being produced by the current order and cancellations.
const engineEmitHighWatermarkRatio = 0.75

func (me *MatchingEngine) emitBackpressured() bool {
	if me == nil || me.ExecutionCh == nil || cap(me.ExecutionCh) == 0 {
		return false
	}
	threshold := int(float64(cap(me.ExecutionCh)) * engineEmitHighWatermarkRatio)
	return len(me.ExecutionCh) >= threshold
}

// Engine은 매칭 엔진의 소비자 표면이다. 구현: MatchingEngine(단일), ShardedEngine(B-3).
type Engine interface {
	SubmitOrder(*Order)                                     // 블로킹 — 부트스트랩/리플레이 전용
	TrySubmitOrder(order *Order, within time.Duration) bool // 바운디드 — 라이브 HTTP 경로
	IsIntakeAdmissible(coinSymbol string) bool              // 유입 게이트(DB 작업 전)
	CancelOrder(CancelOrderCommand) CancelOrderResult
	RequestOrderBookSnapshot(coinSymbol string, depth int) (OrderBookSnapshot, error)
}

type MatchingEngine struct {
	OrderBook   *OrderBook
	OrderBooks  map[string]*OrderBook
	OrderCh     chan *Order
	CancelCh    chan CancelOrderCommand
	TradeCh     chan *model.Trade
	ExecutionCh chan ExecutionEvent
	SnapshotCh  chan OrderBookSnapshot
	engineID    string
	tradeSeq    int64

	MatchLatencyObserver func(time.Duration)

	// Observers는 quantum 계측용 콜백 묶음이다. 제로값이면 전부 비활성이다.
	// Start() 전에 설정하고 실행 중 교체하지 않는다 — 재대입은 data race다.
	Observers EngineObservers

	// lastMatchTrades·sliceEmitBlock은 한 조각의 누적값이다. 엔진
	// goroutine에서만 쓴다. processOrder가 매 주문 시작 시 0으로 되돌린다 —
	// Match 안에서 초기화하면 Match를 직접 부르는 테스트 30여 곳의 누적값이
	// 섞인다.
	lastMatchTrades int
	sliceEmitBlock  time.Duration

	// quantum 값. 0은 matchSlice의 무제한 sentinel과 충돌하므로 항상 1
	// 이상이어야 한다. 여기 기본값은 개발·테스트용이며, production 값은
	// 로컬 탐색 결과로 확정한다.
	maxMatchesPerTurn     int
	maxConsecutiveCancels int

	// snapshotCache는 심볼별 최신 스냅샷(*OrderBookSnapshot)을 담는다. 엔진 goroutine이
	// 코얼레싱 티커에서 Store하고, REST 핸들러가 락 없이 Load한다.
	snapshotCache    sync.Map
	dirtySymbols     map[string]bool // 엔진 goroutine 로컬 — 락 불필요
	snapshotInterval time.Duration

	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
}

const DefaultSnapshotDepth = 30

// defaultSnapshotInterval은 스냅샷 코얼레싱 주기입니다. 이 주기 동안 한 심볼에
// 주문이 아무리 많이 와도 스냅샷은 1회만 생성·브로드캐스트됩니다.
const defaultSnapshotInterval = 100 * time.Millisecond

var engineInstanceCounter uint64

type CancelOrderCommand struct {
	// CommandID는 이 취소를 내구 기록한 cancel_commands 행이다. 엔진은 값을
	// 해석하지 않고 OrderCancelled에 그대로 실어 보내기만 한다 — outbox가 execution
	// 행 INSERT와 이 command의 PROCESSED를 한 트랜잭션에 묶을 때 필요하다.
	// 0이면 command 없이 들어온 취소이며 outbox는 완료 대상에서 제외한다.
	CommandID  uint64
	CoinSymbol string
	OrderID    uint
	Side       model.OrderSide
	Price      decimal.Decimal
	// EnqueuedAt은 CancelOrder가 채운다. 제로값이면 큐 대기 관측을 건너뛴다 —
	// 테스트가 직접 구성한 command가 가짜 지연을 만들지 않게 하기 위해서다.
	EnqueuedAt time.Time
	ResponseCh chan CancelOrderResult
}

type CancelOrderResult struct {
	Removed bool
	Err     error
}

type ExecutionEvent struct {
	Trade           *model.Trade
	MarketOrderDone *MarketOrderDone
	OrderCancelled  *OrderCancelled
}

type MarketOrderDone struct {
	OrderID              uint
	CoinSymbol           string
	Side                 model.OrderSide
	FilledAmount         decimal.Decimal
	FilledQuoteAmount    decimal.Decimal
	RemainingAmount      decimal.Decimal
	RemainingQuoteAmount decimal.Decimal
}

// OrderCancelled는 취소로 오더북에서 실제 제거된 주문의 실행 이벤트 페이로드다.
// ProcessOrderCancellation이 OrderID로 DB에서 주문을 재조회하므로 식별자만 담는다.
type OrderCancelled struct {
	CommandID     uint64
	OrderID       uint
	CoinSymbol    string
	Side          model.OrderSide
	EngineEventID string
}

type OrderBookSnapshot struct {
	CoinSymbol string           `json:"coin_symbol,omitempty"`
	Asks       []PriceLevelData `json:"asks"`
	Bids       []PriceLevelData `json:"bids"`
}

type PriceLevelData struct {
	Price    decimal.Decimal `json:"price"`
	Quantity decimal.Decimal `json:"quantity"`
}

func NewMatchingEngine() *MatchingEngine {
	defaultBook := NewOrderBook()
	return &MatchingEngine{
		OrderBook:        defaultBook,
		OrderBooks:       make(map[string]*OrderBook),
		OrderCh:          make(chan *Order, 1024),
		CancelCh:         make(chan CancelOrderCommand, 1024),
		TradeCh:          make(chan *model.Trade, 1024),
		ExecutionCh:      make(chan ExecutionEvent, 1024),
		SnapshotCh:       make(chan OrderBookSnapshot, 256),
		engineID:         newEngineID(),
		dirtySymbols:     make(map[string]bool),
		snapshotInterval: defaultSnapshotInterval,

		maxMatchesPerTurn:     defaultMaxMatchesPerTurn,
		maxConsecutiveCancels: defaultMaxConsecutiveCancels,
		stopCh:           make(chan struct{}),
		doneCh:           make(chan struct{}),
	}
}

func (me *MatchingEngine) Start() {
	go func() {
		defer close(me.doneCh)
		ticker := time.NewTicker(me.interval())
		defer ticker.Stop()
		for {
			// 취소 우선: 대기 중 취소를 먼저 논블로킹으로 전부 드레인.
			select {
			case cmd := <-me.CancelCh:
				me.processCancel(cmd)
				continue
			default:
			}
			// 취소가 없을 때만 주문/ticker/stop.
			orderCh := me.OrderCh
			if me.emitBackpressured() {
				orderCh = nil // 하류 포화 — 신규 주문 억제, 취소 emit 헤드룸 확보
			}
			select {
			case cmd := <-me.CancelCh:
				me.processCancel(cmd)
			case order := <-orderCh:
				me.processOrder(order)
			case <-ticker.C:
				// 코얼레싱: dirty 심볼의 스냅샷만 생성해 캐시 저장 + 논블로킹 브로드캐스트.
				me.flushSnapshots()
			case <-me.stopCh:
				// graceful shutdown: 이미 접수된 주문/취소를 모두 처리한 뒤 마지막
				// 스냅샷을 flush하고 종료한다. 이 루프가 ExecutionCh와 SnapshotCh의
				// 유일한 writer이므로, 드레인 완료 후의 close가 downstream(outbox
				// writer, 스냅샷 소비자) 종료를 도미노로 전파한다.
				me.drainPendingWork()
				me.flushSnapshots()
				if me.ExecutionCh != nil {
					close(me.ExecutionCh)
				}
				close(me.SnapshotCh)
				return
			}
		}
	}()
}

// processOrder는 매칭 후 심볼을 dirty로 표시만 한다. 스냅샷 생성·브로드캐스트는
// 코얼레싱 티커가 담당하므로, 매칭 루프가 스냅샷 비용이나 소비자 속도에 결박되지 않는다.
func (me *MatchingEngine) processOrder(order *Order) {
	if order == nil {
		return
	}
	if !order.EnqueuedAt.IsZero() {
		me.Observers.orderAdmitted(time.Since(order.EnqueuedAt))
	}
	// 조각화 이전에는 "조각 = 주문 전체"다. 조각화 후와 같은 지표를 채우기
	// 위해 Slice/OrderDone을 여기서 1회 낸다. 이 둘이 비면
	// matches_per_slice와 emit_block_per_slice의 baseline이 없어 전후
	// 비교가 불가능하다.
	me.lastMatchTrades = 0
	me.sliceEmitBlock = 0
	admissible := me.orderIsAdmissible(order)
	me.Match(order)
	if me.MatchLatencyObserver != nil && !order.EnqueuedAt.IsZero() {
		me.MatchLatencyObserver(time.Since(order.EnqueuedAt))
	}
	// 즉시 완료 경로는 매칭을 수행하지 않으므로 셀 체결이 없다. 여기서
	// OrderDone(0)/Slice(0)를 내면 executions_per_order 분포가 0으로
	// 오염되고, "OrderAdmitted만 발생" 계약과 어긋난다.
	//
	// Slice는 Match(= 잔량 등록·MarketOrderDone 포함) 뒤에 부른다. 마지막
	// emit의 블로킹 시간이 sliceEmitBlock에 들어간 뒤여야 한다 — 순서가
	// 뒤집히면 하류 포화 시 가장 오래 막히는 지점이 측정에서 빠진다.
	if admissible {
		me.Observers.slice(me.lastMatchTrades, me.sliceEmitBlock)
		me.Observers.orderDone(me.lastMatchTrades)
	}
	me.markDirty(order.CoinSymbol)
}

// orderIsAdmissible은 Match가 실제 매칭을 수행할 주문인지를 Match와 같은
// 기준으로 판단한다.
func (me *MatchingEngine) orderIsAdmissible(order *Order) bool {
	switch order.Side {
	case model.OrderSideBuy:
		return order.OrderType == model.OrderTypeMarket || order.Amount.GreaterThan(decimal.Zero)
	case model.OrderSideSell:
		return order.Amount.GreaterThan(decimal.Zero)
	}
	return false
}

func (me *MatchingEngine) processCancel(cmd CancelOrderCommand) {
	if !cmd.EnqueuedAt.IsZero() {
		me.Observers.cancel(time.Since(cmd.EnqueuedAt))
	}
	result := me.handleCancel(cmd)
	if cmd.ResponseCh != nil {
		cmd.ResponseCh <- result
	}
	if result.Removed {
		me.markDirty(cmd.CoinSymbol)
		me.emitOrderCancelled(cmd)
	}
}

// markDirty는 다음 티커에 스냅샷을 다시 만들어야 할 심볼을 기록한다.
// 엔진 goroutine에서만 호출되므로 락이 필요 없다.
func (me *MatchingEngine) markDirty(coinSymbol string) {
	if me.dirtySymbols == nil {
		me.dirtySymbols = make(map[string]bool)
	}
	me.dirtySymbols[coinSymbol] = true
}

// flushSnapshots는 dirty 심볼의 최신 스냅샷을 생성해 캐시에 저장하고, 소비자에게
// 논블로킹으로 전송한다. SnapshotCh가 가득 차면 건너뛴다 — 어차피 다음 티커에
// 더 최신 스냅샷을 다시 보내므로 오래된 스냅샷을 기다릴 이유가 없다.
func (me *MatchingEngine) flushSnapshots() {
	for coinSymbol := range me.dirtySymbols {
		snapshot := me.GetOrderBookSnapshot(coinSymbol)
		me.storeSnapshot(coinSymbol, snapshot)
		select {
		case me.SnapshotCh <- snapshot:
		default:
		}
		delete(me.dirtySymbols, coinSymbol)
	}
}

func (me *MatchingEngine) storeSnapshot(coinSymbol string, snapshot OrderBookSnapshot) {
	cached := snapshot
	me.snapshotCache.Store(coinSymbol, &cached)
}

func (me *MatchingEngine) interval() time.Duration {
	if me.snapshotInterval > 0 {
		return me.snapshotInterval
	}
	return defaultSnapshotInterval
}

func (me *MatchingEngine) drainPendingWork() {
	for {
		select {
		case order := <-me.OrderCh:
			me.processOrder(order)
		case cmd := <-me.CancelCh:
			me.processCancel(cmd)
		default:
			return
		}
	}
}

// Stop은 엔진 루프에 종료를 지시합니다. 루프는 접수된 주문/취소를 드레인한 뒤
// ExecutionCh·SnapshotCh를 닫고 Done()을 통해 완료를 알립니다.
// HTTP 서버가 먼저 닫혀 새 주문 유입이 멈춘 뒤에 호출해야 합니다.
func (me *MatchingEngine) Stop() {
	me.stopOnce.Do(func() {
		if me.stopCh != nil {
			close(me.stopCh)
		}
	})
}

// Done은 엔진 루프가 드레인을 마치고 종료됐을 때 닫히는 채널을 반환합니다.
func (me *MatchingEngine) Done() <-chan struct{} {
	return me.doneCh
}

// RequestOrderBookSnapshot은 캐시에서 최신 스냅샷을 락 없이 읽어 반환합니다.
// 엔진 루프에 요청을 보내지 않으므로 조회가 매칭과 경쟁하지 않습니다. 캐시는
// DefaultSnapshotDepth로 생성되므로 요청 depth는 그 값으로 상한 클램프한 뒤
// 캐시본을 잘라 반환합니다. 아직 스냅샷이 없는 심볼은 빈 오더북을 반환합니다
// (신규 심볼은 실제로 비어 있고, 첫 티커 후 채워집니다).
func (me *MatchingEngine) RequestOrderBookSnapshot(coinSymbol string, depth int) (OrderBookSnapshot, error) {
	if me == nil {
		return OrderBookSnapshot{}, ErrSnapshotEngineUnavailable
	}
	depth = normalizeSnapshotDepth(depth)
	if depth > DefaultSnapshotDepth {
		depth = DefaultSnapshotDepth
	}
	cached := me.loadSnapshot(coinSymbol)
	return truncateSnapshot(cached, depth), nil
}

func (me *MatchingEngine) loadSnapshot(coinSymbol string) OrderBookSnapshot {
	if value, ok := me.snapshotCache.Load(coinSymbol); ok {
		if snapshot, ok := value.(*OrderBookSnapshot); ok && snapshot != nil {
			return *snapshot
		}
	}
	return OrderBookSnapshot{CoinSymbol: coinSymbol}
}

// truncateSnapshot은 스냅샷을 요청 depth로 자릅니다. 캐시본을 변형하지 않도록
// 슬라이스를 복사합니다(캐시된 스냅샷은 여러 조회가 공유하는 불변 데이터).
func truncateSnapshot(snapshot OrderBookSnapshot, depth int) OrderBookSnapshot {
	result := OrderBookSnapshot{CoinSymbol: snapshot.CoinSymbol}
	if len(snapshot.Asks) > 0 {
		n := min(depth, len(snapshot.Asks))
		result.Asks = append([]PriceLevelData(nil), snapshot.Asks[:n]...)
	}
	if len(snapshot.Bids) > 0 {
		n := min(depth, len(snapshot.Bids))
		result.Bids = append([]PriceLevelData(nil), snapshot.Bids[:n]...)
	}
	return result
}

// SubmitOrder는 주문을 엔진 루프에 넘긴다(기존 OrderCh 직접 송신과 동일 의미).
func (me *MatchingEngine) SubmitOrder(order *Order) { me.OrderCh <- order }

// TrySubmitOrder는 within 시간 안에 OrderCh에 넣지 못하면 false를 반환한다(무한
// 블로킹 없음). false일 때 주문은 채널에 들어가지 않았음이 select로 보장된다.
func (me *MatchingEngine) TrySubmitOrder(order *Order, within time.Duration) bool {
	timer := time.NewTimer(within)
	defer timer.Stop()
	select {
	case me.OrderCh <- order:
		return true
	case <-timer.C:
		return false
	}
}

// IsIntakeAdmissible는 OrderCh 점유가 high-watermark 미만이면 true. 단일 엔진이라
// coinSymbol은 무시한다(인터페이스 통일을 위해 받음 — ShardedEngine이 사용).
func (me *MatchingEngine) IsIntakeAdmissible(coinSymbol string) bool {
	return len(me.OrderCh) < int(float64(cap(me.OrderCh))*orderIntakeHighWatermarkRatio) &&
		!me.emitBackpressured()
}

func (me *MatchingEngine) CancelOrder(cmd CancelOrderCommand) CancelOrderResult {
	if me == nil || me.CancelCh == nil {
		return CancelOrderResult{Err: ErrCancelOrderEngineUnavailable}
	}
	if cmd.EnqueuedAt.IsZero() {
		cmd.EnqueuedAt = time.Now()
	}
	if cmd.ResponseCh == nil {
		cmd.ResponseCh = make(chan CancelOrderResult, 1)
	}

	select {
	case me.CancelCh <- cmd:
	case <-time.After(time.Second):
		return CancelOrderResult{Err: ErrCancelOrderTimedOut}
	}

	select {
	case result := <-cmd.ResponseCh:
		return result
	case <-time.After(time.Second):
		return CancelOrderResult{Err: ErrCancelOrderTimedOut}
	}
}

func (me *MatchingEngine) handleCancel(cmd CancelOrderCommand) CancelOrderResult {
	if cmd.OrderID == 0 || cmd.CoinSymbol == "" || !cmd.Price.GreaterThanOrEqual(decimal.Zero) {
		return CancelOrderResult{Err: ErrCancelOrderInvalidCommand}
	}
	if cmd.Side != model.OrderSideBuy && cmd.Side != model.OrderSideSell {
		return CancelOrderResult{Err: ErrCancelOrderInvalidCommand}
	}

	book := me.GetOrderBook(cmd.CoinSymbol)
	removed := book.RemoveOrder(&Order{
		ID:         cmd.OrderID,
		CoinSymbol: cmd.CoinSymbol,
		Side:       cmd.Side,
		Price:      cmd.Price,
	})
	if !removed {
		return CancelOrderResult{Err: ErrCancelOrderNotFound}
	}
	return CancelOrderResult{Removed: true}
}

func (me *MatchingEngine) GetOrderBook(coinSymbol string) *OrderBook {
	if coinSymbol == "" {
		return me.OrderBook
	}

	if me.OrderBooks == nil {
		me.OrderBooks = make(map[string]*OrderBook)
	}

	book, ok := me.OrderBooks[coinSymbol]
	if ok {
		return book
	}

	if len(me.OrderBooks) == 0 && me.OrderBook != nil {
		me.OrderBooks[coinSymbol] = me.OrderBook
		return me.OrderBook
	}

	book = NewOrderBook()
	me.OrderBooks[coinSymbol] = book
	return book
}

func (me *MatchingEngine) Match(order *Order) {
	if order == nil {
		return
	}

	book := me.GetOrderBook(order.CoinSymbol)

	switch order.Side {
	case model.OrderSideBuy:
		if order.OrderType == model.OrderTypeMarket {
			me.matchMarketBuy(book, order)
		} else {
			if !order.Amount.GreaterThan(decimal.Zero) {
				return
			}
			me.matchBuy(book, order)
		}
	case model.OrderSideSell:
		if !order.Amount.GreaterThan(decimal.Zero) {
			return
		}
		if order.OrderType == model.OrderTypeMarket {
			me.matchMarketSell(book, order)
		} else {
			me.matchSell(book, order)
		}
	default:
		return
	}

	if order.OrderType == model.OrderTypeMarket {
		me.emitMarketOrderDone(order)
		return
	}

	if order.Amount.GreaterThan(decimal.Zero) {
		book.AddOrder(order)
	}
}

func (me *MatchingEngine) matchBuy(book *OrderBook, order *Order) {
	for order.Amount.GreaterThan(decimal.Zero) {
		sellLevel, orderIndex, ok := bestMatchableSellOrder(book, order)
		if !ok {
			return
		}

		sellOrder := sellLevel.Orders.At(orderIndex)
		tradeQty := decimal.Min(order.Amount, sellOrder.Amount)
		if !tradeQty.GreaterThan(decimal.Zero) {
			return
		}

		order.Amount = order.Amount.Sub(tradeQty)
		order.FilledAmount = order.FilledAmount.Add(tradeQty)
		sellOrder.Amount = sellOrder.Amount.Sub(tradeQty)
		sellOrder.FilledAmount = sellOrder.FilledAmount.Add(tradeQty)

		if !sellOrder.Amount.GreaterThan(decimal.Zero) {
			sellLevel.Orders.Remove(orderIndex)
		}
		if sellLevel.Orders.Len() == 0 {
			book.SellOrders.Delete(sellLevel)
		}

		me.emitTrade(me.newTrade(order.CoinSymbol, sellLevel.Price, tradeQty, order.ID, sellOrder.ID))
	}
}

func (me *MatchingEngine) matchSell(book *OrderBook, order *Order) {
	for order.Amount.GreaterThan(decimal.Zero) {
		buyLevel, orderIndex, ok := bestMatchableBuyOrder(book, order)
		if !ok {
			return
		}

		buyOrder := buyLevel.Orders.At(orderIndex)
		tradeQty := decimal.Min(order.Amount, buyOrder.Amount)
		if !tradeQty.GreaterThan(decimal.Zero) {
			return
		}

		order.Amount = order.Amount.Sub(tradeQty)
		order.FilledAmount = order.FilledAmount.Add(tradeQty)
		buyOrder.Amount = buyOrder.Amount.Sub(tradeQty)
		buyOrder.FilledAmount = buyOrder.FilledAmount.Add(tradeQty)

		if !buyOrder.Amount.GreaterThan(decimal.Zero) {
			buyLevel.Orders.Remove(orderIndex)
		}
		if buyLevel.Orders.Len() == 0 {
			book.BuyOrders.Delete(buyLevel)
		}

		me.emitTrade(me.newTrade(order.CoinSymbol, buyLevel.Price, tradeQty, buyOrder.ID, order.ID))
	}
}

func (me *MatchingEngine) matchMarketBuy(book *OrderBook, order *Order) {
	for order.QuoteAmount.GreaterThan(decimal.Zero) {
		sellLevel, orderIndex, ok := bestMarketSellOrder(book, order)
		if !ok {
			return
		}

		sellOrder := sellLevel.Orders.At(orderIndex)
		// Div는 DivisionPrecision(16자리)에서 반올림하므로 클램프 체결 시
		// price * maxQtyByQuote가 잔여 예산을 초과할 수 있다(관측: +1.8775e-9류
		// 잔차). QuoRem은 같은 정밀도에서 몫을 내림(0 방향 절삭)하므로
		// price * maxQtyByQuote <= order.QuoteAmount가 항상 보장된다.
		maxQtyByQuote, _ := order.QuoteAmount.QuoRem(sellLevel.Price, int32(decimal.DivisionPrecision))
		tradeQty := decimal.Min(maxQtyByQuote, sellOrder.Amount)
		if !tradeQty.GreaterThan(decimal.Zero) {
			return
		}
		executionQuote := sellLevel.Price.Mul(tradeQty)

		order.QuoteAmount = order.QuoteAmount.Sub(executionQuote)
		order.FilledQuoteAmount = order.FilledQuoteAmount.Add(executionQuote)
		order.FilledAmount = order.FilledAmount.Add(tradeQty)
		sellOrder.Amount = sellOrder.Amount.Sub(tradeQty)
		sellOrder.FilledAmount = sellOrder.FilledAmount.Add(tradeQty)

		if !sellOrder.Amount.GreaterThan(decimal.Zero) {
			sellLevel.Orders.Remove(orderIndex)
		}
		if sellLevel.Orders.Len() == 0 {
			book.SellOrders.Delete(sellLevel)
		}

		me.emitTrade(me.newTrade(order.CoinSymbol, sellLevel.Price, tradeQty, order.ID, sellOrder.ID))
	}
}

func (me *MatchingEngine) matchMarketSell(book *OrderBook, order *Order) {
	for order.Amount.GreaterThan(decimal.Zero) {
		buyLevel, orderIndex, ok := bestMarketBuyOrder(book, order)
		if !ok {
			return
		}

		buyOrder := buyLevel.Orders.At(orderIndex)
		tradeQty := decimal.Min(order.Amount, buyOrder.Amount)
		if !tradeQty.GreaterThan(decimal.Zero) {
			return
		}
		executionQuote := buyLevel.Price.Mul(tradeQty)

		order.Amount = order.Amount.Sub(tradeQty)
		order.FilledAmount = order.FilledAmount.Add(tradeQty)
		order.FilledQuoteAmount = order.FilledQuoteAmount.Add(executionQuote)
		buyOrder.Amount = buyOrder.Amount.Sub(tradeQty)
		buyOrder.FilledAmount = buyOrder.FilledAmount.Add(tradeQty)

		if !buyOrder.Amount.GreaterThan(decimal.Zero) {
			buyLevel.Orders.Remove(orderIndex)
		}
		if buyLevel.Orders.Len() == 0 {
			book.BuyOrders.Delete(buyLevel)
		}

		me.emitTrade(me.newTrade(order.CoinSymbol, buyLevel.Price, tradeQty, buyOrder.ID, order.ID))
	}
}

func (me *MatchingEngine) emitTrade(trade *model.Trade) {
	select {
	case me.TradeCh <- trade:
	default:
	}
	if me.ExecutionCh != nil {
		me.sendExecution(EmitTrade, ExecutionEvent{Trade: trade})
	}
	me.lastMatchTrades++
}

// sendExecution은 ExecutionCh로의 블로킹 send 시간을 관측한다.
// send 자체에는 timeout이 없다 — 하류가 멈추면 여기서 무기한 블로킹한다.
// 그래서 quantum은 emit "시도 횟수"만 보장하고 wall-clock은 보장하지 않는다.
//
// 막히지 않은 send도 관측한다. 그래야 emit_block_seconds의 표본 수가 곧
// emit 횟수이고, GCP에서 _count > 0을 배선 확인으로 쓸 수 있다.
// 논블로킹 fast path로 time.Now() 두 번을 아끼는 안을 재봤지만, 계측
// 오버헤드 자체가 실행 간 변동(±10~20%)에 묻히는 수준이라(중앙값 기준
// BulkFill +3.3%, _workspace/quantum/bench-*.txt) 지표 의미를 흐릴 만한
// 이유가 되지 못했다.
//
// 이 머신의 클럭 해상도는 ~645µs이므로 막히지 않은 send는 0으로 기록된다.
// 그것이 정상이다 — _sum이 0이어도 _count는 emit 횟수와 같아야 한다.
func (me *MatchingEngine) sendExecution(kind EmitKind, event ExecutionEvent) {
	start := time.Now()
	me.ExecutionCh <- event
	blocked := time.Since(start)
	me.Observers.emitBlock(kind, blocked)
	me.sliceEmitBlock += blocked
}

func (me *MatchingEngine) emitMarketOrderDone(order *Order) {
	if me.ExecutionCh == nil {
		return
	}
	me.sendExecution(EmitMarketDone, ExecutionEvent{
		MarketOrderDone: &MarketOrderDone{
			OrderID:              order.ID,
			CoinSymbol:           order.CoinSymbol,
			Side:                 order.Side,
			FilledAmount:         order.FilledAmount,
			FilledQuoteAmount:    order.FilledQuoteAmount,
			RemainingAmount:      order.Amount,
			RemainingQuoteAmount: order.QuoteAmount,
		},
	})
}

func (me *MatchingEngine) emitOrderCancelled(cmd CancelOrderCommand) {
	if me.ExecutionCh == nil {
		return
	}
	_, eventID := me.nextTradeEvent()
	me.sendExecution(EmitCancelled, ExecutionEvent{
		OrderCancelled: &OrderCancelled{
			CommandID:     cmd.CommandID,
			OrderID:       cmd.OrderID,
			CoinSymbol:    cmd.CoinSymbol,
			Side:          cmd.Side,
			EngineEventID: eventID,
		},
	})
}

func (me *MatchingEngine) newTrade(coinSymbol string, price decimal.Decimal, quantity decimal.Decimal, buyOrderID uint, sellOrderID uint) *model.Trade {
	sequence, eventID := me.nextTradeEvent()
	return &model.Trade{
		EngineSequence: sequence,
		EngineEventID:  eventID,
		CoinSymbol:     coinSymbol,
		Price:          price,
		Quantity:       quantity,
		TradedAt:       time.Now(),
		BuyOrderID:     buyOrderID,
		SellOrderID:    sellOrderID,
	}
}

func (me *MatchingEngine) nextTradeEvent() (int64, string) {
	if me.engineID == "" {
		me.engineID = newEngineID()
	}
	me.tradeSeq++
	return me.tradeSeq, fmt.Sprintf("%s-%d", me.engineID, me.tradeSeq)
}

func newEngineID() string {
	instance := atomic.AddUint64(&engineInstanceCounter, 1)
	return fmt.Sprintf("engine-%d-%d", time.Now().UTC().UnixNano(), instance)
}

func bestMatchableSellOrder(book *OrderBook, incoming *Order) (*PriceLevel, int, bool) {
	var matchLevel *PriceLevel
	matchIndex := -1

	book.SellOrders.Ascend(func(level *PriceLevel) bool {
		if incoming.Price.LessThan(level.Price) {
			return false
		}
		if index := firstNonSelfOrderIndex(level, incoming); index >= 0 {
			matchLevel = level
			matchIndex = index
			return false
		}
		return true
	})

	return matchLevel, matchIndex, matchLevel != nil
}

func bestMarketSellOrder(book *OrderBook, incoming *Order) (*PriceLevel, int, bool) {
	var matchLevel *PriceLevel
	matchIndex := -1

	book.SellOrders.Ascend(func(level *PriceLevel) bool {
		if index := firstNonSelfOrderIndex(level, incoming); index >= 0 {
			matchLevel = level
			matchIndex = index
			return false
		}
		return true
	})

	return matchLevel, matchIndex, matchLevel != nil
}

func bestMatchableBuyOrder(book *OrderBook, incoming *Order) (*PriceLevel, int, bool) {
	var matchLevel *PriceLevel
	matchIndex := -1

	book.BuyOrders.Descend(func(level *PriceLevel) bool {
		if level.Price.LessThan(incoming.Price) {
			return false
		}
		if index := firstNonSelfOrderIndex(level, incoming); index >= 0 {
			matchLevel = level
			matchIndex = index
			return false
		}
		return true
	})

	return matchLevel, matchIndex, matchLevel != nil
}

func bestMarketBuyOrder(book *OrderBook, incoming *Order) (*PriceLevel, int, bool) {
	var matchLevel *PriceLevel
	matchIndex := -1

	book.BuyOrders.Descend(func(level *PriceLevel) bool {
		if index := firstNonSelfOrderIndex(level, incoming); index >= 0 {
			matchLevel = level
			matchIndex = index
			return false
		}
		return true
	})

	return matchLevel, matchIndex, matchLevel != nil
}

func firstNonSelfOrderIndex(level *PriceLevel, incoming *Order) int {
	if level == nil || level.Orders == nil || incoming == nil {
		return -1
	}
	for i := 0; i < level.Orders.Len(); i++ {
		if !isSelfTrade(incoming, level.Orders.At(i)) {
			return i
		}
	}
	return -1
}

func isSelfTrade(incoming *Order, resting *Order) bool {
	return incoming != nil &&
		resting != nil &&
		incoming.UserID != 0 &&
		resting.UserID != 0 &&
		incoming.UserID == resting.UserID
}

func (me *MatchingEngine) GetOrderBookSnapshot(coinSymbols ...string) OrderBookSnapshot {
	coinSymbol := ""
	if len(coinSymbols) > 0 {
		coinSymbol = coinSymbols[0]
	}
	return me.GetOrderBookSnapshotWithDepth(coinSymbol, DefaultSnapshotDepth)
}

func (me *MatchingEngine) GetOrderBookSnapshotWithDepth(coinSymbol string, depth int) OrderBookSnapshot {
	book := me.OrderBook
	if coinSymbol != "" {
		book = me.GetOrderBook(coinSymbol)
	}

	snapshot := OrderBookSnapshot{
		CoinSymbol: coinSymbol,
	}
	if book == nil {
		return snapshot
	}

	depth = normalizeSnapshotDepth(depth)
	book.SellOrders.Ascend(func(level *PriceLevel) bool {
		if len(snapshot.Asks) >= depth {
			return false
		}
		qty := decimal.Zero
		for i := 0; i < level.Orders.Len(); i++ {
			qty = qty.Add(level.Orders.At(i).Amount)
		}
		if qty.GreaterThan(decimal.Zero) {
			snapshot.Asks = append(snapshot.Asks, PriceLevelData{
				Price:    level.Price,
				Quantity: qty,
			})
		}
		return true
	})

	book.BuyOrders.Descend(func(level *PriceLevel) bool {
		if len(snapshot.Bids) >= depth {
			return false
		}
		qty := decimal.Zero
		for i := 0; i < level.Orders.Len(); i++ {
			qty = qty.Add(level.Orders.At(i).Amount)
		}
		if qty.GreaterThan(decimal.Zero) {
			snapshot.Bids = append(snapshot.Bids, PriceLevelData{
				Price:    level.Price,
				Quantity: qty,
			})
		}
		return true
	})

	return snapshot
}

func normalizeSnapshotDepth(depth int) int {
	if depth <= 0 {
		return DefaultSnapshotDepth
	}
	return depth
}
