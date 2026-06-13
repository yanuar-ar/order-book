package spsc

import "testing"

func BenchmarkPushPop(b *testing.B) {
	r := New[int](1024)
	b.ReportAllocs()
	b.ResetTimer()
	var out int
	for i := 0; i < b.N; i++ {
		r.Push(i)
		r.Pop(&out)
	}
}

// TestRingHotPathZeroAlloc gates the SPSC hot path at 0 allocations per
// operation — the ring is the engine's foundational zero-alloc primitive, so a
// regression here must fail the suite, not just show up in a benchmark.
func TestRingHotPathZeroAlloc(t *testing.T) {
	res := testing.Benchmark(BenchmarkPushPop)
	if allocs := res.AllocsPerOp(); allocs != 0 {
		t.Fatalf("ring push/pop allocates %d/op, want 0", allocs)
	}
}
