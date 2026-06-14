package sequencer

import (
	"errors"
	"testing"

	"github.com/yanuar-ar/order-book/internal/spsc"
	"github.com/yanuar-ar/order-book/internal/types"
)

// stubReplicator records replicated commands and exposes a settable watermark,
// a settable per-call Replicate error, and a settable latched fatal.
type stubReplicator struct {
	cmds        []types.Command
	repSeq      types.Seq
	replicateOn error // returned by Replicate when non-nil
	fatal       error
}

func (r *stubReplicator) Replicate(c types.Command) error {
	if r.replicateOn != nil {
		return r.replicateOn
	}
	r.cmds = append(r.cmds, c)
	return nil
}
func (r *stubReplicator) Flush() error             { return nil }
func (r *stubReplicator) Drain() error             { return nil }
func (r *stubReplicator) ReplicatedSeq() types.Seq { return r.repSeq }
func (r *stubReplicator) Fatal() error             { return r.fatal }
func (r *stubReplicator) Close() error             { return nil }

func drainSteps(s *Sequencer) {
	for s.Step() {
	}
}

// Positive: every sequenced command is replicated, and the ack gate is the min
// of the durable and replicated watermarks.
func TestReleaseSeqIsMinOfDurableAndReplicated(t *testing.T) {
	in := spsc.NewCommand(16)
	in.Push(cmd(1))
	in.Push(cmd(2))
	in.Push(cmd(3))
	rep := &stubReplicator{repSeq: 2}
	s := New(Config{Inputs: []*spsc.RingCommand{in}, Journal: &fakeJournal{}, Replicator: rep, Router: &fakeRouter{}, Clock: clockSeq(1)})
	drainSteps(s)

	if got := s.DurableSeq(); got != 3 {
		t.Fatalf("durableSeq = %d, want 3", got)
	}
	if got := s.ReleaseSeq(); got != 2 {
		t.Fatalf("ReleaseSeq = %d, want min(3,2)=2 (standby lags)", got)
	}
	rep.repSeq = 5 // standby ahead of durable: durable caps the gate
	if got := s.ReleaseSeq(); got != 3 {
		t.Fatalf("ReleaseSeq = %d, want min(3,5)=3 (durable caps)", got)
	}
	if len(rep.cmds) != 3 {
		t.Fatalf("replicated %d commands, want 3 (every Seq streamed)", len(rep.cmds))
	}
}

// Edge: with the default NopReplicator the gate collapses to durableSeq, so
// replication-off behavior is unchanged.
func TestNopReplicatorReleaseEqualsDurable(t *testing.T) {
	in := spsc.NewCommand(16)
	in.Push(cmd(1))
	in.Push(cmd(2))
	s, _, _ := newSeq(t, []*spsc.RingCommand{in}, nil, nil, clockSeq(1))
	drainSteps(s)
	if s.ReleaseSeq() != s.DurableSeq() {
		t.Fatalf("ReleaseSeq %d != DurableSeq %d with NopReplicator", s.ReleaseSeq(), s.DurableSeq())
	}
}

// Negative: a replicator that dies on its own goroutine halts the idle
// sequencer, so no further output is released above the frozen watermark.
func TestSequencerHaltsOnReplicatorFatal(t *testing.T) {
	in := spsc.NewCommand(16)
	in.Push(cmd(1))
	rep := &stubReplicator{}
	r := &fakeRouter{}
	s := New(Config{Inputs: []*spsc.RingCommand{in}, Journal: &fakeJournal{}, Replicator: rep, Router: r, Clock: clockSeq(1)})
	drainSteps(s) // cmd1 routed + flushed; now idle

	rep.fatal = errors.New("standby link dead")
	s.Step() // idle: observes replicator.Fatal()
	if s.Fatal() == nil {
		t.Fatal("expected fatal latched from replicator death")
	}
	in.Push(cmd(2))
	if s.Step() {
		t.Fatal("Step did work after fatal latched")
	}
	if len(r.cmds) != 1 {
		t.Fatalf("routed %d commands, want 1 (cmd2 never routed after fatal)", len(r.cmds))
	}
}

// Negative: a Replicate error latches fatal before the command is routed — no
// ack is released for an unreplicated command in sync mode.
func TestSequencerHaltsOnReplicateError(t *testing.T) {
	in := spsc.NewCommand(16)
	in.Push(cmd(1))
	rep := &stubReplicator{replicateOn: errors.New("send failed")}
	r := &fakeRouter{}
	s := New(Config{Inputs: []*spsc.RingCommand{in}, Journal: &fakeJournal{}, Replicator: rep, Router: r, Clock: clockSeq(1)})
	s.Step()
	if s.Fatal() == nil {
		t.Fatal("expected fatal from Replicate error")
	}
	if len(r.cmds) != 0 {
		t.Fatalf("routed %d commands, want 0 (route happens after Replicate succeeds)", len(r.cmds))
	}
}
