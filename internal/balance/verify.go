package balance

import (
	"fmt"
	"sort"

	"github.com/yanuar-ar/order-book/internal/types"
)

// Verify checks the ledger's structural invariants (testing guide §3.A subset)
// and returns the first violation, or nil. Intended for tests and debug
// assertions, not the hot path.
//
// Covered invariants:
//   - INV-BAL-01 non-negative: every available, reserved, and fee balance is
//     non-negative.
//   - Reservation consistency (the ledger half of INV-BAL-03): each
//     (account, asset) Reserved equals the sum of that account|asset's open
//     reservation records, and no reservation record is negative.
//
// The book↔ledger cross-check (the other half of INV-BAL-03: the reserved
// order set equals the open resting orders plus pending stops) and conservation
// (INV-BAL-04) span packages and live in tests/property.
func (l *Ledger) Verify() error {
	for k, b := range l.bal {
		if b.Available < 0 || b.Reserved < 0 {
			return fmt.Errorf("INV-BAL-01: acct %d asset %d available=%d reserved=%d", k.Acct, k.Asset, b.Available, b.Reserved)
		}
	}
	for asset, amt := range l.fees {
		if amt < 0 {
			return fmt.Errorf("INV-BAL-01: fee account asset %d = %d", asset, amt)
		}
	}

	sum := make(map[key]int64, len(l.res))
	for id, r := range l.res {
		if r.remaining < 0 {
			return fmt.Errorf("INV-BAL-03: reservation for order %d has negative remaining %d", id, r.remaining)
		}
		sum[key{r.acct, r.asset}] += r.remaining
	}
	// Every (acct,asset) with reservations must match its Reserved balance...
	for k, want := range sum {
		if got := l.bal[k].Reserved; got != want {
			return fmt.Errorf("INV-BAL-03: acct %d asset %d reserved=%d but reservations sum to %d", k.Acct, k.Asset, got, want)
		}
	}
	// ...and every (acct,asset) with a non-zero Reserved must be backed by
	// reservation records (catches reserved funds with no owning order).
	for k, b := range l.bal {
		if b.Reserved != sum[k] {
			return fmt.Errorf("INV-BAL-03: acct %d asset %d reserved=%d but reservations sum to %d", k.Acct, k.Asset, b.Reserved, sum[k])
		}
	}
	return nil
}

// ReservedOrders returns the IDs of all orders currently holding a reservation,
// sorted ascending. tests/property uses it to assert the reserved-order set
// equals the open resting orders plus pending stops (INV-BAL-03).
func (l *Ledger) ReservedOrders() []types.OrderID {
	out := make([]types.OrderID, 0, len(l.res))
	for id := range l.res {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
