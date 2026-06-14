package market

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/yanuar-ar/order-book/internal/types"
	"github.com/yanuar-ar/order-book/internal/wal"
)

// epochCmd is a deposit carrying an explicit leadership term. Deposits need no
// book state, so they exercise the recover/replay path without market setup.
func epochCmd(seq types.Seq, epoch uint64, acct types.AccountID, amount int64) types.Command {
	return types.Command{
		Seq: seq, Epoch: epoch, Type: types.CmdDeposit,
		Account: acct, Asset: usdt, Amount: amount,
	}
}

// writeEpochWAL journals cmds (in order, Seq must be contiguous from 1) to a
// fresh WAL directory, stamping each record's payload with the command's epoch.
func writeEpochWAL(t *testing.T, cmds ...types.Command) string {
	t.Helper()
	dir := t.TempDir()
	w, err := wal.OpenWriter(dir, 0)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	for _, c := range cmds {
		rec := wal.Record{
			Seq:     uint64(c.Seq),
			TsNanos: c.TsNanos,
			Type:    uint16(c.Type),
			Payload: types.EncodeCommand(c),
		}
		if err := w.Append(rec); err != nil {
			t.Fatalf("append seq %d: %v", c.Seq, err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return dir
}

// Positive: a node's leadership term is reconstructed from the WAL on a cold
// restart, so fencing survives a restart with no snapshot ahead of the tail.
func TestRecover_EpochAdvancesMonotonically(t *testing.T) {
	walDir := writeEpochWAL(t,
		epochCmd(1, 0, 1, 100),
		epochCmd(2, 0, 1, 50),
		epochCmd(3, 2, 1, 25), // a promotion bumped the term to 2 mid-log
		epochCmd(4, 2, 1, 25),
	)
	e, err := Recover(snapCfg(2), walDir, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if e.Epoch() != 2 {
		t.Fatalf("recovered epoch = %d, want 2 (highest term in the log)", e.Epoch())
	}
	if e.Seq() != 4 {
		t.Fatalf("recovered seq = %d, want 4", e.Seq())
	}
}

// Negative: a record whose term steps backwards (a spliced zombie write from a
// fenced old primary) halts recovery — it is never applied.
func TestRecover_StaleEpochFenced(t *testing.T) {
	walDir := writeEpochWAL(t,
		epochCmd(1, 1, 1, 100),
		epochCmd(2, 1, 1, 50),
		epochCmd(3, 0, 1, 25), // backwards term: must be fenced
	)
	if _, err := Recover(snapCfg(2), walDir, t.TempDir(), nil); err != ErrStaleEpoch {
		t.Fatalf("recover err = %v, want ErrStaleEpoch", err)
	}
}

// Edge: epoch persists through a snapshot. A promoted node that snapshots at
// term e and cold-restarts from snapshot + empty tail keeps term e, so a revived
// stale-epoch primary is still fenced.
func TestRecover_EpochPersistsThroughSnapshot(t *testing.T) {
	e := NewEngine(snapCfg(2))
	run(t, e, dep(1, usdt, 1000))
	e.SetEpoch(3) // simulate a promotion to term 3

	snapDir := t.TempDir()
	snapPath := filepath.Join(snapDir, fmt.Sprintf("%020d.snap", uint64(e.Seq())))
	if err := e.Snapshot(snapPath); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// Cold restart from snapshot + empty WAL tail.
	re, err := Recover(snapCfg(2), t.TempDir(), snapDir, nil)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if re.Epoch() != 3 {
		t.Fatalf("recovered epoch = %d, want 3 (preserved through snapshot)", re.Epoch())
	}
}
