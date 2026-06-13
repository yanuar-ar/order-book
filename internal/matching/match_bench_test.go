package matching

import (
	"testing"

	"github.com/yanuar-ar/order-book/internal/orderbook"
	"github.com/yanuar-ar/order-book/internal/types"
)

// BenchmarkMatchSingle measures the steady-state cost of one aggressor matching
// one resting order on a warmed engine. The engine reuses its fill buffers
// across Submits, so the hot path is allocation-free after warmup.
func BenchmarkMatchSingle(b *testing.B) {
	e := NewEngine(orderbook.New(0, 16), nil, 1)
	// One deep resting sell to take 1 unit from on each iteration.
	e.Submit(lim(1, 10, 100, types.Sell, 100, types.Qty(b.N)+1))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.Submit(lim(2, 20, 200, types.Buy, 100, 1)) // crosses, fully fills 1 unit
	}
}

// TestMatchHotPathZeroAlloc gates the warmed matching path at 0 allocs/op.
func TestMatchHotPathZeroAlloc(t *testing.T) {
	res := testing.Benchmark(BenchmarkMatchSingle)
	if a := res.AllocsPerOp(); a != 0 {
		t.Fatalf("warmed match allocates %d/op, want 0", a)
	}
}
