package sequencer

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/yanuar-ar/order-book/internal/types"
)

// fakeLink is an in-memory StandbyLink for testing the AsyncReplicator. wal holds
// the full durable command history (indexed by Seq-1) so Fetch can backfill
// dropped records; applied records what Send actually delivered, in order.
type fakeLink struct {
	wal     []types.Command // wal[i] has Seq i+1
	applied []types.Command // consumer-goroutine only; read by test after Close
	acked   atomic.Uint64
	gate    atomic.Bool // when true, Send spins (simulates a slow/blocked standby)
	fatal   atomic.Pointer[error]
}

func (l *fakeLink) Send(c types.Command) error {
	for l.gate.Load() {
		if l.fatal.Load() != nil {
			break
		}
	}
	if e := l.fatal.Load(); e != nil {
		return *e
	}
	l.applied = append(l.applied, c)
	l.acked.Store(uint64(c.Seq))
	return nil
}

func (l *fakeLink) AckedSeq() types.Seq { return types.Seq(l.acked.Load()) }

func (l *fakeLink) Fetch(afterSeq types.Seq) ([]types.Command, error) {
	if int(afterSeq) >= len(l.wal) {
		return nil, nil
	}
	return l.wal[afterSeq:], nil // wal[afterSeq] has Seq afterSeq+1
}

func (l *fakeLink) Fatal() error {
	if e := l.fatal.Load(); e != nil {
		return *e
	}
	return nil
}
func (l *fakeLink) Close() error { return nil }

func (l *fakeLink) setFatal(err error) { l.fatal.Store(&err) }

func repCmd(seq types.Seq) types.Command {
	return types.Command{Seq: seq, Type: types.CmdNewOrder, OrderID: types.OrderID(seq), Qty: 1}
}

func walOf(n int) []types.Command {
	w := make([]types.Command, n)
	for i := 0; i < n; i++ {
		w[i] = repCmd(types.Seq(i + 1))
	}
	return w
}

// Positive: every replicated command reaches the standby in order, and the
// replicated watermark converges to the last submitted Seq.
func TestAsyncReplicator_StreamsAndConverges(t *testing.T) {
	const n = 5000
	link := &fakeLink{wal: walOf(n)}
	r := NewAsyncReplicator(link, 1024, -1)
	for i := 1; i <= n; i++ {
		if err := r.Replicate(repCmd(types.Seq(i))); err != nil {
			t.Fatalf("replicate %d: %v", i, err)
		}
	}
	if err := r.Drain(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got := r.ReplicatedSeq(); got != types.Seq(n) {
		t.Fatalf("replicatedSeq = %d, want %d", got, n)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	assertContiguous(t, link.applied, n)
}

// Edge (the decouple guarantee): with the standby blocked, Replicate never
// stalls the producer — every command is submitted and the ring overflows —
// while replicatedSeq stays frozen. Releasing the standby converges without loss,
// because dropped records are backfilled from the WAL.
func TestAsyncReplicator_NonBlockingUnderSlowStandby(t *testing.T) {
	const n = 20000
	link := &fakeLink{wal: walOf(n)}
	link.gate.Store(true) // standby blocked from the start
	r := NewAsyncReplicator(link, 256, -1)

	for i := 1; i <= n; i++ {
		if err := r.Replicate(repCmd(types.Seq(i))); err != nil {
			t.Fatalf("replicate %d: %v", i, err)
		}
	}
	// Producer finished despite the blocked standby: the ring (256) cannot hold
	// 20000, so this only returns if Replicate is truly non-blocking.
	if got := r.lastSubmitted.Load(); got != n {
		t.Fatalf("lastSubmitted = %d, want %d", got, n)
	}
	if got := r.ReplicatedSeq(); got >= types.Seq(n) {
		t.Fatalf("replicatedSeq = %d advanced while standby blocked — not decoupled", got)
	}

	link.gate.Store(false) // standby recovers
	if err := r.Drain(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got := r.ReplicatedSeq(); got != types.Seq(n) {
		t.Fatalf("replicatedSeq = %d after recovery, want %d", got, n)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// No record lost despite massive ring overflow: backfill recovered them all.
	assertContiguous(t, link.applied, n)
}

// Negative: a link that dies latches the replicator fatal, so Replicate surfaces
// it and the consumer stops.
func TestAsyncReplicator_FatalFromLink(t *testing.T) {
	link := &fakeLink{wal: walOf(100)}
	r := NewAsyncReplicator(link, 1024, -1)
	link.setFatal(errors.New("standby link dead"))
	// The consumer observes link.Fatal() and latches; Replicate then surfaces it.
	var lastErr error
	for i := 1; i <= 100; i++ {
		if err := r.Replicate(repCmd(types.Seq(i))); err != nil {
			lastErr = err
			break
		}
	}
	_ = r.Close()
	if lastErr == nil && r.Fatal() == nil {
		t.Fatal("expected a latched fatal from the dead link")
	}
}

// assertContiguous checks applied is exactly Seq 1..n in order, no gaps/dups.
func assertContiguous(t *testing.T, applied []types.Command, n int) {
	t.Helper()
	if len(applied) != n {
		t.Fatalf("standby applied %d commands, want %d (gap or duplicate)", len(applied), n)
	}
	for i, c := range applied {
		if c.Seq != types.Seq(i+1) {
			t.Fatalf("applied[%d].Seq = %d, want %d (out of order)", i, c.Seq, i+1)
		}
	}
}

// BenchmarkReplicate gates the producer-side hand-off at zero allocations.
func BenchmarkReplicate(b *testing.B) {
	link := &fakeLink{wal: walOf(b.N + 1)}
	r := NewAsyncReplicator(link, 1<<16, -1)
	defer r.Close()
	c := repCmd(1)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Seq = types.Seq(i + 1)
		_ = r.Replicate(c)
	}
}
