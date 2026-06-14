---
date: 2026-06-14
topic: lmax-hotpath-1m-tps
---

# LMAX-Fast Serial Hot Path (1M TPS Durable) — Requirements

## Summary

Optimize the serial hot path to sustain **1M commands/sec on the durable
(real-WAL) serial path** while keeping the single-writer balance authority,
following the LMAX lesson that throughput comes from a leaner single writer — not
from sharding it. Staged behind a hard 1M gate: zero-alloc hand-rolled codec
first, then edge journalling only if needed, then stop and escalate to a separate
balance-parallelization brainstorm if still short.

---

## Problem Frame

The serial engine tops out at ~940k cmd/s and the full parallel engine is worse
(~307k, dispatch-bound), so 1M is currently unreachable. LMAX hit ~6M on a
*single* business thread by keeping it pure business logic — no I/O, no
allocation, lock-free — and pushing journalling to a parallel edge consumer. Our
single-writer balance authority is already LMAX-aligned; the gap is that our one
thread does work LMAX kept off it:

- **Reflective encode on every command.** `internal/types/codec.go`'s
  `EncodeCommand` uses `binary.Write` (reflection) plus a fresh `bytes.Buffer`,
  and `internal/sequencer/sequencer.go` calls it *unconditionally* in
  `sequenceAndRoute` to build the WAL payload — even when the journal is a no-op.
  The record framing (`internal/wal/record.go`) allocates again. So the ~940k
  ceiling already pays a reflective encode + two allocations per command.
- **Journalling on the business thread.** On the durable path the sequencer also
  does the per-command `Write` syscall inline; LMAX's journaller was a separate
  consumer so the business thread never touched it.

This brainstorm closes that gap the LMAX way — faster single writer, journalling
at the edge — and does not touch the single-writer balance authority.

---

## Key Decisions

- **Codec-first, edge journalling contingent.** Phase 1 is the zero-alloc
  hand-rolled codec (small, contained); phase 2 (edge journalling) fires only if
  phase 1's measured durable-serial number is under 1M. De-risks the just-shipped
  durable-ack barrier — only touch the watermark path if the number demands it.

- **Byte-identical codec, no format bump.** The hand-rolled encoder must reproduce
  the current reflective encoder's bytes exactly, so existing WALs replay with no
  migration. The WAL byte layout is a durability contract (`internal/wal`); a
  version bump is the fallback only if byte-identical proves impossible.

- **Hard 1M gate with a fixed escalation ladder.** 1M durable-serial is a pass
  requirement, not a target: (1) zero-alloc codec → measure; (2) if <1M, edge
  journalling → measure; (3) if still <1M, stop and open a separate
  balance-authority-parallelization brainstorm. Each phase is gated on a real
  measurement.

- **Single-writer balance authority is untouched.** Its parallelization is the
  explicit escalation exit, a separate future brainstorm — never in scope here.

- **Edge journalling = LMAX input-disruptor pattern.** When it fires, encode +
  `Write` + fsync move off the sequencer onto a single parallel consumer that
  reads commands in `Seq` order and advances `durableSeq`. The durable-ack
  barrier's ack gate stays on `durableSeq`, so the persist-before-output
  guarantee is unchanged — only *where* the journalling work runs changes.

---

## Requirements

**Zero-alloc codec (Phase 1)**

- R1. Replace the reflective command/record encode on the hot path with a
  hand-rolled little-endian encoder that allocates zero bytes per command.
- R2. The new encoder is **byte-identical** to the current `EncodeCommand` output,
  so existing WAL segments replay unchanged (no format-version bump).
- R3. A bench gate asserts the codec/sequencer encode path is zero-alloc, in the
  style of the existing `spsc`/`matching`/`balance` zero-alloc gates.

**Durable measurement harness**

- R4. The `throughput` tool can drive the **durable path** — a real WAL writer
  (temp dir, group-commit) wired in — not only the no-op journal it uses today,
  so the 1M gate is measured against real durability.
- R5. Measurement is reported for `throughput -topology serial` with the real WAL
  on the reference machine (this dev machine, 8-core arm64), before and after each
  phase.

**Edge journalling (Phase 2, contingent on R-gate)**

- R6. If the post-codec durable-serial number is under 1M, move encode + write +
  fsync off the sequencer thread to a single parallel journaller consumer that
  preserves `Seq` order and advances `durableSeq`.
- R7. Edge journalling preserves the durable-ack barrier's guarantees: no ack is
  released above `durableSeq`; replay is byte-identical regardless of where
  journalling runs; fail-stop still halts on a journaller write/sync error.

**Gate & escalation**

- R8. 1M durable-serial is a hard pass gate. If neither phase reaches it, stop and
  record the ceiling plus a pointer to a new balance-parallelization brainstorm —
  do not modify the balance authority here.

