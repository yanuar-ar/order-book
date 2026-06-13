package orderbook

import (
	"strings"
	"testing"

	"github.com/yanuar-ar/order-book/internal/types"
)

// buildBook returns a well-formed book: two bid levels (one with two FIFO
// orders), one ask level, and an iceberg whose display < qty.
func buildBook(t *testing.T) *Book {
	t.Helper()
	b := New(0, 16)
	b.Insert(NewResting{ID: 1, Account: 1, Side: types.Buy, Price: 100, Qty: 5})
	b.Insert(NewResting{ID: 2, Account: 2, Side: types.Buy, Price: 100, Qty: 3}) // same level, FIFO after #1
	b.Insert(NewResting{ID: 3, Account: 3, Side: types.Buy, Price: 99, Qty: 4})
	b.Insert(NewResting{ID: 4, Account: 4, Side: types.Sell, Price: 101, Qty: 2})
	b.Insert(NewResting{ID: 5, Account: 5, Side: types.Sell, Price: 102, Qty: 10, Display: 2}) // iceberg
	return b
}

// ---- Positive ----

func TestVerify_WellFormedBookPasses(t *testing.T) {
	if err := buildBook(t).Verify(); err != nil {
		t.Fatalf("well-formed book failed Verify: %v", err)
	}
}

// ---- Edge ----

func TestVerify_EmptyBookPasses(t *testing.T) {
	if err := New(0, 16).Verify(); err != nil {
		t.Fatalf("empty book failed Verify: %v", err)
	}
}

func TestVerify_SingleOrderPasses(t *testing.T) {
	b := New(0, 16)
	b.Insert(NewResting{ID: 1, Account: 1, Side: types.Buy, Price: 100, Qty: 5})
	if err := b.Verify(); err != nil {
		t.Fatalf("single-order book failed Verify: %v", err)
	}
}

func TestVerify_LevelEmptiedByCancelPasses(t *testing.T) {
	b := New(0, 16)
	b.Insert(NewResting{ID: 1, Account: 1, Side: types.Buy, Price: 100, Qty: 5})
	b.Cancel(1) // empties and drops the level; slot returns to free-list
	if err := b.Verify(); err != nil {
		t.Fatalf("after cancel-to-empty, Verify failed: %v", err)
	}
	if len(b.bidPrices) != 0 || len(b.bidLevels) != 0 {
		t.Fatalf("level not dropped after emptying: prices=%v levels=%d", b.bidPrices, len(b.bidLevels))
	}
}

// ---- Negative: each subtest corrupts one invariant and expects its INV-ID ----

func TestVerify_DetectsCorruption(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(b *Book)
		wantID string
	}{
		{"price slice unsorted", func(b *Book) {
			b.bidPrices[0], b.bidPrices[1] = b.bidPrices[1], b.bidPrices[0]
		}, "INV-OB-02"},
		{"price/level count mismatch", func(b *Book) {
			b.bidPrices = append(b.bidPrices, 200)
		}, "INV-OB-02"},
		{"broken forward link orphans tail", func(b *Book) {
			head := b.idIndex[1]
			b.arena[head].next = NilIdx
		}, "INV-OB-03"},
		{"prev link mismatch", func(b *Book) {
			second := b.idIndex[2]
			b.arena[second].prev = NilIdx
		}, "INV-OB-03"},
		{"level total wrong", func(b *Book) {
			b.bidLevels[100].totalQty += 7
		}, "INV-OB-04"},
		{"leaked arena slot", func(b *Book) {
			b.arena = append(b.arena, orderNode{}) // neither reachable nor free
		}, "INV-OB-05"},
		{"slot both reachable and free", func(b *Book) {
			b.free = append(b.free, b.idIndex[1])
		}, "INV-OB-05"},
		{"duplicate free-list entry", func(b *Book) {
			// Force a free slot to exist, then duplicate it.
			b.Cancel(1)
			b.free = append(b.free, b.free[len(b.free)-1])
		}, "INV-OB-05"},
		{"stale idIndex entry", func(b *Book) {
			b.idIndex[999] = b.idIndex[1]
		}, "INV-OB-06"},
		{"zero remaining bound", func(b *Book) {
			b.arena[b.idIndex[1]].remaining = 0
		}, "INV-OB-09"},
		{"display exceeds remaining", func(b *Book) {
			b.arena[b.idIndex[1]].display = 99
		}, "INV-OB-09"},
		{"hidden inconsistent", func(b *Book) {
			b.arena[b.idIndex[5]].hidden = 0 // iceberg: remaining-display != 0
		}, "INV-OB-09"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := buildBook(t)
			if err := b.Verify(); err != nil {
				t.Fatalf("precondition: book should start valid, got %v", err)
			}
			tc.break_(b)
			err := b.Verify()
			if err == nil {
				t.Fatalf("expected %s violation, got nil", tc.wantID)
			}
			if !strings.Contains(err.Error(), tc.wantID) {
				t.Fatalf("expected %s, got: %v", tc.wantID, err)
			}
		})
	}
}
