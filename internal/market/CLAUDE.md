# internal/market

Engine assembly: wraps each market's book + matcher as a **shard** and wires the
sequencer, balance authority, and shards together. Provides both the serial
(v1) and parallel topologies — proven equivalent by digest-equality in tests.

## Files

- `shard.go` — `Shard`: owns one market's book + `matching.Engine`. `Submit`,
  `Cancel`, `AmendDown`, `SetSink` (stop-activation sink).
- `engine.go` — `Core` (implements `sequencer.Router`): reserves funds, routes
  funded orders to shards, settles fills inline, manages reservation lifecycle.
  Serial v1: the sequencer's routing thread matches and settles inline.
- `parallel.go` — `ParallelEngine`: offloads matching to per-worker goroutines
  (configurable market→worker assignment) while keeping sequencing and the
  balance authority single-writer. Control drives ops in strict `Seq` order,
  blocking on each worker result, so produced state is **identical** to serial.
- `snapshot.go` — `Engine.Snapshot`/`Restore` assembly over the `wal` container:
  a versioned header (format version, money-scale config, market/asset layout,
  `Seq`), then ledger / open-map / books / stops sections. Restore validates the
  header, rebuilds all state, primes the `Seq` watermark, and runs a post-rebuild
  self-check (ledger + book invariants) so a CRC-clean-but-logically-corrupt
  snapshot is rejected. `StateFingerprint` is the complete engine-vs-engine
  equality oracle (per-order reservations, open.qty, iceberg peak/hidden, stops,
  lastPrice) used by INV-DET-02. Also the open-map codec.
- `snapshotter.go` — `Snapshotter`: count- and/or time-triggered snapshots (one
  goroutine, quiesced boundary), files named by `Seq`, retention of the last K
  (WAL never GC'd), and `LatestSnapshot` for recovery.
- `recover.go` — `Recover`: load latest snapshot + replay WAL tail, falling back
  to full replay from `Seq` 0 (logged) on a missing/corrupt/incompatible
  snapshot; primes the sequencer to the final journaled `Seq` for live resume.

## Constraints

- The `shardOps` interface is the seam between `Core` and matching; serial binds
  it to a local `*Shard`, parallel to a remote worker. Either way, ops run in
  strict `Seq` order — behavior must stay identical.
- Inline settlement equals deterministic `(aggressorSeq, matchIndex)` order
  because a command's fills all carry that `Seq` as `AggressorSeq`. Don't break
  that assumption.
- Raw single-market throughput is bounded by the serial balance authority;
  parallelism wins on matching, not on shared balance.

## Testing (positive / negative / edge + invariant)

`engine_test.go`, `parallel_test.go` exist. Cover:
- **Positive**: full order lifecycle (reserve → match → settle → release) across
  markets; cancel/amend reservation release.
- **Edge/negative**: rejected orders leave book + balances unchanged;
  cross-market same-account flow (`INV-BAL-09`); reservation release on
  cancel/complete (`INV-BAL-07`, `INV-MET-01/02`).
- **Equivalence (critical)**: `ParallelEngine` and serial `Engine` produce
  byte-identical state for the same command stream (digest-equality).
