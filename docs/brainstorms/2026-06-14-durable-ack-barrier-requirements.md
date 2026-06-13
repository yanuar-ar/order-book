---
date: 2026-06-14
topic: durable-ack-barrier
---

# Durable-Ack Barrier — Requirements

## Summary

Add a durable-ack barrier so the engine never exposes an ack for a command that
is not yet on disk. Matching stays speculative and in-memory at microsecond
latency; acks are withheld until a drain-driven group-commit `fsync` advances a
`durableSeq` watermark, and any WAL write or sync failure fail-stops the engine.

---

## Problem Frame

The sequencer currently journals fire-and-forget and acks immediately
(`internal/sequencer/sequencer.go:160-171`):

```go
_ = s.journal.Append(...)   // error ignored; bytes only in page cache
s.router.OnCommand(*c)      // matches, settles, and acks right away
```

Two failure modes live in those two lines. First, the `Append` error is
discarded (`_ =`): if the write fails — disk full, segment roll error — the
engine still matches and acks a command that was never journaled, and replay
loses it. Second, even on a successful `Append` the bytes sit in the OS page
cache; `wal.Sync()` (the `fsync`) has not run, so a crash in that window leaves a
client holding an ack for an order the restarted engine never saw.

Both violate the engine's own determinism contract (core principle #2: every
command that gets a `Seq` is journaled, so replay is a straight re-application)
and LMAX principle #7 ("persist before release output"). For a money-handling
system this is the one mapped LMAX gap that is a correctness hole rather than a
performance optimization — the others (output disruptor, configurable wait
strategy, parallel sharding, multicast ring) are refinements the reference
itself defers and single-node-at-100k-TPS does not yet need.

---

## Key Decisions

- **Speculative match, gate output.** Matching and settlement run in-memory
  immediately; only the externally observable ack is held until the command is
  durable. Match latency stays in microseconds and `fsync` overlaps matching.
  In-memory state may run ahead of disk, which is safe: a crash discards it and
  replay rebuilds state from the durable log alone. Chosen over persist-then-match
  (fsync the batch before routing to matching), which is simpler but adds the
  batch-window latency to the critical path before matching can start.

- **Fail-stop on any WAL failure.** An `Append` or `Sync` error stops the engine:
  no further matching, no release of any pending ack, surface a fatal error for
  operator intervention. A broken log cannot safely ack anything, and stopping
  preserves the gap-free `Seq` contiguity that replay depends on (the recovery
  rule is "gap → HALT, don't guess"). Chosen over reject-and-continue, which
  would have to unwind an already-assigned `Seq` to avoid a contiguity hole.

- **Output-side watermark, never journaled.** The `durableSeq` watermark, the
  flush trigger, and the pending-ack gate live entirely on the output side. They
  never affect `Seq` assignment, captured timestamps, or fill ordering, so two
  engines with different batch windows replay byte-identically. The flush policy
  is a runtime knob, not a determinism input.

- **Drain-driven flush with a count cap.** Flush (`Sync` + advance `durableSeq` +
  release pending acks) fires when the input ring drains empty, or when a count
  of unsynced records reaches a cap `N`, whichever comes first. This reproduces
  the LMAX batching effect: light load empties the ring each iteration and
  flushes immediately (low latency); heavy load lets batches grow to `N` and
  amortizes `fsync`. No timer or clock read is needed. `N` is a tuning parameter
  fixed by benchmarking, not hardcoded in this brainstorm.

- **Reserve `ClientReqID` now, defer dedup logic.** Add a `ClientReqID` field to
  `Command` and the WAL record codec now — a cheap change today and a painful
  WAL-format migration later. The dedup-set enforcement that makes recovery
  exactly-once is deferred until a real gateway with retrying clients exists.

---

## Requirements

**Durability barrier**

- R1. The engine must not expose an ack whose `Seq` is greater than `durableSeq`.
  `durableSeq` is the highest `Seq` whose WAL bytes have been `fsync`-ed.
- R2. Matching and settlement may proceed in-memory before the command is
  durable (speculative), but their results must not become externally observable
  through any ack until R1 is satisfied.
- R3. Acks already carry `Seq` (`internal/types/types.go:153`) and are appended
  in `Seq` order; the release gate is expressed against that ordering — a
  consumer drains acks only up to `durableSeq`.

**Flush policy**

- R4. A flush performs `wal.Sync()`, advances `durableSeq` to the last appended
  `Seq`, and releases all pending acks at or below the new `durableSeq`, in that
  order.
- R5. A flush fires when the external input ring drains empty with unsynced
  records pending, or when the unsynced-record count reaches cap `N`, whichever
  comes first.
- R6. The flush trigger, `durableSeq`, and the pending-ack gate must never be
  journaled and must never influence `Seq` assignment, captured timestamps, or
  fill ordering.

**Failure handling**

- R7. A non-nil error from `journal.Append` or `wal.Sync` fail-stops the engine:
  matching halts, no pending ack is released, and a fatal error is surfaced. The
  swallowed `_ =` on the append path is removed.
