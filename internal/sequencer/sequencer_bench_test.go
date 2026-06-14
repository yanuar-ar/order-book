package sequencer

import (
	"testing"

	"github.com/yanuar-ar/order-book/internal/spsc"
	"github.com/yanuar-ar/order-book/internal/types"
	"github.com/yanuar-ar/order-book/internal/wal"
)

// discardJournal accepts records without retaining anything — a zero-alloc
// journal so the bench measures the sequencer's own encode path.
type discardJournal struct{}

func (discardJournal) Append(wal.Record) error { return nil }

// discardRouter drops routed commands and settlements (no allocation).
type discardRouter struct{}

func (discardRouter) OnCommand(types.Command) {}
func (discardRouter) OnSettlement(types.Fill) {}

// TestStepZeroAlloc gates the sequencer's per-command path (assign Seq, encode
// into the reusable buffer, journal, route) at 0 allocs/op.
func TestStepZeroAlloc(t *testing.T) {
	res := testing.Benchmark(BenchmarkStep)
	if a := res.AllocsPerOp(); a != 0 {
		t.Fatalf("Sequencer.Step allocates %d/op, want 0", a)
	}
}

// BenchmarkStep measures one sequenced command through the hot path in lockstep
// (push one, step one), so the preallocated ring never grows. Use `-benchmem`.
func BenchmarkStep(b *testing.B) {
	in := spsc.NewCommand(1 << 12)
	s := New(Config{Inputs: []*spsc.RingCommand{in}, Journal: discardJournal{}, Router: discardRouter{}, Clock: func() int64 { return 1 }})
	c := types.Command{Type: types.CmdNewOrder, OrderID: 1, Qty: 1}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		in.Push(c)
		s.Step()
	}
}
