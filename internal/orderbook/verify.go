package orderbook

import (
	"fmt"

	"github.com/yanuar-ar/order-book/internal/types"
)

// Verify checks the book's structural invariants (testing guide §3.C) and
// returns the first violation found, or nil when the book is well-formed. It is
// intended for tests and debug assertions, never the hot path: it walks every
// level and every arena slot, so it is O(orders + arena).
//
// Covered invariants:
//   - INV-OB-02 price ordering: per-side price slices are strictly ascending and
//     agree with the level map (best bid = last bid price, best ask = first).
//   - INV-OB-03 FIFO list integrity: head.prev == NilIdx, tail.next == NilIdx,
//     reciprocal next/prev links, no cycles, traversal length == node count.
//   - INV-OB-04 level totals: level.totalQty == Σ display of its nodes.
//   - INV-OB-05 arena/free-list: every arena slot is reachable from exactly one
//     level XOR on the free-list — no leaked or double-owned slots.
//   - INV-OB-06 idIndex consistency: idIndex is exactly the set of reachable
//     orders, each mapping to the slot whose node carries that id.
//   - INV-OB-07 ID uniqueness: no two reachable nodes share an id (a corollary
//     of the bijection between idIndex and reachable slots).
//   - INV-OB-09 bounds: 0 < display <= remaining and hidden == remaining-display
//     for every resting order.
//
// INV-OB-08 (time priority) is the FIFO list order itself, which INV-OB-03
// validates; nodes carry no seq to assert monotonicity beyond list order.
func (b *Book) Verify() error {
	reachable := make(map[uint32]types.OrderID, len(b.idIndex))

	if err := b.verifySide(types.Buy, b.bidPrices, b.bidLevels, reachable); err != nil {
		return err
	}
	if err := b.verifySide(types.Sell, b.askPrices, b.askLevels, reachable); err != nil {
		return err
	}
	if err := b.verifyArena(reachable); err != nil {
		return err
	}
	return b.verifyIDIndex(reachable)
}

func (b *Book) verifySide(side types.Side, prices []types.Price, levels map[types.Price]*level, reachable map[uint32]types.OrderID) error {
	// INV-OB-02: strictly ascending, no duplicates, and a bijection with the map.
	for i := 1; i < len(prices); i++ {
		if prices[i] <= prices[i-1] {
			return fmt.Errorf("INV-OB-02: side %d price slice not strictly ascending at %d: %d then %d", side, i, prices[i-1], prices[i])
		}
	}
	if len(prices) != len(levels) {
		return fmt.Errorf("INV-OB-02: side %d has %d prices but %d levels", side, len(prices), len(levels))
	}
	for _, p := range prices {
		if _, ok := levels[p]; !ok {
			return fmt.Errorf("INV-OB-02: side %d price %d in slice but no level", side, p)
		}
	}

	for price, lv := range levels {
		if lv.price != price {
			return fmt.Errorf("INV-OB-02: side %d level keyed %d carries price %d", side, price, lv.price)
		}
		var sumDisplay types.Qty
		count := 0
		prev := NilIdx
		for idx := lv.head; idx != NilIdx; idx = b.arena[idx].next {
			if int(idx) >= len(b.arena) {
				return fmt.Errorf("INV-OB-03: side %d price %d node idx %d out of arena range", side, price, idx)
			}
			if _, dup := reachable[idx]; dup {
				return fmt.Errorf("INV-OB-03/05: slot %d reachable from two places (cycle or shared)", idx)
			}
			n := b.arena[idx]
			if n.prev != prev {
				return fmt.Errorf("INV-OB-03: side %d price %d slot %d prev=%d, expected %d", side, price, idx, n.prev, prev)
			}
			if n.side != side || n.price != price {
				return fmt.Errorf("INV-OB-03: slot %d in side %d price %d carries side %d price %d", idx, side, price, n.side, n.price)
			}
			if n.remaining <= 0 || n.display <= 0 || n.display > n.remaining {
				return fmt.Errorf("INV-OB-09: slot %d bad bounds remaining=%d display=%d", idx, n.remaining, n.display)
			}
			if n.hidden != n.remaining-n.display {
				return fmt.Errorf("INV-OB-09: slot %d hidden=%d != remaining-display=%d", idx, n.hidden, n.remaining-n.display)
			}
			reachable[idx] = n.id
			sumDisplay += n.display
			count++
			prev = idx
			if count > len(b.arena) {
				return fmt.Errorf("INV-OB-03: side %d price %d list exceeds arena size (cycle)", side, price)
			}
		}
		if count == 0 {
			return fmt.Errorf("INV-OB-03: side %d price %d level is empty but still present", side, price)
		}
		if lv.tail != prev {
			return fmt.Errorf("INV-OB-03: side %d price %d tail=%d, expected %d", side, price, lv.tail, prev)
		}
		if sumDisplay != lv.totalQty {
			return fmt.Errorf("INV-OB-04: side %d price %d totalQty=%d, Σdisplay=%d", side, price, lv.totalQty, sumDisplay)
		}
	}
	return nil
}

func (b *Book) verifyArena(reachable map[uint32]types.OrderID) error {
	free := make(map[uint32]bool, len(b.free))
	for _, idx := range b.free {
		if int(idx) >= len(b.arena) {
			return fmt.Errorf("INV-OB-05: free-list slot %d out of arena range", idx)
		}
		if free[idx] {
			return fmt.Errorf("INV-OB-05: slot %d appears twice in the free-list", idx)
		}
		free[idx] = true
	}
	for idx := 0; idx < len(b.arena); idx++ {
		u := uint32(idx)
		_, isReachable := reachable[u]
		isFree := free[u]
		if isReachable && isFree {
			return fmt.Errorf("INV-OB-05: slot %d both reachable and free", idx)
		}
		if !isReachable && !isFree {
			return fmt.Errorf("INV-OB-05: slot %d leaked (neither reachable nor free)", idx)
		}
	}
	return nil
}

func (b *Book) verifyIDIndex(reachable map[uint32]types.OrderID) error {
	if len(b.idIndex) != len(reachable) {
		return fmt.Errorf("INV-OB-06: idIndex has %d entries but %d orders are reachable", len(b.idIndex), len(reachable))
	}
	for id, idx := range b.idIndex {
		gotID, ok := reachable[idx]
		if !ok {
			return fmt.Errorf("INV-OB-06: idIndex[%d]=%d points to an unreachable slot", id, idx)
		}
		if gotID != id {
			return fmt.Errorf("INV-OB-06/07: idIndex[%d]=%d but that slot carries id %d", id, idx, gotID)
		}
	}
	return nil
}
