package sequencer

import (
	"errors"
	"testing"

	"github.com/yanuar-ar/order-book/internal/spsc"
	"github.com/yanuar-ar/order-book/internal/types"
	"github.com/yanuar-ar/order-book/internal/wal"
)

// cloneRecord copies a record's payload so a retaining test journal honors the
// Journal.Append no-retain contract (the sequencer reuses its payload buffer).
func cloneRecord(r wal.Record) wal.Record {
	r.Payload = append([]byte(nil), r.Payload...)
	return r
}

type fakeJournal struct{ recs []wal.Record }

func (j *fakeJournal) Append(r wal.Record) error { j.recs = append(j.recs, cloneRecord(r)); return nil }

// failingJournal fails Append on the failAt-th call (1-based), succeeding before.
type failingJournal struct {
	recs   []wal.Record
	calls  int
	failAt int
	err    error
}

func (j *failingJournal) Append(r wal.Record) error {
	j.calls++
	if j.calls == j.failAt {
		return j.err
	}
	j.recs = append(j.recs, cloneRecord(r))
	return nil
}

type fakeRouter struct {
	cmds        []types.Command
	settlements []types.Fill
}

func (r *fakeRouter) OnCommand(c types.Command) { r.cmds = append(r.cmds, c) }
func (r *fakeRouter) OnSettlement(f types.Fill) { r.settlements = append(r.settlements, f) }

// clockSeq returns successive controlled timestamps.
func clockSeq(vals ...int64) ClockFunc {
	i := 0
	return func() int64 {
		v := vals[i%len(vals)]
		i++
		return v
	}
}

func cmd(id types.OrderID) types.Command {
	return types.Command{Type: types.CmdNewOrder, OrderID: id, Qty: 1}
}

func newSeq(t *testing.T, inputs []*spsc.RingCommand, fills []*spsc.RingFill, reinject *spsc.RingCommand, clock ClockFunc) (*Sequencer, *fakeJournal, *fakeRouter) {
	t.Helper()
	j, r := &fakeJournal{}, &fakeRouter{}
	s := New(Config{Reinject: reinject, Inputs: inputs, Fills: fills, Journal: j, Router: r, Clock: clock})
	return s, j, r
}

// ---- Positive ----

func TestSeqMonotonicContiguousAndJournaled(t *testing.T) {
	in := spsc.NewCommand(16)
	in.Push(cmd(100))
	in.Push(cmd(101))
	in.Push(cmd(102))
	s, j, r := newSeq(t, []*spsc.RingCommand{in}, nil, nil, clockSeq(0))

	for i := 0; i < 3; i++ {
		s.Step()
	}
	if len(r.cmds) != 3 {
		t.Fatalf("routed %d commands, want 3", len(r.cmds))
	}
	for i, c := range r.cmds {
		if c.Seq != types.Seq(i+1) {
			t.Errorf("cmd %d Seq = %d, want %d", i, c.Seq, i+1)
		}
	}
	if len(j.recs) != 3 || j.recs[0].Seq != 1 || j.recs[2].Seq != 3 {
		t.Fatalf("journal seqs = %v, want 1,2,3", recSeqs(j.recs))
	}
}

func TestTimestampCapturedOncePerCommand(t *testing.T) {
	in := spsc.NewCommand(16)
	in.Push(cmd(1))
	in.Push(cmd(2))
	s, j, r := newSeq(t, []*spsc.RingCommand{in}, nil, nil, clockSeq(100, 200))
	s.Step()
	s.Step()
	if r.cmds[0].TsNanos != 100 || r.cmds[1].TsNanos != 200 {
		t.Fatalf("timestamps = %d,%d, want 100,200", r.cmds[0].TsNanos, r.cmds[1].TsNanos)
	}
	if j.recs[0].TsNanos != 100 || j.recs[1].TsNanos != 200 {
		t.Fatalf("journal ts = %d,%d, want 100,200", j.recs[0].TsNanos, j.recs[1].TsNanos)
	}
}

