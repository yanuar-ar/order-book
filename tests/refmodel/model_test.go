package refmodel

import (
	"testing"

	"github.com/yanuar-ar/order-book/internal/types"
)

const (
	base  types.AssetID = 1
	quote types.AssetID = 2
)

func setup() *Model {
	return New(Config{
		QtyScale: 1, FeeScale: 100, MakerFee: 1, TakerFee: 2,
		Markets: map[types.MarketID]MarketSpec{0: {Base: base, Quote: quote}},
	})
}

func deposit(m *Model, acct types.AccountID, asset types.AssetID, amt int64) {
	m.Apply(types.Command{Type: types.CmdDeposit, Account: acct, Asset: asset, Amount: amt})
}

func newOrderCmd(id types.OrderID, acct types.AccountID, side types.Side, typ types.OrderType, tif types.TIF, price, qty types.Price) types.Command {
	return types.Command{Type: types.CmdNewOrder, Market: 0, OrderID: id, Account: acct, Side: side, OrdType: typ, Tif: tif, Price: types.Price(price), Qty: types.Qty(qty)}
}

func (m *Model) availOf(a types.AccountID, asset types.AssetID) int64 {
	return m.bal[acctKey{a, asset}].available
}
func (m *Model) reservedOf(a types.AccountID, asset types.AssetID) int64 {
	return m.bal[acctKey{a, asset}].reserved
}

// conserves asserts per-asset conservation: Σ(available+reserved) + fees == net deposits.
func (m *Model) conserves(t *testing.T, asset types.AssetID, netDeposit int64) {
	t.Helper()
	var total int64
	for k, b := range m.bal {
		if k.asset == asset {
			total += b.available + b.reserved
		}
	}
	total += m.fees[asset]
	if total != netDeposit {
		t.Fatalf("asset %d not conserved: total %d, want %d", asset, total, netDeposit)
	}
}

func (m *Model) restingCount() int {
	n := 0
	for _, b := range m.books {
		n += len(*b)
	}
	return n
}

// ---- Positive ----

func TestModel_LimitCrossFillsAtMakerPrice(t *testing.T) {
	m := setup()
	deposit(m, 1, base, 100)    // seller
	deposit(m, 2, quote, 10000) // buyer
	m.Apply(newOrderCmd(1, 1, types.Sell, types.Limit, types.GTC, 100, 5))
	m.Apply(newOrderCmd(2, 2, types.Buy, types.Limit, types.GTC, 105, 5)) // crosses, fills at maker 100

	if m.restingCount() != 0 {
		t.Fatalf("book should be empty after full cross, have %d resting", m.restingCount())
	}
	if got := m.availOf(2, base); got != 5 {
		t.Fatalf("buyer base = %d, want 5", got)
	}
	if got := m.availOf(1, base); got != 95 {
		t.Fatalf("seller base = %d, want 95", got)
	}
	if m.reservedOf(1, base) != 0 || m.reservedOf(2, quote) != 0 {
		t.Fatalf("reservations not released: sellerBase=%d buyerQuote=%d", m.reservedOf(1, base), m.reservedOf(2, quote))
	}
	m.conserves(t, base, 100)
	m.conserves(t, quote, 10000)
}

func TestModel_PartialFillRestsRemainder(t *testing.T) {
	m := setup()
	deposit(m, 1, base, 100)
	deposit(m, 2, quote, 10000)
	m.Apply(newOrderCmd(1, 1, types.Sell, types.Limit, types.GTC, 100, 3))
	m.Apply(newOrderCmd(2, 2, types.Buy, types.Limit, types.GTC, 100, 5)) // fills 3, rests 2

	if m.restingCount() != 1 {
		t.Fatalf("want 1 resting (buyer remainder), have %d", m.restingCount())
	}
	if got := m.availOf(2, base); got != 3 {
		t.Fatalf("buyer base = %d, want 3", got)
	}
	snap := m.Snapshot()
	if len(snap.Orders) != 1 || snap.Orders[0].ID != 2 || snap.Orders[0].Remaining != 2 || snap.Orders[0].Side != types.Buy {
		t.Fatalf("resting remainder = %+v, want buy id2 remaining2", snap.Orders)
	}
	m.conserves(t, base, 100)
	m.conserves(t, quote, 10000)
}

