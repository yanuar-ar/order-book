package property

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/yanuar-ar/order-book/internal/market"
	"github.com/yanuar-ar/order-book/internal/types"
	"github.com/yanuar-ar/order-book/internal/wal"
)

// replayTailInto replays WAL records with Seq > afterSeq into a stops-suppressed
// engine restored from a snapshot.
func replayTailInto(t *testing.T, e *market.Engine, dir string, afterSeq types.Seq) {
	t.Helper()
	err := wal.Replay(dir, uint64(afterSeq), func(rec wal.Record) error {
		cmd, derr := types.DecodeCommand(rec.Payload)
		if derr != nil {
			return derr
		}
		e.ApplyJournaled(cmd)
		return nil
	})
	if err != nil {
		t.Fatalf("tail replay: %v", err)
	}
}

// checkSnapshotEquivalence is the INV-DET-02 body: run a stream into a journaled
// engine, snapshot at a mid-stream Seq S (after the first `split` orders), finish
// the stream, then assert that (restore@S + replay (S,N]) reproduces byte-
// identical state to a full replay of [0,N] — and to the original engine.
func checkSnapshotEquivalence(t *testing.T, seed int64, n, split int) {
	t.Helper()
	snapshotEquivalence(t, GenSharp(seed, n), split, seed)
}

func snapshotEquivalence(t *testing.T, stream Stream, split int, seed int64) {
	t.Helper()
	dir := t.TempDir()

	w, err := wal.OpenWriter(dir, 0)
	if err != nil {
		t.Fatalf("open WAL: %v", err)
	}
	cfg := engineCfg()
	cfg.Journal = w
	a := market.NewEngine(cfg)

	net := map[types.AssetID]int64{}
	for _, c := range stream.Deposits {
		applyNet(net, a, c)
	}
	for i := 0; i < split && i < len(stream.Orders); i++ {
		c := stream.Orders[i]
		c.Seq = types.Seq(i + 1)
		applyNet(net, a, c)
	}
	a.Drain()

	snapPath := filepath.Join(dir, "snap")
	if err := a.Snapshot(snapPath); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	s := a.Seq()

	for i := split; i < len(stream.Orders); i++ {
		c := stream.Orders[i]
		c.Seq = types.Seq(i + 1)
		applyNet(net, a, c)
	}
	a.Drain()
	if err := w.Close(); err != nil {
		t.Fatalf("close WAL: %v", err)
	}

	// Reference: a full replay of the whole WAL from Seq 0.
	full := replayInto(t, dir)

	// Under test: restore the snapshot, then replay only the tail (Seq > S).
	restored, err := market.Restore(engineCfg(), snapPath)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	replayTailInto(t, restored, dir, s)

	// INV-DET-02: restore+tail == full replay == the original live engine.
	// Equality is over the complete state fingerprint (books incl. iceberg
	// peak/hidden, ledger incl. per-order reservations, stops, open-map). The
	// sequencer counter is intentionally excluded: replay via ApplyJournaled does
	// not advance it (priming it to the final watermark for live resume is U11's
	// job), so it is not part of the replayed-state equivalence.
	if !bytes.Equal(restored.StateFingerprint(), full.StateFingerprint()) {
		t.Fatalf("INV-DET-02 (seed %d, split %d): restore+tail diverged from full replay", seed, split)
	}
	if !bytes.Equal(restored.StateFingerprint(), a.StateFingerprint()) {
		t.Fatalf("INV-DET-02 (seed %d, split %d): restore+tail diverged from the live engine", seed, split)
	}
	if err := CheckAllInvariants(restored, net); err != nil {
		t.Fatalf("restored+tail state violates invariants (seed %d): %v", seed, err)
	}
}

// TestRecoverySnapshotReplayEquivalence asserts INV-DET-02 across many seeds and
// snapshot points, including the boundary splits (very early / very late) and the
// no-tail case (snapshot after the last order).
func TestRecoverySnapshotReplayEquivalence(t *testing.T) {
	const n = 800
	for seed := int64(1); seed <= 12; seed++ {
		seed := seed
		t.Run("", func(t *testing.T) {
			t.Parallel()
			// A spread of snapshot points: near-start, middle, near-end, and no-tail.
			for _, split := range []int{1, n / 3, n / 2, (2 * n) / 3, n - 1, n} {
				checkSnapshotEquivalence(t, seed, n, split)
			}
		})
	}
}

// FuzzSnapshotEquivalence is the coverage-guided INV-DET-02 guard: for any decoded
// stream it snapshots at the midpoint and asserts restore+tail == full replay.
// Seed corpus is the permanent regression set.
// Run: go test -run '^$' -fuzz=FuzzSnapshotEquivalence ./tests/property/
func FuzzSnapshotEquivalence(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{2, 1, 0, 0, 1, 100, 5, 0, 2, 1, 1, 0, 3, 100, 5, 0})
	f.Add(bytes.Repeat([]byte{3, 2, 1, 0, 5, 100, 2, 0}, 12))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			data = data[:4096]
		}
		stream := decodeStream(data)
		if len(stream.Orders) == 0 {
			return
		}
		snapshotEquivalence(t, stream, len(stream.Orders)/2, 0)
	})
}
