package market

import (
	"errors"
	"testing"

	"github.com/yanuar-ar/order-book/internal/balance"
	"github.com/yanuar-ar/order-book/internal/types"
	"github.com/yanuar-ar/order-book/internal/wal"
)

func fixedClock(v *int64) func() int64 { return func() int64 { return *v } }

// alwaysFailJournal fails every Append, used to drive a fail-stop during a
// snapshot's drain.
type alwaysFailJournal struct{}

func (alwaysFailJournal) Append(wal.Record) error { return errors.New("journal append failed") }

// ---- U4: snapshot ordering vs the durable watermark ----

func TestSnapshot_SeqNeverExceedsDurable(t *testing.T) {
	e := newEng(0, 0, 100, false)
	for _, c := range []types.Command{dep(1, usdt, 1000), dep(2, btc, 10), order(m0, 1, 10, types.Buy, types.Limit, 100, 5)} {
		if !e.Submit(c) {
			t.Fatal("ingress full")
		}
	}
	// Step without draining: speculative state exists above the watermark.
	for i := 0; i < 3; i++ {
		e.Step()
	}
	if e.DurableSeq() >= e.Seq() {
		t.Fatalf("precondition: durableSeq=%d should trail seq=%d before snapshot", e.DurableSeq(), e.Seq())
	}

	dir := t.TempDir()
	clk := int64(0)
	s := NewSnapshotter(dir, 1, 0, 5, fixedClock(&clk))
	s.Anchor(0)
	if wrote, err := s.Maybe(e, 3); err != nil || !wrote {
		t.Fatalf("snapshot did not fire (wrote=%v err=%v)", wrote, err)
	}
	// The snapshot drained first, so the watermark caught up and the published
	// file (named by Seq) is fully durable.
	if e.DurableSeq() != e.Seq() {
		t.Fatalf("after snapshot durableSeq=%d != seq=%d", e.DurableSeq(), e.Seq())
	}
	if files, _ := snapshotFiles(dir); len(files) != 1 {
		t.Fatalf("expected 1 snapshot file, got %d", len(files))
	}
}

func TestSnapshot_AbortsOnFailStopDuringDrain(t *testing.T) {
	cfg := Config{
		Markets:  map[types.MarketID]balance.MarketSpec{m0: {Base: btc, Quote: usdt}},
		QtyScale: 1, FeeScale: 100, RingSize: 1024, CapHint: 256,
		Journal: alwaysFailJournal{},
	}
	e := NewEngine(cfg)
	if !e.Submit(dep(1, usdt, 1000)) {
		t.Fatal("ingress full")
	}

	dir := t.TempDir()
	clk := int64(0)
	s := NewSnapshotter(dir, 1, 0, 5, fixedClock(&clk))
	s.Anchor(0)
	// Snapshot drains, the pending command's append fails -> fatal latches ->
	// Snapshot must return the error and publish nothing.
	if _, err := s.Maybe(e, 1); err == nil {
		t.Fatal("snapshot should abort on a fail-stop during drain")
	}
	if e.Fatal() == nil {
		t.Fatal("engine should be fail-stopped")
	}
	if files, _ := snapshotFiles(dir); len(files) != 0 {
		t.Fatalf("no snapshot should be published on fail-stop, got %d files", len(files))
	}
}

// ---- Count trigger ----

func TestSnapshotter_CountTrigger(t *testing.T) {
	e := populated(t)
	dir := t.TempDir()
	clk := int64(0)
	s := NewSnapshotter(dir, 2, 0, 5, fixedClock(&clk))
	s.Anchor(0)

	if wrote, _ := s.Maybe(e, 1); wrote {
		t.Fatal("snapshot fired before reaching the count threshold")
	}
	if wrote, err := s.Maybe(e, 2); err != nil || !wrote {
		t.Fatalf("snapshot did not fire at the count threshold (wrote=%v err=%v)", wrote, err)
	}
	files, _ := snapshotFiles(dir)
	if len(files) != 1 {
		t.Fatalf("expected 1 snapshot file, got %d", len(files))
	}
}

// ---- Time trigger ----

func TestSnapshotter_TimeTrigger(t *testing.T) {
	e := populated(t)
	dir := t.TempDir()
	clk := int64(100)
	s := NewSnapshotter(dir, 0, 60, 5, fixedClock(&clk))
	s.Anchor(0) // lastTime = 100

	if wrote, _ := s.Maybe(e, 999); wrote {
		t.Fatal("time snapshot fired with no elapsed time")
	}
	clk = 159
	if wrote, _ := s.Maybe(e, 999); wrote {
		t.Fatal("time snapshot fired one second early")
	}
	clk = 160
	if wrote, err := s.Maybe(e, 999); err != nil || !wrote {
		t.Fatalf("time snapshot did not fire at the interval (wrote=%v err=%v)", wrote, err)
	}
}

// ---- Retention / GC ----

func TestSnapshotter_RetentionKeepsNewest(t *testing.T) {
	e := populated(t)
	dir := t.TempDir()
	clk := int64(0)
	s := NewSnapshotter(dir, 1, 0, 2, fixedClock(&clk)) // everyN=1, keep 2
	applied := int64(100)
	s.Anchor(applied)

	for i := 0; i < 4; i++ {
		// Advance the engine's Seq so each snapshot gets a distinct filename.
		run(t, e, order(m0, 1, types.OrderID(200+i), types.Buy, types.Limit, 80, 1))
		applied++
		if wrote, err := s.Maybe(e, applied); err != nil || !wrote {
			t.Fatalf("snapshot %d did not fire (wrote=%v err=%v)", i, wrote, err)
		}
	}
	files, _ := snapshotFiles(dir)
	if len(files) != 2 {
		t.Fatalf("retention kept %d files, want 2", len(files))
	}
	// The retained files are the two newest, and LatestSnapshot points at the last.
	latest, ok := LatestSnapshot(dir)
	if !ok {
		t.Fatal("LatestSnapshot found nothing")
	}
	if latest[len(latest)-len(files[len(files)-1]):] != files[len(files)-1] {
		t.Fatalf("LatestSnapshot = %q, want suffix %q", latest, files[len(files)-1])
	}
}

func TestLatestSnapshot_EmptyDir(t *testing.T) {
	if _, ok := LatestSnapshot(t.TempDir()); ok {
		t.Fatal("LatestSnapshot reported a file in an empty dir")
	}
	if _, ok := LatestSnapshot(t.TempDir() + "/does-not-exist"); ok {
		t.Fatal("LatestSnapshot reported a file in a missing dir")
	}
}