func TestModel_IcebergSweepsHiddenSlices(t *testing.T) {
	m := setup()
	deposit(m, 1, base, 100)
	deposit(m, 2, quote, 10000)
	// Iceberg sell: total 10, display 2.
	ice := newOrderCmd(1, 1, types.Sell, types.Limit, types.GTC, 100, 10)
	ice.Flags = types.FlagIceberg
	ice.DisplayQty = 2
	m.Apply(ice)
	m.Apply(newOrderCmd(2, 2, types.Buy, types.Limit, types.GTC, 100, 10)) // sweeps all hidden slices

	if m.restingCount() != 0 {
		t.Fatalf("iceberg should be fully consumed, have %d resting", m.restingCount())
	}
	if got := m.availOf(2, base); got != 10 {
		t.Fatalf("buyer base = %d, want 10 (all slices swept)", got)
	}
	m.conserves(t, base, 100)
	m.conserves(t, quote, 10000)
}

// ---- Negative ----

func TestModel_FOKOneShortRejectsWithNoStateChange(t *testing.T) {
	m := setup()
	deposit(m, 1, base, 100)
	deposit(m, 2, quote, 10000)
	m.Apply(newOrderCmd(1, 1, types.Sell, types.Limit, types.GTC, 100, 4)) // only 4 available
	m.Apply(newOrderCmd(2, 2, types.Buy, types.Limit, types.FOK, 100, 5))  // needs 5 -> kill

	if got := m.availOf(2, base); got != 0 {
		t.Fatalf("FOK should not fill: buyer base = %d, want 0", got)
	}
	if m.reservedOf(2, quote) != 0 {
		t.Fatalf("FOK reservation not released: %d", m.reservedOf(2, quote))
	}
	if m.availOf(2, quote) != 10000 {
		t.Fatalf("buyer quote = %d, want 10000 (untouched)", m.availOf(2, quote))
	}
	snap := m.Snapshot()
	if len(snap.Orders) != 1 || snap.Orders[0].ID != 1 {
		t.Fatalf("only the seller order should rest, got %+v", snap.Orders)
	}
}

func TestModel_PostOnlyCrossRejected(t *testing.T) {
	m := setup()
	deposit(m, 1, base, 100)
	deposit(m, 2, quote, 10000)
	m.Apply(newOrderCmd(1, 1, types.Sell, types.Limit, types.GTC, 100, 4))
	po := newOrderCmd(2, 2, types.Buy, types.Limit, types.GTC, 100, 5) // would cross at 100
	po.Flags = types.FlagPostOnly
	m.Apply(po)

	if m.availOf(2, base) != 0 || m.reservedOf(2, quote) != 0 || m.availOf(2, quote) != 10000 {
		t.Fatalf("post-only cross should leave buyer untouched: base=%d resQuote=%d availQuote=%d",
			m.availOf(2, base), m.reservedOf(2, quote), m.availOf(2, quote))
	}
	if m.restingCount() != 1 {
		t.Fatalf("only the seller should rest, have %d", m.restingCount())
	}
}

// ---- Edge ----

func TestModel_MarketOnEmptyBookFillsNothing(t *testing.T) {
	m := setup()
	deposit(m, 2, quote, 10000)
	m.Apply(newOrderCmd(2, 2, types.Buy, types.Market, types.GTC, 0, 5)) // no opposite liquidity

	if m.restingCount() != 0 {
		t.Fatalf("market order must not rest, have %d", m.restingCount())
	}
	if m.availOf(2, quote) != 10000 || m.reservedOf(2, quote) != 0 {
		t.Fatalf("market-on-empty should release reservation: avail=%d reserved=%d", m.availOf(2, quote), m.reservedOf(2, quote))
	}
}

func TestModel_SelfTradePreventionCancelsAggressor(t *testing.T) {
	m := setup()
	deposit(m, 1, base, 100)
	deposit(m, 1, quote, 10000)
	m.Apply(newOrderCmd(1, 1, types.Sell, types.Limit, types.GTC, 100, 5)) // own resting sell
	m.Apply(newOrderCmd(2, 1, types.Buy, types.Limit, types.GTC, 105, 5))  // own buy -> STP, no fill

	if m.availOf(1, base) != 95 { // 5 still reserved on the sell, nothing traded
		t.Fatalf("base available = %d, want 95 (only the resting sell reserved)", m.availOf(1, base))
	}
	if m.reservedOf(1, quote) != 0 {
		t.Fatalf("aggressor buy reservation should be released after STP, reserved=%d", m.reservedOf(1, quote))
	}
	snap := m.Snapshot()
	if len(snap.Orders) != 1 || snap.Orders[0].ID != 1 {
		t.Fatalf("only the resting sell should remain, got %+v", snap.Orders)
	}
	m.conserves(t, base, 100)
	m.conserves(t, quote, 10000)
}
