package balance

import (
	"reflect"
	"testing"

	"github.com/yanuar-ar/order-book/internal/types"
)

func snapshotCfg() Config {
	return Config{
		QtyScale: 1, FeeScale: 100, MakerFee: 1, TakerFee: 2,
		Markets: map[types.MarketID]MarketSpec{0: {Base: 1, Quote: 2}},
	}
}

// ---- Positive ----

func TestLedgerSnapshot_RoundTripPreservesState(t *testing.T) {
	l := verifyTestLedger(t) // deposits, a buy reservation, a sell reservation
	l.fees[2] = 7            // a non-zero fee to exercise the fee section

	section := l.EncodeSnapshot()
	got, err := Restore(snapshotCfg(), section)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	wantBals, wantFees := l.Dump()
	gotBals, gotFees := got.Dump()
	if !reflect.DeepEqual(wantBals, gotBals) {
		t.Fatalf("balances differ:\n want %+v\n got  %+v", wantBals, gotBals)
	}
	if !reflect.DeepEqual(wantFees, gotFees) {
		t.Fatalf("fees differ:\n want %+v\n got  %+v", wantFees, gotFees)
	}
	if !reflect.DeepEqual(l.ReservedOrders(), got.ReservedOrders()) {
		t.Fatalf("reserved order set differs: %v vs %v", l.ReservedOrders(), got.ReservedOrders())
	}
	if err := got.Verify(); err != nil {
		t.Fatalf("restored ledger fails Verify: %v", err)
	}
}

// ---- Edge ----

func TestLedgerSnapshot_SettledUnreleasedReservationSurvives(t *testing.T) {
	l := New(snapshotCfg())
	l.Deposit(1, 1, 100) // base for the seller
	l.Deposit(2, 2, 100) // quote for the buyer
	// Seller reserves 4 base under order 11.
	if _, ok := l.Reserve(types.FundedOrder{Market: 0, Account: 1, OrderID: 11, Side: types.Sell, Qty: 4}); !ok {
		t.Fatal("sell reserve rejected")
	}
	// Buyer reserves quote under order 10 so the fill has both sides.
	if _, ok := l.Reserve(types.FundedOrder{Market: 0, Account: 2, OrderID: 10, Side: types.Buy, OrdType: types.Limit, Price: 5, Qty: 4}); !ok {
		t.Fatal("buy reserve rejected")
	}
	// Fully settle the sell: remaining 4 - 4 = 0, but the res entry stays until Release.
	l.Settle(types.Fill{Market: 0, Price: 5, Qty: 4, Taker: types.Buy, BuyOrder: 10, SellOrder: 11, BuyAccount: 2, SellAccount: 1})

	section := l.EncodeSnapshot()
	got, err := Restore(snapshotCfg(), section)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	// The zero-remaining reservation must have round-tripped: Release finds it.
	if !got.Release(11) {
		t.Fatal("Release(11) failed after restore — zero-remaining reservation was orphaned")
	}
}

func TestLedgerSnapshot_EmptyRoundTrips(t *testing.T) {
	l := New(snapshotCfg())
	got, err := Restore(snapshotCfg(), l.EncodeSnapshot())
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	b, f := got.Dump()
	if len(b) != 0 || len(f) != 0 || len(got.ReservedOrders()) != 0 {
		t.Fatalf("empty ledger did not round-trip to empty: bals=%v fees=%v res=%v", b, f, got.ReservedOrders())
	}
	if err := got.Verify(); err != nil {
		t.Fatalf("empty restored ledger fails Verify: %v", err)
	}
}

// No re-rounding: restored Reserved is byte-exact, and a follow-up AmendReduce
// behaves identically to the same operation on the original ledger.
func TestLedgerSnapshot_NoReRoundingDrift(t *testing.T) {
	build := func() *Ledger {
		l := New(snapshotCfg())
		l.Deposit(1, 2, 1000)
		l.Reserve(types.FundedOrder{Market: 0, Account: 1, OrderID: 10, Side: types.Buy, OrdType: types.Limit, Price: 7, Qty: 9})
		l.AmendReduce(10, types.Buy, 7, 5) // shrink once before snapshot
		return l
	}
	orig := build()
	got, err := Restore(snapshotCfg(), orig.EncodeSnapshot())
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got.Reserved(1, 2) != orig.Reserved(1, 2) {
		t.Fatalf("restored Reserved %d != original %d (re-rounding drift)", got.Reserved(1, 2), orig.Reserved(1, 2))
	}
	// Amend both again with identical args; results must stay identical.
	orig.AmendReduce(10, types.Buy, 7, 3)
	got.AmendReduce(10, types.Buy, 7, 3)
	if got.Reserved(1, 2) != orig.Reserved(1, 2) || got.Available(1, 2) != orig.Available(1, 2) {
		t.Fatalf("post-restore amend diverged: got {res %d avail %d} orig {res %d avail %d}",
			got.Reserved(1, 2), got.Available(1, 2), orig.Reserved(1, 2), orig.Available(1, 2))
	}
}

// ---- Negative ----

func TestLedgerSnapshot_TruncatedSectionRejected(t *testing.T) {
	l := verifyTestLedger(t)
	full := l.EncodeSnapshot()
	for _, n := range []int{0, 3, len(full) - 1} {
		if n < 0 {
			continue
		}
		if _, err := Restore(snapshotCfg(), full[:n]); err == nil {
			t.Fatalf("Restore accepted a truncated section of len %d", n)
		}
	}
}
