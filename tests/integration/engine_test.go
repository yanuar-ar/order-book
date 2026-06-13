// Package integration exercises the fully assembled engine across multiple
// markets with a shared balance authority.
package integration

import (
	"fmt"
	"strings"
	"testing"

	"github.com/yanuar-ar/order-book/internal/balance"
	"github.com/yanuar-ar/order-book/internal/market"
	"github.com/yanuar-ar/order-book/internal/types"
	"github.com/yanuar-ar/order-book/internal/wal"
)

const (
	btc  types.AssetID  = 1
	usdt types.AssetID  = 2
	eth  types.AssetID  = 3
	sol  types.AssetID  = 4
	mBTC types.MarketID = 0
	mETH types.MarketID = 1
	mSOL types.MarketID = 2
)

func specs() map[types.MarketID]balance.MarketSpec {
	return map[types.MarketID]balance.MarketSpec{
		mBTC: {Base: btc, Quote: usdt},
		mETH: {Base: eth, Quote: usdt},
		mSOL: {Base: sol, Quote: usdt},
	}
}

func cfg(maker, taker int64) market.Config {
	return market.Config{Markets: specs(), QtyScale: 1, FeeScale: 100, MakerFee: maker, TakerFee: taker, RingSize: 4096, CapHint: 1024}
}

func dep(acct types.AccountID, asset types.AssetID, amt int64) types.Command {
	return types.Command{Type: types.CmdDeposit, Account: acct, Asset: asset, Amount: amt}
}
func ord(m types.MarketID, acct types.AccountID, id types.OrderID, side types.Side, typ types.OrderType, tif types.TIF, price types.Price, qty types.Qty) types.Command {
	return types.Command{Type: types.CmdNewOrder, Market: m, Account: acct, OrderID: id, Side: side, OrdType: typ, Tif: tif, Price: price, Qty: qty}
}

func run(e *market.Engine, cmds ...types.Command) {
	for _, c := range cmds {
		e.Submit(c)
	}
	e.Drain()
}

// digest is a canonical, layout-independent snapshot of engine state.
func digest(e *market.Engine) string {
	var b strings.Builder
	bals, fees := e.Ledger().Dump()
	for _, x := range bals {
		fmt.Fprintf(&b, "B %d/%d a%d r%d\n", x.Acct, x.Asset, x.Available, x.Reserved)
	}
	for _, f := range fees {
		fmt.Fprintf(&b, "F %d %d\n", f.Asset, f.Amount)
	}
	for _, m := range e.MarketIDs() {
		for _, o := range e.Shard(m).Book().Dump() {
			fmt.Fprintf(&b, "O %d s%d %d %d@ r%d d%d\n", m, o.Side, o.ID, o.Price, o.Remaining, o.Display)
		}
		if lp, ok := e.Shard(m).Book().LastPrice(); ok {
			fmt.Fprintf(&b, "L %d %d\n", m, lp)
		}
	}
	return b.String()
}

// checkInvariants asserts the global invariants: no negative balances, value
// conservation per asset against net deposits, and no crossed books.
func checkInvariants(t *testing.T, e *market.Engine, deposited map[types.AssetID]int64) {
	t.Helper()
	bals, fees := e.Ledger().Dump()
	total := map[types.AssetID]int64{}
	for _, x := range bals {
		if x.Available < 0 || x.Reserved < 0 {
			t.Fatalf("negative balance: acct %d asset %d avail %d reserved %d", x.Acct, x.Asset, x.Available, x.Reserved)
		}
		total[x.Asset] += x.Available + x.Reserved
	}
	for _, f := range fees {
		if f.Amount < 0 {
			t.Fatalf("negative fee balance asset %d: %d", f.Asset, f.Amount)
		}
		total[f.Asset] += f.Amount
	}
	for asset, want := range deposited {
		if total[asset] != want {
			t.Fatalf("value not conserved for asset %d: total %d, want %d", asset, total[asset], want)
		}
	}
	for _, m := range e.MarketIDs() {
		bid, hasBid := e.Shard(m).Book().BestBid()
		ask, hasAsk := e.Shard(m).Book().BestAsk()
		if hasBid && hasAsk && bid >= ask {
			t.Fatalf("market %d crossed book: bid %d >= ask %d", m, bid, ask)
		}
	}
}

// AE7: shared quote currency cannot be reserved twice across markets.
func TestCrossMarketDoubleReserveRejected(t *testing.T) {
	e := market.NewEngine(cfg(0, 0))
	run(e,
		dep(1, usdt, 100),
		ord(mBTC, 1, 10, types.Buy, types.Limit, types.GTC, 100, 1), // reserves all 100 USDT, rests
		ord(mETH, 1, 11, types.Buy, types.Limit, types.GTC, 50, 1),  // needs 50 more -> rejected
	)
	if _, ok := e.Shard(mETH).Book().BestBid(); ok {
		t.Fatal("second order rested despite insufficient shared USDT")
	}
	if _, ok := e.Shard(mBTC).Book().BestBid(); !ok {
		t.Fatal("first order should rest")
	}
	last := e.Acks()[len(e.Acks())-1]
	if last.Status != types.AckRejected || last.Reason != types.ReasonInsufficientFunds {
		t.Fatalf("second order ack = %+v, want Rejected/InsufficientFunds", last)
	}
}

