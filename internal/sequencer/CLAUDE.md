# internal/sequencer

The engine's **single ordering authority**. One goroutine assigns the global,
contiguous `Seq` and timestamp, journals each command to the WAL, and routes it
onward; it also drains market fills and applies settlement in deterministic
`(aggressorSeq, matchIndex)` order.

## Files

- `sequencer.go` — `Sequencer`, the `Journal` and `Router` interfaces,
  `ClockFunc`. Fans in producer command rings + a priority re-inject ring (stop
  activations), assigns order, journals (via a `Journaller`), routes. `SetSeq`
  primes the counter to a snapshot watermark during restore (quiesced only) so
  post-restore commands continue contiguously. Owns the **durable-ack barrier**
  flush-trigger policy (`DurableSeq`, `DrainJournal`, `Fatal`) — see below.
- `journaller.go` — the `Journaller` seam (Append / Flush / Drain / DurableSeq /
  Fatal / Close) and `SyncJournaller`, the default that journals inline on the
  sequencer goroutine (the historical behavior; used by every test and replay).
- `async_journaller.go` — `AsyncJournaller`: moves WAL Append + fsync onto a
  dedicated consumer goroutine (the LMAX "Journaller") so the matcher never
  blocks on durability. An SPSC journal ring carries records (FIFO → the on-disk
  byte stream is identical to inline journaling), an atomic publishes
  `durableSeq`, and `Append` backpressures (spins, never drops) when the ring is
  full. An Append/Sync error latches a fatal the sequencer observes via
  `Fatal()`. Selected by `market.Config.AsyncJournal`.
- `replicator.go` — the `Replicator` seam (Replicate / Flush / Drain /
  ReplicatedSeq / Fatal / Close), the structural mirror of `Journaller` for the
  hot-standby stream; `NopReplicator`, the default for replication `off`
  (`ReplicatedSeq` is `+inf`, so the ack gate stays `durableSeq`-only); and the
  `StandbyLink` transport seam (Send / AckedSeq / Fetch / Fatal / Close). The
  sequencer stamps a leadership-term `Epoch` on every command (bumped on
  promotion), calls `Replicate` after the durable `Append` in `sequenceAndRoute`,
  gates output on `ReleaseSeq() = min(durableSeq, replicatedSeq)`, and halts on a
  replicator fatal.
- `async_replicator.go` — `AsyncReplicator`: streams to the standby off the
  sequencer goroutine via a `StandbyLink`, publishing `replicatedSeq` from the
  link's `AckedSeq`. The one deliberate divergence from `AsyncJournaller`:
  `Replicate` is **non-blocking** — a full ring drops the command (still durable
  in the WAL) and the consumer backfills it via `StandbyLink.Fetch`, so a
  slow/dead standby stalls acks only, never journaling or matching. Selected by
  `market.Config.ReplicationMode` (`buildReplicator`).

## Durable-ack barrier

The "persist before output" rule (LMAX principle #7): no command's ack may be
exposed before its WAL bytes are durable.

- **Speculative match, gated output.** Matching/settlement run in-memory as soon
  as a command is routed; only the externally observable ack is held. The
  watermark `durableSeq` is the highest `Seq` whose bytes have been `fsync`-ed;
  `DurableSeq()` exposes it and the market layer gates `Acks()` on it.
- **Drain-driven group-commit.** A flush (`Sync` → advance `durableSeq` → release
  pending acks) fires when the input ring drains with records pending, or when an
  unsynced batch reaches the flush cap (`Config.FlushCap`, default
  `defaultFlushCap`). `flush()` captures the last-appended `Seq` **before** `Sync`
  so the watermark never over-claims. Every sequenced record — reinject (stop
  activation) path included — increments the unsynced count, so a stop
  activation's ack is never stranded above `durableSeq`. The command payload is
  encoded into a reusable per-sequencer buffer (`EncodeCommandInto`), so
  journaling allocates nothing on the hot path (gated by `TestStepZeroAlloc`).
- **Output-side only.** `durableSeq`, the unsynced counter, and flush timing are
  never journaled and never affect `Seq`, timestamps, or fill order — replay is
  byte-identical regardless of flush cadence. `FlushCap` governs durable
  throughput vs durable-ack latency: bigger batches amortize the WAL `write`+`fsync`
  over more commands. The inline (`SyncJournaller`) single-thread durable ceiling
  is the no-op ceiling minus the sequencer's own I/O time (~820k cmd/s durable on
  the dev machine); the **`AsyncJournaller`** lifts journaling off the matcher
  goroutine and clears the **1M durable** target (~1.38M cmd/s measured). With
  async, `DrainJournal` is the barrier that quiesce points (snapshot, drain-then-
  read) use to wait for the consumer to catch up before reading durable state.
- **Fail-stop.** A non-nil `Append`/`Sync` error latches a terminal `fatal`:
  `Step` becomes a no-op, `Run` exits, no pending ack is released, and the host
  (`cmd/engine`) / snapshotter surface it via `Fatal()`. The WAL is the source of
  truth; nothing advances once journaling is broken.

## Determinism model (the whole point)

- **Every** command that receives a `Seq` is journaled — external commands and
  stop activations alike — so the WAL is a complete, contiguous log and replay
  is a straight re-application (no regeneration).
- Stop re-triggering is **suppressed during replay** (the activations are
  already in the log).
- Time enters only via the injected `ClockFunc`, captured **once per command**.
  Never read the wall clock elsewhere.
- The re-inject ring has priority over external producers; external producers
  are drained round-robin. Don't introduce ordering that depends on `map`
  iteration or goroutine scheduling.

## Testing (positive / negative / edge + invariant)

`sequencer_test.go` exists. Cover:
- **Positive**: contiguous `Seq` assignment; commands journaled then routed in
  order; fills settled in `(aggressorSeq, matchIndex)` order (`INV-DET-04`).
- **Edge**: re-inject priority vs external producers; full/empty rings;
  interleaving of many producers (determinism `INV-MAT-08`).
- **Negative**: journal append failure latches `fatal`, releases no ack, and
  halts further work.
- **Barrier**: flush on cap and on ring-drain advance `durableSeq`; reinject
  records are flushed (not stranded); no-op journal still advances the watermark;
  no `fsync` on an idle engine; the journaled byte stream is invariant to flush
  cadence (`TestWalBytesInvariantToFlushCadence`).
- **Determinism/recovery**: same command stream → byte-identical state
  (`INV-DET-01`); replay suppresses stop re-trigger; subset-replay equivalence
  (`INV-MET-04`).
