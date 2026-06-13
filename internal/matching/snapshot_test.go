package matching

import (
	"reflect"
	"testing"

	"github.com/yanuar-ar/order-book/internal/orderbook"
	"github.com/yanuar-ar/order-book/internal/types"
)

func stopOrds(e *Engine) []types.FundedOrder {
	out := make([]types.FundedOrder, len(e.stops))
	for i, s := range e.stops {
		out[i] = s.ord
	}
	return out
}

// ---- Positive ----

func TestStopSnapshot_RoundTripPreservesFullOrder(t *testing.T) {
	e, _ := newEngine()
	// A buy stop-limit with iceberg display + a sell stop-market with MaxQuote.
	e.addStop(types.FundedOrder{Seq: 5, Market: 0, Account: 1, OrderID: 10, Side: types.Buy, OrdType: types.StopLimit, Tif: types.GTC, Flags: types.FlagIceberg, Price: 110, StopPrice: 105, Qty: 8, DisplayQty: 2})
	e.addStop(types.FundedOrder{Seq: 3, Market: 0, Account: 2, OrderID: 11, Side: types.Sell, OrdType: types.Stop, Tif: types.IOC, Price: 0, StopPrice: 90, Qty: 4, MaxQuote: 1234})

	e2, _ := newEngine()
	if err := e2.RestoreSnapshot(e.EncodeSnapshot()); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	// Both engines, sorted deterministically by EncodeSnapshot, must match field-for-field.
	want := stopOrds(e)
	got := stopOrds(e2)
	// e1 is in append order (5,3); restore sorts by Seq → (3,5). Compare as sets via StopDump.
	if !reflect.DeepEqual(e.StopDump(), e2.StopDump()) {
		t.Fatalf("StopDump differs:\n want %+v\n got  %+v", e.StopDump(), e2.StopDump())
	}
	// Full-fidelity check on the lossy fields StopDump drops (Tif/Flags/DisplayQty/MaxQuote).
	byID := func(os []types.FundedOrder) map[types.OrderID]types.FundedOrder {
		m := map[types.OrderID]types.FundedOrder{}
		for _, o := range os {
			m[o.OrderID] = o
		}
		return m
	}
	w, g := byID(want), byID(got)
	for id, wo := range w {
		if !reflect.DeepEqual(wo, g[id]) {
			t.Fatalf("order %d differs after restore:\n want %+v\n got  %+v", id, wo, g[id])
		}
	}
}

// ---- Edge: trigger after restore preserves activation Seq order ----

func TestStopSnapshot_TriggerAfterRestoreOrdersBySeq(t *testing.T) {
	src, _ := newEngine()
	// Two buy stops, same trigger price, different originating Seq.
	src.addStop(types.FundedOrder{Seq: 9, Market: 0, Account: 1, OrderID: 20, Side: types.Buy, OrdType: types.Stop, StopPrice: 100, Qty: 1})
	src.addStop(types.FundedOrder{Seq: 4, Market: 0, Account: 1, OrderID: 21, Side: types.Buy, OrdType: types.Stop, StopPrice: 100, Qty: 1})

	e2 := NewEngine(orderbook.New(0, 64), &collectSink{}, 1)
	sink := &collectSink{}
	e2.SetSink(sink)
	if err := e2.RestoreSnapshot(src.EncodeSnapshot()); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	// Price reaches the trigger; both fire, ordered by Seq (4 before 9).
	e2.Book().SetLastPrice(100) // boundary: last == StopPrice triggers a buy stop
	e2.triggerStops()
	if len(sink.cmds) != 2 {
		t.Fatalf("expected 2 activations, got %d", len(sink.cmds))
	}
	if sink.cmds[0].OrderID != 21 || sink.cmds[1].OrderID != 20 {
		t.Fatalf("activation order = [%d %d], want [21 20] (by Seq 4 then 9)", sink.cmds[0].OrderID, sink.cmds[1].OrderID)
	}
	if e2.PendingStops() != 0 {
		t.Fatalf("expected stop table drained, %d remain", e2.PendingStops())
	}
}

// ---- Edge: empty ----

func TestStopSnapshot_EmptyRoundTrips(t *testing.T) {
	e, _ := newEngine()
	e2, _ := newEngine()
	if err := e2.RestoreSnapshot(e.EncodeSnapshot()); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	if e2.PendingStops() != 0 {
		t.Fatalf("empty stop table restored with %d stops", e2.PendingStops())
	}
}

// ---- Negative ----

func TestStopSnapshot_TruncatedSectionRejected(t *testing.T) {
	e, _ := newEngine()
	e.addStop(types.FundedOrder{Seq: 1, OrderID: 1, Side: types.Buy, OrdType: types.Stop, StopPrice: 100, Qty: 1})
	full := e.EncodeSnapshot()
	for _, n := range []int{0, 3, len(full) - 1} {
		e2, _ := newEngine()
		if err := e2.RestoreSnapshot(full[:n]); err == nil {
			t.Fatalf("RestoreSnapshot accepted truncated section of len %d", n)
		}
	}
}