func TestFillsSettledInDeterministicOrder(t *testing.T) {
	f0 := spsc.NewFill(16)
	f1 := spsc.NewFill(16)
	// Push out of arrival order across two rings.
	f0.Push(types.Fill{AggressorSeq: 2, MatchIndex: 0})
	f0.Push(types.Fill{AggressorSeq: 1, MatchIndex: 1})
	f1.Push(types.Fill{AggressorSeq: 1, MatchIndex: 0})
	s, _, r := newSeq(t, nil, []*spsc.RingFill{f0, f1}, nil, clockSeq(0))
	s.Step()
	want := [][2]uint64{{1, 0}, {1, 1}, {2, 0}}
	if len(r.settlements) != 3 {
		t.Fatalf("settlements = %d, want 3", len(r.settlements))
	}
	for i, f := range r.settlements {
		if uint64(f.AggressorSeq) != want[i][0] || uint64(f.MatchIndex) != want[i][1] {
			t.Errorf("settlement %d = (%d,%d), want (%d,%d)", i, f.AggressorSeq, f.MatchIndex, want[i][0], want[i][1])
		}
	}
}

func TestRoundRobinFanIn(t *testing.T) {
	a := spsc.NewCommand(16)
	b := spsc.NewCommand(16)
	a.Push(cmd(10))
	a.Push(cmd(11))
	b.Push(cmd(20))
	b.Push(cmd(21))
	s, _, r := newSeq(t, []*spsc.RingCommand{a, b}, nil, nil, clockSeq(0))
	for i := 0; i < 4; i++ {
		s.Step()
	}
	got := []types.OrderID{r.cmds[0].OrderID, r.cmds[1].OrderID, r.cmds[2].OrderID, r.cmds[3].OrderID}
	want := []types.OrderID{10, 20, 11, 21} // alternating A,B,A,B
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fan-in order = %v, want %v", got, want)
		}
	}
}

// ---- Negative / edge ----

func TestReinjectDrainedBeforeExternalInSameTick(t *testing.T) {
	in := spsc.NewCommand(16)
	rein := spsc.NewCommand(16)
	in.Push(cmd(100))
	rein.Push(cmd(900))
	s, j, r := newSeq(t, []*spsc.RingCommand{in}, nil, rein, clockSeq(0))
	s.Step() // one tick: drain reinject (all), then one external
	if len(r.cmds) != 2 {
		t.Fatalf("routed %d, want 2 (reinject + external)", len(r.cmds))
	}
	if r.cmds[0].OrderID != 900 || r.cmds[0].Seq != 1 {
		t.Fatalf("first routed = id %d seq %d, want reinject id900 seq1", r.cmds[0].OrderID, r.cmds[0].Seq)
	}
	if r.cmds[1].OrderID != 100 || r.cmds[1].Seq != 2 {
		t.Fatalf("second routed = id %d seq %d, want external id100 seq2", r.cmds[1].OrderID, r.cmds[1].Seq)
	}
	if len(j.recs) != 2 {
		t.Fatalf("both reinject and external must be journaled, got %d", len(j.recs))
	}
}

func TestJournalAppendFailureLatchesFatalAndReleasesNoAck(t *testing.T) {
	in := spsc.NewCommand(16)
	in.Push(cmd(100)) // first command: journals fine
	in.Push(cmd(101)) // second command: Append fails
	in.Push(cmd(102)) // must never be routed after fatal
	j := &failingJournal{failAt: 2, err: errors.New("disk full")}
	r := &fakeRouter{}
	s := New(Config{Inputs: []*spsc.RingCommand{in}, Journal: j, Router: r, Clock: clockSeq(0)})

	if !s.Step() { // command 100
		t.Fatal("first Step should do work")
	}
	s.Step() // command 101: Append fails -> latch fatal, no route

	if s.Fatal() == nil {
		t.Fatal("Fatal() should be set after an Append failure")
	}
	// The failed command must not have been routed (no ack produced for it).
	if len(r.cmds) != 1 || r.cmds[0].OrderID != 100 {
		t.Fatalf("routed = %+v, want only the first command (no route for the failed one)", r.cmds)
	}
	// Once fatal, further Steps are no-ops: command 102 is never routed.
	if s.Step() {
		t.Fatal("Step after fatal should report no work")
	}
	if len(r.cmds) != 1 {
		t.Fatalf("routed %d commands after fatal, want 1 (no further routing)", len(r.cmds))
	}
}

