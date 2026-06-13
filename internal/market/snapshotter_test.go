package market

import (
	"testing"

	"github.com/yanuar-ar/order-book/internal/types"
)

func fixedClock(v *int64) func() int64 { return func() int64 { return *v } }

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
