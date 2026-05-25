package matching

import (
	"errors"
	"fmt"
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
)

type MatchingEngine struct {
	OrderBook  *OrderBook
	OrderBooks map[string]*OrderBook
	OrderCh    chan *Order
	CancelCh   chan CancelOrderCommand
	TradeCh    chan *model.Trade
	SnapshotCh chan OrderBookSnapshot
	engineID   string
	tradeSeq   int64
}

const DefaultSnapshotDepth = 30

var engineInstanceCounter uint64

type CancelOrderCommand struct {
	CoinSymbol string
	OrderID    uint
	Side       model.OrderSide
	Price      decimal.Decimal
	ResponseCh chan CancelOrderResult
}

type CancelOrderResult struct {
	Removed bool
	Err     error
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
		OrderBook:  defaultBook,
		OrderBooks: make(map[string]*OrderBook),
		OrderCh:    make(chan *Order, 1024),
		CancelCh:   make(chan CancelOrderCommand, 1024),
		TradeCh:    make(chan *model.Trade, 1024),
		SnapshotCh: make(chan OrderBookSnapshot, 256),
		engineID:   newEngineID(),
	}
}

func (me *MatchingEngine) Start() {
	go func() {
		for {
			select {
			case order := <-me.OrderCh:
				if order == nil {
					continue
				}
				me.Match(order)
				me.SnapshotCh <- me.GetOrderBookSnapshot(order.CoinSymbol)
			case cmd := <-me.CancelCh:
				result := me.handleCancel(cmd)
				if cmd.ResponseCh != nil {
					cmd.ResponseCh <- result
				}
				if result.Removed {
					me.SnapshotCh <- me.GetOrderBookSnapshot(cmd.CoinSymbol)
				}
			}
		}
	}()
}

func (me *MatchingEngine) CancelOrder(cmd CancelOrderCommand) CancelOrderResult {
	if me == nil || me.CancelCh == nil {
		return CancelOrderResult{Err: ErrCancelOrderEngineUnavailable}
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
	if order == nil || !order.Amount.GreaterThan(decimal.Zero) {
		return
	}

	book := me.GetOrderBook(order.CoinSymbol)

	switch order.Side {
	case model.OrderSideBuy:
		me.matchBuy(book, order)
	case model.OrderSideSell:
		me.matchSell(book, order)
	default:
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

		me.TradeCh <- me.newTrade(order.CoinSymbol, sellLevel.Price, tradeQty, order.ID, sellOrder.ID)
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

		me.TradeCh <- me.newTrade(order.CoinSymbol, buyLevel.Price, tradeQty, buyOrder.ID, order.ID)
	}
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
