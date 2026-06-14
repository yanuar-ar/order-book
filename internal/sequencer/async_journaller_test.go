package sequencer

import (
	"errors"
	"runtime"
	"testing"

	"github.com/yanuar-ar/order-book/internal/spsc"
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

// ---- U3: a sequencer wired with an async journaller halts on its fatal ----

func TestSequencerHaltsOnAsyncJournallerFatal(t *testing.T) {
	boom := errors.New("async io down")
	fj := &failingJournal{failAt: 5, err: boom}
	aj := NewAsyncJournaller(fj, 0, 1, -1) // cap 1 → records 1..4 durable, 5th fails
	defer aj.Close()

	in := spsc.NewCommand(64)
	r := &fakeRouter{}
	s := New(Config{Inputs: []*spsc.RingCommand{in}, Journaller: aj, Router: r, Clock: func() int64 { return 1 }})
	for i := 1; i <= 10; i++ {
		in.Push(cmd(types.OrderID(i)))
	}

	// Step until the async fatal propagates (bounded; either Append returns it on
	// the busy path or the idle Fatal() check catches it).
	var fatal error
	for i := 0; i < 200000 && fatal == nil; i++ {
		s.Step()
		fatal = s.Fatal()
		runtime.Gosched()
	}
	if !errors.Is(fatal, boom) {
		t.Fatalf("sequencer Fatal = %v, want %v", fatal, boom)
	}
	if s.Step() {
		t.Fatalf("Step did work after fatal latched")
	}
	if got := s.DurableSeq(); got >= 5 {
		t.Fatalf("durableSeq = %d, want < 5 (failure at the 5th record; no ack above it)", got)
	}
}

// ---- U4: DrainJournal barriers until the async consumer is durable ----

func TestSequencerDrainJournalWaitsForAsyncDurability(t *testing.T) {
	j := &fakeJournal{}
	// Huge batch cap → the consumer only flushes when the ring drains, so
	// DurableSeq genuinely lags Seq until DrainJournal forces the wait.
	aj := NewAsyncJournaller(j, 0, 1<<20, -1)
	defer aj.Close()

	in := spsc.NewCommand(64)
	s := New(Config{Inputs: []*spsc.RingCommand{in}, Journaller: aj, Router: &fakeRouter{}, Clock: func() int64 { return 1 }})
	const n = 20
	for i := 1; i <= n; i++ {
		in.Push(cmd(types.OrderID(i)))
	}
	for s.Step() { // sequence every command into the journal ring
	}
	if err := s.DrainJournal(); err != nil {
		t.Fatalf("DrainJournal: %v", err)
	}
	if s.DurableSeq() != s.Seq() {
		t.Fatalf("after DrainJournal durableSeq=%d, want Seq=%d", s.DurableSeq(), s.Seq())
	}
	if s.DurableSeq() != n {
		t.Fatalf("durableSeq=%d, want %d", s.DurableSeq(), n)
	}
}

func TestSequencerDrainJournalSurfacesAsyncFatal(t *testing.T) {
	boom := errors.New("drain-time io down")
	j := &syncFailJournal{err: boom}
	aj := NewAsyncJournaller(j, 0, 1<<20, -1) // flush only on ring-drain → fails at DrainJournal
	defer aj.Close()

	in := spsc.NewCommand(16)
	s := New(Config{Inputs: []*spsc.RingCommand{in}, Journaller: aj, Router: &fakeRouter{}, Clock: func() int64 { return 1 }})
	in.Push(cmd(1))
	for s.Step() {
	}
	if err := s.DrainJournal(); !errors.Is(err, boom) {
		t.Fatalf("DrainJournal = %v, want %v", err, boom)
	}
	if err := s.Fatal(); !errors.Is(err, boom) {
		t.Fatalf("Fatal after DrainJournal = %v, want %v (must latch for drain-then-check callers)", err, boom)
	}
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
