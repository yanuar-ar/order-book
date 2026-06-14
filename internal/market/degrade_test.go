package market

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/yanuar-ar/order-book/internal/balance"
	"github.com/yanuar-ar/order-book/internal/sequencer"
	"github.com/yanuar-ar/order-book/internal/spsc"
	"github.com/yanuar-ar/order-book/internal/types"
)

func degradeCmd(seq types.Seq) types.Command {
	return types.Command{Seq: seq, Type: types.CmdDegradeToSolo}
}
func rearmCmd(seq types.Seq) types.Command { return types.Command{Seq: seq, Type: types.CmdRearm} }

// laggingRep is a fixed-watermark replicator for exercising the ack gate.
type laggingRep struct{ repSeq types.Seq }

func (r *laggingRep) Replicate(types.Command) error { return nil }
func (r *laggingRep) Flush() error                  { return nil }
func (r *laggingRep) Drain() error                  { return nil }
func (r *laggingRep) ReplicatedSeq() types.Seq      { return r.repSeq }
func (r *laggingRep) Fatal() error                  { return nil }
func (r *laggingRep) Close() error                  { return nil }

// Positive (AE3): with the standby lagging, the synced gate is min(durable,
// replicated); once degraded it drops to durable alone, releasing the stalled
// tail.
func TestReleaseGate_DegradeDropsReplicationRequirement(t *testing.T) {
	cfg := snapCfg(2)
	core := &Core{
		shards: map[types.MarketID]shardOps{}, ledger: balance.New(balanceConfig(cfg)),
		open: map[types.OrderID]openOrder{}, filters: cfg.Filters, qtyScale: cfg.QtyScale,
		syncRep: true, // sync mode: acks gate on the standby
	}
	in := spsc.NewCommand(16)
	in.Push(dep(1, usdt, 10))
	in.Push(dep(1, usdt, 10))
	in.Push(dep(1, usdt, 10))
	rep := &laggingRep{repSeq: 1}
	seq := sequencer.New(sequencer.Config{
		Inputs: []*spsc.RingCommand{in}, Journal: noopJournal{}, Replicator: rep, Router: core, Clock: counterClock(),
	})
	for seq.Step() {
	}
	if seq.DurableSeq() != 3 {
		t.Fatalf("durableSeq = %d, want 3", seq.DurableSeq())
	}
	if g := releaseGate(seq, core); g != 1 {
		t.Fatalf("synced gate = %d, want min(3,1)=1 (standby lags)", g)
	}
	core.degraded = true
	if g := releaseGate(seq, core); g != 3 {
		t.Fatalf("degraded gate = %d, want 3 (durable alone)", g)
	}
}

// Positive: async replication streams but does NOT gate acks — acks release on
// durability alone even while the standby lags (replication off the critical
// path). Only sync mode waits for the standby.
func TestReleaseGate_AsyncReleasesOnDurable(t *testing.T) {
	cfg := snapCfg(2)
	build := func(sync bool) (*sequencer.Sequencer, *Core) {
		core := &Core{
			shards: map[types.MarketID]shardOps{}, ledger: balance.New(balanceConfig(cfg)),
			open: map[types.OrderID]openOrder{}, filters: cfg.Filters, qtyScale: cfg.QtyScale,
			syncRep: sync,
		}
		in := spsc.NewCommand(16)
		in.Push(dep(1, usdt, 10))
		in.Push(dep(1, usdt, 10))
		in.Push(dep(1, usdt, 10))
		seq := sequencer.New(sequencer.Config{
			Inputs: []*spsc.RingCommand{in}, Journal: noopJournal{},
			Replicator: &laggingRep{repSeq: 1}, Router: core, Clock: counterClock(),
		})
		for seq.Step() {
		}
		return seq, core
	}

	seqAsync, coreAsync := build(false)
	if g := releaseGate(seqAsync, coreAsync); g != 3 {
		t.Fatalf("async gate = %d, want durableSeq=3 (replication off the critical path)", g)
	}
	seqSync, coreSync := build(true)
	if g := releaseGate(seqSync, coreSync); g != 1 {
		t.Fatalf("sync gate = %d, want min(3,1)=1 (waits for the standby)", g)
	}
}

// Positive: degrade and re-arm flip the mode through normal command flow.
func TestDegrade_RearmFlipMode(t *testing.T) {
	e := NewEngine(repCfg())
	defer e.Close()
	run(t, e, dep(1, usdt, 100))
	if e.Degraded() {
		t.Fatal("engine should start synced")
	}
	run(t, e, degradeCmd(0)) // Seq assigned by the sequencer; the test value is ignored
	if !e.Degraded() {
		t.Fatal("degrade command did not set degraded")
	}
	run(t, e, rearmCmd(0))
	if e.Degraded() {
		t.Fatal("re-arm command did not clear degraded")
	}
}

// Edge (AE4): a degrade record replays deterministically — full WAL replay
// reconstructs the gate mode without it living in the state fingerprint.
func TestDegrade_ReconstructedByReplay(t *testing.T) {
	walDir := writeEpochWAL(t,
		epochCmd(1, 0, 1, 100),
		degradeCmd(2),
		epochCmd(3, 0, 1, 50),
	)
	e, err := Recover(snapCfg(2), walDir, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if !e.Degraded() {
		t.Fatal("degraded mode not reconstructed by replay")
	}

	// A subsequent re-arm clears it on replay too.
	walDir2 := writeEpochWAL(t,
		epochCmd(1, 0, 1, 100),
		degradeCmd(2),
		rearmCmd(3),
	)
	e2, err := Recover(snapCfg(2), walDir2, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("recover2: %v", err)
	}
	if e2.Degraded() {
		t.Fatal("re-arm not reconstructed by replay")
	}
}

// Edge (B2): the gate mode survives a snapshot taken while degraded.
func TestDegrade_PersistsThroughSnapshot(t *testing.T) {
	e := NewEngine(snapCfg(2))
	run(t, e, dep(1, usdt, 1000), degradeCmd(0))
	if !e.Degraded() {
		t.Fatal("precondition: engine should be degraded")
	}
	snapDir := t.TempDir()
	if err := e.Snapshot(filepath.Join(snapDir, "snap")); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	re, err := Restore(snapCfg(2), filepath.Join(snapDir, "snap"))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !re.Degraded() {
		t.Fatal("degraded mode not persisted through snapshot")
	}
}

// Edge (boundary agreement): degrade does not change the state fingerprint, so a
// degraded primary and its standby remain fingerprint-equal.
func TestDegrade_NotInFingerprint(t *testing.T) {
	e := NewEngine(repCfg())
	defer e.Close()
	run(t, e, dep(1, usdt, 1000), degradeCmd(0))
	s := e.Standby()
	if !bytes.Equal(e.StateFingerprint(), s.Engine().StateFingerprint()) {
		t.Fatal("degrade leaked into the fingerprint (primary vs standby differ)")
	}
}
