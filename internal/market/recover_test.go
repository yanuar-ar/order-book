package market

import (
	"path/filepath"
	"testing"

	"github.com/yanuar-ar/order-book/internal/wal"
)

// A snapshot that is byte-valid and CRC-clean but logically inconsistent (a
// reservation amount no longer matches the reserved balance) must be rejected by
// the post-rebuild self-check, not loaded as poisoned state.
func TestRestore_SelfCheckRejectsLogicalCorruption(t *testing.T) {
	e := populated(t)
	good := filepath.Join(t.TempDir(), "good")
	if err := e.Snapshot(good); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	seq, sections, err := wal.ReadSnapshot(good)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	// Corrupt the last reservation's `remaining` (the final 8-byte field before
	// the 1-byte side) in the ledger section. CRC is recomputed on write, so this
	// is logically — not physically — corrupt.
	led := sections[secLedger]
	if len(led) < 10 {
		t.Fatal("ledger section unexpectedly small — no reservations to corrupt")
	}
	led[len(led)-5] ^= 0xFF

	bad := filepath.Join(t.TempDir(), "bad")
	if err := wal.WriteSnapshot(bad, seq, sections); err != nil {
		t.Fatalf("write tampered snapshot: %v", err)
	}

	if _, err := Restore(snapCfg(2), bad); err != ErrSnapshotIncompatible {
		t.Fatalf("expected ErrSnapshotIncompatible from self-check, got %v", err)
	}
}
