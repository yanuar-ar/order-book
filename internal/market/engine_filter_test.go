package market

import (
	"testing"

	"github.com/yanuar-ar/order-book/internal/balance"
	"github.com/yanuar-ar/order-book/internal/types"
)

// newFilterEng builds a single-market (BTC/USDT) engine with a known filter set.
// QtyScale is 1, so Notional(price, qty) == price*qty.
func newFilterEng(minNotional int64) *Engine {
	filters := map[types.MarketID]types.MarketFilters{
		m0: {
			TickSize: 10, MinPrice: 100, MaxPrice: 1000,
			StepSize: 5, MinQty: 10, MaxQty: 1000,
			MktStepSize: 5, MktMinQty: 10, MktMaxQty: 1000,
			MinNotional: minNotional, MaxNotional: 1_000_000,
		},
	}
	return NewEngine(Config{
		Markets: map[types.MarketID]balance.MarketSpec{m0: {Base: btc, Quote: usdt}},
		Filters: filters, QtyScale: 1, FeeScale: 100, RingSize: 1024, CapHint: 256,
	})
}

func ackFor(e *Engine, id types.OrderID) (types.Ack, bool) {
	for _, a := range e.Acks() {
		if a.OrderID == id {
			return a, true
		}
	}
	return types.Ack{}, false
}

func wantReject(t *testing.T, e *Engine, id types.OrderID, reason types.RejectReason) {
	t.Helper()
	a, ok := ackFor(e, id)
	if !ok {
		t.Fatalf("no ack for order %d", id)
	}
	if a.Status != types.AckRejected || a.Reason != reason {
		t.Fatalf("order %d ack = (status %d, reason %d), want (Rejected, reason %d)", id, a.Status, a.Reason, reason)
	}
}

func wantAccepted(t *testing.T, e *Engine, id types.OrderID) {
	t.Helper()
	a, ok := ackFor(e, id)
	if !ok {
		t.Fatalf("no ack for order %d", id)
	}
	if a.Status == types.AckRejected {
		t.Fatalf("order %d unexpectedly rejected (reason %d)", id, a.Reason)
	}
}

func iceberg(id types.OrderID, side types.Side, price types.Price, qty, display types.Qty) types.Command {
	return types.Command{Type: types.CmdNewOrder, Market: m0, Account: 1, OrderID: id, Side: side,
		OrdType: types.Limit, Tif: types.GTC, Flags: types.FlagIceberg, Price: price, Qty: qty, DisplayQty: display}
}

// TestNewOrderFilterRejections covers AE1, AE2, AE3, AE7, AE8 and market-lot.
func TestNewOrderFilterRejections(t *testing.T) {
	e := newFilterEng(2000)
	run(t, e,
		dep(1, usdt, 1_000_000),
		dep(1, btc, 1_000_000),
		order(m0, 1, 1, types.Buy, types.Limit, 105, 20), // AE1 off-tick price
		order(m0, 1, 2, types.Buy, types.Limit, 100, 12), // AE2 off-step qty
		order(m0, 1, 3, types.Buy, types.Limit, 100, 10), // AE3 notional 1000 < 2000
		types.Command{Type: types.CmdNewOrder, Market: m0, Account: 1, OrderID: 4, Side: types.Sell,
			OrdType: types.Market, Tif: types.GTC, Qty: 5}, // market below min lot
		types.Command{Type: types.CmdNewOrder, Market: m0, Account: 1, OrderID: 5, Side: types.Buy,
			OrdType: types.StopLimit, Tif: types.GTC, StopPrice: 105, Price: 100, Qty: 20}, // AE7 off-tick trigger
		iceberg(6, types.Sell, 100, 100, 5), // AE8 display below min
	)
	wantReject(t, e, 1, types.ReasonPriceFilter)
	wantReject(t, e, 2, types.ReasonLotSize)
	wantReject(t, e, 3, types.ReasonNotional)
	wantReject(t, e, 4, types.ReasonMarketLotSize)
	wantReject(t, e, 5, types.ReasonPriceFilter)
	wantReject(t, e, 6, types.ReasonLotSize)

	// No-mutation: every order was rejected, so no funds are reserved and the
	// book is empty.
	led := e.Ledger()
	if led.Reserved(1, usdt) != 0 || led.Reserved(1, btc) != 0 {
		t.Errorf("reservations leaked after rejections: usdt=%d btc=%d", led.Reserved(1, usdt), led.Reserved(1, btc))
	}
	if led.Available(1, usdt) != 1_000_000 || led.Available(1, btc) != 1_000_000 {
		t.Errorf("balances changed after rejections: usdt=%d btc=%d", led.Available(1, usdt), led.Available(1, btc))
	}
	if d := e.Shard(m0).Book().Depth(types.Buy, 5); len(d) != 0 {
		t.Errorf("rejected orders rested on the book: %+v", d)
	}
}

// TestNewOrderFilterAccepts covers AE4 (exact-boundary accept).
func TestNewOrderFilterAccepts(t *testing.T) {
	e := newFilterEng(2000)
	run(t, e,
		dep(1, usdt, 1_000_000),
		order(m0, 1, 10, types.Buy, types.Limit, 100, 20), // notional exactly 2000, qty on step
	)
	wantAccepted(t, e, 10)
	if d := e.Shard(m0).Book().Depth(types.Buy, 5); len(d) != 1 || d[0].Price != 100 {
		t.Errorf("accepted order did not rest: %+v", d)
	}
}

// TestMarketOrderNotionalFailOpen covers AE5: a market order on a never-traded
// market skips the notional check.
func TestMarketOrderNotionalFailOpen(t *testing.T) {
	e := newFilterEng(2000)
	run(t, e,
		dep(1, btc, 1_000_000),
		// Market sell qty 10 (>= mkt min, on step). No prior trade -> no reference
		// price -> notional skipped. Empty book -> no fills, accepted then canceled.
		types.Command{Type: types.CmdNewOrder, Market: m0, Account: 1, OrderID: 20, Side: types.Sell,
			OrdType: types.Market, Tif: types.GTC, Qty: 10},
	)
	wantAccepted(t, e, 20)
}
