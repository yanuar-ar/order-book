# internal/sequencer

The engine's **single ordering authority**. One goroutine assigns the global,
contiguous `Seq` and timestamp, journals each command to the WAL, and routes it
onward; it also drains market fills and applies settlement in deterministic
`(aggressorSeq, matchIndex)` order.

## Files

- `sequencer.go` — `Sequencer`, the `Journal` and `Router` interfaces,
  `ClockFunc`. Fans in producer command rings + a priority re-inject ring (stop
  activations), assigns order, journals, routes. `SetSeq` primes the counter to a
  snapshot watermark during restore (quiesced only) so post-restore commands
  continue contiguously. Also owns the **durable-ack barrier** (`DurableSeq`,
  `Fatal`) — see below.

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
  over more commands. The inline single-thread durable ceiling is the no-op
  ceiling minus the sequencer's own I/O time (~980k cmd/s on the dev machine);
  clearing 1M needs edge journalling (fsync off the sequencer thread), deferred.
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
