---
date: 2026-06-14
topic: replicator-hot-standby
---

# Replicator — Hot Standby with Epoch-Fenced Failover

## Summary

Add an LMAX-style **replicator**: a second consumer of the sequencer's sequenced
command stream that streams every `Seq` to a hot standby node and publishes a
`replicatedSeq` watermark which gates client acks in synchronous mode — exactly
mirroring the existing `AsyncJournaller`/`durableSeq` pattern. A standby is
promoted to primary by **manual, epoch-fenced** action; the fencing token makes
split-brain impossible without writing any consensus code. The change ships with
its full correctness harness: a replication differential test, new `INV-REP-*`
invariants asserted after every command, a fuzz/`rapid` extension that injects
replication and failure operations, and a chaos suite (default
`OB_JOURNAL_MODE=async`) covering four failure modes.

## Problem Frame

The engine is a mature single-node deterministic state machine: WAL + snapshot +
replay give durability against process crashes, and the `AsyncJournaller` already
proves off-thread, watermark-gated durability works at speed. But every durable
copy lives on one machine. A catastrophic loss of the primary's host or disk
loses the book and the ledger outright — there is no second node that can take
over. `ARCHITECTURE.md` sketches a future Raft replicator but marks it
out-of-scope for v1, and the codebase has zero networking or multi-node code.

