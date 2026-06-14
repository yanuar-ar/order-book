package sequencer

import (
	"errors"
	"testing"

	"github.com/yanuar-ar/order-book/internal/types"
	"github.com/yanuar-ar/order-book/internal/wal"
)

// jcmd builds a sequenced command with the given Seq (the journaller reads only
// Seq/TsNanos/Type; the rest round-trips through the codec).
func jcmd(seq int) types.Command {
	return types.Command{Seq: types.Seq(seq), Type: types.CmdNewOrder, OrderID: types.OrderID(seq), Qty: 1}
}

// syncFailJournal accepts Appends but fails Sync, to exercise the fsync fail-stop.
type syncFailJournal struct {
	appends int
	err     error
}

func (j *syncFailJournal) Append(wal.Record) error { j.appends++; return nil }
func (j *syncFailJournal) Sync() error             { return j.err }

// ---- Positive: durability + ordering through a real WAL ----

func TestAsyncJournallerDurableInOrderRealWAL(t *testing.T) {
	dir := t.TempDir()
	w, err := wal.OpenWriter(dir, 0)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	aj := NewAsyncJournaller(w, 0, 0, -1)
	const n = 64
	for i := 1; i <= n; i++ {
		if err := aj.Append(jcmd(i)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if err := aj.Close(); err != nil { // stop + final flush
		t.Fatalf("close: %v", err)
	}
	if got := aj.DurableSeq(); got != types.Seq(n) {
		t.Fatalf("durableSeq = %d, want %d", got, n)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("writer close: %v", err)
	}
	var got []uint64
	if err := wal.Replay(dir, 0, func(r wal.Record) error { got = append(got, r.Seq); return nil }); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(got) != n {
		t.Fatalf("replayed %d records, want %d", len(got), n)
	}
	for i, s := range got {
		if s != uint64(i+1) {
			t.Fatalf("record %d has Seq %d, want %d (out of order)", i, s, i+1)
		}
	}
}

// ---- Edge: backpressure (ring smaller than the stream) drops nothing ----

func TestAsyncJournallerBackpressureNoDrop(t *testing.T) {
	j := &fakeJournal{}
	aj := NewAsyncJournaller(j, 2, 0, -1) // tiny ring forces Append to spin
	const n = 1000
	for i := 1; i <= n; i++ {
		if err := aj.Append(jcmd(i)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if err := aj.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := aj.DurableSeq(); got != types.Seq(n) {
		t.Fatalf("durableSeq = %d, want %d", got, n)
	}
	if len(j.recs) != n {
		t.Fatalf("journaled %d records, want %d", len(j.recs), n)
	}
	for i, r := range j.recs {
		if r.Seq != uint64(i+1) {
			t.Fatalf("record %d has Seq %d, want %d (reordered under backpressure)", i, r.Seq, i+1)
		}
	}
}

// ---- Edge: Close flushes a sub-batch that never hit the cap ----

func TestAsyncJournallerCloseFlushesPending(t *testing.T) {
	j := &fakeJournal{}
	aj := NewAsyncJournaller(j, 0, 1<<20, -1) // cap far above the stream
	for i := 1; i <= 3; i++ {
		if err := aj.Append(jcmd(i)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if err := aj.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := aj.DurableSeq(); got != 3 {
		t.Fatalf("durableSeq = %d, want 3 (Close must flush the pending batch)", got)
	}
}

// ---- Negative: an Append failure fail-stops, freezing the watermark ----

func TestAsyncJournallerFatalOnAppendError(t *testing.T) {
	boom := errors.New("append boom")
	j := &failingJournal{failAt: 5, err: boom}
	aj := NewAsyncJournaller(j, 0, 1, -1) // cap 1 → each ok record is durable
	for i := 1; i <= 10; i++ {
		if err := aj.Append(jcmd(i)); err != nil {
			break // producer observes the latched fatal and stops
		}
	}
	if err := aj.Drain(); !errors.Is(err, boom) {
		t.Fatalf("Drain after append failure = %v, want %v", err, boom)
	}
	if err := aj.Fatal(); !errors.Is(err, boom) {
		t.Fatalf("Fatal = %v, want %v", err, boom)
	}
	if got := aj.DurableSeq(); got != 4 {
		t.Fatalf("durableSeq = %d, want 4 (records before the 5th failure)", got)
	}
	_ = aj.Close()
}

// ---- Negative: an fsync failure fail-stops ----

func TestAsyncJournallerFatalOnSyncError(t *testing.T) {
	boom := errors.New("sync boom")
	j := &syncFailJournal{err: boom}
	aj := NewAsyncJournaller(j, 0, 1, -1) // cap 1 → flush (fsync) after the first record
	_ = aj.Append(jcmd(1))
	if err := aj.Drain(); !errors.Is(err, boom) {
		t.Fatalf("Drain after sync failure = %v, want %v", err, boom)
	}
	if err := aj.Fatal(); !errors.Is(err, boom) {
		t.Fatalf("Fatal = %v, want %v", err, boom)
	}
	if got := aj.DurableSeq(); got != 0 {
		t.Fatalf("durableSeq = %d, want 0 (fsync never succeeded)", got)
	}
	_ = aj.Close()
}

// ---- Zero-alloc: Append must not allocate on the producer hot path ----

func BenchmarkAsyncAppend(b *testing.B) {
	aj := NewAsyncJournaller(discardJournal{}, 1<<16, 1<<20, -1)
	defer aj.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := aj.Append(jcmd(i + 1)); err != nil {
			b.Fatalf("append: %v", err)
		}
	}
}
