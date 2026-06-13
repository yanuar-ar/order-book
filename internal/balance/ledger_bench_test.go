package balance

import (
	"testing"

	"github.com/yanuar-ar/order-book/internal/types"
)

// TestBalanceHotPathZeroAlloc gates the reserve/settle/release lifecycle at
// 0 allocs/op (reservations are stored by value, not pointer).
func TestBalanceHotPathZeroAlloc(t *testing.T) {
	res := testing.Benchmark(BenchmarkReserveSettleRelease)
	if a := res.AllocsPerOp(); a != 0 {
		t.Fatalf("reserve/settle/release allocates %d/op, want 0", a)
	}
}

// BenchmarkReserveSettleRelease measures the balance lifecycle for one order.
// Use `go test -bench=. -benchmem` to report allocations.
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
