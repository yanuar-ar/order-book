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

// PriceLevel is an aggregated price level: total visible quantity at a price.
type PriceLevel struct {
	Price types.Price
	Qty   types.Qty
}

// Depth returns up to n price levels for a side, best price first (highest bid /
// lowest ask), each with its total visible quantity. Used for market-data and
// terminal display.
func (b *Book) Depth(side types.Side, n int) []PriceLevel {
	out := make([]PriceLevel, 0, n)
	if side == types.Buy {
		// bidPrices ascending; best (highest) is last.
		for i := len(b.bidPrices) - 1; i >= 0 && len(out) < n; i-- {
			p := b.bidPrices[i]
			out = append(out, PriceLevel{Price: p, Qty: b.bidLevels[p].totalQty})
		}
	} else {
		// askPrices ascending; best (lowest) is first.
		for i := 0; i < len(b.askPrices) && len(out) < n; i++ {
			p := b.askPrices[i]
			out = append(out, PriceLevel{Price: p, Qty: b.askLevels[p].totalQty})
		}
	}
	return out
}
