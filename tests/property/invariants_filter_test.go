package property

import (
	"strings"
	"testing"

	"github.com/yanuar-ar/order-book/internal/balance"
	"github.com/yanuar-ar/order-book/internal/market"
	"github.com/yanuar-ar/order-book/internal/types"
)

// filteredInvConfig is a single-market engine config with a known filter set.
func filteredInvConfig(minQty int64) market.Config {
	return market.Config{
		Markets: map[types.MarketID]balance.MarketSpec{0: {Base: 1, Quote: usdt}},
		Filters: map[types.MarketID]types.MarketFilters{
			0: {
				TickSize: 1, MinPrice: 1, MaxPrice: 1000,
				StepSize: 1, MinQty: minQty, MaxQty: 1000,
				MktStepSize: 1, MktMinQty: minQty, MktMaxQty: 1000,
				MinNotional: 0, MaxNotional: 100_000_000,
			},
		},
		QtyScale: 1, FeeScale: 100, RingSize: 1 << 12, CapHint: 256,
	}
}

func fund(e *market.Engine, dep map[types.AssetID]int64) {
	for a := types.AccountID(1); a <= 3; a++ {
		e.Submit(types.Command{Type: types.CmdDeposit, Account: a, Asset: usdt, Amount: 1_000_000})
		dep[usdt] += 1_000_000
		e.Submit(types.Command{Type: types.CmdDeposit, Account: a, Asset: 1, Amount: 10_000})
		dep[1] += 10_000
	}
}

// TestInvAri07_HealthyEngineWithFilters is the positive case.
func TestInvAri07_HealthyEngineWithFilters(t *testing.T) {
	e := market.NewEngine(filteredInvConfig(1))
	dep := map[types.AssetID]int64{}
	fund(e, dep)
	e.Submit(types.Command{Type: types.CmdNewOrder, Market: 0, Account: 1, OrderID: 1, Side: types.Buy, OrdType: types.Limit, Tif: types.GTC, Price: 95, Qty: 3})
	e.Submit(types.Command{Type: types.CmdNewOrder, Market: 0, Account: 2, OrderID: 2, Side: types.Sell, OrdType: types.Limit, Tif: types.GTC, Price: 105, Qty: 3})
	e.Drain()
	if err := CheckAllInvariants(e, dep); err != nil {
		t.Fatalf("healthy filtered engine failed invariants: %v", err)
	}
}

// TestInvAri07_PartialFillBelowMinQtyAllowed: a partial fill may leave a resting
// remainder below MinQty; that is valid resting state, not an INV-ARI-07
// violation.
func TestInvAri07_PartialFillBelowMinQtyAllowed(t *testing.T) {
	e := market.NewEngine(filteredInvConfig(2)) // MinQty = 2
	dep := map[types.AssetID]int64{}
	fund(e, dep)
	// Seller rests qty 3; buyer takes 2, leaving remaining 1 (< MinQty 2) resting.
	e.Submit(types.Command{Type: types.CmdNewOrder, Market: 0, Account: 1, OrderID: 1, Side: types.Sell, OrdType: types.Limit, Tif: types.GTC, Price: 100, Qty: 3})
	e.Submit(types.Command{Type: types.CmdNewOrder, Market: 0, Account: 2, OrderID: 2, Side: types.Buy, OrdType: types.Limit, Tif: types.GTC, Price: 100, Qty: 2})
	e.Drain()
	// Confirm a remainder of 1 is actually resting.
	d := e.Shard(0).Book().Dump()
	if len(d) != 1 || d[0].Remaining != 1 {
		t.Fatalf("expected a resting remainder of 1, got %+v", d)
	}
	if err := CheckAllInvariants(e, dep); err != nil {
		t.Fatalf("sub-MinQty remainder wrongly flagged: %v", err)
	}
}

// TestInvAri07_DetectsOffGridResting is the negative case: a resting order that
// no longer satisfies its market's filters trips INV-ARI-07. We rest a valid
// order, then tighten the market's tick size so the resting price is off-tick.
func TestInvAri07_DetectsOffGridResting(t *testing.T) {
	e := market.NewEngine(filteredInvConfig(1))
	dep := map[types.AssetID]int64{}
	fund(e, dep)
	e.Submit(types.Command{Type: types.CmdNewOrder, Market: 0, Account: 1, OrderID: 1, Side: types.Buy, OrdType: types.Limit, Tif: types.GTC, Price: 100, Qty: 3})
	e.Drain()
	if err := CheckAllInvariants(e, dep); err != nil {
		t.Fatalf("precondition: engine should be healthy, got %v", err)
	}
	// Tighten the tick size so the resting price 100 is now off-tick (100 % 7 != 0).
	flt := e.Filters()
	spec := flt[0]
	spec.TickSize = 7
	flt[0] = spec
	err := CheckAllInvariants(e, dep)
	if err == nil || !strings.Contains(err.Error(), "INV-ARI-07") {
		t.Fatalf("expected INV-ARI-07, got %v", err)
	}
}
