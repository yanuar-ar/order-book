package market

import (
	"testing"

	"github.com/yanuar-ar/order-book/internal/balance"
	"github.com/yanuar-ar/order-book/internal/types"
)

const (
	btc  types.AssetID  = 1
	usdt types.AssetID  = 2
	eth  types.AssetID  = 3
	m0   types.MarketID = 0 // BTC/USDT
	m1   types.MarketID = 1 // ETH/USDT
)

func newEng(maker, taker, feeScale int64, twoMarkets bool) *Engine {
	specs := map[types.MarketID]balance.MarketSpec{m0: {Base: btc, Quote: usdt}}
	if twoMarkets {
		specs[m1] = balance.MarketSpec{Base: eth, Quote: usdt}
	}
	return NewEngine(Config{
		Markets: specs, QtyScale: 1, FeeScale: feeScale,
		MakerFee: maker, TakerFee: taker, RingSize: 1024, CapHint: 256,
	})
}

func dep(acct types.AccountID, asset types.AssetID, amt int64) types.Command {
	return types.Command{Type: types.CmdDeposit, Account: acct, Asset: asset, Amount: amt}
}
func order(mkt types.MarketID, acct types.AccountID, id types.OrderID, side types.Side, typ types.OrderType, price types.Price, qty types.Qty) types.Command {
	return types.Command{Type: types.CmdNewOrder, Market: mkt, Account: acct, OrderID: id, Side: side, OrdType: typ, Tif: types.GTC, Price: price, Qty: qty}
}

func run(t *testing.T, e *Engine, cmds ...types.Command) {
	t.Helper()
	for _, c := range cmds {
		if !e.Submit(c) {
			t.Fatalf("ingress full submitting %+v", c)
		}
	}
	e.Drain()
}

// ---- U3: ack-release gate ----

func TestAckGateWithholdsUndurableAcks(t *testing.T) {
	e := newEng(0, 0, 100, false)
	cmds := []types.Command{dep(1, usdt, 1000), dep(2, btc, 10), dep(3, usdt, 500)}
	for _, c := range cmds {
		if !e.Submit(c) {
			t.Fatal("ingress full")
		}
	}
	// Step each command without draining. The flush cap is large and the ring is
	// emptied only by the final pop, so no flush fires mid-loop: durableSeq stays
	// 0 and every ack is speculative.
	for range cmds {
		e.Step()
	}
	if e.Seq() != 3 {
		t.Fatalf("Seq = %d after stepping 3 commands, want 3", e.Seq())
	}
	if got := len(e.Acks()); got != 0 {
		t.Fatalf("Acks() = %d before any flush, want 0 (all speculative above durableSeq)", got)
	}
	e.Drain() // ring already empty: the drain flush advances the watermark to Seq
	if got := len(e.Acks()); got != 3 {
		t.Fatalf("Acks() = %d after Drain, want 3 (all durable)", got)
	}
	for _, a := range e.Acks() {
		if a.Seq > e.Seq() {
			t.Fatalf("released ack Seq %d exceeds durableSeq", a.Seq)
		}
	}
}

func TestAckGateAppliesToRejections(t *testing.T) {
	// A rejected order still acks; the rejection ack is gated identically to an
	// accepted one (rejection moves no balance, so only the gate proves release).
	e := newEng(0, 0, 100, false)
	e.Submit(dep(1, usdt, 10))                                 // tiny balance
	e.Submit(order(m0, 1, 10, types.Buy, types.Limit, 100, 5)) // needs 500, rejected
	e.Step()
	e.Step()
	if got := len(e.Acks()); got != 0 {
		t.Fatalf("Acks() = %d before flush, want 0 (rejection ack also gated)", got)
	}
	e.Drain()
	acks := e.Acks()
	if len(acks) != 2 {
		t.Fatalf("Acks() = %d after Drain, want 2 (deposit + rejection)", len(acks))
	}
	if acks[1].Status != types.AckRejected {
		t.Fatalf("order ack status = %v, want AckRejected", acks[1].Status)
	}
}

// ---- Positive ----

func TestEndToEndDepositMatchSettle(t *testing.T) {
	e := newEng(0, 0, 100, false)
	run(t, e,
		dep(2, btc, 10),
		dep(1, usdt, 1000),
		order(m0, 2, 20, types.Sell, types.Limit, 100, 5),
		order(m0, 1, 10, types.Buy, types.Limit, 100, 5),
	)
	led := e.Ledger()
	if led.Available(1, btc) != 5 {
		t.Errorf("buyer BTC = %d, want 5", led.Available(1, btc))
	}
	if led.Available(1, usdt) != 500 {
		t.Errorf("buyer USDT = %d, want 500", led.Available(1, usdt))
	}
	if led.Available(2, usdt) != 500 {
		t.Errorf("seller USDT = %d, want 500", led.Available(2, usdt))
	}
	if led.Reserved(1, usdt) != 0 || led.Reserved(2, btc) != 0 {
		t.Errorf("reservations not cleared: buyer %d seller %d", led.Reserved(1, usdt), led.Reserved(2, btc))
	}
}

