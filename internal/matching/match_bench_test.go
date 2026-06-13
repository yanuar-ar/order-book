package matching

import (
	"testing"

	"github.com/yanuar-ar/order-book/internal/orderbook"
	"github.com/yanuar-ar/order-book/internal/types"
)

// BenchmarkMatchSingle measures one aggressor matching one resting order. Use
// `go test -bench=. -benchmem` to report allocations. The matcher currently
// allocates a per-call fills slice; driving this to zero (writing fills into a
// caller-owned buffer/sink) is the remaining hot-path optimization.
func BenchmarkMatchSingle(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		e := NewEngine(orderbook.New(0, 8), nil)
		e.Submit(types.FundedOrder{OrderID: 1, Account: 1, Side: types.Sell, OrdType: types.Limit, Tif: types.GTC, Price: 100, Qty: 1})
		b.StartTimer()
		e.Submit(types.FundedOrder{Seq: 2, OrderID: 2, Account: 2, Side: types.Buy, OrdType: types.Limit, Tif: types.GTC, Price: 100, Qty: 1})
	}
}
