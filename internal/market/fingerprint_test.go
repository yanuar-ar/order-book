package market

import (
	"reflect"
	"testing"

	"github.com/yanuar-ar/order-book/internal/orderbook"
	"github.com/yanuar-ar/order-book/internal/types"
)

// withoutStop builds the same scenario as populated() but omits the pending stop.
func withoutStop(t *testing.T) *Engine {
	t.Helper()
	e := NewEngine(snapCfg(2))
	run(t, e,
		dep(2, btc, 100),
		dep(1, usdt, 100000),
		types.Command{Type: types.CmdNewOrder, Market: m0, Account: 2, OrderID: 20, Side: types.Sell, OrdType: types.Limit, Tif: types.GTC, Flags: types.FlagIceberg, Price: 100, Qty: 10, DisplayQty: 3},
		order(m0, 1, 10, types.Buy, types.Limit, 100, 4),
		order(m0, 1, 11, types.Buy, types.Limit, 90, 5),
	)
	return e
}

func TestStateFingerprint_EqualForEqualState(t *testing.T) {
	a := populated(t)
	b := populated(t)
	if !reflect.DeepEqual(a.StateFingerprint(), b.StateFingerprint()) {
		t.Fatal("equal states produced different fingerprints")
	}
}

// The fingerprint catches a pending stop (and its reservation) even though the
// two engines have identical resting books — exactly the hollowness the old
// book+ledger digest risked.
func TestStateFingerprint_DistinguishesStops(t *testing.T) {
	withStop := populated(t)
	noStop := withoutStop(t)
	if !reflect.DeepEqual(withStop.Shard(m0).Book().Dump(), noStop.Shard(m0).Book().Dump()) {
		t.Fatal("precondition: resting books should be identical with/without the stop")
	}
	if reflect.DeepEqual(withStop.StateFingerprint(), noStop.StateFingerprint()) {
		t.Fatal("fingerprint did not distinguish a pending stop")
	}
}

func TestStateFingerprint_DistinguishesOpenQty(t *testing.T) {
	a := populated(t)
	b := populated(t)
	// Mutate one open-order's tracked qty on b only.
	oo := b.core.open[11]
	oo.qty++
	b.core.open[11] = oo
	if reflect.DeepEqual(a.StateFingerprint(), b.StateFingerprint()) {
		t.Fatal("fingerprint did not distinguish a changed open.qty")
	}
}

// Two books that differ only in iceberg peak (identical display and remaining)
// must produce different fingerprints — the field the old digest could not see.
func TestStateFingerprint_DistinguishesIcebergPeak(t *testing.T) {
	a := NewEngine(snapCfg(2))
	b := NewEngine(snapCfg(2))
	a.Shard(m0).Book().InsertRestored(orderbook.RestoredOrder{ID: 1, Account: 1, Side: types.Sell, Price: 100, Remaining: 9, Display: 2, Hidden: 7, Peak: 3})
	b.Shard(m0).Book().InsertRestored(orderbook.RestoredOrder{ID: 1, Account: 1, Side: types.Sell, Price: 100, Remaining: 9, Display: 2, Hidden: 7, Peak: 4})
	if reflect.DeepEqual(a.StateFingerprint(), b.StateFingerprint()) {
		t.Fatal("fingerprint did not distinguish iceberg peak")
	}
}
