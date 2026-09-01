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

	// sliceEmitBlock은 조각 하나의 emit 블로킹 누적값이다. runSlice가 조각
	// 시작마다 0으로 되돌린다. 엔진 goroutine에서만 쓴다.
	sliceEmitBlock time.Duration

	// quantum 값. 0은 matchSlice의 무제한 sentinel과 충돌하므로 항상 1
	// 이상이어야 한다. 여기 기본값은 개발·테스트용이며, production 값은
	// 로컬 탐색 결과로 확정한다.
	maxMatchesPerTurn     int
	maxConsecutiveCancels int

	// 스케줄러 지속 상태. 엔진 goroutine에서만 쓴다.
	//
	// pendingCancel·pendingOrder·tickerDue는 blocking select가 latch한
	// 작업이다. 그 select는 작업을 실행하지 않고 latch만 하며, 다음 turn이
	// 처리한다 — turn_duration에 블로킹 대기가 섞이지 않게 하기 위해서다.
	activeSweep          *activeSweep
	shuttingDown         bool
	cancelsSinceProgress int
	pendingCancel        *CancelOrderCommand
	pendingOrder         *Order
	tickerDue            bool

	// crashHook은 테스트 전용이다. nil이 아니고 true를 반환하면 엔진 루프가
	// drain·flush·채널 close 없이 즉시 반환한다 — 프로세스 크래시와 같은
	// 상태를 만든다. 프로덕션에서는 항상 nil이다.
	//
	// Start() 전에 설치하고 arm은 hook 내부의 atomic으로 한다. 실행 중
	// 함수 필드를 교체하면 data race다.
	crashHook func() bool

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
			if me.runTurn(ticker) {
				return
			}
		}
	}()
}

// runTurn은 quantum 경계 하나를 실행한다. true를 반환하면 엔진이 종료됐다.
//
// 계약: blocking select는 작업을 실행하지 않고 latch만 한다. latch된 작업은
// 다음 turn에서 처리된다 — 그래야 모든 실제 작업이 계측 구간 안에서 일어나고
// turn_duration에 블로킹 대기가 섞이지 않는다.
func (me *MatchingEngine) runTurn(ticker *time.Ticker) bool {
	turnStart := time.Now()

	// 0. turn 시작 상태를 고정한다. 5단계가 3단계 실행 **후의** activeSweep을
	//    다시 읽으면, 마지막 조각이 끝난 turn에서 P-a와 P-b/P-c/P-d가 함께
	//    발생해 "turn당 progress 정확히 하나" 불변식이 깨진다. 그러면 sweep이
	//    끝난 turn만 취소 상한을 두 배로 얻는 비대칭이 생긴다.
	hadActive := me.activeSweep != nil

	// 1. cancel phase — 상한 안에서만 드레인한다. 상한이 없으면 취소 홍수가
	//    OrderCh를 영원히 굶긴다.
	for me.cancelsSinceProgress < me.maxConsecutiveCancels {
		cmd, ok := me.takeCancel()
		if !ok {
			break
		}
		me.processCancel(cmd)
		me.cancelsSinceProgress++
	}

	// 2. ticker phase — 코얼레싱된 스냅샷을 발행한다.
	if me.tickerDue {
		me.tickerDue = false
		me.flushSnapshots()
	} else {
		select {
		case <-ticker.C:
			me.flushSnapshots()
		default:
		}
	}

	// 3. slice phase
	if me.activeSweep != nil {
		me.runSlice()
		me.cancelsSinceProgress = 0 // P-a
	}

	// 3.5 crash hook (테스트 전용). 조각 사이에서만 발동한다.
	//     drain·flush·close 없이 반환해 프로세스 크래시와 같은 상태를 만든다.
	if me.crashHook != nil && me.crashHook() {
		return true
	}

	// 4. stop latch
	if !me.shuttingDown {
		select {
		case <-me.stopCh:
			me.shuttingDown = true
		default:
		}
	}

	// 5. admission phase — turn 시작 시점에 슬롯이 있었으면 건너뛴다.
	//    그 sweep이 끝난 주문의 다음 작업은 다음 turn의 5단계가 받는다.
	if !hadActive {
		me.admitPhase()
	}

	me.Observers.turn(time.Since(turnStart))

	// 6. 남은 일이 있으면 블로킹하지 않고 다음 turn으로.
	if me.activeSweep != nil || me.pendingCancel != nil || me.pendingOrder != nil || me.tickerDue {
		return false
	}

	// 7. shutdown drain 완료 판정.
	if me.shuttingDown {
		if me.latchOneNonBlocking() {
			return false
		}
		// activeSweep이 없고 두 채널의 논블로킹 recv가 모두 비었다 → drain 완료.
		// 이 루프가 ExecutionCh와 SnapshotCh의 유일한 writer이므로, close가
		// downstream(outbox writer, 스냅샷 소비자) 종료를 도미노로 전파한다.
		me.flushSnapshots()
		if me.ExecutionCh != nil {
			close(me.ExecutionCh)
		}
		close(me.SnapshotCh)
		return true
	}

	// 8. blocking select — 실행하지 않고 latch만 한다.
	orderCh := me.OrderCh
	if me.emitBackpressured() {
		orderCh = nil // 하류 포화 — 신규 주문 억제, 취소 emit 헤드룸 확보
	}
	select {
	case cmd := <-me.CancelCh:
		me.pendingCancel = &cmd
	case order := <-orderCh:
		me.pendingOrder = order
	case <-ticker.C:
		me.tickerDue = true
	case <-me.stopCh:
		me.shuttingDown = true
	}
	return false
}

