// Package orderbook implements a per-market limit order book.
//
// Orders live in a preallocated arena addressed by uint32 indices (no pointers
// in the hot data), linked into intrusive FIFO lists per price level. Price
// levels are tracked in a map keyed by price with a per-side sorted price slice
// for best-price traversal; the bounded tick-ladder optimization is deferred to
// the performance phase. Cancelled slots are recycled through a free-list.
package orderbook

import (
	"sort"

	"github.com/yanuar-ar/order-book/internal/types"
)

// NilIdx marks the absence of an arena slot (list terminator / empty level).
const NilIdx uint32 = 0xFFFFFFFF

// orderNode is one resting order. Fixed-size, pointer-free.
type orderNode struct {
	id        types.OrderID
	account   types.AccountID
	price     types.Price
	remaining types.Qty // total unfilled quantity (visible + hidden)
	display   types.Qty // currently visible quantity
	hidden    types.Qty // reserve quantity for iceberg replenishment
	peak      types.Qty // iceberg visible-chunk size used when replenishing
	side      types.Side
	typ       types.OrderType
	tif       types.TIF
	flags     types.Flags
	next      uint32 // newer order in the same level, or NilIdx
	prev      uint32 // older order in the same level, or NilIdx
}

// level is a FIFO queue of orders resting at one price.
type level struct {
	price    types.Price
	head     uint32    // oldest order
	tail     uint32    // newest order
	totalQty types.Qty // sum of visible (display) quantity
}

// NewResting describes an order to rest in the book.
type NewResting struct {
	ID      types.OrderID
	Account types.AccountID
	Side    types.Side
	Price   types.Price
	Qty     types.Qty // total quantity to rest
	Display types.Qty // visible portion; <=0 or >=Qty means fully visible
	Typ     types.OrderType
	Tif     types.TIF
	Flags   types.Flags
}

// Book is a single market's order book.
type Book struct {
	Market types.MarketID

	arena []orderNode
	free  []uint32

	bidLevels map[types.Price]*level
	askLevels map[types.Price]*level
	bidPrices []types.Price // sorted ascending; best bid is the last element
	askPrices []types.Price // sorted ascending; best ask is the first element

	idIndex map[types.OrderID]uint32

	lastPrice types.Price
	hasLast   bool
}

// New returns an empty book for market with arena capacity hint cap.
func New(market types.MarketID, capHint int) *Book {
	if capHint < 16 {
		capHint = 16
	}
	return &Book{
		Market:    market,
		arena:     make([]orderNode, 0, capHint),
		free:      make([]uint32, 0, capHint),
		bidLevels: make(map[types.Price]*level, capHint),
		askLevels: make(map[types.Price]*level, capHint),
		bidPrices: make([]types.Price, 0, capHint),
		askPrices: make([]types.Price, 0, capHint),
		idIndex:   make(map[types.OrderID]uint32, capHint),
	}
}

func (b *Book) levelsFor(s types.Side) map[types.Price]*level {
	if s == types.Buy {
		return b.bidLevels
	}
	return b.askLevels
}

// alloc returns a free arena slot, reusing the free-list when possible.
func (b *Book) alloc() uint32 {
	if n := len(b.free); n > 0 {
		idx := b.free[n-1]
		b.free = b.free[:n-1]
		return idx
	}
	b.arena = append(b.arena, orderNode{})
	return uint32(len(b.arena) - 1)
}

// release returns a slot to the free-list.
func (b *Book) release(idx uint32) {
	b.arena[idx] = orderNode{} // clear so stale data can't leak through reuse
	b.free = append(b.free, idx)
}

// Len reports the number of resting orders.
func (b *Book) Len() int { return len(b.idIndex) }

// LastPrice returns the most recent trade price and whether one has occurred.
func (b *Book) LastPrice() (types.Price, bool) { return b.lastPrice, b.hasLast }

// SetLastPrice records the most recent trade price (used by the matcher).
func (b *Book) SetLastPrice(p types.Price) { b.lastPrice, b.hasLast = p, true }

// Insert rests a new order and returns its arena index. The order's display
// quantity defaults to its full quantity unless an iceberg display is given.
func (b *Book) Insert(o NewResting) uint32 {
	idx := b.alloc()
	display := o.Display
	if display <= 0 || display >= o.Qty {
		display = o.Qty
	}
	b.arena[idx] = orderNode{
		id:        o.ID,
		account:   o.Account,
		price:     o.Price,
		remaining: o.Qty,
		display:   display,
		hidden:    o.Qty - display,
		peak:      display,
		side:      o.Side,
		typ:       o.Typ,
		tif:       o.Tif,
		flags:     o.Flags,
		next:      NilIdx,
		prev:      NilIdx,
	}

	lv := b.levelOrCreate(o.Side, o.Price)
	// Append to the FIFO tail (newest).
	if lv.head == NilIdx {
		lv.head, lv.tail = idx, idx
	} else {
		b.arena[lv.tail].next = idx
		b.arena[idx].prev = lv.tail
		lv.tail = idx
	}
	lv.totalQty += display
	b.idIndex[o.ID] = idx
	return idx
}

