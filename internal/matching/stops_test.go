package matching

import (
	"testing"

	"github.com/yanuar-ar/order-book/internal/types"
)

// stop builds a Stop/Stop-Limit funded order. stopPrice triggers; price is the
// limit price used when a Stop-Limit activates.
func stop(seq types.Seq, acct types.AccountID, id types.OrderID, side types.Side, typ types.OrderType, stopPrice, price types.Price, qty types.Qty) types.FundedOrder {
	o := fo(seq, acct, id, side, typ, types.GTC, price, qty)
	o.StopPrice = stopPrice
	return o
}

// ---- Positive: triggering ----

func TestStopMarketTriggersOnPriceMove(t *testing.T) {
	e, sink := newEngine()
	e.Submit(lim(1, 10, 100, types.Sell, 106, 1)) // maker to trade against
	pend := e.Submit(stop(2, 20, 200, types.Buy, types.Stop, 105, 0, 2))
	if !pend.Pending || e.PendingStops() != 1 {
		t.Fatalf("stop should be pending, got %+v pendingN=%d", pend, e.PendingStops())
	}
	// A trade at 106 pushes lastPrice to 106, crossing the buy-stop at 105.
	e.Submit(lim(3, 30, 300, types.Buy, 106, 1))
	if e.PendingStops() != 0 {
		t.Fatalf("stop not consumed, pending=%d", e.PendingStops())
	}
	if len(sink.cmds) != 1 {
		t.Fatalf("sink got %d cmds, want 1", len(sink.cmds))
	}
	c := sink.cmds[0]
	if c.OrdType != types.Market || c.Side != types.Buy || c.Qty != 2 {
		t.Fatalf("activation = %+v, want Market Buy qty2", c)
	}
}

func TestStopLimitActivatesAsLimitAtPrice(t *testing.T) {
	e, sink := newEngine()
	e.Submit(lim(1, 10, 100, types.Sell, 106, 1))
	e.Submit(stop(2, 20, 200, types.Buy, types.StopLimit, 105, 107, 2)) // limit price 107
	e.Submit(lim(3, 30, 300, types.Buy, 106, 1))                        // trade @106 -> trigger
	if len(sink.cmds) != 1 {
		t.Fatalf("sink got %d cmds, want 1", len(sink.cmds))
	}
	c := sink.cmds[0]
	if c.OrdType != types.Limit || c.Price != 107 {
		t.Fatalf("activation = %+v, want Limit @107", c)
	}
}

func TestSellStopTriggersWhenPriceFalls(t *testing.T) {
	e, sink := newEngine()
	e.Submit(lim(1, 10, 100, types.Buy, 94, 1)) // resting bid to trade against
	e.Submit(stop(2, 20, 200, types.Sell, types.Stop, 95, 0, 2))
	// A sell trading at 94 pushes lastPrice to 94 (<= 95) -> sell-stop triggers.
	e.Submit(lim(3, 30, 300, types.Sell, 94, 1))
	if len(sink.cmds) != 1 || sink.cmds[0].Side != types.Sell {
		t.Fatalf("sell-stop activation = %+v, want one Sell activation", sink.cmds)
	}
}

// ---- Negative / edge: no trigger, ordering ----

func TestStopNotTriggeredStaysPending(t *testing.T) {
	e, sink := newEngine()
	e.Submit(lim(1, 10, 100, types.Sell, 106, 1))
	e.Submit(stop(2, 20, 200, types.Buy, types.Stop, 200, 0, 2)) // far above
	e.Submit(lim(3, 30, 300, types.Buy, 106, 1))                 // trade @106, < 200
	if e.PendingStops() != 1 {
		t.Fatalf("stop should remain pending, pending=%d", e.PendingStops())
	}
	if len(sink.cmds) != 0 {
		t.Fatalf("no activation expected, got %d", len(sink.cmds))
	}
}

func TestSimultaneousStopsActivateInSeqOrder(t *testing.T) {
	e, sink := newEngine()
	e.Submit(lim(1, 10, 100, types.Sell, 106, 1))
	// Two buy-stops triggered by the same trade; OrderID encodes the Seq so we
	// can assert activation order is by ascending originating Seq.
	e.Submit(stop(5, 20, 50, types.Buy, types.Stop, 105, 0, 1)) // Seq 5
	e.Submit(stop(3, 21, 30, types.Buy, types.Stop, 105, 0, 1)) // Seq 3
	e.Submit(lim(9, 30, 300, types.Buy, 106, 1))                // trade @106 triggers both
	if len(sink.cmds) != 2 {
		t.Fatalf("want 2 activations, got %d", len(sink.cmds))
	}
	if sink.cmds[0].OrderID != 30 || sink.cmds[1].OrderID != 50 {
		t.Fatalf("activation order by OrderID = [%d %d], want [30 50] (ascending Seq)", sink.cmds[0].OrderID, sink.cmds[1].OrderID)
	}
}

// ---- StopDump ----

func TestStopDump_EmptyWhenNoStops(t *testing.T) {
	e, _ := newEngine()
	if d := e.StopDump(); len(d) != 0 {
		t.Fatalf("StopDump on fresh engine = %v, want empty", d)
	}
}

func TestStopDump_SingleStop(t *testing.T) {
	e, _ := newEngine()
	e.Submit(stop(2, 20, 200, types.Buy, types.Stop, 105, 0, 3)) // far below; stays pending
	d := e.StopDump()
	if len(d) != 1 {
		t.Fatalf("StopDump len = %d, want 1", len(d))
	}
	got := d[0]
	if got.OrderID != 200 || got.Account != 20 || got.Side != types.Buy ||
		got.OrdType != types.Stop || got.StopPrice != 105 || got.Qty != 3 || got.Seq != 2 {
		t.Fatalf("StopDump[0] = %+v, fields do not match the submitted stop", got)
	}
}

func TestStopDump_SortedBySeqThenID(t *testing.T) {
	e, _ := newEngine()
	// Submit out of Seq order; far-from-trigger so all stay pending.
	e.Submit(stop(5, 20, 50, types.Buy, types.Stop, 500, 0, 1))
	e.Submit(stop(3, 21, 30, types.Buy, types.Stop, 500, 0, 1))
	e.Submit(stop(3, 22, 31, types.Buy, types.Stop, 500, 0, 1)) // same Seq, higher ID
	d := e.StopDump()
	if len(d) != 3 {
		t.Fatalf("StopDump len = %d, want 3", len(d))
	}
	wantSeq := []types.Seq{3, 3, 5}
	wantID := []types.OrderID{30, 31, 50}
	for i := range d {
		if d[i].Seq != wantSeq[i] || d[i].OrderID != wantID[i] {
			t.Fatalf("StopDump[%d] = seq %d id %d, want seq %d id %d", i, d[i].Seq, d[i].OrderID, wantSeq[i], wantID[i])
		}
	}
}
