package balance

import (
	"testing"

	"github.com/yanuar-ar/order-book/internal/types"
)

// BenchmarkReserveSettleRelease measures the balance lifecycle for one order.
// Use `go test -bench=. -benchmem` to report allocations. Reserve currently
// allocates a reservation record per order; an arena-backed reservation table
// is the remaining hot-path optimization.
func BenchmarkReserveSettleRelease(b *testing.B) {
	l := newLedger(0, 0, 100)
	l.Deposit(1, usdt, 1<<62)
	l.Deposit(2, btc, 1<<40)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := types.OrderID(i)
		l.Reserve(types.FundedOrder{OrderID: id, Account: 1, Market: mkt, Side: types.Buy, OrdType: types.Limit, Price: 10, Qty: 1})
		l.Settle(types.Fill{Taker: types.Buy, Market: mkt, Price: 10, Qty: 1, BuyOrder: id, SellOrder: id + 1<<32, BuyAccount: 1, SellAccount: 2})
		l.Release(id)
	}
}
