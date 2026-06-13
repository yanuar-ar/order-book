package matching

import (
	"sort"

	"github.com/yanuar-ar/order-book/internal/types"
)

// stopOrder is a Stop or Stop-Limit order held off-book awaiting its trigger.
type stopOrder struct {
	ord types.FundedOrder
}

// PendingStops reports how many stop orders are awaiting their trigger.
func (e *Engine) PendingStops() int { return len(e.stops) }

// StopView is a read-only view of one pending stop order, for invariant checks
// and tests.
type StopView struct {
	OrderID   types.OrderID
	Account   types.AccountID
	Side      types.Side
	OrdType   types.OrderType
	Price     types.Price
	StopPrice types.Price
	Qty       types.Qty
	Seq       types.Seq
}

// StopDump returns every pending stop order in deterministic order (by Seq,
// then OrderID). The off-book stops hold reservations, so tests/property
// includes them when checking reserved == open orders (INV-BAL-03), and the
// dump lets stop semantics (INV-STP-*) be asserted directly.
func (e *Engine) StopDump() []StopView {
	out := make([]StopView, 0, len(e.stops))
	for _, s := range e.stops {
		o := s.ord
		out = append(out, StopView{
			OrderID: o.OrderID, Account: o.Account, Side: o.Side, OrdType: o.OrdType,
			Price: o.Price, StopPrice: o.StopPrice, Qty: o.Qty, Seq: o.Seq,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Seq != out[j].Seq {
			return out[i].Seq < out[j].Seq
		}
		return out[i].OrderID < out[j].OrderID
	})
	return out
}

func (e *Engine) addStop(o types.FundedOrder) {
	e.stops = append(e.stops, stopOrder{ord: o})
}

// triggerStops activates every stop whose trigger condition is met by the
// current trade price. A buy-stop triggers when last >= stopPrice; a sell-stop
// triggers when last <= stopPrice. Triggered stops are activated in a total
// order by their originating Seq (ascending) so replay is deterministic, and
// emitted to the Sink as new commands (Stop -> Market, Stop-Limit -> Limit).
func (e *Engine) triggerStops() {
	last, ok := e.book.LastPrice()
	if !ok || len(e.stops) == 0 {
		return
	}
	var triggered []types.FundedOrder
	kept := e.stops[:0]
	for _, s := range e.stops {
		o := s.ord
		var trig bool
		if o.Side == types.Buy {
			trig = last >= o.StopPrice
		} else {
			trig = last <= o.StopPrice
		}
		if trig {
			triggered = append(triggered, o)
		} else {
			kept = append(kept, s)
		}
	}
	e.stops = kept
	if len(triggered) == 0 {
		return
	}
	sort.Slice(triggered, func(i, j int) bool { return triggered[i].Seq < triggered[j].Seq })
	for _, o := range triggered {
		e.sink.Emit(activationCommand(o))
	}
}

// activationCommand converts a triggered stop into the new order command that
// re-enters the pipeline. Seq is left zero; the sequencer assigns a fresh Seq
// on re-injection.
func activationCommand(o types.FundedOrder) types.Command {
	cmd := types.Command{
		Type:       types.CmdNewOrder,
		Market:     o.Market,
		Account:    o.Account,
		OrderID:    o.OrderID,
		Side:       o.Side,
		Tif:        o.Tif,
		Flags:      o.Flags,
		Qty:        o.Qty,
		DisplayQty: o.DisplayQty,
	}
	if o.OrdType == types.Stop {
		cmd.OrdType = types.Market
	} else {
		cmd.OrdType = types.Limit
		cmd.Price = o.Price
	}
	return cmd
}