func TestStopReentryThroughSequencer(t *testing.T) {
	e := newEng(0, 0, 100, false)
	run(t, e,
		dep(1, usdt, 10000), // stop owner
		dep(2, btc, 100),    // maker
		dep(3, usdt, 10000), // trigger buyer
		order(m0, 2, 200, types.Sell, types.Limit, 106, 1), // hit by trigger trade
		order(m0, 2, 201, types.Sell, types.Limit, 106, 2), // liquidity for activated stop
		// buy-stop: triggers when last >= 105, then runs as market buy qty2
		types.Command{Type: types.CmdNewOrder, Market: m0, Account: 1, OrderID: 300, Side: types.Buy, OrdType: types.Stop, StopPrice: 105, Qty: 2, Tif: types.GTC},
		order(m0, 3, 400, types.Buy, types.Limit, 106, 1), // trades @106 -> lastPrice 106 -> stop fires
	)
	led := e.Ledger()
	if led.Available(1, btc) != 2 {
		t.Fatalf("stop owner BTC = %d, want 2 (stop activated and filled via re-injection)", led.Available(1, btc))
	}
	if led.Available(3, btc) != 1 {
		t.Fatalf("trigger buyer BTC = %d, want 1", led.Available(3, btc))
	}
	// 7 external commands + 1 re-injected activation = 8 sequenced.
	if e.Seq() != 8 {
		t.Fatalf("Seq = %d, want 8 (incl. one re-injected activation)", e.Seq())
	}
}

func TestRoutingToCorrectMarket(t *testing.T) {
	e := newEng(0, 0, 100, true)
	run(t, e,
		dep(1, usdt, 100000),
		order(m0, 1, 10, types.Buy, types.Limit, 100, 1), // rests in BTC/USDT
		order(m1, 1, 11, types.Buy, types.Limit, 50, 1),  // rests in ETH/USDT
	)
	if bid, ok := e.Shard(m0).Book().BestBid(); !ok || bid != 100 {
		t.Errorf("m0 best bid = %d (ok %v), want 100", bid, ok)
	}
	if bid, ok := e.Shard(m1).Book().BestBid(); !ok || bid != 50 {
		t.Errorf("m1 best bid = %d (ok %v), want 50", bid, ok)
	}
}

// ---- Negative ----

func TestInsufficientFundsRejected(t *testing.T) {
	e := newEng(0, 0, 100, false)
	run(t, e, order(m0, 1, 10, types.Buy, types.Limit, 100, 5)) // no deposit
	if _, ok := e.Shard(m0).Book().BestBid(); ok {
		t.Fatal("rejected order should not rest in the book")
	}
	acks := e.Acks()
	last := acks[len(acks)-1]
	if last.Status != types.AckRejected || last.Reason != types.ReasonInsufficientFunds {
		t.Fatalf("ack = %+v, want Rejected/InsufficientFunds", last)
	}
}

func TestCancelUnknownOrderRejected(t *testing.T) {
	e := newEng(0, 0, 100, false)
	run(t, e, types.Command{Type: types.CmdCancel, Market: m0, OrderID: 999})
	last := e.Acks()[0]
	if last.Status != types.AckRejected || last.Reason != types.ReasonUnknownOrder {
		t.Fatalf("ack = %+v, want Rejected/UnknownOrder", last)
	}
}

// ---- Edge ----

func TestCancelReleasesReservation(t *testing.T) {
	e := newEng(0, 0, 100, false)
	run(t, e,
		dep(1, usdt, 1000),
		order(m0, 1, 10, types.Buy, types.Limit, 100, 5), // reserves 500, rests
		types.Command{Type: types.CmdCancel, Market: m0, Account: 1, OrderID: 10},
	)
	led := e.Ledger()
	if led.Reserved(1, usdt) != 0 || led.Available(1, usdt) != 1000 {
		t.Fatalf("after cancel: avail %d reserved %d, want 1000/0", led.Available(1, usdt), led.Reserved(1, usdt))
	}
	if _, ok := e.Shard(m0).Book().BestBid(); ok {
		t.Fatal("cancelled order still resting")
	}
}

func TestConservationAcrossEngineWithFees(t *testing.T) {
	// taker 2%, maker 1% at feeScale 100. One trade of 10 @ 100 = notional 1000.
	e := newEng(1, 2, 100, false)
	run(t, e,
		dep(1, usdt, 1020), // buyer (taker): notional 1000 + 20 fee
		dep(2, btc, 10),    // seller (maker)
		order(m0, 2, 20, types.Sell, types.Limit, 100, 10),
		order(m0, 1, 10, types.Buy, types.Limit, 100, 10), // aggressor/taker
	)
	led := e.Ledger()
	totalUSDT := led.Available(1, usdt) + led.Reserved(1, usdt) +
		led.Available(2, usdt) + led.Reserved(2, usdt) + led.Fees(usdt)
	if totalUSDT != 1020 {
		t.Errorf("USDT not conserved: %d, want 1020", totalUSDT)
	}
	totalBTC := led.Available(1, btc) + led.Reserved(1, btc) + led.Available(2, btc) + led.Reserved(2, btc)
	if totalBTC != 10 {
		t.Errorf("BTC not conserved: %d, want 10", totalBTC)
	}
	if led.Fees(usdt) != 30 {
		t.Errorf("fees = %d, want 30 (20 taker + 10 maker)", led.Fees(usdt))
	}
}

func TestAmendQtyDownReleasesAndKeepsResting(t *testing.T) {
	e := newEng(0, 0, 100, false)
	run(t, e,
		dep(1, usdt, 1000),
		order(m0, 1, 10, types.Buy, types.Limit, 100, 5), // reserve 500, rests
		types.Command{Type: types.CmdAmend, Market: m0, Account: 1, OrderID: 10, Price: 100, Qty: 2},
	)
	led := e.Ledger()
	if led.Reserved(1, usdt) != 200 { // notional 100*2
		t.Errorf("reserved after amend-down = %d, want 200", led.Reserved(1, usdt))
	}
	if led.Available(1, usdt) != 800 {
		t.Errorf("available after amend-down = %d, want 800", led.Available(1, usdt))
	}
	if bid, ok := e.Shard(m0).Book().BestBid(); !ok || bid != 100 {
		t.Errorf("order should remain resting at 100, best bid ok=%v val=%d", ok, bid)
	}
}