func TestOrderTypesAndInvariants(t *testing.T) {
	e := market.NewEngine(cfg(1, 2))
	deposited := map[types.AssetID]int64{usdt: 0, btc: 0}
	depUSDT := func(acct types.AccountID, amt int64) types.Command {
		deposited[usdt] += amt
		return dep(acct, usdt, amt)
	}
	depBTC := func(acct types.AccountID, amt int64) types.Command { deposited[btc] += amt; return dep(acct, btc, amt) }

	run(e,
		depBTC(2, 1000),
		depUSDT(1, 100000),
		depUSDT(3, 100000),
		// Resting asks to trade against.
		ord(mBTC, 2, 20, types.Sell, types.Limit, types.GTC, 100, 10),
		// FOK that cannot fully fill -> rejected, no execution.
		ord(mBTC, 1, 30, types.Buy, types.Limit, types.FOK, 100, 50),
		// IOC that partially fills then cancels remainder.
		ord(mBTC, 3, 40, types.Buy, types.Limit, types.IOC, 100, 4),
	)
	if q := e.Shard(mBTC).Book().LevelQty(types.Sell, 100); q != 6 {
		t.Fatalf("ask remaining after FOK-reject + IOC-4 = %d, want 6", q)
	}
	checkInvariants(t, e, deposited)
}

// A market buy against deep, cheap liquidity must be bounded by the buyer's
// funds — it can never out-spend its reservation, and value stays conserved.
func TestMarketBuyFundsCapConservation(t *testing.T) {
	e := market.NewEngine(cfg(0, 0)) // zero fee, QtyScale=1: notional = price*qty
	deposited := map[types.AssetID]int64{usdt: 50, btc: 1000}
	run(e,
		dep(2, btc, 1000),
		dep(1, usdt, 50),
		ord(mBTC, 2, 20, types.Sell, types.Limit, types.GTC, 1, 1000), // 1000 units @ price 1
		// Market buy for far more than the buyer can afford (only 50 USDT).
		types.Command{Type: types.CmdNewOrder, Market: mBTC, Account: 1, OrderID: 10, Side: types.Buy, OrdType: types.Market, Tif: types.GTC, Qty: 1000},
	)
	led := e.Ledger()
	if got := led.Available(1, btc); got != 50 {
		t.Fatalf("buyer BTC = %d, want 50 (capped by 50 USDT budget)", got)
	}
	if led.Available(1, usdt) != 0 {
		t.Fatalf("buyer USDT = %d, want 0 (fully spent within budget)", led.Available(1, usdt))
	}
	checkInvariants(t, e, deposited)
}

func TestRecoveryDeterminismViaReplay(t *testing.T) {
	dir := t.TempDir()
	w, err := wal.OpenWriter(dir, 0)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	cA := cfg(1, 2)
	cA.Journal = w
	eA := market.NewEngine(cA)

	stream1 := []types.Command{
		dep(1, usdt, 100000),
		dep(2, btc, 1000),
		dep(3, usdt, 100000),
		ord(mBTC, 2, 200, types.Sell, types.Limit, types.GTC, 106, 5), // maker
		ord(mBTC, 1, 100, types.Buy, types.Limit, types.GTC, 100, 3),  // rests (no cross)
		// buy-stop triggers when last >= 105 then runs as market buy
		{Type: types.CmdNewOrder, Market: mBTC, Account: 3, OrderID: 300, Side: types.Buy, OrdType: types.Stop, StopPrice: 105, Qty: 2, Tif: types.GTC},
		ord(mBTC, 3, 400, types.Buy, types.Limit, types.GTC, 106, 1), // trades @106 -> triggers stop
	}
	run(eA, stream1...)
	if err := w.Close(); err != nil {
		t.Fatalf("wal close: %v", err)
	}
	digestA := digest(eA)

	// Replay into a fresh engine with stops suppressed (activations are in the WAL).
	cB := cfg(1, 2)
	cB.SuppressStops = true
	eB := market.NewEngine(cB)
	if err := wal.Replay(dir, 0, func(rec wal.Record) error {
		cmd, derr := types.DecodeCommand(rec.Payload)
		if derr != nil {
			return derr
		}
		eB.ApplyJournaled(cmd)
		return nil
	}); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if digest(eB) != digestA {
		t.Fatalf("recovery mismatch:\n--- A ---\n%s\n--- B (replayed) ---\n%s", digestA, digest(eB))
	}

	// Follow-on guard: identical further input keeps the two engines equal.
	eB.EnableStops()
	stream2 := []types.Command{
		ord(mBTC, 3, 500, types.Sell, types.Limit, types.IOC, 100, 1), // hits resting bid 100
		{Type: types.CmdCancel, Market: mBTC, Account: 2, OrderID: 200},
	}
	run(eA, stream2...)
	run(eB, stream2...)
	if digest(eA) != digest(eB) {
		t.Fatalf("follow-on divergence:\n--- A ---\n%s\n--- B ---\n%s", digest(eA), digest(eB))
	}
}
