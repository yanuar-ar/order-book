package property

import (
	"testing"

	"github.com/yanuar-ar/order-book/internal/types"
)

// TestDifferentialFilterRejections runs a hand-built stream of on-grid and
// off-grid orders through the engine and the reference model and asserts they
// stay in lockstep — both must accept the valid orders and reject the
// filter-violating ones identically (parity is what makes the oracle trustworthy
// for the filter feature). genFilters() applies MinQty=2, MinNotional=200, and a
// market lot floor of 2.
func TestDifferentialFilterRejections(t *testing.T) {
	deposits, net := standardPrelude()
	mkt := genMarkets[0]
	var id types.OrderID = 5000
	next := func() types.OrderID { id++; return id }

	lim := func(acct types.AccountID, side types.Side, price types.Price, qty types.Qty) types.Command {
		return types.Command{Type: types.CmdNewOrder, Market: mkt, Account: acct, OrderID: next(),
			Side: side, OrdType: types.Limit, Tif: types.GTC, Price: price, Qty: qty}
	}
	mktSell := func(acct types.AccountID, qty types.Qty) types.Command {
		return types.Command{Type: types.CmdNewOrder, Market: mkt, Account: acct, OrderID: next(),
			Side: types.Sell, OrdType: types.Market, Tif: types.GTC, Qty: qty}
	}

	orders := []types.Command{
		lim(1, types.Buy, 100, 5),  // valid: notional 500, qty>=2
		lim(2, types.Sell, 100, 5), // valid: crosses -> trade, sets last price
		lim(1, types.Buy, 100, 1),  // reject: qty 1 < MinQty 2 (ReasonLotSize)
		lim(1, types.Buy, 90, 2),   // reject: notional 180 < 200 (ReasonNotional)
		mktSell(3, 1),              // reject: market qty 1 < MktMinQty 2
		mktSell(3, 4),              // valid market sell (qty>=2); notional via last price
		lim(2, types.Sell, 101, 3), // valid: rests
	}

	stream := Stream{Deposits: deposits, Orders: orders, NetDeposits: net}
	if err := RunDifferential(stream); err != nil {
		t.Fatalf("engine and model diverged on filtered stream: %v", err)
	}
}
