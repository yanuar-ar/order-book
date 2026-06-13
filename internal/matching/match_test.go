package matching

import (
	"testing"

	"github.com/yanuar-ar/order-book/internal/orderbook"
	"github.com/yanuar-ar/order-book/internal/types"
)

type collectSink struct{ cmds []types.Command }

func (s *collectSink) Emit(c types.Command) { s.cmds = append(s.cmds, c) }

func newEngine() (*Engine, *collectSink) {
	s := &collectSink{}
	return NewEngine(orderbook.New(0, 64), s), s
}

// fo builds a funded order; price 0 with Market type means no price limit.
func fo(seq types.Seq, acct types.AccountID, id types.OrderID, side types.Side, typ types.OrderType, tif types.TIF, price types.Price, qty types.Qty) types.FundedOrder {
	return types.FundedOrder{Seq: seq, Account: acct, OrderID: id, Side: side, OrdType: typ, Tif: tif, Price: price, Qty: qty}
}

func lim(seq types.Seq, acct types.AccountID, id types.OrderID, side types.Side, price types.Price, qty types.Qty) types.FundedOrder {
	return fo(seq, acct, id, side, types.Limit, types.GTC, price, qty)
}

// ---- Positive cases ----

func TestLimitFullFill(t *testing.T) {
	e, _ := newEngine()
	e.Submit(lim(1, 10, 100, types.Sell, 100, 5)) // resting ask
	res := e.Submit(lim(2, 20, 200, types.Buy, 100, 5))
	if len(res.Fills) != 1 || res.Fills[0].Qty != 5 || res.Fills[0].Price != 100 {
		t.Fatalf("fills = %+v, want one fill qty5 @100", res.Fills)
	}
	if res.Rested {
		t.Fatal("full fill should not rest a remainder")
	}
	if res.Fills[0].Taker != types.Buy {
		t.Fatalf("taker = %v, want Buy", res.Fills[0].Taker)
	}
}

func TestLimitPartialThenRest(t *testing.T) {
	e, _ := newEngine()
	e.Submit(lim(1, 10, 100, types.Sell, 100, 3))
	res := e.Submit(lim(2, 20, 200, types.Buy, 100, 5))
	if len(res.Fills) != 1 || res.Fills[0].Qty != 3 {
		t.Fatalf("fills = %+v, want one fill qty3", res.Fills)
	}
	if !res.Rested || res.RestedQty != 2 {
		t.Fatalf("rest = (%v,%d), want (true,2)", res.Rested, res.RestedQty)
	}
	if bid, ok := e.Book().BestBid(); !ok || bid != 100 {
		t.Fatalf("remainder did not rest as bid@100 (best bid ok=%v val=%d)", ok, bid)
	}
}

func TestMarketSweepsMultipleLevels(t *testing.T) {
	e, _ := newEngine()
	e.Submit(lim(1, 10, 100, types.Sell, 100, 2))
	e.Submit(lim(2, 10, 101, types.Sell, 101, 3))
	res := e.Submit(fo(3, 20, 300, types.Buy, types.Market, types.GTC, 0, 4))
	if len(res.Fills) != 2 {
		t.Fatalf("fills = %d, want 2", len(res.Fills))
	}
	if res.Fills[0].Price != 100 || res.Fills[0].Qty != 2 || res.Fills[1].Price != 101 || res.Fills[1].Qty != 2 {
		t.Fatalf("fills = %+v, want 2@100 then 2@101", res.Fills)
	}
	if res.Rested {
		t.Fatal("market order must never rest")
	}
}

func TestIOCFillsThenCancelsRemainder(t *testing.T) {
	e, _ := newEngine()
	e.Submit(lim(1, 10, 100, types.Sell, 100, 3))
	res := e.Submit(fo(2, 20, 200, types.Buy, types.Limit, types.IOC, 100, 5))
	if len(res.Fills) != 1 || res.Fills[0].Qty != 3 {
		t.Fatalf("fills = %+v, want qty3", res.Fills)
	}
	if res.Rested {
		t.Fatal("IOC remainder must be cancelled, not rested")
	}
	if _, ok := e.Book().BestBid(); ok {
		t.Fatal("IOC left a resting bid")
	}
}