func (me *MatchingEngine) takeCancel() (CancelOrderCommand, bool) {
	if me.pendingCancel != nil {
		cmd := *me.pendingCancel
		me.pendingCancel = nil
		return cmd, true
	}
	select {
	case cmd := <-me.CancelCh:
		return cmd, true
	default:
		return CancelOrderCommand{}, false
	}
}

// admitPhase는 P-b/P-c/P-d 중 하나를 반드시 발생시킨다. 그 불변식이
// cancelsSinceProgress가 상한에 고정되는 wedge를 막는다.
//
// latch된 pendingOrder는 emitBackpressured() 검사보다 **먼저** 처리한다.
// 게이트는 OrderCh에서의 신규 유입만 억제하는 장치이고, 이미 엔진 안으로
// 들어온 작업에는 적용되지 않는다. 순서가 뒤집히면 cancel phase가 만든
// backpressure에 latch된 주문이 걸려 무기한 park한다.
func (me *MatchingEngine) admitPhase() {
	if me.pendingOrder != nil {
		order := me.pendingOrder
		me.pendingOrder = nil
		me.admit(order)
		me.cancelsSinceProgress = 0 // P-b
		return
	}
	if !me.shuttingDown && me.emitBackpressured() {
		me.cancelsSinceProgress = 0 // P-d: 의도된 유입 억제 — 굶는 주체가 없다
		return
	}
	select {
	case order := <-me.OrderCh:
		me.admit(order)
	default:
	}
	me.cancelsSinceProgress = 0 // P-b 또는 P-c
}

func (me *MatchingEngine) admit(order *Order) {
	if sweep := me.admitOrder(order); sweep != nil {
		me.activeSweep = sweep
	}
}

// runSlice는 조각 하나를 실행하고, 마지막 조각이면 슬롯을 비운다.
//
// Slice 관측은 finishOrder 뒤다 — 마지막 조각의 MarketOrderDone emit
// 블로킹이 sliceEmitBlock에 들어간 뒤여야 한다.
func (me *MatchingEngine) runSlice() {
	sweep := me.activeSweep
	me.sliceEmitBlock = 0
	trades, done := me.matchSlice(sweep.book, sweep.order, me.maxMatchesPerTurn)
	sweep.trades += trades
	// 조각마다 dirty를 찍는다. Match 완료 후에만 찍으면 sweep 도중의 ticker가
	// 그 심볼을 보지 못해 캐시와 WebSocket 스냅샷이 sweep 내내 멈춘다.
	if trades >= 1 {
		me.markDirty(sweep.order.CoinSymbol)
	}
	if !done {
		me.Observers.slice(trades, me.sliceEmitBlock)
		me.Observers.yield()
		return
	}
	me.finishOrder(sweep.book, sweep.order)
	me.Observers.slice(trades, me.sliceEmitBlock)
	me.observeMatchLatency(sweep.order)
	me.Observers.orderDone(sweep.trades)
	me.activeSweep = nil
}

