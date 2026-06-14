package property

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/yanuar-ar/order-book/internal/market"
	"github.com/yanuar-ar/order-book/internal/types"
	"github.com/yanuar-ar/order-book/internal/wal"
)

// runWithWAL applies a stream to a fresh engine backed by a real WAL in a temp
// dir and returns the canonical state digest plus the raw journaled bytes. When
// async is set, journaling runs on the AsyncJournaller goroutine.
func runWithWAL(t *testing.T, s Stream, async bool) (digest string, walBytes []byte) {
	t.Helper()
	dir := t.TempDir()
	w, err := wal.OpenWriter(dir, 0)
	if err != nil {
		t.Fatalf("open WAL: %v", err)
	}
	cfg := engineCfg()
	cfg.Journal = w
	if async {
		cfg.AsyncJournal = true
		cfg.JournalCore = -1
	}
	e := market.NewEngine(cfg)
	for _, c := range s.Deposits {
		e.Submit(c)
	}
	for i, c := range s.Orders {
		c.Seq = types.Seq(i + 1)
		e.Submit(c)
	}
	e.Drain()                         // quiesce + barrier on durability
	if err := e.Close(); err != nil { // stop async journaller, final flush
		t.Fatalf("engine close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("WAL close: %v", err)
	}
	return engineState(e).Canonical(), readWALBytes(t, dir)
}

func readWALBytes(t *testing.T, dir string) []byte {
	t.Helper()
	segs, err := filepath.Glob(filepath.Join(dir, "*.wal"))
	if err != nil {
		t.Fatalf("glob WAL: %v", err)
	}
	sort.Strings(segs)
	var out []byte
	for _, seg := range segs {
		b, err := os.ReadFile(seg)
		if err != nil {
			t.Fatalf("read %s: %v", seg, err)
		}
		out = append(out, b...)
	}
	return out
}

// TestAsyncWALByteIdenticalToSync is the core safety property (R1, R10): moving
// journaling off the matcher goroutine produces a byte-identical WAL and an
// identical final state — the async path is determinism-transparent.
func TestAsyncWALByteIdenticalToSync(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    Stream
	}{
		{"broad", GenBroad(7, 1500)},
		{"sharp", GenSharp(7, 1500)},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dSync, walSync := runWithWAL(t, tc.s, false)
			dAsync, walAsync := runWithWAL(t, tc.s, true)
			if dSync != dAsync {
				t.Fatal("async state digest differs from sync")
			}
			if !bytes.Equal(walSync, walAsync) {
				t.Fatalf("async WAL bytes differ from sync (%d vs %d) — journaling reordered records",
					len(walSync), len(walAsync))
			}
		})
	}
}

// TestAsyncSameSeedDeterministic: the async path itself is deterministic — the
// same seed run twice yields identical WAL bytes and state (output-side watermark
// timing must not leak into state).
func TestAsyncSameSeedDeterministic(t *testing.T) {
	s := GenBroad(7, 1500)
	d1, w1 := runWithWAL(t, s, true)
	d2, w2 := runWithWAL(t, s, true)
	if d1 != d2 {
		t.Fatal("async same-seed state diverged")
	}
	if !bytes.Equal(w1, w2) {
		t.Fatal("async same-seed WAL bytes diverged")
	}
}

// TestDifferentialAsyncMatchesOracle (U7): the async-journaller engine agrees
// with the independent reference oracle on canonical state and every invariant
// after each command, across the broad and sharp generators.
func TestDifferentialAsyncMatchesOracle(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    Stream
	}{
		{"broad-1", GenBroad(1, 1000)},
		{"broad-42", GenBroad(42, 1000)},
		{"sharp-2", GenSharp(2, 1000)},
		{"sharp-99", GenSharp(99, 1000)},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if err := RunDifferentialAsync(tc.s); err != nil {
				t.Fatalf("async engine diverged from oracle: %v", err)
			}
		})
	}
}

// TestAsyncWatermarkInvariants exercises INV-JRN-02/03/04 dynamically: across a
// run the durable watermark is monotonic and never exceeds Seq, every exposed
// ack is at or below it, and after Drain it equals Seq.
func TestAsyncWatermarkInvariants(t *testing.T) {
	dir := t.TempDir()
	w, err := wal.OpenWriter(dir, 0)
	if err != nil {
		t.Fatalf("open WAL: %v", err)
	}
	defer w.Close()
	cfg := engineCfg()
	cfg.Journal = w
	cfg.AsyncJournal = true
	cfg.JournalCore = -1
	cfg.JournalBatchCap = 16 // small batch → the watermark advances mid-run
	e := market.NewEngine(cfg)
	defer e.Close()

	s := GenSharp(13, 1200)
	for _, c := range s.Deposits {
		e.Submit(c)
	}
	var prevDurable types.Seq
	checkInvariants := func() {
		d := e.DurableSeq()
		if d < prevDurable {
			t.Fatalf("INV-JRN-02 violated: durableSeq went backward %d -> %d", prevDurable, d)
		}
		if d > e.Seq() {
			t.Fatalf("watermark %d exceeds Seq %d", d, e.Seq())
		}
		for _, a := range e.Acks() {
			if a.Seq > d {
				t.Fatalf("INV-JRN-03 violated: ack Seq %d above durableSeq %d", a.Seq, d)
			}
		}
		prevDurable = d
	}
	for i, c := range s.Orders {
		c.Seq = types.Seq(i + 1)
		for !e.Submit(c) {
			e.Step()
		}
		e.Step()
		checkInvariants()
	}
	e.Drain()
	if e.DurableSeq() != e.Seq() {
		t.Fatalf("INV-JRN-04 violated: after Drain durableSeq=%d != Seq=%d", e.DurableSeq(), e.Seq())
	}
}
