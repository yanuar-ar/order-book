# internal/wal

Single-node durability: a segmented **write-ahead log**, snapshots, and replay.
Clustering/failover and backup/DR are out of scope for v1.

## Files

- `wal.go` — `Writer`: appends records to segmented log files (`%06d.wal`,
  default 1 GiB/segment). Used by a **single writer goroutine** (the sequencer).
- `record.go` — on-disk record framing.
- `snapshot.go` — point-in-time state snapshot (book + ledger).
- `replay.go` — replay records (optionally from a snapshot) to rebuild state.

## Constraints

- Single writer only. The record/segment format and the `types` codec byte
  layout are a **durability contract** — changing either breaks recovery of
  existing logs. Version the format if you must change it.
- Replay must be a pure re-application: same log → byte-identical state. No
  wall-clock, no regeneration of journaled events (stops are already logged).

## Testing (positive / negative / edge + invariant)

`wal_test.go` exists. Cover:
- **Positive**: append → replay reproduces records exactly; segment rollover at
  the size boundary; `load snapshot@S + replay (S,N] == replay [0,N]`
  (`INV-DET-02`).
- **Negative/edge**: **torn-tail** (truncated final record) — recovery
  truncates cleanly and the rebuilt state still satisfies all A–E invariants
  (`INV-DET-03`); empty dir; multi-segment replay ordering.
- **Determinism**: two replays of the same log → byte-identical (`INV-DET-01`).
