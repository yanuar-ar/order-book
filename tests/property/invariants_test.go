// Package property runs randomized load against the engine and asserts global
// invariants hold at every step, plus same-seed determinism.
package property

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/yanuar-ar/order-book/internal/balance"
	"github.com/yanuar-ar/order-book/internal/market"
	"github.com/yanuar-ar/order-book/internal/types"
)

const (
	usdt        types.AssetID = 2
	numAccounts               = 8
	depositUSDT               = int64(1_000_000)
	depositBase               = int64(10_000)
)

// markets: three markets sharing USDT as quote.
var marketBase = map[types.MarketID]types.AssetID{
	0: 1, // BTC
	1: 3, // ETH
	2: 4, // SOL
}

func engineConfig() market.Config {
	specs := map[types.MarketID]balance.MarketSpec{}
	for m, base := range marketBase {
		specs[m] = balance.MarketSpec{Base: base, Quote: usdt}
	}
	return market.Config{Markets: specs, QtyScale: 1, FeeScale: 100, MakerFee: 1, TakerFee: 2, RingSize: 1 << 14, CapHint: 4096}
}

// genStream deterministically builds a deposit prelude and n random orders for
// a given seed. Pure function of (seed, n). Deposits and orders are returned
// separately so callers can apply all deposits before asserting conservation.
func genStream(seed int64, n int) (deposits, orders []types.Command, deposited map[types.AssetID]int64) {
	r := rand.New(rand.NewSource(seed))
	deposited = map[types.AssetID]int64{}

	for acct := types.AccountID(1); acct <= numAccounts; acct++ {
		deposits = append(deposits, types.Command{Type: types.CmdDeposit, Account: acct, Asset: usdt, Amount: depositUSDT})
		deposited[usdt] += depositUSDT
		for _, base := range marketBase {
			deposits = append(deposits, types.Command{Type: types.CmdDeposit, Account: acct, Asset: base, Amount: depositBase})
			deposited[base] += depositBase
		}
	}

	mkts := []types.MarketID{0, 1, 2}
	types_ := []types.OrderType{types.Limit, types.Market, types.Limit, types.Limit}
	tifs := []types.TIF{types.GTC, types.GTC, types.IOC, types.FOK}
	var id types.OrderID = 1000
	for i := 0; i < n; i++ {
		id++
		m := mkts[r.Intn(len(mkts))]
		acct := types.AccountID(1 + r.Intn(numAccounts))
		side := types.Side(r.Intn(2))
		ti := r.Intn(len(types_))
		ot := types_[ti]
		tif := tifs[ti]
		price := types.Price(95 + r.Intn(11)) // 95..105 around mid 100
		qty := types.Qty(1 + r.Intn(5))
		c := types.Command{Type: types.CmdNewOrder, Market: m, Account: acct, OrderID: id, Side: side, OrdType: ot, Tif: tif, Price: price, Qty: qty}
		if r.Intn(20) == 0 { // occasional post-only
			c.Flags = types.FlagPostOnly
		}
		orders = append(orders, c)
	}
	return deposits, orders, deposited
}

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
			fmt.Fprintf(&b, "O %d s%d %d %d r%d d%d\n", m, o.Side, o.ID, o.Price, o.Remaining, o.Display)
		}
	}
	return b.String()
}

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
			t.Fatalf("negative fees asset %d: %d", f.Asset, f.Amount)
		}
		total[f.Asset] += f.Amount
	}
	for asset, want := range deposited {
		if total[asset] != want {
			t.Fatalf("asset %d not conserved: total %d, want %d", asset, total[asset], want)
		}
	}
	for _, m := range e.MarketIDs() {
		bid, hb := e.Shard(m).Book().BestBid()
		ask, ha := e.Shard(m).Book().BestAsk()
		if hb && ha && bid >= ask {
			t.Fatalf("market %d crossed: bid %d >= ask %d", m, bid, ask)
		}
	}
}

// TestRandomLoadHoldsInvariants drives random load, checking invariants
// incrementally and at the end.
func TestRandomLoadHoldsInvariants(t *testing.T) {
	deposits, orders, deposited := genStream(20260613, 3000)
	e := market.NewEngine(engineConfig())
	for _, c := range deposits {
		e.Submit(c)
	}
	e.Drain()
	checkInvariants(t, e, deposited)
	for i, c := range orders {
		e.Submit(c)
		if i%200 == 0 {
			e.Drain()
			checkInvariants(t, e, deposited)
		}
	}
	e.Drain()
	checkInvariants(t, e, deposited)
}

// TestSameSeedProducesIdenticalState runs the same seed twice and asserts the
// final canonical state is identical.
func TestSameSeedProducesIdenticalState(t *testing.T) {
	deposits, orders, _ := genStream(42, 2000)
	all := append(append([]types.Command{}, deposits...), orders...)

	e1 := market.NewEngine(engineConfig())
	for _, c := range all {
		e1.Submit(c)
	}
	e1.Drain()

	e2 := market.NewEngine(engineConfig())
	for _, c := range all {
		e2.Submit(c)
	}
	e2.Drain()

	if digest(e1) != digest(e2) {
		t.Fatal("same-seed runs diverged: engine is not deterministic")
	}
}

// TestDifferentSeedsDiverge is a sanity check that the generator and digest are
// actually sensitive to input (guards against a digest that ignores state).
func TestDifferentSeedsDiverge(t *testing.T) {
	d1, o1, _ := genStream(1, 1000)
	d2, o2, _ := genStream(2, 1000)
	e1 := market.NewEngine(engineConfig())
	for _, c := range append(d1, o1...) {
		e1.Submit(c)
	}
	e1.Drain()
	e2 := market.NewEngine(engineConfig())
	for _, c := range append(d2, o2...) {
		e2.Submit(c)
	}
	e2.Drain()
	if digest(e1) == digest(e2) {
		t.Fatal("different seeds produced identical state — digest likely insensitive")
	}
}
