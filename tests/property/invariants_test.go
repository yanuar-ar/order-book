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

// legacyStream deterministically builds a deposit prelude and n random orders for
// a given seed. Pure function of (seed, n). Deposits and orders are returned
// separately so callers can apply all deposits before asserting conservation.
func legacyStream(seed int64, n int) (deposits, orders []types.Command, deposited map[types.AssetID]int64) {
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
	if err := CheckAllInvariants(e, deposited); err != nil {
		t.Fatal(err)
	}
}

// TestCheckAllInvariants_PassesOnHealthyEngine is the positive case: a funded
// engine with non-crossing resting orders satisfies every invariant.
func TestCheckAllInvariants_PassesOnHealthyEngine(t *testing.T) {
	e := market.NewEngine(engineConfig())
	deposited := map[types.AssetID]int64{}
	for a := types.AccountID(1); a <= 3; a++ {
		e.Submit(types.Command{Type: types.CmdDeposit, Account: a, Asset: usdt, Amount: 100_000})
		deposited[usdt] += 100_000
		for _, base := range marketBase {
			e.Submit(types.Command{Type: types.CmdDeposit, Account: a, Asset: base, Amount: 1_000})
			deposited[base] += 1_000
		}
	}
	e.Submit(types.Command{Type: types.CmdNewOrder, Market: 0, Account: 1, OrderID: 1, Side: types.Buy, OrdType: types.Limit, Tif: types.GTC, Price: 95, Qty: 2})
	e.Submit(types.Command{Type: types.CmdNewOrder, Market: 0, Account: 2, OrderID: 2, Side: types.Sell, OrdType: types.Limit, Tif: types.GTC, Price: 105, Qty: 2})
	e.Drain()
	if err := CheckAllInvariants(e, deposited); err != nil {
		t.Fatalf("healthy engine failed CheckAllInvariants: %v", err)
	}
}

// TestCheckAllInvariants_EmptyEngine is the edge case: nothing deposited or
// ordered.
func TestCheckAllInvariants_EmptyEngine(t *testing.T) {
	e := market.NewEngine(engineConfig())
	e.Drain()
	if err := CheckAllInvariants(e, map[types.AssetID]int64{}); err != nil {
		t.Fatalf("empty engine failed CheckAllInvariants: %v", err)
	}
}

// TestCheckAllInvariants_DetectsConservationBreak is the negative case: a
// netDeposits total that disagrees with ledger state trips INV-BAL-04.
func TestCheckAllInvariants_DetectsConservationBreak(t *testing.T) {
	e := market.NewEngine(engineConfig())
	e.Submit(types.Command{Type: types.CmdDeposit, Account: 1, Asset: usdt, Amount: 1000})
	e.Drain()
	err := CheckAllInvariants(e, map[types.AssetID]int64{usdt: 999})
	if err == nil || !strings.Contains(err.Error(), "INV-BAL-04") {
		t.Fatalf("expected INV-BAL-04, got %v", err)
	}
}

// TestRandomLoadHoldsInvariants drives random load, checking invariants
// incrementally and at the end.
func TestRandomLoadHoldsInvariants(t *testing.T) {
	deposits, orders, deposited := legacyStream(20260613, 3000)
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
	deposits, orders, _ := legacyStream(42, 2000)
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

// TestTightBudgetMarketLoadConserves stresses the market-buy funds cap: small
// quote balances plus a market-heavy flow means many market buys would
// out-spend their funds if uncapped. Conservation and no-negative must still
// hold at every checkpoint.
func TestTightBudgetMarketLoadConserves(t *testing.T) {
	r := rand.New(rand.NewSource(2026))
	e := market.NewEngine(engineConfig())
	deposited := map[types.AssetID]int64{}

	const accts = 6
	for a := types.AccountID(1); a <= accts; a++ {
		e.Submit(types.Command{Type: types.CmdDeposit, Account: a, Asset: usdt, Amount: 2000})
		deposited[usdt] += 2000
		for _, b := range marketBase {
			e.Submit(types.Command{Type: types.CmdDeposit, Account: a, Asset: b, Amount: 5000})
			deposited[b] += 5000
		}
	}
	e.Drain()
	checkInvariants(t, e, deposited)

	mkts := []types.MarketID{0, 1, 2}
	var id types.OrderID = 5000
	for i := 0; i < 4000; i++ {
		id++
		m := mkts[r.Intn(len(mkts))]
		acct := types.AccountID(1 + r.Intn(accts))
		side := types.Side(r.Intn(2))
		c := types.Command{Type: types.CmdNewOrder, Market: m, Account: acct, OrderID: id, Side: side, Price: types.Price(95 + r.Intn(11)), Qty: types.Qty(1 + r.Intn(40))}
		if r.Intn(2) == 0 { // half are market orders that could out-spend without the cap
			c.OrdType = types.Market
		} else {
			c.OrdType = types.Limit
		}
		e.Submit(c)
		if i%200 == 0 {
			e.Drain()
			checkInvariants(t, e, deposited)
		}
	}
	e.Drain()
	checkInvariants(t, e, deposited)
}

// TestDifferentSeedsDiverge is a sanity check that the generator and digest are
// actually sensitive to input (guards against a digest that ignores state).
func TestDifferentSeedsDiverge(t *testing.T) {
	d1, o1, _ := legacyStream(1, 1000)
	d2, o2, _ := legacyStream(2, 1000)
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
