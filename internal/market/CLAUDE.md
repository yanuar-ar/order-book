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
