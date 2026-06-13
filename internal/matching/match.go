// Package matching implements price-time (FIFO) matching over an order book,
// supporting all eight order types plus self-trade prevention.
//
// The engine matches "active" orders (Limit, Market, IOC, FOK, Post-Only,
// Iceberg). Stop and Stop-Limit orders are held off-book in a stop table and
// activated by trade-price movement (see stops.go); activations are delivered
// through a preallocated Sink, never a per-call closure, so the path stays
// allocation-free.
package matching

import (
	"github.com/yanuar-ar/order-book/internal/orderbook"
	"github.com/yanuar-ar/order-book/internal/types"
)

// Sink receives commands produced by stop activations. It is set once on the
// Engine; the sequencer wires it to re-injection in U8.
type Sink interface {
	Emit(types.Command)
}

// Engine matches funded orders against one market's book.
type Engine struct {
	book  *orderbook.Book
	sink  Sink
	stops []stopOrder
}

// NewEngine returns an engine over book, emitting stop activations to sink.
func NewEngine(book *orderbook.Book, sink Sink) *Engine {
	return &Engine{book: book, sink: sink}
}

// Book exposes the underlying book (read access for callers/tests).
func (e *Engine) Book() *orderbook.Book { return e.book }

// Result reports the outcome of submitting one order.
type Result struct {
	Fills     []types.Fill
	Rested    bool
	RestedQty types.Qty
	Rejected  bool
	Reason    types.RejectReason
	STP       bool // self-trade prevention cancelled the aggressor remainder
	Pending   bool // stop/stop-limit stored, awaiting trigger
}

// Submit processes a funded order. Stop and Stop-Limit orders are stored for
// later triggering; all other types are matched immediately. After an active
// order is processed, any stops triggered by the resulting trade price are
// activated through the Sink in deterministic order.
func (e *Engine) Submit(o types.FundedOrder) Result {
	if o.OrdType == types.Stop || o.OrdType == types.StopLimit {
		e.addStop(o)
		return Result{Pending: true}
	}
	res := e.matchActive(o)
	e.triggerStops()
	return res
}

func crosses(aggSide types.Side, aggPrice types.Price, ordType types.OrderType, restingPrice types.Price) bool {
	if ordType == types.Market {
		return true
	}
	if aggSide == types.Buy {
		return aggPrice >= restingPrice
	}
	return aggPrice <= restingPrice
}

func (e *Engine) matchActive(o types.FundedOrder) Result {
	oppSide := o.Side.Opposite()
	res := Result{}

	// Post-Only must not cross; if it would, reject without matching.
	if o.Flags.Has(types.FlagPostOnly) {
		if front, ok := e.book.FrontResting(oppSide); ok && crosses(o.Side, o.Price, o.OrdType, front.Price) {
			return Result{Rejected: true, Reason: types.ReasonPostOnlyCross}
		}
		e.rest(o, o.Qty)
		return Result{Rested: true, RestedQty: o.Qty}
	}

	// FOK: only execute if the whole quantity can fill immediately.
	if o.Tif == types.FOK {
		pred := func(p types.Price) bool { return crosses(o.Side, o.Price, o.OrdType, p) }
		if e.book.MatchableQty(oppSide, o.Account, pred, o.Qty) < o.Qty {
			return Result{Rejected: true, Reason: types.ReasonFOKUnfillable}
		}
	}

	remaining := o.Qty
	var matchIdx uint32
	for remaining > 0 {
		front, ok := e.book.FrontResting(oppSide)
		if !ok || !crosses(o.Side, o.Price, o.OrdType, front.Price) {
			break
		}
		if front.Account == o.Account {
			// Self-trade prevention: stop the pair, cancel the aggressor remainder.
			res.STP = true
			remaining = 0
			break
		}
		x := remaining
		if front.Display < x {
			x = front.Display
		}
		res.Fills = append(res.Fills, e.buildFill(o, front, x, matchIdx))
		matchIdx++
		remaining -= x
		e.book.SetLastPrice(front.Price)
		e.book.ConsumeFront(oppSide, front.Idx, x)
	}

	// Remainder handling by type.
	switch {
	case res.STP:
		// remainder cancelled by STP; nothing rests
	case o.OrdType == types.Market:
		// market never rests; remainder cancelled
	case o.Tif == types.IOC || o.Tif == types.FOK:
		// remainder cancelled (FOK fully filled by construction)
	default: // Limit / GTC (incl. iceberg)
		if remaining > 0 {
			e.rest(o, remaining)
			res.Rested = true
			res.RestedQty = remaining
		}
	}
	return res
}

func (e *Engine) buildFill(o types.FundedOrder, front orderbook.RestingView, qty types.Qty, idx uint32) types.Fill {
	f := types.Fill{
		AggressorSeq: o.Seq,
		MatchIndex:   idx,
		Taker:        o.Side,
		Market:       o.Market,
		Price:        front.Price, // trade executes at the resting (maker) price
		Qty:          qty,
	}
	if o.Side == types.Buy {
		f.BuyOrder, f.BuyAccount = o.OrderID, o.Account
		f.SellOrder, f.SellAccount = front.ID, front.Account
	} else {
		f.SellOrder, f.SellAccount = o.OrderID, o.Account
		f.BuyOrder, f.BuyAccount = front.ID, front.Account
	}
	return f
}

// rest places the unfilled remainder into the book as a maker order.
func (e *Engine) rest(o types.FundedOrder, remaining types.Qty) {
	display := remaining
	if o.Flags.Has(types.FlagIceberg) && o.DisplayQty > 0 && o.DisplayQty < remaining {
		display = o.DisplayQty
	}
	e.book.Insert(orderbook.NewResting{
		ID:      o.OrderID,
		Account: o.Account,
		Side:    o.Side,
		Price:   o.Price,
		Qty:     remaining,
		Display: display,
		Typ:     types.Limit,
		Tif:     o.Tif,
		Flags:   o.Flags,
	})
}
