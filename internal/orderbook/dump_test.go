package orderbook

import (
	"testing"

	"github.com/yanuar-ar/order-book/internal/types"
)

// ---- Depth: positive ----

func TestDepthBestFirstWithQty(t *testing.T) {
	b := New(0, 16)
	b.Insert(limit(1, types.Buy, 100, 5))
	b.Insert(limit(2, types.Buy, 101, 7)) // best bid
	b.Insert(limit(3, types.Buy, 99, 3))
	b.Insert(limit(4, types.Sell, 105, 4))
	b.Insert(limit(5, types.Sell, 104, 6)) // best ask
	b.Insert(limit(6, types.Sell, 106, 2))

	bids := b.Depth(types.Buy, 3)
	if len(bids) != 3 || bids[0].Price != 101 || bids[1].Price != 100 || bids[2].Price != 99 {
		t.Fatalf("bid depth = %+v, want 101,100,99 (best first)", bids)
	}
	if bids[0].Qty != 7 {
		t.Fatalf("best bid qty = %d, want 7", bids[0].Qty)
	}
	asks := b.Depth(types.Sell, 3)
	if len(asks) != 3 || asks[0].Price != 104 || asks[1].Price != 105 || asks[2].Price != 106 {
		t.Fatalf("ask depth = %+v, want 104,105,106 (best first)", asks)
	}
}

func TestDepthAggregatesQtyPerLevel(t *testing.T) {
	b := New(0, 16)
	b.Insert(limit(1, types.Buy, 100, 5))
	b.Insert(limit(2, types.Buy, 100, 8)) // same level
	d := b.Depth(types.Buy, 1)
	if len(d) != 1 || d[0].Qty != 13 {
		t.Fatalf("aggregated level qty = %+v, want 13", d)
	}
}

// ---- Depth: edge ----

func TestDepthEmptyBook(t *testing.T) {
	b := New(0, 16)
	if d := b.Depth(types.Buy, 5); len(d) != 0 {
		t.Fatalf("empty-book bid depth = %+v, want none", d)
	}
	if d := b.Depth(types.Sell, 5); len(d) != 0 {
		t.Fatalf("empty-book ask depth = %+v, want none", d)
	}
}

func TestDepthNZero(t *testing.T) {
	b := New(0, 16)
	b.Insert(limit(1, types.Buy, 100, 5))
	if d := b.Depth(types.Buy, 0); len(d) != 0 {
		t.Fatalf("Depth(n=0) = %+v, want none", d)
	}
}

func TestDepthNExceedsLevels(t *testing.T) {
	b := New(0, 16)
	b.Insert(limit(1, types.Buy, 100, 5))
	b.Insert(limit(2, types.Buy, 99, 5))
	if d := b.Depth(types.Buy, 50); len(d) != 2 {
		t.Fatalf("Depth(n>levels) = %d levels, want 2", len(d))
	}
}

// ---- Dump: canonical order ----

func TestDumpCanonicalOrder(t *testing.T) {
	b := New(0, 16)
	b.Insert(limit(1, types.Buy, 101, 1))
	b.Insert(limit(2, types.Buy, 100, 1))
	b.Insert(limit(3, types.Sell, 104, 1))
	b.Insert(limit(4, types.Sell, 105, 1))
	d := b.Dump()
	// bids ascending then asks ascending: 100,101,104,105
	if len(d) != 4 {
		t.Fatalf("dump len = %d, want 4", len(d))
	}
	wantPrices := []types.Price{100, 101, 104, 105}
	for i, rd := range d {
		if rd.Price != wantPrices[i] {
			t.Fatalf("dump[%d] price = %d, want %d (order %+v)", i, rd.Price, wantPrices[i], d)
		}
	}
}

func TestDumpEmpty(t *testing.T) {
	if d := New(0, 16).Dump(); len(d) != 0 {
		t.Fatalf("empty dump = %+v, want none", d)
	}
}