**End-of-work guards** (standing execution instructions)

- R9. Run the full suite at the end — `make lint test race property` and a short
  fuzz slice — to confirm no regression before declaring done; fix any failure.
- R10. Update the relevant `CLAUDE.md` files (at least `internal/types`,
  `internal/sequencer`, and — if Phase 2 fires — `internal/wal`/`internal/market`)
  to document the new codec and any journalling-topology change.

---

## Acceptance Examples

- AE1. **Covers R2.** **Given** a WAL segment written by the current reflective
  codec, **when** replayed after the hand-rolled codec lands, **then** the rebuilt
  state is byte-identical — old logs are not broken.
- AE2. **Covers R1, R3.** **Given** the zero-alloc bench over the encode path,
  **then** it reports 0 allocs/op and the CI gate fails on any regression.
- AE3. **Covers R4, R5.** **Given** `throughput -topology serial` with a real WAL,
  **then** it reports a durable sustained cmd/s figure (not the no-op-journal
  number).
- AE4. **Covers R7.** **Given** edge journalling active, **when** the same stream
  runs under two flush cadences, **then** the journaled bytes and final state are
  identical; and a journaller write error fail-stops the engine with no ack
  released above `durableSeq`.
- AE5. **Covers R8.** **Given** both phases land and durable-serial is still under
  1M, **then** the work stops with the measured ceiling documented and the balance
  authority unchanged.

---

## Success Criteria

- Durable-serial throughput **≥ 1,000,000 cmd/s** on the reference machine via
  `throughput -topology serial` with a real WAL (the hard gate).
- Encode path is **zero-alloc** under the bench gate (R3).
- Existing WALs replay byte-identically (R2); the full property/differential/
  determinism/recovery suite stays green (R9).
- The balance authority is unchanged; if 1M is unmet, the ceiling and the
  escalation pointer are documented (R8).
- Relevant `CLAUDE.md` files updated (R10).

---

## Scope Boundaries

### Deferred for later

- Parallelizing the balance authority / control path (sharded or optimistic
  balance, per-asset locking). This is the escalation *exit ramp* if both phases
  miss 1M — a separate brainstorm, never modified here.

### Outside this work's identity

- Parallel-topology tuning. The target is the *serial* durable ceiling; the
  parallel engine is dispatch-bound and a different problem.
- New persistence formats or compression. Byte-identical codec only.

---

## Dependencies / Assumptions

- **Phase 2 depends on the durable-ack barrier** (`durableSeq`, group-commit
  flush, fail-stop) which is in PR #5 (`feat/durable-ack-barrier`), **not yet
  merged to `main`**. Phase 1 (codec) is independent — `internal/types/codec.go`
  exists on `main` today. Edge journalling must build on the merged barrier.
- Assumes the byte-identical codec is achievable; the current layout is a fixed
  little-endian struct dump, so a manual encoder matching it is expected to be
  straightforward. If not, a versioned format is the fallback (Outstanding).
- The 1M figure is hardware-specific; "pass" is defined on the reference dev
  machine (8-core arm64). On other hardware the same code is re-measured, not
  assumed.
- Assumes group-commit amortizes fsync enough that per-command durability cost is
  dominated by encode + the `Write` syscall — the two things these phases remove
  from the business thread.

---

## Outstanding Questions

**Deferred to planning**

- Whether the hand-rolled encoder can be byte-identical for every field/order
  type, or whether one field forces a versioned format (resolve by reading the
  exact `Command` layout during planning).
- Whether Phase 1 alone reaches 1M (decides if Phase 2 runs at all) — answered by
  measurement, not design.

---

## Sources / Research

- LMAX reference: `docs/designs/lmax-reference.md` §3 (single-threaded BLP), §4
  (journaller as a parallel input-disruptor consumer), §10 (single-writer
  principle), §11 (Go notes).
- Hot-path encode: `internal/types/codec.go` (`EncodeCommand` reflective
  `binary.Write` + buffer), `internal/sequencer/sequencer.go` (`sequenceAndRoute`
  calls it unconditionally), `internal/wal/record.go` (`encodeRecord` alloc).
- Durability contract: `internal/wal/CLAUDE.md` (codec byte layout = recovery
  contract; version on change).
- Durable-ack barrier (Phase 2 dependency): PR #5 `feat/durable-ack-barrier`
  (`durableSeq`, drain-driven group-commit, fail-stop).
- Measurement tool: `cmd/throughput` (offloaded generation, serial/parallel),
  currently wired to the no-op journal via `cmd/internal/harness` `DefaultConfig`.
- Baselines this session: serial ceiling ~940k (no-op journal), full parallel
  ~307k.