func TestInjectWithoutReinjectRingReturnsFalse(t *testing.T) {
	s, _, _ := newSeq(t, nil, nil, nil, clockSeq(0))
	if s.Inject(cmd(1)) {
		t.Fatal("Inject should return false when no reinject ring is configured")
	}
}

func TestEmptyStepReturnsFalse(t *testing.T) {
	s, _, _ := newSeq(t, []*spsc.RingCommand{spsc.NewCommand(16)}, nil, nil, clockSeq(0))
	if s.Step() {
		t.Fatal("Step on empty inputs should report no work")
	}
}

func TestInjectThenStepSequences(t *testing.T) {
	rein := spsc.NewCommand(16)
	s, j, r := newSeq(t, nil, nil, rein, clockSeq(50))
	if !s.Inject(cmd(900)) {
		t.Fatal("Inject failed")
	}
	if !s.Step() {
		t.Fatal("Step should sequence the injected command")
	}
	if len(r.cmds) != 1 || r.cmds[0].Seq != 1 || r.cmds[0].OrderID != 900 {
		t.Fatalf("injected routing = %+v, want seq1 id900", r.cmds)
	}
	if len(j.recs) != 1 {
		t.Fatal("injected command must be journaled")
	}
}

// syncingJournal records appends and counts fsyncs (it exposes Sync, so the
// sequencer treats it as a durable journal).
type syncingJournal struct {
	recs  []wal.Record
	syncs int
}

func (j *syncingJournal) Append(r wal.Record) error {
	j.recs = append(j.recs, cloneRecord(r))
	return nil
}
func (j *syncingJournal) Sync() error { j.syncs++; return nil }

func drainSeq(s *Sequencer) {
	for s.Step() {
	}
}

// ---- U2: durable watermark + group-commit flush ----

func TestFlushOnCapAdvancesDurableSeq(t *testing.T) {
	in := spsc.NewCommand(16)
	for id := 0; id < 5; id++ {
		in.Push(cmd(types.OrderID(id)))
	}
	j := &syncingJournal{}
	s := New(Config{Inputs: []*spsc.RingCommand{in}, Journal: j, Router: &fakeRouter{}, Clock: clockSeq(0)})
	s.setFlushCap(3)

	s.Step() // seq1, unsynced1
	s.Step() // seq2, unsynced2
	s.Step() // seq3 -> cap reached -> flush
	if s.DurableSeq() != 3 {
		t.Fatalf("durableSeq = %d, want 3 after cap flush", s.DurableSeq())
	}
	if j.syncs != 1 {
		t.Fatalf("syncs = %d, want 1 (one batch fsync)", j.syncs)
	}
}

func TestFlushOnRingDrainAdvancesDurableSeq(t *testing.T) {
	in := spsc.NewCommand(16)
	in.Push(cmd(1))
	in.Push(cmd(2))
	j := &syncingJournal{}
	s := New(Config{Inputs: []*spsc.RingCommand{in}, Journal: j, Router: &fakeRouter{}, Clock: clockSeq(0)})

	drainSeq(s) // cap=64 not reached; flush fires when the ring drains
	if s.DurableSeq() != 2 || s.Seq() != 2 {
		t.Fatalf("durableSeq=%d seq=%d, want both 2", s.DurableSeq(), s.Seq())
	}
	if j.syncs != 1 {
		t.Fatalf("syncs = %d, want 1 (single drain flush)", j.syncs)
	}
}

func TestReinjectRecordsAreFlushed(t *testing.T) {
	// A stop activation sequenced while the external ring is empty must increment
	// the unsynced count and be flushed before the engine goes idle — its ack
	// must not be stranded above durableSeq.
	rein := spsc.NewCommand(16)
	rein.Push(cmd(900))
	j := &syncingJournal{}
	s := New(Config{Reinject: rein, Inputs: nil, Journal: j, Router: &fakeRouter{}, Clock: clockSeq(0)})

	drainSeq(s)
	if s.DurableSeq() != 1 || s.Seq() != 1 {
		t.Fatalf("durableSeq=%d seq=%d, want both 1 (reinject flushed)", s.DurableSeq(), s.Seq())
	}
	if j.syncs != 1 {
		t.Fatalf("syncs = %d, want 1", j.syncs)
	}
}

