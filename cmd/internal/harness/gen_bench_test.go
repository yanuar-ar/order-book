package harness

import (
	"math/rand"
	"testing"

	"github.com/yanuar-ar/order-book/internal/orderbook"
	"github.com/yanuar-ar/order-book/internal/types"
)

// BenchmarkMeasurePathPerCommand locks in that the loadtest measuring loop's
// harness-side per-command work — live-mid generation plus latency recording —
// allocates nothing in steady state (U1 / origin R2). The engine's own per-Step
// cost (including the unbounded core.acks growth) is deferred to the gateway
// phase and is intentionally excluded here.
func BenchmarkMeasurePathPerCommand(b *testing.B) {
	eng, cleanup, err := BuildEngine("serial", nil, DefaultConfig())
	if err != nil {
		b.Fatal(err)
	}
	defer cleanup()
	Fund(eng, 100)
	SeedBook(eng, rand.New(rand.NewSource(1)), 100)

	var books [3]*orderbook.Book
	for m := types.MarketID(0); m < 3; m++ {
		books[m] = eng.Shard(m).Book()
	}
	h := NewHist()
	pr := rand.New(rand.NewSource(2))
	var id types.OrderID = 1 << 32

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id++
		c := GenLiveMid(&books, pr, id, 100) // live-mid generation (reads book)
		h.Record(int64(c.OrderID % 100_000)) // latency recording into the fixed-bucket histogram
	}
}
