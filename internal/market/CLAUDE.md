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
  `Engine.Acks()` is **gated on the durable watermark**: it returns only acks at
  or below `DurableSeq()` (the durable-ack barrier — acks above it are
  speculative). After `Drain` the watermark equals `Seq`, so every ack releases
  and drain-then-read callers are unaffected. `Fatal()`/`DurableSeq()` expose the
  sequencer's barrier state; `ParallelEngine.Acks()` gates identically via the
  shared `releasedAcks` helper and shared sequencer. `Config.AsyncJournal` (via
  `buildJournaller`, shared by both topologies) moves WAL fsync onto a dedicated
  journaller goroutine — the path to 1M durable TPS; `Engine`/`ParallelEngine`
  `Close` stop that goroutine before the host closes the WAL, and `Drain`/
  `SyncJournal` barrier on it (`Sequencer.DrainJournal`) so snapshots never
  persist state the WAL cannot back. The async path is proven behavior-transparent
  by sync-vs-async state/ack equivalence and a byte-identical-WAL determinism test.
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
  (WAL never GC'd), and `LatestSnapshot` for recovery. The drain before publish
  flushes the WAL, so a published snapshot's `Seq` is always `<= durableSeq`; a
  fail-stop during that drain aborts publication (checks `Fatal()` after `Drain`)
  rather than persisting state the WAL cannot back.
- `recover.go` — `Recover`: load latest snapshot + replay WAL tail, falling back
  to full replay from `Seq` 0 (logged) on a missing/corrupt/incompatible
  snapshot; primes the sequencer to the final journaled `Seq` for live resume.
  Also primes the leadership `Epoch` and fences the replay (`ErrStaleEpoch`): a
  command whose term steps below the highest seen is a spliced zombie record and
  halts recovery.

## Replication (hot standby)

`Engine.Acks()` / `ParallelEngine.Acks()` gate on
`Sequencer.ReleaseSeq() = min(durableSeq, replicatedSeq)`, so in sync replication
mode a command is confirmed only once it is both locally durable and replicated;
with replication `off` the replicated watermark is `+inf` and the gate collapses
to `durableSeq` (behavior-neutral). `Config.ReplicationMode`/`ReplicationRing`
select the replicator. The live `AsyncReplicator`, the in-process `StandbyLink`,
the standby apply/catch-up path, manual epoch-fenced promotion, and degrade-to-
solo are tracked in
`docs/plans/2026-06-14-007-feat-replicator-hot-standby-plan.md` (U4–U10).

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
