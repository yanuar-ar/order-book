package orderbook

import (
	"testing"

	"github.com/yanuar-ar/order-book/internal/types"
)

func limit(id types.OrderID, side types.Side, price types.Price, qty types.Qty) NewResting {
	return NewResting{ID: id, Account: types.AccountID(id), Side: side, Price: price, Qty: qty, Typ: types.Limit, Tif: types.GTC}
}

// levelOrder walks a level head->tail and returns the order ids in FIFO order.
func (b *Book) levelOrder(s types.Side, price types.Price) []types.OrderID {
	lv, ok := b.levelsFor(s)[price]
	if !ok {
		return nil
	}
	var ids []types.OrderID
	for idx := lv.head; idx != NilIdx; idx = b.arena[idx].next {
		ids = append(ids, b.arena[idx].id)
	}
	return ids
}

func TestInsertMaintainsFIFOPerLevel(t *testing.T) {
	b := New(0, 16)
	b.Insert(limit(1, types.Buy, 100, 10))
	b.Insert(limit(2, types.Buy, 100, 10))
	b.Insert(limit(3, types.Buy, 100, 10))
	got := b.levelOrder(types.Buy, 100)
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("FIFO order = %v, want [1 2 3]", got)
	}
	if q := b.LevelQty(types.Buy, 100); q != 30 {
		t.Fatalf("level qty = %d, want 30", q)
	}
}

func TestCancelRecyclesFreeListSlot(t *testing.T) {
	b := New(0, 16)
	b.Insert(limit(1, types.Buy, 100, 10))
	arenaLenBefore := len(b.arena)
	if _, _, _, ok := b.Cancel(1); !ok {
		t.Fatal("cancel of existing order failed")
	}
	if len(b.free) != 1 {
		t.Fatalf("free-list len = %d, want 1 after cancel", len(b.free))
	}
	// A subsequent insert reuses the freed slot rather than growing the arena.
	b.Insert(limit(2, types.Buy, 100, 10))
	if len(b.arena) != arenaLenBefore {
		t.Fatalf("arena grew to %d; expected slot reuse (was %d)", len(b.arena), arenaLenBefore)
	}
	if len(b.free) != 0 {
		t.Fatalf("free-list len = %d, want 0 after reuse", len(b.free))
	}
}

func TestBestBidAskUpdateOnInsertAndCancel(t *testing.T) {
	b := New(0, 16)
	if _, ok := b.BestBid(); ok {
		t.Fatal("empty book reported a best bid")
	}
	if _, ok := b.BestAsk(); ok {
		t.Fatal("empty book reported a best ask")
	}

	b.Insert(limit(1, types.Buy, 100, 5))
	b.Insert(limit(2, types.Buy, 101, 5)) // higher bid becomes best
	b.Insert(limit(3, types.Sell, 105, 5))
	b.Insert(limit(4, types.Sell, 104, 5)) // lower ask becomes best

	if bid, _ := b.BestBid(); bid != 101 {
		t.Fatalf("best bid = %d, want 101", bid)
	}
	if ask, _ := b.BestAsk(); ask != 104 {
		t.Fatalf("best ask = %d, want 104", ask)
	}

	// Cancelling the touch should move best to the next level.
	b.Cancel(2)
	b.Cancel(4)
	if bid, _ := b.BestBid(); bid != 100 {
		t.Fatalf("best bid after cancel = %d, want 100", bid)
	}
	if ask, _ := b.BestAsk(); ask != 105 {
		t.Fatalf("best ask after cancel = %d, want 105", ask)
	}
}

func TestCancelEmptiesLevelAndRemovesPrice(t *testing.T) {
	b := New(0, 16)
	b.Insert(limit(1, types.Buy, 100, 5))
	b.Cancel(1)
	if _, ok := b.bidLevels[100]; ok {
		t.Fatal("level not removed after last order cancelled")
	}
	if len(b.bidPrices) != 0 {
		t.Fatalf("bidPrices = %v, want empty", b.bidPrices)
	}
	if _, ok := b.BestBid(); ok {
		t.Fatal("best bid present after cancelling only order")
	}
}

func TestIDIndexConsistency(t *testing.T) {
	b := New(0, 16)
	b.Insert(limit(7, types.Sell, 200, 3))
	if !b.Has(7) {
		t.Fatal("Has(7) false after insert")
	}
	b.Cancel(7)
	if b.Has(7) {
		t.Fatal("Has(7) true after cancel")
	}
	if _, _, _, ok := b.Cancel(7); ok {
		t.Fatal("double cancel reported success")
	}
}

func TestAmendDownKeepsTimePriority(t *testing.T) {
	b := New(0, 16)
	b.Insert(limit(1, types.Buy, 100, 10))
	b.Insert(limit(2, types.Buy, 100, 10))
	if !b.AmendDown(1, 4) {
		t.Fatal("AmendDown failed")
	}
	got := b.levelOrder(types.Buy, 100)
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("order after amend = %v, want [1 2] (priority kept)", got)
	}
	// total visible qty: 4 + 10 = 14
	if q := b.LevelQty(types.Buy, 100); q != 14 {
		t.Fatalf("level qty after amend = %d, want 14", q)
	}
	if b.arena[b.idIndex[1]].remaining != 4 {
		t.Fatalf("remaining after amend = %d, want 4", b.arena[b.idIndex[1]].remaining)
	}
}

func TestAmendDownRejectsIncreaseOrEqual(t *testing.T) {
	b := New(0, 16)
	b.Insert(limit(1, types.Buy, 100, 10))
	if b.AmendDown(1, 10) {
		t.Fatal("AmendDown to equal qty should be rejected")
	}
	if b.AmendDown(1, 12) {
		t.Fatal("AmendDown to larger qty should be rejected")
	}
	if b.AmendDown(99, 1) {
		t.Fatal("AmendDown of missing order should be rejected")
	}
}

func TestIcebergDisplayHiddenSplit(t *testing.T) {
	b := New(0, 16)
	b.Insert(NewResting{ID: 1, Side: types.Buy, Price: 100, Qty: 10, Display: 2, Typ: types.Limit, Flags: types.FlagIceberg})
	n := b.arena[b.idIndex[1]]
	if n.display != 2 || n.hidden != 8 || n.remaining != 10 {
		t.Fatalf("iceberg split = display %d hidden %d remaining %d, want 2/8/10", n.display, n.hidden, n.remaining)
	}
	// Only the visible portion counts toward level total.
	if q := b.LevelQty(types.Buy, 100); q != 2 {
		t.Fatalf("iceberg level qty = %d, want 2 (visible only)", q)
	}
}