This brainstorm deliberately moves the v1 line drawn in `CLAUDE.md` and
`internal/wal/CLAUDE.md` ("Clustering/failover and backup/DR are out of scope for
v1"): it adds high-availability hot-standby failover, while keeping the scope
narrow enough to avoid taking on a consensus implementation.

## Key Decisions

- **Hot-standby failover, not a passive backup file.** The replicator's job is
  continuous availability: a live secondary consumes the same command stream and
  can become primary. This is the LMAX replicator role, not an off-box WAL copy.

- **Manual promotion + epoch fencing, no built-in election.** Promotion is
  triggered by an operator or external script. Every applied command carries a
  monotonic **epoch** (term); a node that presents a stale epoch is rejected.
  Epoch fencing is the part that prevents split-brain and is the shared core of
  *any* future failover model — so building it now is strictly on the path to an
  external arbiter or built-in Raft later, with no rework of the data plane.
  Built-in consensus is the highest-risk path to the ledger and is excluded.

- **Replication mode is tunable (sync / async / off), mirroring
  `OB_JOURNAL_MODE`.** In sync mode the sequencer gates client acks behind
  `replicatedSeq`; in async mode the standby may lag within a bounded, documented
  window. Operators choose safety vs. latency. Every invariant must hold in every
  mode.

- **Standby-down defaults to stall (consistency-first).** When the standby is
  unreachable in sync mode, the primary stops releasing acks rather than confirm
  an order the standby has not seen. Degrading to solo (keep trading with reduced
  durability) is an explicit, operator-armed action — never a silent timeout.

- **The degrade-to-solo transition is a sequenced, journaled command.** It gets a
  `Seq` and is written to the WAL like any other command, so the durability
  guarantee change is part of the deterministic history: replay reproduces it
  exactly, and "no ack past `replicatedSeq` unless a degrade command precedes it"
  becomes a checkable invariant.

- **Safe-ack contract under the production config is a `min` of two watermarks.**
  With async journaling (the 1M-TPS path) and replication both active, an order is
  confirmed only once it is both locally fsync'd and replicated:
  `confirmed ⊆ min(durableSeq, replicatedSeq)`.

## Architecture (conceptual)

The replicator is a new consumer of the sequenced stream, structurally a sibling
of the existing journaller. The sequencer fans each command to both; an ack is
released only when both watermarks have passed it (sync mode).

```mermaid
flowchart TB
  EXT[External commands] --> SEQ[Sequencer<br/>assigns Seq + epoch]
  SEQ -->|fan-out| JNL[AsyncJournaller<br/>publishes durableSeq]
  SEQ -->|fan-out| REP[Replicator<br/>publishes replicatedSeq]
  REP -->|stream Seq+epoch| STBY[Standby node<br/>applies, stays hot]
  JNL --> ACK{ack gate<br/>min(durableSeq, replicatedSeq)}
  REP --> ACK
  ACK --> CLIENT[Client confirmation]
```

## Requirements

### Replication data plane

- R1. The sequencer streams every sequenced command — external commands *and*
  internal stop activations, anything that receives a `Seq` — to the replicator,
  in `Seq` order, with no gaps and no reordering.
- R2. The standby applies the replicated stream through the same journaled-apply
  path as recovery, so its state is byte-identical to the primary's at any given
  `Seq`.
- R3. The replicator publishes a monotonic `replicatedSeq` watermark (the highest
  `Seq` the standby has durably acknowledged), readable by the ack gate.
- R4. The replicated record stream reuses the existing WAL record framing
  (`Seq, TsNanos, Type, Flags, Payload`, CRC32) so a corrupt record is detected
  and rejected on receipt.
- R5. Replication mode is selected by config (`sync` / `async` / `off`), read once
  at startup, in the same style as `OB_JOURNAL_MODE`.

### Ack safety

- R6. In sync mode, no client ack is released for a command at `Seq = S` until
  `replicatedSeq ≥ S` **and** `durableSeq ≥ S` — i.e. `confirmed ⊆
  min(durableSeq, replicatedSeq)`.
- R7. In async mode, acks proceed without waiting on `replicatedSeq`; the standby's
  lag must stay within a bounded window that the system documents and exposes.

### Epoch fencing and promotion

- R8. Every applied command carries an epoch (term). A promoted standby increments
  the epoch before applying any command as primary.
- R9. A node rejects any command stamped with an epoch lower than its current
  epoch, on both the live path and the replay path. A revived old primary
  presenting a stale epoch can never mutate state.
- R10. Promotion is an explicit operation (operator/script triggered). After
  promotion the new primary resumes the sequence at its last applied `Seq` with the
  incremented epoch, and its state passes `CheckAllInvariants`.

### Standby-down behavior

- R11. In sync mode, when the standby is unreachable, the primary defaults to
  **stall**: it stops releasing acks past the last replicated `Seq` and never
  confirms an unreplicated command.
- R12. Degrade-to-solo is an explicit, operator-armed command that gets a `Seq`,
  is journaled, and replays deterministically. After it, the ack gate no longer
  requires `replicatedSeq` to advance.

### Backpressure and catch-up

- R13. When the standby (or its transport) cannot keep up, the replicator applies
  backpressure rather than dropping records — records are never silently lost,
  matching the existing `AsyncJournaller` spin-on-full behavior.
- R14. A standby that restarts or falls behind re-syncs from snapshot + WAL tail
  and converges to fingerprint-equality with the primary, with no gaps.

## Acceptance Examples

- AE1. Safe-ack under async journaling.
  - **Covers R6.** Mode: replication `sync`, `OB_JOURNAL_MODE=async`.
  - **Given** a batch of orders is sequenced but the standby has only acked up to
    `Seq = 100` and the journaller has fsync'd up to `Seq = 120`.
  - **Then** no client ack is released beyond `Seq = 100`
    (`min(120, 100) = 100`).

- AE2. Zombie primary is fenced.
  - **Covers R8, R9.** The standby is promoted, incrementing epoch from `e` to
    `e+1`.
  - **When** the old primary revives and submits a command stamped epoch `e`.
  - **Then** the new primary rejects it; no state mutation; the ledger never
    diverges.

- AE3. Stall on standby loss.
  - **Covers R11.** Mode: replication `sync`. The standby becomes unreachable
    mid-stream at `Seq = 200`.
  - **Then** the primary releases no acks past `Seq = 200`, and continues to
    journal locally, until either the standby returns or a degrade-to-solo command
    is issued.

- AE4. Degrade-to-solo replays deterministically.
  - **Covers R12.** A degrade-to-solo command is issued at `Seq = 205`.
  - **Then** on full WAL replay, the same command appears at `Seq = 205`, and acks
    after it no longer require `replicatedSeq` — the recovered state is
    byte-identical to the live run.

## Test Strategy

The change touches matching-adjacent, balance, and durability paths, so it falls
squarely under the mandatory three-layer harness. New tests extend the existing
machinery (`tests/property` `CheckAllInvariants`, `tests/refmodel` oracle, the
`rapid` state machine, `RunDifferentialAsync`) rather than standing up a parallel
harness.

### New invariants (`INV-REP-*`)

Asserted after every command, in the same place `CheckAllInvariants` runs today.

- INV-REP-01. **Standby equivalence.** At any acknowledged `replicatedSeq = S`,
  the standby's `StateFingerprint()` equals the primary's fingerprint at `Seq = S`.
- INV-REP-02. **Prefix ordering.** The standby's applied command stream is a
  gap-free, in-order prefix of the primary's journaled stream.
- INV-REP-03. **Ack safety.** No confirmed command exceeds
  `min(durableSeq, replicatedSeq)`, unless a degrade-to-solo command precedes it in
  the log.
- INV-REP-04. **Single primary / fencing.** No two nodes ever apply a command at
  the same `Seq` under different epochs; stale-epoch commands never mutate state.
- INV-REP-05. **Promotion correctness.** A promoted standby's post-promotion state
  passes all existing `INV-*` checks and continues to match the reference model.
- INV-REP-06. **Catch-up convergence.** A restarted/lagging standby converges to
  fingerprint-equality with no gaps.

### Differential

Add a `RunDifferentialReplicated` alongside `RunDifferentialAsync`: drive primary,
standby, and the reference model through the same stream; after every command
assert (a) primary matches the oracle, (b) standby fingerprint equals the primary
at its acked watermark, and (c) all `INV-*` and `INV-REP-*` hold. A promotion
mid-stream must leave the promoted node still matching the oracle for the
remainder of the stream.

### Property / fuzz

Extend the `rapid` state machine and native `go test -fuzz` corpus with
replication and failure operations as first-class generated steps: advance/stall
the standby ack, drop the standby, restart and catch up, partition, promote, and
revive a stale-epoch old primary. Shrinking must reproduce any
fingerprint-divergence or fencing violation. Every fixed bug adds a permanent
regression seed under `testdata/fuzz/`.

### Chaos suite

Failure-injection tests, **default `OB_JOURNAL_MODE=async`** to mirror the
production 1M-TPS config, each asserting `confirmed ⊆ min(durableSeq,
replicatedSeq)` survives and the post-failure state passes `CheckAllInvariants`:

| Scenario | Inject | Assert |
|---|---|---|
| Primary crash mid-batch | Kill primary with un-fsync'd and un-replicated tails | No confirmed order lost; standby promotes cleanly; invariants hold |
| Zombie primary / fencing | Promote standby (epoch++), revive old primary | Stale-epoch commands rejected; no split-brain; ledger never diverges |
| Standby crash + catch-up | Kill and restart standby | Converges to fingerprint-equality; no gaps; no reordering |
| Partition / slow standby | Partition or throttle standby | Sync-mode stall (default); bounded async lag; clean backpressure; clean resume on heal |

## Scope Boundaries

- Automatic leader election / consensus (Raft or equivalent) — deferred. Epoch
  fencing is built now as the shared core that makes election addable later
  without touching the data plane.
- More than one standby (N>1) — this iteration is a single hot standby (1+1). The
  data-plane fan-out should not preclude N later.
- Read-serving standbys (offloading market-data reads) — out. The standby is a
  passive applier; it serves no client reads.
- External-arbiter and built-in election promotion mechanics — out; promotion is
  manual/script-triggered this iteration.

## Dependencies / Assumptions

- The transport between primary and standby is assumed to deliver bytes; ordering
  and integrity are re-established on receipt via FIFO + CRC32, not assumed from
  the transport. Transport choice is a planning decision.
- The SPSC ring is strictly single-consumer today, so fanning the sequenced stream
  to both the journaller and the replicator needs a second ring or a
  multi-consumer barrier. This is the one genuinely new mechanism (vs. mirroring
  `AsyncJournaller`) and its design is deferred to planning.
- Carrying the epoch on every command likely touches the WAL record/codec (e.g.
  via `Flags` or a new field). The exact encoding is a planning decision; the
  requirement is only that the epoch is journaled and fenced on replay.

## Outstanding Questions

### Resolve before planning

- None blocking. The product shape, failover model, sync semantics, and test scope
  are pinned.

### Deferred to planning

- Multi-consumer fan-out mechanism: second SPSC ring vs. shared multi-consumer
  barrier.
- Epoch encoding in the WAL record (reuse `Flags` vs. new field) and snapshot
  format impact.
- Transport for the replicated stream and how promotion is signaled to the engine.
- The exact bounded-lag metric and how it is exposed for async mode (R7).

## Sources

- `internal/sequencer/journaller.go` — the `Journaller` seam the replicator
  mirrors.
- `internal/sequencer/async_journaller.go` — `durableSeq`/`lastSubmitted`
  watermark + spin-on-full backpressure; the structural template for the
  replicator.
- `internal/wal/record.go` — record framing (`Seq, TsNanos, Type, Flags, Payload`,
  CRC32) the replicated stream reuses.
- `internal/market/recover.go` — snapshot + tail replay; the standby's apply path
  and catch-up basis.
- `internal/spsc/ring.go` — single-consumer ring; the fan-out constraint.
- `tests/property/differential.go` — `RunDifferential` / `RunDifferentialAsync`,
  the template for `RunDifferentialReplicated`.
- `tests/property/invariants.go` — `CheckAllInvariants`, where `INV-REP-*` attach.
- `ARCHITECTURE.md` — the `Raft replicator (out-of-scope v1)` sketch this iteration
  partially realizes (data plane + fencing, not election).
