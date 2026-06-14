package wal

import (
	"testing"
)

// TestAppendZeroAlloc gates the hot append path at 0 allocs/op: the writer
// frames into a reusable buffer and copies the payload, so steady-state appends
// allocate nothing.
func TestAppendZeroAlloc(t *testing.T) {
	res := testing.Benchmark(BenchmarkAppend)
	if a := res.AllocsPerOp(); a != 0 {
		t.Fatalf("Writer.Append allocates %d/op, want 0", a)
	}
}

// BenchmarkAppend measures one record append (no Sync). Use `-benchmem` to
// report allocations.
func BenchmarkAppend(b *testing.B) {
	w, err := OpenWriter(b.TempDir(), 1<<30)
	if err != nil {
		b.Fatalf("OpenWriter: %v", err)
	}
	defer w.Close()
	payload := make([]byte, 102) // CommandSize-shaped payload
	rec := Record{Seq: 1, TsNanos: 1, Type: 0, Payload: payload}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec.Seq = uint64(i)
		if err := w.Append(rec); err != nil {
			b.Fatalf("append: %v", err)
		}
	}
}
