package orderbook

import "github.com/yanuar-ar/order-book/internal/types"

// MatchableQty walks the resting side best-first and sums the quantity an
// aggressor could match, using the crosses predicate for the price test. It
// stops at the first non-crossing level and treats the first own-account order
// as a hard boundary (self-trade prevention cancels the aggressor there), so
// the sum reflects what would actually fill. The walk caps once the running sum
// reaches capAt. It mutates nothing.
func (b *Book) MatchableQty(restSide types.Side, ownAccount types.AccountID, crosses func(types.Price) bool, capAt types.Qty) types.Qty {
	prices := b.askPrices // ascending; best ask first
	ascending := true
	if restSide == types.Buy {
		prices = b.bidPrices // ascending; best bid is last
		ascending = false
	}
	var sum types.Qty
	n := len(prices)
	for k := 0; k < n; k++ {
		price := prices[k]
		if !ascending {
			price = prices[n-1-k]
		}
		if !crosses(price) {
			break
		}
		lv := b.levelsFor(restSide)[price]
		for idx := lv.head; idx != NilIdx; idx = b.arena[idx].next {
			on := &b.arena[idx]
			if on.account == ownAccount {
				return sum // STP boundary
			}
			sum += on.remaining
			if sum >= capAt {
				return sum
			}
		}
	}
	return sum
}