// latchOneNonBlocking은 shutdown drain 전용이다. OrderCh에 게이트를 걸지
// 않는다 — 이미 접수된 작업이기 때문이다. HTTP·hold coordinator·cancel
// worker가 먼저 종료돼 새 producer가 없다는 lifecycle을 전제로 하며,
// 채널 len()으로 판정하지 않는다.
func (me *MatchingEngine) latchOneNonBlocking() bool {
	select {
	case cmd := <-me.CancelCh:
		me.pendingCancel = &cmd
		return true
	default:
	}
	select {
	case order := <-me.OrderCh:
		me.pendingOrder = order
		return true
	default:
	}
	return false
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

// activeSweep은 조각 사이에 살아남는 유일한 sweep 상태다. 재개에 필요한 것은
// 주문 포인터뿐이다 — 네 match 루프는 반복 사이에 지역 상태를 들고 가지 않고
// 매번 bestMatchable*로 상대를 새로 찾는다. 그것이 조각 사이에 처리된 취소를
// 반영하는 유일하게 안전한 방법이다.
type activeSweep struct {
	order  *Order
	book   *OrderBook
	trades int
}

// matchSlice는 최대 budget개의 체결을 만들고 돌아온다.
// done=true는 "더 체결할 수 없다"이고, budget 소진(done=false)과 다르다.
func (me *MatchingEngine) matchSlice(book *OrderBook, order *Order, budget int) (int, bool) {
	switch order.Side {
	case model.OrderSideBuy:
		if order.OrderType == model.OrderTypeMarket {
			return me.matchMarketBuy(book, order, budget)
		}
		return me.matchBuy(book, order, budget)
	case model.OrderSideSell:
		if order.OrderType == model.OrderTypeMarket {
			return me.matchMarketSell(book, order, budget)
		}
		return me.matchSell(book, order, budget)
	}
	return 0, true
}

// finishOrder는 마지막 조각에서만 부른다.
// 시장가면 MarketOrderDone 1회, 지정가 잔량이 있으면 book에 등록한다.
func (me *MatchingEngine) finishOrder(book *OrderBook, order *Order) {
	if order.OrderType == model.OrderTypeMarket {
		me.emitMarketOrderDone(order)
		return
	}
	if order.Amount.GreaterThan(decimal.Zero) {
		book.AddOrder(order)
		me.markDirty(order.CoinSymbol)
	}
}

// admitOrder는 주문을 슬롯에 올린다. 슬롯을 만들 수 없는 즉시 완료 주문은
// nil을 반환하되 observer는 정확히 1회 호출한다(nil 주문 제외).
// 이 가드들은 조각화 이전 Match 앞머리의 조기 반환과 의미가 같아야 한다.
func (me *MatchingEngine) admitOrder(order *Order) *activeSweep {
	if order == nil {
		return nil
	}
	if !order.EnqueuedAt.IsZero() {
		me.Observers.orderAdmitted(time.Since(order.EnqueuedAt))
	}
	if !me.orderIsAdmissible(order) {
		me.observeMatchLatency(order)
		return nil
	}
	return &activeSweep{order: order, book: me.GetOrderBook(order.CoinSymbol)}
}

func (me *MatchingEngine) observeMatchLatency(order *Order) {
	if me.MatchLatencyObserver != nil && !order.EnqueuedAt.IsZero() {
		me.MatchLatencyObserver(time.Since(order.EnqueuedAt))
	}
}

// Match는 조각화 없이 주문을 끝까지 처리한다. 테스트·벤치 약 30곳이 이
// 시그니처를 쓰므로 그대로 둔다. budget 0이 무제한 sentinel이다.
func (me *MatchingEngine) Match(order *Order) {
	sweep := me.admitOrder(order)
	if sweep == nil {
		return
	}
	me.matchSlice(sweep.book, sweep.order, 0)
	me.finishOrder(sweep.book, sweep.order)
}

// budget 0은 무제한(public Match 전용) sentinel이다. budget > 0이면 trades가
// budget에 닿을 때 yield한다. 반환값 done=true는 "더 체결할 수 없다"이고
// budget 소진(done=false)과 다르다.
//
// 예산 검사는 루프 조건 **뒤**에 있다. 예산 경계에서 정확히 전량 체결된
// sweep이 같은 조각에서 done=true로 끝나야 빈 조각이 생기지 않는다.
func (me *MatchingEngine) matchBuy(book *OrderBook, order *Order, budget int) (int, bool) {
	trades := 0
	for order.Amount.GreaterThan(decimal.Zero) {
		if budget > 0 && trades >= budget {
			return trades, false
		}
		sellLevel, orderIndex, ok := bestMatchableSellOrder(book, order)
		if !ok {
			return trades, true
		}

		sellOrder := sellLevel.Orders.At(orderIndex)
		tradeQty := decimal.Min(order.Amount, sellOrder.Amount)
		if !tradeQty.GreaterThan(decimal.Zero) {
			return trades, true
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
		trades++
	}
	return trades, true
}

func (me *MatchingEngine) matchSell(book *OrderBook, order *Order, budget int) (int, bool) {
	trades := 0
	for order.Amount.GreaterThan(decimal.Zero) {
		if budget > 0 && trades >= budget {
			return trades, false
		}
		buyLevel, orderIndex, ok := bestMatchableBuyOrder(book, order)
		if !ok {
			return trades, true
		}

		buyOrder := buyLevel.Orders.At(orderIndex)
		tradeQty := decimal.Min(order.Amount, buyOrder.Amount)
		if !tradeQty.GreaterThan(decimal.Zero) {
			return trades, true
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
		trades++
	}
	return trades, true
}

func (me *MatchingEngine) matchMarketBuy(book *OrderBook, order *Order, budget int) (int, bool) {
	trades := 0
	for order.QuoteAmount.GreaterThan(decimal.Zero) {
		if budget > 0 && trades >= budget {
			return trades, false
		}
		sellLevel, orderIndex, ok := bestMarketSellOrder(book, order)
		if !ok {
			return trades, true
		}

		sellOrder := sellLevel.Orders.At(orderIndex)
		// Div는 DivisionPrecision(16자리)에서 반올림하므로 클램프 체결 시
		// price * maxQtyByQuote가 잔여 예산을 초과할 수 있다(관측: +1.8775e-9류
		// 잔차). QuoRem은 같은 정밀도에서 몫을 내림(0 방향 절삭)하므로
		// price * maxQtyByQuote <= order.QuoteAmount가 항상 보장된다.
		maxQtyByQuote, _ := order.QuoteAmount.QuoRem(sellLevel.Price, int32(decimal.DivisionPrecision))
		tradeQty := decimal.Min(maxQtyByQuote, sellOrder.Amount)
		if !tradeQty.GreaterThan(decimal.Zero) {
			return trades, true
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
		trades++
	}
	return trades, true
}

func (me *MatchingEngine) matchMarketSell(book *OrderBook, order *Order, budget int) (int, bool) {
	trades := 0
	for order.Amount.GreaterThan(decimal.Zero) {
		if budget > 0 && trades >= budget {
			return trades, false
		}
		buyLevel, orderIndex, ok := bestMarketBuyOrder(book, order)
		if !ok {
			return trades, true
		}

		buyOrder := buyLevel.Orders.At(orderIndex)
		tradeQty := decimal.Min(order.Amount, buyOrder.Amount)
		if !tradeQty.GreaterThan(decimal.Zero) {
			return trades, true
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
		trades++
	}
	return trades, true
}

func (me *MatchingEngine) emitTrade(trade *model.Trade) {
	select {
	case me.TradeCh <- trade:
	default:
	}
	if me.ExecutionCh != nil {
		me.sendExecution(EmitTrade, ExecutionEvent{Trade: trade})
	}
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
