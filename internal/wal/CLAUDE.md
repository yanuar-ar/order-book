# internal/wal

Single-node durability: a segmented **write-ahead log**, snapshots, and replay.
Clustering/failover and backup/DR are out of scope for v1.

## Files

- `wal.go` — `Writer`: appends records to segmented log files (`%06d.wal`,
  default 1 GiB/segment). Used by a **single writer goroutine** (the sequencer).
  `Append` frames into a reusable buffer and **buffers the batch in memory** (no
  syscall); `Sync` flushes it with **one `write` + one `fsync`** (group-commit
  batches both syscalls, not just the fsync). Buffering until `Sync` is safe under
  the durable-ack barrier — nothing is durable until the watermark advances on
  `Sync`. `Append` does not retain `Record.Payload` past the call (it copies),
  and is zero-alloc in steady state (gated by `TestAppendZeroAlloc`). Batch size
  is the sequencer's `flushCap`, which governs durable throughput vs durable-ack
  latency.
- `record.go` — on-disk record framing.
- `snapshot.go` — point-in-time state snapshot (book + ledger). The container
  header carries the watermark `Seq` and the leadership `Epoch` (primed on
  Restore, so fencing survives a cold restart from snapshot + empty tail).
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
  the size boundary; snapshot container round-trip (`WriteSnapshot`/`ReadSnapshot`
  CRC + sections). The full `load snapshot@S + replay (S,N] == replay [0,N]`
  equivalence (`INV-DET-02`) is asserted at the engine level — it needs the engine
  snapshot/restore API — in `tests/property/snapshot_test.go`.
- **Negative/edge**: **torn-tail** (truncated final record) — recovery
  truncates cleanly and the rebuilt state still satisfies all A–E invariants
  (`INV-DET-03`); empty dir; multi-segment replay ordering.
- **Determinism**: two replays of the same log → byte-identical (`INV-DET-01`).