func TestFOKSufficientFillsFully(t *testing.T) {
	e, _ := newEngine()
	e.Submit(lim(1, 10, 100, types.Sell, 100, 3))
	e.Submit(lim(2, 10, 101, types.Sell, 100, 2))
	res := e.Submit(fo(3, 20, 300, types.Buy, types.Limit, types.FOK, 100, 5))
	if res.Rejected {
		t.Fatalf("FOK with sufficient depth rejected: %v", res.Reason)
	}
	total := types.Qty(0)
	for _, f := range res.Fills {
		total += f.Qty
	}
	if total != 5 {
		t.Fatalf("FOK filled %d, want 5", total)
	}
}

func TestPostOnlyNonCrossingRests(t *testing.T) {
	e, _ := newEngine()
	e.Submit(lim(1, 10, 100, types.Sell, 105, 5)) // ask above
	o := lim(2, 20, 200, types.Buy, 100, 5)
	o.Flags = types.FlagPostOnly
	res := e.Submit(o)
	if res.Rejected {
		t.Fatal("non-crossing post-only should rest, not reject")
	}
	if !res.Rested || res.RestedQty != 5 {
		t.Fatalf("post-only rest = (%v,%d), want (true,5)", res.Rested, res.RestedQty)
	}
}

func TestIcebergReplenishAndRequeue(t *testing.T) {
	e, _ := newEngine()
	// Maker A: iceberg sell @100, total 6, visible 2 (hidden 4).
	a := lim(1, 10, 100, types.Sell, 100, 6)
	a.Flags = types.FlagIceberg
	a.DisplayQty = 2
	e.Submit(a)
	// Maker B: plain sell @100, qty 3, queued behind A.
	e.Submit(lim(2, 11, 200, types.Sell, 100, 3))

	// Buy 2 consumes A's visible chunk; A replenishes and re-queues behind B.
	res := e.Submit(fo(3, 20, 300, types.Buy, types.Market, types.GTC, 0, 2))
	if len(res.Fills) != 1 || res.Fills[0].Qty != 2 {
		t.Fatalf("fills = %+v, want one fill qty2 from iceberg", res.Fills)
	}
	front, ok := e.Book().FrontResting(types.Sell)
	if !ok || front.ID != 200 {
		t.Fatalf("front after replenish = %+v, want maker B (200) — A should be re-queued behind", front)
	}
}

// ---- Negative cases ----

func TestFOKInsufficientRejectsWithoutExecution(t *testing.T) {
	e, _ := newEngine()
	e.Submit(lim(1, 10, 100, types.Sell, 100, 3))
	res := e.Submit(fo(2, 20, 200, types.Buy, types.Limit, types.FOK, 100, 5))
	if !res.Rejected || res.Reason != types.ReasonFOKUnfillable {
		t.Fatalf("res = %+v, want Rejected FOKUnfillable", res)
	}
	if len(res.Fills) != 0 {
		t.Fatal("FOK reject must not execute any fills")
	}
	if q := e.Book().LevelQty(types.Sell, 100); q != 3 {
		t.Fatalf("book changed after FOK reject: ask qty %d, want 3", q)
	}
}

func TestPostOnlyCrossingRejects(t *testing.T) {
	e, _ := newEngine()
	e.Submit(lim(1, 10, 100, types.Sell, 100, 5))
	o := lim(2, 20, 200, types.Buy, 100, 5) // would cross
	o.Flags = types.FlagPostOnly
	res := e.Submit(o)
	if !res.Rejected || res.Reason != types.ReasonPostOnlyCross {
		t.Fatalf("res = %+v, want Rejected PostOnlyCross", res)
	}
	if res.Rested {
		t.Fatal("rejected post-only must not rest")
	}
	if q := e.Book().LevelQty(types.Sell, 100); q != 5 {
		t.Fatalf("resting ask changed: %d, want 5", q)
	}
}

