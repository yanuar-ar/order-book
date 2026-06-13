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
  continue contiguously.

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
- **Negative**: journal append failure handling.
- **Determinism/recovery**: same command stream → byte-identical state
  (`INV-DET-01`); replay suppresses stop re-trigger; subset-replay equivalence
  (`INV-MET-04`).
