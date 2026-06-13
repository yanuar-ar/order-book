package market

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/yanuar-ar/order-book/internal/balance"
	"github.com/yanuar-ar/order-book/internal/types"
)

// inspectable is satisfied by both *Engine and *ParallelEngine.
type inspectable interface {
	Ledger() *balance.Ledger
	Shard(types.MarketID) *Shard
	MarketIDs() []types.MarketID
}

type drivable interface {
	Submit(types.Command) bool
	Step() bool
	Drain()
}

func digestOf(e inspectable) string {
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
			fmt.Fprintf(&b, "O %d s%d %d %d r%d d%d\n", m, o.Side, o.ID, o.Price, o.Remaining, o.Display)
		}
		if lp, ok := e.Shard(m).Book().LastPrice(); ok {
			fmt.Fprintf(&b, "L %d %d\n", m, lp)
		}
	}
	return b.String()
}

func feed(d drivable, cmds []types.Command) {
	for _, c := range cmds {
		for !d.Submit(c) {
			d.Step() // ingress full: drain one, then retry (single-goroutine driver)
		}
	}
	d.Drain()
}

func parallelCfg() Config {
	return Config{
		Markets:  map[types.MarketID]balance.MarketSpec{m0: {Base: btc, Quote: usdt}, m1: {Base: eth, Quote: usdt}},
		QtyScale: 1, FeeScale: 100, MakerFee: 1, TakerFee: 2, RingSize: 4096, CapHint: 1024,
	}
}

// buildStream: deposits + random orders across both markets, deterministic.
func buildStream(seed int64, n int) (cmds []types.Command, deposited map[types.AssetID]int64) {
	r := rand.New(rand.NewSource(seed))
	deposited = map[types.AssetID]int64{}
	for a := types.AccountID(1); a <= 6; a++ {
		cmds = append(cmds, dep(a, usdt, 1_000_000))
		deposited[usdt] += 1_000_000
		cmds = append(cmds, dep(a, btc, 100_000))
		deposited[btc] += 100_000
		cmds = append(cmds, dep(a, eth, 100_000))
		deposited[eth] += 100_000
	}
	mkts := []types.MarketID{m0, m1}
	var id types.OrderID = 1000
	for i := 0; i < n; i++ {
		id++
		mk := mkts[r.Intn(len(mkts))]
		side := types.Side(r.Intn(2))
		typ, tif := types.Limit, types.GTC
		switch x := r.Intn(100); {
		case x < 12:
			typ = types.Market
		case x < 18:
			tif = types.IOC
		}
		cmds = append(cmds, types.Command{
			Type: types.CmdNewOrder, Market: mk, Account: types.AccountID(1 + r.Intn(6)),
			OrderID: id, Side: side, OrdType: typ, Tif: tif,
			Price: types.Price(95 + r.Intn(11)), Qty: types.Qty(1 + r.Intn(5)),
		})
	}
	return cmds, deposited
}

func checkInv(t *testing.T, e inspectable, deposited map[types.AssetID]int64) {
	t.Helper()
	bals, fees := e.Ledger().Dump()
	total := map[types.AssetID]int64{}
	for _, x := range bals {
		if x.Available < 0 || x.Reserved < 0 {
			t.Fatalf("negative balance acct %d asset %d: %d/%d", x.Acct, x.Asset, x.Available, x.Reserved)
		}
		total[x.Asset] += x.Available + x.Reserved
	}
	for _, f := range fees {
		total[f.Asset] += f.Amount
	}
	for a, want := range deposited {
		if total[a] != want {
			t.Fatalf("asset %d not conserved: %d, want %d", a, total[a], want)
		}
	}
}

// ---- positive: parallel == serial ----

func TestParallelMatchesSerialIsolated(t *testing.T) {
	cmds, deposited := buildStream(11, 5000)

	serial := NewEngine(parallelCfg())
	feed(serial, cmds)

	par := NewParallelEngine(parallelCfg(), [][]types.MarketID{{m0}, {m1}})
	feed(par, cmds)
	par.Close()

	if digestOf(serial) != digestOf(par) {
		t.Fatal("parallel (isolated workers) diverged from serial")
	}
	checkInv(t, par, deposited)
}

func TestParallelMatchesSerialShared(t *testing.T) {
	cmds, _ := buildStream(11, 5000)

	serial := NewEngine(parallelCfg())
	feed(serial, cmds)

	// Both markets share one worker; result must still equal serial.
	par := NewParallelEngine(parallelCfg(), [][]types.MarketID{{m0, m1}})
	feed(par, cmds)
	par.Close()

	if digestOf(serial) != digestOf(par) {
		t.Fatal("parallel (shared worker) diverged from serial")
	}
}

func TestParallelDefaultGroupingMatchesSerial(t *testing.T) {
	cmds, _ := buildStream(99, 3000)
	serial := NewEngine(parallelCfg())
	feed(serial, cmds)
	par := NewParallelEngine(parallelCfg(), nil) // default: one worker per market
	feed(par, cmds)
	par.Close()
	if digestOf(serial) != digestOf(par) {
		t.Fatal("parallel (default grouping) diverged from serial")
	}
}

// ---- negative / edge ----

func TestParallelInsufficientFundsRejected(t *testing.T) {
	par := NewParallelEngine(parallelCfg(), [][]types.MarketID{{m0}, {m1}})
	defer par.Close()
	feed(par, []types.Command{order(m0, 1, 10, types.Buy, types.Limit, 100, 5)}) // no deposit
	if _, ok := par.Shard(m0).Book().BestBid(); ok {
		t.Fatal("rejected order rested in the book")
	}
	last := par.Acks()[len(par.Acks())-1]
	if last.Status != types.AckRejected || last.Reason != types.ReasonInsufficientFunds {
		t.Fatalf("ack = %+v, want Rejected/InsufficientFunds", last)
	}
}

func TestParallelCancelUnknownRejected(t *testing.T) {
	par := NewParallelEngine(parallelCfg(), [][]types.MarketID{{m0}, {m1}})
	defer par.Close()
	feed(par, []types.Command{{Type: types.CmdCancel, Market: m0, OrderID: 777}})
	if par.Acks()[0].Reason != types.ReasonUnknownOrder {
		t.Fatalf("ack = %+v, want UnknownOrder", par.Acks()[0])
	}
}

func TestParallelCrossMarketReserveReject(t *testing.T) {
	par := NewParallelEngine(parallelCfg(), [][]types.MarketID{{m0}, {m1}})
	defer par.Close()
	feed(par, []types.Command{
		dep(1, usdt, 102), // exactly covers one buy @100 + 2% taker fee
		order(m0, 1, 10, types.Buy, types.Limit, 100, 1), // reserves all 102 USDT
		order(m1, 1, 11, types.Buy, types.Limit, 50, 1),  // shared USDT exhausted -> reject
	})
	if _, ok := par.Shard(m1).Book().BestBid(); ok {
		t.Fatal("second cross-market order rested despite exhausted shared balance")
	}
}
