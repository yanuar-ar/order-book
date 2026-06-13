package orderbook

import (
	"reflect"
	"testing"

	"github.com/yanuar-ar/order-book/internal/types"
)

// node returns the resting orderNode for an id (white-box helper).
func (b *Book) node(id types.OrderID) orderNode { return b.arena[b.idIndex[id]] }

// ---- Positive ----

func TestBookSnapshot_RoundTripPreservesFIFO(t *testing.T) {
	b := New(0, 16)
	// Two bid levels, two orders at the best bid to exercise FIFO ordering.
	b.Insert(NewResting{ID: 1, Account: 7, Side: types.Buy, Price: 100, Qty: 5})
	b.Insert(NewResting{ID: 2, Account: 7, Side: types.Buy, Price: 100, Qty: 3})
	b.Insert(NewResting{ID: 3, Account: 8, Side: types.Buy, Price: 99, Qty: 4})
	b.Insert(NewResting{ID: 4, Account: 9, Side: types.Sell, Price: 105, Qty: 6})
	b.SetLastPrice(101)

	b2 := New(0, 16)
	if err := b2.RestoreSnapshot(b.EncodeSnapshot()); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	if !reflect.DeepEqual(b.Dump(), b2.Dump()) {
		t.Fatalf("dump differs:\n want %+v\n got  %+v", b.Dump(), b2.Dump())
	}
	if lp, ok := b2.LastPrice(); lp != 101 || !ok {
		t.Fatalf("lastPrice = (%d,%v), want (101,true)", lp, ok)
	}
	if err := b2.Verify(); err != nil {
		t.Fatalf("restored book fails Verify (INV-OB-05): %v", err)
	}
}

// ---- Edge: iceberg mid-refill ----

func TestBookSnapshot_IcebergMidChunkReconstructsAllFields(t *testing.T) {
	b := New(0, 16)
	// Iceberg: total 10, visible chunk (peak) 3.
	b.Insert(NewResting{ID: 1, Account: 1, Side: types.Sell, Price: 50, Qty: 10, Display: 3, Flags: types.FlagIceberg})
	idx := b.idIndex[1]
	// Consume 1 of the visible 3 → display 2 (< peak 3), hidden still 7, remaining 9.
	b.ConsumeFront(types.Sell, idx, 1)

	pre := b.node(1)
	if pre.display != 2 || pre.peak != 3 || pre.hidden != 7 || pre.remaining != 9 {
		t.Fatalf("precondition wrong: %+v", pre)
	}

	b2 := New(0, 16)
	if err := b2.RestoreSnapshot(b.EncodeSnapshot()); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	got := b2.node(1)
	if got.display != 2 || got.peak != 3 || got.hidden != 7 || got.remaining != 9 {
		t.Fatalf("restored iceberg fields = %+v, want display2 peak3 hidden7 remaining9", got)
	}
	if got.display+got.hidden != got.remaining {
		t.Fatalf("invariant display+hidden==remaining violated: %+v", got)
	}
	// Next refill must use the original peak (3), not the partial display (2).
	idx2 := b2.idIndex[1]
	b2.ConsumeFront(types.Sell, idx2, 2) // exhaust visible 2 → refill from hidden
	after := b2.node(1)
	if after.display != 3 {
		t.Fatalf("refill chunk = %d, want original peak 3", after.display)
	}
}

// ---- Edge: empty + lastPrice variants ----

func TestBookSnapshot_EmptyRoundTrips(t *testing.T) {
	b := New(0, 16) // never traded
	b2 := New(0, 16)
	if err := b2.RestoreSnapshot(b.EncodeSnapshot()); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	if b2.Len() != 0 {
		t.Fatalf("empty book restored with %d orders", b2.Len())
	}
	if _, ok := b2.LastPrice(); ok {
		t.Fatal("empty/never-traded book restored with hasLast=true")
	}
	if err := b2.Verify(); err != nil {
		t.Fatalf("empty restored book fails Verify: %v", err)
	}
}

// ---- Negative ----

func TestBookSnapshot_TruncatedSectionRejected(t *testing.T) {
	b := New(0, 16)
	b.Insert(NewResting{ID: 1, Account: 1, Side: types.Buy, Price: 100, Qty: 5})
	full := b.EncodeSnapshot()
	for _, n := range []int{0, 5, len(full) - 1} {
		b2 := New(0, 16)
		if err := b2.RestoreSnapshot(full[:n]); err == nil {
			t.Fatalf("RestoreSnapshot accepted a truncated section of len %d", n)
		}
	}
}
