// Package property runs randomized load against the engine and asserts global
// invariants hold at every step, plus same-seed determinism, differential
// equality against a reference model, and recovery correctness.
package property

import (
	"fmt"

	"github.com/yanuar-ar/order-book/internal/market"
	"github.com/yanuar-ar/order-book/internal/types"
)

// CheckAllInvariants runs the applicable invariant taxonomy (testing guide §3)
// against the engine after a command has been fully processed and drained. It
// returns the first violation found, or nil. netDeposits is the running total
// of deposits minus withdrawals per asset, required for the conservation check.
//
// It composes each package's structural Verify() with the cross-package money
// and book invariants no single package can see:
//   - INV-OB-* via Book.Verify() per market
//   - INV-BAL-01 + reservation consistency via Ledger.Verify()
//   - INV-OB-01 no crossed resting book per market
//   - INV-BAL-04 per-asset conservation including the fee account
//   - INV-BAL-03 the reserved-order set equals open resting orders plus pending
//     stops. Recomputing each order's locked amount from book quantity is
//     infeasible under this engine's reservation model (a buy reserves
//     notional + worst-case taker fee, and a market buy reserves the account's
//     entire available quote), so the exact, checkable form is the set
//     bijection here plus Ledger.Verify()'s per-account reservation sum.
func CheckAllInvariants(e *market.Engine, netDeposits map[types.AssetID]int64) error {
	led := e.Ledger()
	if err := led.Verify(); err != nil {
		return err
	}

	for _, m := range e.MarketIDs() {
		bk := e.Shard(m).Book()
		if err := bk.Verify(); err != nil {
			return fmt.Errorf("market %d: %w", m, err)
		}
		bid, hb := bk.BestBid()
		ask, ha := bk.BestAsk()
		if hb && ha && bid >= ask {
			return fmt.Errorf("INV-OB-01: market %d crossed: bid %d >= ask %d", m, bid, ask)
		}
	}

	// INV-BAL-04: per-asset conservation (available + reserved + fees == net deposits).
	bals, fees := led.Dump()
	total := map[types.AssetID]int64{}
	for _, b := range bals {
		total[b.Asset] += b.Available + b.Reserved
	}
	for _, f := range fees {
		total[f.Asset] += f.Amount
	}
	for asset, want := range netDeposits {
		if total[asset] != want {
			return fmt.Errorf("INV-BAL-04: asset %d total %d != net deposits %d (leak %d)", asset, total[asset], want, total[asset]-want)
		}
	}
	for asset, got := range total {
		if _, ok := netDeposits[asset]; !ok && got != 0 {
			return fmt.Errorf("INV-BAL-04: asset %d total %d but no net deposits recorded", asset, got)
		}
	}

	// INV-BAL-03: reserved-order set == resting orders ∪ pending stops.
	reserved := map[types.OrderID]bool{}
	for _, id := range led.ReservedOrders() {
		reserved[id] = true
	}
	open := map[types.OrderID]bool{}
	for _, m := range e.MarketIDs() {
		for _, o := range e.Shard(m).Book().Dump() {
			open[o.ID] = true
		}
		for _, s := range e.Shard(m).StopDump() {
			open[s.OrderID] = true
		}
	}
	for id := range reserved {
		if !open[id] {
			return fmt.Errorf("INV-BAL-03: order %d holds a reservation but is neither resting nor a pending stop (leaked reservation)", id)
		}
	}
	for id := range open {
		if !reserved[id] {
			return fmt.Errorf("INV-BAL-03: order %d is open but holds no reservation (unreserved exposure)", id)
		}
	}
	return nil
}
