package orderbook

import "github.com/yanuar-ar/order-book/internal/types"

// RestingDump is a canonical, deterministic view of one resting order, for
// state comparison (recovery determinism) and tests.
type RestingDump struct {
	Side      types.Side
	Price     types.Price
	ID        types.OrderID
	Remaining types.Qty
	Display   types.Qty
}

// Dump returns every resting order in deterministic order: bids then asks, each
// by ascending price, FIFO (oldest first) within a level. The order does not
// depend on arena slot identity or free-list state, so two logically equal
// books produce identical dumps regardless of physical layout.
func (b *Book) Dump() []RestingDump {
	out := make([]RestingDump, 0, len(b.idIndex))
	appendSide := func(side types.Side, prices []types.Price) {
		levels := b.levelsFor(side)
		for _, p := range prices {
			lv := levels[p]
			for idx := lv.head; idx != NilIdx; idx = b.arena[idx].next {
				n := b.arena[idx]
				out = append(out, RestingDump{Side: side, Price: p, ID: n.id, Remaining: n.remaining, Display: n.display})
			}
		}
	}
	appendSide(types.Buy, b.bidPrices)
	appendSide(types.Sell, b.askPrices)
	return out
}
