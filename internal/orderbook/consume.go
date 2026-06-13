package orderbook

import "github.com/yanuar-ar/order-book/internal/types"

// RestingView is a read-only snapshot of a resting order, used by the matcher
// to decide how much to fill before mutating the book.
type RestingView struct {
	Idx       uint32
	ID        types.OrderID
	Account   types.AccountID
	Price     types.Price
	Display   types.Qty // quantity matchable right now
	Remaining types.Qty // total unfilled (visible + hidden)
}

// FrontResting returns the oldest order at the best level on the given side
// (the next order an aggressor on the opposite side would match against).
func (b *Book) FrontResting(side types.Side) (RestingView, bool) {
	var price types.Price
	var ok bool
	if side == types.Buy {
		price, ok = b.BestBid()
	} else {
		price, ok = b.BestAsk()
	}
	if !ok {
		return RestingView{}, false
	}
	lv := b.levelsFor(side)[price]
	n := &b.arena[lv.head]
	return RestingView{
		Idx:       lv.head,
		ID:        n.id,
		Account:   n.account,
		Price:     n.price,
		Display:   n.display,
		Remaining: n.remaining,
	}, true
}

// ConsumeFront fills qty against the front resting order at side's best level.
// qty must be > 0 and <= the order's current visible display. It returns true
// when the order leaves the book (fully filled). When an iceberg's visible
// portion is exhausted but hidden quantity remains, the order is replenished
// from hidden and re-queued at the back of its level (losing time priority),
// and false is returned.
func (b *Book) ConsumeFront(side types.Side, idx uint32, qty types.Qty) (removed bool) {
	n := &b.arena[idx]
	lv := b.levelsFor(side)[n.price]

	n.display -= qty
	n.remaining -= qty
	lv.totalQty -= qty

	if n.display > 0 {
		return false // partial fill of the visible portion
	}
	if n.hidden > 0 {
		// Replenish the visible chunk from hidden and re-queue at the tail.
		refill := n.peak
		if refill > n.hidden {
			refill = n.hidden
		}
		n.hidden -= refill
		n.display = refill
		lv.totalQty += refill
		b.moveToTail(side, idx)
		return false
	}
	// Fully consumed: detach and recycle. visibleQty already removed above.
	b.unlink(idx, n.side, n.price, 0)
	id := n.id
	b.release(idx)
	delete(b.idIndex, id)
	return true
}

// moveToTail relocates order idx to the back of its (non-empty) level.
func (b *Book) moveToTail(side types.Side, idx uint32) {
	lv := b.levelsFor(side)[b.arena[idx].price]
	if lv.tail == idx {
		return // already last
	}
	n := &b.arena[idx]
	// Detach from current position (it is not the tail).
	if n.prev != NilIdx {
		b.arena[n.prev].next = n.next
	} else {
		lv.head = n.next
	}
	if n.next != NilIdx {
		b.arena[n.next].prev = n.prev
	}
	// Append at tail.
	n.prev = lv.tail
	n.next = NilIdx
	b.arena[lv.tail].next = idx
	lv.tail = idx
}