func (b *Book) levelOrCreate(s types.Side, price types.Price) *level {
	levels := b.levelsFor(s)
	if lv, ok := levels[price]; ok {
		return lv
	}
	lv := &level{price: price, head: NilIdx, tail: NilIdx}
	levels[price] = lv
	b.addPrice(s, price)
	return lv
}

// Cancel removes the order with the given id, returning its remaining quantity,
// price, side, and true if found.
func (b *Book) Cancel(id types.OrderID) (remaining types.Qty, price types.Price, side types.Side, ok bool) {
	idx, found := b.idIndex[id]
	if !found {
		return 0, 0, 0, false
	}
	n := b.arena[idx]
	b.unlink(idx, n.side, n.price, n.display)
	b.release(idx)
	delete(b.idIndex, id)
	return n.remaining, n.price, n.side, true
}

// unlink detaches slot idx from its level, dropping the level when it empties.
// visibleQty is the display quantity to subtract from the level total.
func (b *Book) unlink(idx uint32, side types.Side, price types.Price, visibleQty types.Qty) {
	levels := b.levelsFor(side)
	lv := levels[price]
	n := &b.arena[idx]
	if n.prev != NilIdx {
		b.arena[n.prev].next = n.next
	} else {
		lv.head = n.next
	}
	if n.next != NilIdx {
		b.arena[n.next].prev = n.prev
	} else {
		lv.tail = n.prev
	}
	lv.totalQty -= visibleQty
	if lv.head == NilIdx {
		delete(levels, price)
		b.removePrice(side, price)
	}
}

// AmendDown reduces an order's quantity in place, preserving time priority.
// newQty must be positive and strictly less than the current remaining; the
// caller handles price changes and increases as cancel/replace.
func (b *Book) AmendDown(id types.OrderID, newQty types.Qty) bool {
	idx, ok := b.idIndex[id]
	if !ok {
		return false
	}
	n := &b.arena[idx]
	if newQty <= 0 || newQty >= n.remaining {
		return false
	}
	// Reduce hidden first, then visible, so the displayed quantity shrinks only
	// when the reduction exceeds the hidden reserve.
	delta := n.remaining - newQty
	lv := b.levelsFor(n.side)[n.price]
	if delta <= n.hidden {
		n.hidden -= delta
	} else {
		visibleDrop := delta - n.hidden
		n.hidden = 0
		n.display -= visibleDrop
		lv.totalQty -= visibleDrop
	}
	n.remaining = newQty
	return true
}

// Has reports whether an order id is resting in the book.
func (b *Book) Has(id types.OrderID) bool {
	_, ok := b.idIndex[id]
	return ok
}

// BestBid returns the highest bid price and true, or false if no bids rest.
func (b *Book) BestBid() (types.Price, bool) {
	if len(b.bidPrices) == 0 {
		return 0, false
	}
	return b.bidPrices[len(b.bidPrices)-1], true
}

// BestAsk returns the lowest ask price and true, or false if no asks rest.
func (b *Book) BestAsk() (types.Price, bool) {
	if len(b.askPrices) == 0 {
		return 0, false
	}
	return b.askPrices[0], true
}

// LevelQty returns the visible quantity resting at a price on a side.
func (b *Book) LevelQty(s types.Side, price types.Price) types.Qty {
	if lv, ok := b.levelsFor(s)[price]; ok {
		return lv.totalQty
	}
	return 0
}

// addPrice inserts price into the side's sorted price slice (ascending) if absent.
func (b *Book) addPrice(s types.Side, price types.Price) {
	slice := &b.bidPrices
	if s == types.Sell {
		slice = &b.askPrices
	}
	i := sort.Search(len(*slice), func(i int) bool { return (*slice)[i] >= price })
	if i < len(*slice) && (*slice)[i] == price {
		return
	}
	*slice = append(*slice, 0)
	copy((*slice)[i+1:], (*slice)[i:])
	(*slice)[i] = price
}

// removePrice deletes price from the side's sorted price slice.
func (b *Book) removePrice(s types.Side, price types.Price) {
	slice := &b.bidPrices
	if s == types.Sell {
		slice = &b.askPrices
	}
	i := sort.Search(len(*slice), func(i int) bool { return (*slice)[i] >= price })
	if i < len(*slice) && (*slice)[i] == price {
		*slice = append((*slice)[:i], (*slice)[i+1:]...)
	}
}