func TestNoFsyncWhenIdle(t *testing.T) {
	j := &syncingJournal{}
	s := New(Config{Inputs: []*spsc.RingCommand{spsc.NewCommand(16)}, Journal: j, Router: &fakeRouter{}, Clock: clockSeq(0)})
	if s.Step() {
		t.Fatal("idle Step should report no work")
	}
	if j.syncs != 0 {
		t.Fatalf("syncs = %d, want 0 (no spurious fsync on an idle engine)", j.syncs)
	}
	if s.DurableSeq() != 0 {
		t.Fatalf("durableSeq = %d, want 0", s.DurableSeq())
	}
}

func TestNoopJournalWatermarkTracksSeq(t *testing.T) {
	// fakeJournal exposes no Sync method; the watermark must still advance so
	// in-memory-journal engines (property/differential tests) release acks.
	in := spsc.NewCommand(16)
	for id := 0; id < 3; id++ {
		in.Push(cmd(types.OrderID(id)))
	}
	s, _, _ := newSeq(t, []*spsc.RingCommand{in}, nil, nil, clockSeq(0))
	drainSeq(s)
	if s.DurableSeq() != s.Seq() || s.Seq() != 3 {
		t.Fatalf("durableSeq=%d seq=%d, want both 3", s.DurableSeq(), s.Seq())
	}
}

func TestWalBytesInvariantToFlushCadence(t *testing.T) {
	// R5: the journaled record stream and Seq assignment must be byte-identical
	// regardless of flush cadence (flush emits no records and never touches Seq).
	stream := func() *spsc.RingCommand {
		in := spsc.NewCommand(64)
		for id := 0; id < 40; id++ {
			in.Push(cmd(types.OrderID(id)))
		}
		return in
	}
	run := func(cap int) []wal.Record {
		j := &syncingJournal{}
		s := New(Config{Inputs: []*spsc.RingCommand{stream()}, Journal: j, Router: &fakeRouter{}, Clock: clockSeq(0)})
		s.setFlushCap(cap)
		drainSeq(s)
		if s.DurableSeq() != s.Seq() {
			t.Fatalf("cap %d: durableSeq=%d != seq=%d after drain", cap, s.DurableSeq(), s.Seq())
		}
		return j.recs
	}
	a, b := run(1), run(1000)
	if len(a) != len(b) {
		t.Fatalf("record counts differ across caps: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Seq != b[i].Seq || string(a[i].Payload) != string(b[i].Payload) {
			t.Fatalf("record %d differs across flush caps: %+v vs %+v", i, a[i], b[i])
		}
	}
}

func recSeqs(recs []wal.Record) []uint64 {
	out := make([]uint64, len(recs))
	for i, r := range recs {
		out[i] = r.Seq
	}
	return out
}

// ---- SetSeq (snapshot restore watermark priming) ----

func TestSetSeqPrimesWatermarkAndContinuesContiguously(t *testing.T) {
	in := spsc.NewCommand(16)
	s, _, r := newSeq(t, []*spsc.RingCommand{in}, nil, nil, clockSeq(0))

	// Prime to a mid-stream watermark, as restore would.
	s.SetSeq(42)
	if s.Seq() != 42 {
		t.Fatalf("Seq after SetSeq(42) = %d, want 42", s.Seq())
	}

	// The next sequenced command must continue at 43, not restart at 1.
	in.Push(cmd(700))
	if !s.Step() {
		t.Fatal("Step should sequence the pushed command")
	}
	if s.Seq() != 43 {
		t.Fatalf("Seq after one step = %d, want 43", s.Seq())
	}
	if len(r.cmds) != 1 || r.cmds[0].Seq != 43 {
		t.Fatalf("post-restore command seq = %+v, want 43", r.cmds)
	}
}

func TestSetSeqZeroIsNoOpOnFreshSequencer(t *testing.T) {
	in := spsc.NewCommand(16)
	s, _, _ := newSeq(t, []*spsc.RingCommand{in}, nil, nil, clockSeq(0))
	s.SetSeq(0)
	if s.Seq() != 0 {
		t.Fatalf("Seq after SetSeq(0) = %d, want 0", s.Seq())
	}
	in.Push(cmd(1))
	s.Step()
	if s.Seq() != 1 {
		t.Fatalf("first command after SetSeq(0) = %d, want 1", s.Seq())
	}
}
