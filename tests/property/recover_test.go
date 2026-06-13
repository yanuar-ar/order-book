package property

import (
	"bytes"
	"os"
	"testing"

	"github.com/yanuar-ar/order-book/internal/market"
	"github.com/yanuar-ar/order-book/internal/types"
	"github.com/yanuar-ar/order-book/internal/wal"
)

// journalWithSnapshot runs a stream into a journaled engine, taking a snapshot
// after the first `split` orders into snapDir. Returns the WAL dir, snap dir, net
// deposits, and the original engine's final fingerprint.
func journalWithSnapshot(t *testing.T, seed int64, n, split int) (walDir, snapDir string, net map[types.AssetID]int64, wantFP []byte) {
	t.Helper()
	stream := GenSharp(seed, n)
	walDir = t.TempDir()
	snapDir = t.TempDir()

	w, err := wal.OpenWriter(walDir, 0)
	if err != nil {
		t.Fatalf("open WAL: %v", err)
	}
	cfg := engineCfg()
	cfg.Journal = w
	a := market.NewEngine(cfg)

	net = map[types.AssetID]int64{}
	for _, c := range stream.Deposits {
		applyNet(net, a, c)
	}
	for i := 0; i < split; i++ {
		c := stream.Orders[i]
		c.Seq = types.Seq(i + 1)
		applyNet(net, a, c)
	}
	a.Drain()
	ss := market.NewSnapshotter(snapDir, 1, 0, 5, func() int64 { return 0 })
	if err := ss.Snapshot(a, int64(split)); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for i := split; i < len(stream.Orders); i++ {
		c := stream.Orders[i]
		c.Seq = types.Seq(i + 1)
		applyNet(net, a, c)
	}
	a.Drain()
	if err := w.Close(); err != nil {
		t.Fatalf("close WAL: %v", err)
	}
	return walDir, snapDir, net, a.StateFingerprint()
}

func TestRecover_SnapshotPlusTail(t *testing.T) {
	walDir, snapDir, net, wantFP := journalWithSnapshot(t, 1, 600, 250)

	e, err := market.Recover(engineCfg(), walDir, snapDir, nil)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if !bytes.Equal(e.StateFingerprint(), wantFP) {
		t.Fatal("recovered state differs from the original engine")
	}
	// Live-resume watermark: the sequencer is primed to the final journaled Seq.
	full := replayInto(t, walDir)
	if e.Seq() == 0 {
		t.Fatal("recovered engine left the sequencer at 0 — live resume would not be contiguous")
	}
	if !bytes.Equal(e.StateFingerprint(), full.StateFingerprint()) {
		t.Fatal("recovered state differs from a full replay")
	}
	if err := CheckAllInvariants(e, net); err != nil {
		t.Fatalf("recovered state violates invariants: %v", err)
	}
}

// AE4: a corrupt (bad-CRC) snapshot is skipped and recovery falls back to a full
// replay from Seq 0, producing valid state, with the fallback logged.
func TestRecover_FallbackOnCorruptSnapshot(t *testing.T) {
	walDir, snapDir, net, wantFP := journalWithSnapshot(t, 2, 600, 250)

	// Corrupt the snapshot file (flip a byte → CRC fails).
	snapPath, ok := market.LatestSnapshot(snapDir)
	if !ok {
		t.Fatal("no snapshot written")
	}
	raw, _ := os.ReadFile(snapPath)
	raw[len(raw)/2] ^= 0xFF
	if err := os.WriteFile(snapPath, raw, 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	var logged string
	e, err := market.Recover(engineCfg(), walDir, snapDir, func(format string, args ...any) {
		logged = format
	})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if logged == "" {
		t.Fatal("fallback was silent — no log message emitted")
	}
	if !bytes.Equal(e.StateFingerprint(), wantFP) {
		t.Fatal("fallback recovery diverged from the original engine")
	}
	if err := CheckAllInvariants(e, net); err != nil {
		t.Fatalf("fallback-recovered state violates invariants: %v", err)
	}
}
