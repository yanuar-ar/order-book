package property

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yanuar-ar/order-book/internal/market"
	"github.com/yanuar-ar/order-book/internal/types"
	"github.com/yanuar-ar/order-book/internal/wal"
)

// journalStream runs a stream through a fresh engine whose Journal is a real
// WAL writer in dir, then closes the writer and returns the engine's canonical
// state plus the exact net external flow (accounting for any withdrawals).
func journalStream(t *testing.T, dir string, s Stream) (string, map[types.AssetID]int64) {
	t.Helper()
	w, err := wal.OpenWriter(dir, 0)
	if err != nil {
		t.Fatalf("open WAL: %v", err)
	}
	cfg := engineCfg()
	cfg.Journal = w
	e := market.NewEngine(cfg)
	net := feedTrackingNet(e, s)
	if err := w.Close(); err != nil {
		t.Fatalf("close WAL: %v", err)
	}
	return engineState(e).Canonical(), net
}

// replayInto builds a stops-suppressed engine and replays the WAL in dir into
// it (activations are already journaled, so re-triggering must be suppressed).
func replayInto(t *testing.T, dir string) *market.Engine {
	t.Helper()
	cfg := engineCfg()
	cfg.SuppressStops = true
	e := market.NewEngine(cfg)
	err := wal.Replay(dir, 0, func(rec wal.Record) error {
		cmd, derr := types.DecodeCommand(rec.Payload)
		if derr != nil {
			return derr
		}
		e.ApplyJournaled(cmd)
		return nil
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	return e
}

// TestRecoveryFullReplayEquivalence asserts INV-DET-01 on the journaled path:
// replaying the WAL into a fresh engine reproduces byte-identical state, and
// the rebuilt state satisfies every invariant.
func TestRecoveryFullReplayEquivalence(t *testing.T) {
	dir := t.TempDir()
	s := GenSharp(5, 1200)
	want, net := journalStream(t, dir, s)

	e2 := replayInto(t, dir)
	if got := engineState(e2).Canonical(); got != want {
		t.Fatal("replayed state differs from original")
	}
	if err := CheckAllInvariants(e2, net); err != nil {
		t.Fatalf("replayed state violates invariants: %v", err)
	}
}

// TestRecoveryTornTail asserts INV-DET-03: truncating the final WAL record
// (a torn write) lets replay stop cleanly, and the rebuilt state still
// satisfies every A–E invariant.
func TestRecoveryTornTail(t *testing.T) {
	dir := t.TempDir()
	s := GenSharp(6, 600)
	journalStream(t, dir, s)

	// Truncate a few bytes off the last segment to simulate a torn tail.
	segs, err := filepath.Glob(filepath.Join(dir, "*.wal"))
	if err != nil || len(segs) == 0 {
		t.Fatalf("no WAL segments: %v", err)
	}
	last := segs[len(segs)-1]
	info, err := os.Stat(last)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < 6 {
		t.Fatalf("segment too small to torn-truncate: %d bytes", info.Size())
	}
	if err := os.Truncate(last, info.Size()-5); err != nil {
		t.Fatal(err)
	}

	// Replay must not error on the torn tail (it stops cleanly there).
	e2 := replayInto(t, dir)
	if err := e2.Ledger().Verify(); err != nil {
		t.Fatalf("torn-tail recovery violates ledger invariants: %v", err)
	}
	for _, m := range e2.MarketIDs() {
		if err := e2.Shard(m).Book().Verify(); err != nil {
			t.Fatalf("torn-tail recovery violates book invariants: %v", err)
		}
	}
}

// TestRecoveryMetamorphicCancelAll asserts INV-MET-01/02: after cancelling every
// open order and pending stop, no reserved funds remain and conservation holds.
func TestRecoveryMetamorphicCancelAll(t *testing.T) {
	e := market.NewEngine(engineCfg())
	s := GenSharp(8, 1000)
	net := feedTrackingNet(e, s)

	for _, m := range e.MarketIDs() {
		for _, o := range e.Shard(m).Book().Dump() {
			e.Submit(types.Command{Type: types.CmdCancel, OrderID: o.ID})
		}
		for _, st := range e.Shard(m).StopDump() {
			e.Submit(types.Command{Type: types.CmdCancel, OrderID: st.OrderID})
		}
	}
	e.Drain()

	for _, m := range e.MarketIDs() {
		if n := len(e.Shard(m).Book().Dump()); n != 0 {
			t.Fatalf("market %d still has %d resting orders after cancel-all", m, n)
		}
		if n := len(e.Shard(m).StopDump()); n != 0 {
			t.Fatalf("market %d still has %d pending stops after cancel-all", m, n)
		}
	}
	bals, _ := e.Ledger().Dump()
	for _, b := range bals {
		if b.Reserved != 0 {
			t.Fatalf("INV-MET-02: acct %d asset %d still reserved %d after cancel-all", b.Acct, b.Asset, b.Reserved)
		}
	}
	if err := CheckAllInvariants(e, net); err != nil {
		t.Fatalf("conservation broken after cancel-all: %v", err)
	}
}

// TestRecoveryMetamorphicBatchInvariance asserts INV-MET-04: draining the stream
// in two batches yields the same state as draining it all at once.
func TestRecoveryMetamorphicBatchInvariance(t *testing.T) {
	s := GenSharp(9, 1000)
	all := append(append([]types.Command{}, s.Deposits...), s.Orders...)

	oneShot := market.NewEngine(engineCfg())
	for _, c := range all {
		oneShot.Submit(c)
	}
	oneShot.Drain()

	twoBatch := market.NewEngine(engineCfg())
	mid := len(all) / 2
	for _, c := range all[:mid] {
		twoBatch.Submit(c)
	}
	twoBatch.Drain()
	for _, c := range all[mid:] {
		twoBatch.Submit(c)
	}
	twoBatch.Drain()

	if engineState(oneShot).Canonical() != engineState(twoBatch).Canonical() {
		t.Fatal("INV-MET-04: two-batch draining diverged from one-shot")
	}
}