- R8. Fail-stop must not leave released acks beyond the last durable `Seq` — on
  stop, pending (undurable) acks are discarded, not flushed.

**Recovery and snapshots**

- R9. On restart, recovery proceeds from the durable log only; the undurable tail
  is handled by the existing torn-tail truncation. Speculative in-memory state
  from before the crash is not relied upon.
- R10. A snapshot must flush the WAL first so that `snapshotSeq ≤ durableSeq`; a
  snapshot must never persist speculative state the WAL cannot back.

**Idempotency groundwork**

- R11. Add a `ClientReqID` field to `Command` and the WAL record codec, versioning
  the WAL format per the `internal/wal` durability-contract rule. The field is
  reserved and persisted; no dedup logic reads it yet.

---

## Acceptance Examples

- AE1. **Covers R1, R2, R4.** **Given** a command sequenced and matched
  in-memory but not yet flushed, **when** a consumer drains acks, **then** the
  command's ack is not returned. **When** a flush completes, **then** the ack
  becomes available.

- AE2. **Covers R7, R8, R9.** **Given** `journal.Append` returns an error,
  **when** the sequencer processes the command, **then** the engine fail-stops,
  releases no pending ack, and on restart replay rebuilds state with neither the
  failed command nor any ack for it.

- AE3. **Covers R1, R9.** **Given** a command that reached durable WAL but whose
  ack was never released before a crash, **when** the engine restarts, **then**
  replay re-applies the command (it is durable) and the client never received an
  ack — the documented double-apply precondition (see Scope Boundaries).

- AE4. **Covers R5.** **Given** a steady high-load stream, **when** unsynced
  records reach `N`, **then** a flush fires without waiting for the ring to
  empty. **Given** an idle engine with one pending record, **when** the ring
  drains, **then** a flush fires immediately.

- AE5. **Covers R6.** **Given** the same command stream replayed under two
  different `N` values, **then** the final state (all books + ledger) is
  byte-identical.

- AE6. **Covers R10.** **Given** unflushed speculative state at `Seq = S+k`,
  **when** a snapshot is requested, **then** the WAL is flushed first and the
  snapshot records `Seq ≤ durableSeq`.

---

## Scope Boundaries

**Deferred for later**

- Dedup-set enforcement (`ClientReqID`-keyed, per-account rolling window). The
  field is reserved in R11; the logic ships before any retrying client. Until it
  lands, crash-recovery plus a retrying client can double-apply a command (AE3).
  v1 has no network gateway and no retrying client, so the gap is latent — but it
  is a hard precondition for any real gateway.
- Output disruptor / async publisher. v1 has no publishing I/O edge to offload;
  the watermark gate over the existing ack slice is sufficient.
- Configurable wait strategy (busy-spin / yield / sleep / block). A latency/CPU
  tuning knob, not a correctness concern.
- Parallel sharding activation (`internal/market/parallel.go`) and a multicast /
  full-Disruptor ring. Throughput refinements the reference defers; serial
  single-node is expected to clear the 100k-TPS target.

**Outside v1**

- Replication / Raft, failover, backup / DR (already out of scope per
  `CLAUDE.md`). The barrier's "persist before output" is the single-node
  analogue of LMAX's journal+replicate sequence barrier; the replicate leg is
  not part of v1.

---

## Dependencies / Assumptions

- Assumes `wal.Sync()` (`internal/wal/wal.go:88`) is the sole durability point
  and that `os.File.Write` with `O_APPEND` lands in the page cache until `Sync`.
- Assumes the serial topology, where the sequencer goroutine drives matching and
  settlement inline (`internal/market/engine.go`). The parallel topology must
  preserve the same barrier semantics if later activated.
- Assumes acks remain `Seq`-tagged and appended in order; the release gate
  depends on that ordering (R3).
- The cap `N` is a tunable to be set by benchmarking against the p50/p99
  durable-ack latency and the zero-alloc gate; it is not fixed here.

---

## Sources / Research

- `docs/designs/lmax-reference.md` — LMAX principle #7 (persist before output),
  §11 (single-node SPSC sufficiency; sharding/multicast deferred).
- `docs/designs/spot-orderbook-engine-design.md` §6.1–6.5 (group-commit, recovery
  rules), §16.4 ("match on commit", speculative-match note), §16.6 (`ClientReqID`
  dedup design).
- `internal/sequencer/sequencer.go:160-171` — the fire-and-forget append site;
  `internal/sequencer/CLAUDE.md` already lists "journal append failure handling"
  as a required negative test.
- `internal/wal/wal.go:62-101` — `Append` (buffered) vs `Sync` (fsync); no
  durable watermark currently tracked.
- `internal/market/engine.go:42,53-147` — inline settlement and the `acks []Ack`
  collection point.
- `internal/types/types.go:153-162` — `Ack` already carries `Seq`.