func TestSelfTradePreventionCancelsAggressorRemainder(t *testing.T) {
	e, _ := newEngine()
	// Same account 10 on both sides.
	e.Submit(lim(1, 10, 100, types.Sell, 100, 5))
	res := e.Submit(lim(2, 10, 200, types.Buy, 100, 5))
	if !res.STP {
		t.Fatal("expected STP to fire on self-match")
	}
	if len(res.Fills) != 0 {
		t.Fatalf("STP must emit no fills, got %d", len(res.Fills))
	}
	if res.Rested {
		t.Fatal("aggressor remainder must be cancelled, not rested")
	}
	if q := e.Book().LevelQty(types.Sell, 100); q != 5 {
		t.Fatalf("resting order touched by STP: ask qty %d, want 5", q)
	}
}

// ---- Edge cases ----

func TestMarketOnEmptyBookCancels(t *testing.T) {
	e, _ := newEngine()
	res := e.Submit(fo(1, 20, 100, types.Buy, types.Market, types.GTC, 0, 5))
	if len(res.Fills) != 0 || res.Rested {
		t.Fatalf("market on empty book = %+v, want no fills, no rest", res)
	}
}

func TestMarketPartialFillCancelsRemainder(t *testing.T) {
	e, _ := newEngine()
	e.Submit(lim(1, 10, 100, types.Sell, 100, 2))
	res := e.Submit(fo(2, 20, 200, types.Buy, types.Market, types.GTC, 0, 5))
	if len(res.Fills) != 1 || res.Fills[0].Qty != 2 {
		t.Fatalf("fills = %+v, want one fill qty2", res.Fills)
	}
	if res.Rested {
		t.Fatal("market remainder must be cancelled")
	}
}

func TestLimitNoCrossJustRests(t *testing.T) {
	e, _ := newEngine()
	e.Submit(lim(1, 10, 100, types.Sell, 105, 5)) // ask above buy price
	res := e.Submit(lim(2, 20, 200, types.Buy, 100, 5))
	if len(res.Fills) != 0 {
		t.Fatalf("non-crossing limit should not fill, got %d fills", len(res.Fills))
	}
	if !res.Rested || res.RestedQty != 5 {
		t.Fatalf("rest = (%v,%d), want (true,5)", res.Rested, res.RestedQty)
	}
}

func TestPriceCrossBoundaryIsInclusive(t *testing.T) {
	e, _ := newEngine()
	e.Submit(lim(1, 10, 100, types.Sell, 100, 5))
	// Buy limit exactly at the ask price must cross (>=).
	res := e.Submit(lim(2, 20, 200, types.Buy, 100, 5))
	if len(res.Fills) != 1 {
		t.Fatalf("buy@100 vs ask@100 should match (inclusive), got %d fills", len(res.Fills))
	}
}

func TestMatchIndexFormsTotalOrderPerAggressor(t *testing.T) {
	e, _ := newEngine()
	e.Submit(lim(1, 10, 100, types.Sell, 100, 2)) // maker A
	e.Submit(lim(2, 11, 101, types.Sell, 100, 2)) // maker B (FIFO behind A)
	res := e.Submit(fo(7, 20, 700, types.Buy, types.Market, types.GTC, 0, 4))
	if len(res.Fills) != 2 {
		t.Fatalf("want 2 fills, got %d", len(res.Fills))
	}
	for i, f := range res.Fills {
		if f.AggressorSeq != 7 {
			t.Errorf("fill %d AggressorSeq = %d, want 7", i, f.AggressorSeq)
		}
		if f.MatchIndex != uint32(i) {
			t.Errorf("fill %d MatchIndex = %d, want %d", i, f.MatchIndex, i)
		}
	}
}
