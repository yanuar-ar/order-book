---
date: 2026-06-13
topic: engine-snapshot-restore
---

# Engine Snapshot/Restore + INV-DET-02

## Summary

Add a production-grade `Engine.Snapshot()` / `Engine.Restore()` that serializes
the engine's complete resumable state at a `Seq` watermark and rebuilds it on
restart, so a restart loads the latest snapshot and replays only the WAL tail
instead of replaying the whole log from `Seq` 0. This unblocks **INV-DET-02**
(snapshot+replay ≡ full replay), which becomes the proof the mechanism is
faithful. Scope covers the full snapshot lifecycle: a cadence policy,
snapshot-file retention/GC, and a corrupt-snapshot fallback.

---

## Problem Frame

Recovery today is full WAL replay from `Seq` 0 (`internal/wal/replay.go`,
exercised by `tests/property/recovery_test.go`). That is correct but its cost
grows without bound: every restart re-applies the entire command history, so a
long-lived market pays a linearly-growing restart latency. The design always
intended `WAL + snapshot + replay` (design §6.4–6.5) precisely to bound this —
but the snapshot half was never built at the engine level.

The byte-level snapshot container exists (`internal/wal/snapshot.go`:
`WriteSnapshot` / `ReadSnapshot` over `(seq, [][]byte sections)`), but nothing
produces those sections from live state and nothing restores from them. The
consequence is recorded in the harness plan itself: INV-DET-02 (mid-stream
snapshot+replay equivalence) is **deferred, not down-scoped**, blocked on a
missing `Engine.Snapshot()/Restore()`
(`docs/plans/2026-06-13-002-feat-invariant-fuzz-harness-plan.md:67`;
`tests/CLAUDE.md:59`). So the engine carries an untested corner of its
determinism contract — exactly the kind of gap the testing guide forbids
shipping with.

---

## Key Decisions

- **Purpose is production durability, not a test prop.** The snapshot is the
  real §6.4–6.5 mechanism (serialize complete state at a watermark, restart =
  load latest snapshot + replay WAL tail). INV-DET-02 is the verification that
  the mechanism is correct, not the reason it exists. This sets the completeness
  bar: a snapshot that round-trips the digest but not a real resume is a defect.

- **Logical reconstruction, not byte-exact arena.** `Book.Dump()` is explicitly
  layout-independent — it emits resting orders bids-then-asks, ascending price,
  FIFO within a level, "regardless of physical layout"
  (`internal/orderbook/dump.go:19`). Restore therefore re-inserts resting orders
  in that canonical FIFO order and lets the arena/free-list rebuild naturally.
  No fragile slot-index or free-list serialization, and INV-DET-02's
  byte-identity (compared via the canonical state digest) still holds because
  the digest is logical.

- **The completeness contract is the load-bearing decision.** The canonical
  state digest compares **books + ledger** (`internal/orderbook/dump.go`,
  `internal/balance/dump.go`). A faithful resume needs strictly more than that.
  The snapshot MUST capture, and Restore MUST rebuild, every component below;
  the equality check used by INV-DET-02 is extended to cover the ones the digest
  omits so the test cannot pass hollow:
  - resting book orders (incl. partial-fill `Remaining` and iceberg
    `Display` / hidden remainder),
  - the matcher **stop table** (untriggered Stop / Stop-Limit orders — not in
    `Book.Dump()`),
  - `Core.open`, the reservation / open-order map (so post-restore cancel/amend
    release funds correctly),
  - the ledger: per-account `Available` / `Reserved` and fee accumulators,
  - the **`Seq` watermark** the snapshot was taken at.

- **WAL is full history, never truncated (v1).** Snapshots are a pure
  restart-speed optimization with zero correctness coupling to WAL retention.
  *Snapshot files* are still GC'd (keep last K); *WAL segments* are kept from
  `Seq` 0. WAL truncation behind a durable snapshot is deferred (see Scope
  Boundaries).

- **Corrupt/missing snapshot → full replay from `Seq` 0.** The WAL is the source
  of truth, and because it is full history that fallback is always reachable. No
  walk-back to an older snapshot in v1.

- **A snapshot is a new durability contract.** Its on-disk encoding is versioned
  and deterministic (sorted, layout-independent), taken only at a clean command
  boundary, and published only after the WAL is durable through `S` — so
  recovery never finds a snapshot ahead of the WAL tail (the invariant already
  documented on `WriteSnapshot`, `internal/wal/snapshot.go:21`).

---

## Requirements

### Snapshot capture

- R1. `Engine.Snapshot()` serializes the complete resumable state listed in the
  completeness contract (Key Decisions) at the engine's current applied `Seq`,
  producing a watermarked snapshot via the existing `wal.WriteSnapshot`
  container.
- R2. Snapshot is taken only at a command boundary — after `Drain`, with no
  partially-applied command — so the captured state is internally consistent.
- R3. A snapshot is published (made discoverable for recovery) only after the
  WAL is durably committed through the snapshot's `Seq` watermark `S`.
- R4. Snapshot encoding is deterministic: the same logical state serializes to
  byte-identical output regardless of map iteration order or arena layout, and
  carries a format version so the contract can evolve without silently
  misreading old snapshots.

### Restore and restart

- R5. `Engine.Restore()` rebuilds a fresh engine to the exact state captured by
  R1 — books (logical FIFO re-insertion), ledger balances/reserved/fees, stop
  table, `Core.open`, and the `Seq` watermark — such that the restored engine is
  state-equal to the engine at snapshot time.
- R6. After Restore, the engine resumes by replaying WAL records with
  `Seq > S` only (reusing `wal.Replay`'s `afterSeq` contract,
  `internal/wal/replay.go:26`), and the result is state-equal to a full replay
  from `Seq` 0 of the same log.
- R7. Restart wiring (`cmd/engine`) selects the latest valid snapshot, restores
  it, and replays the WAL tail; with no snapshot present it replays from `Seq`
  0 unchanged.
- R8. Restore must preserve arena / free-list integrity (INV-OB-05) and leave
  every other `INV-*` satisfied immediately after rebuild, before any tail
  replay.

### Lifecycle

- R9. A snapshot cadence policy triggers `Snapshot()` automatically — at minimum
  a configurable threshold (every N applied commands) plus an end-of-session
  snapshot on graceful shutdown (the §6.4 pause-and-snapshot).
- R10. Snapshot-file retention keeps the last K snapshots (K configurable) and
  GCs older snapshot files; WAL segments are never GC'd in v1.
- R11. On a corrupt or missing snapshot at startup (bad CRC, partial file,
  unreadable), recovery discards it and falls back to full WAL replay from `Seq`
  0; the fallback is logged at startup, not silent.

### Testing (mandatory harness)

- R12. INV-DET-02 lands as a property test over randomized command streams:
  snapshot at a mid-stream `Seq` S, Restore into a fresh engine, replay `(S, N]`,
  and assert the result is state-equal to a full replay of `[0, N]` — using the
  completeness-extended equality, not the books+ledger digest alone.
- R13. The state-equality check is extended to cover stop table, `Core.open`,
  and iceberg hidden remainder, so R12 and the determinism suite cannot pass on
  incomplete snapshots.
- R14. Snapshot/Restore carries explicit positive / negative / edge unit
  coverage (per `CLAUDE.md` testing rules) and a permanent regression seed under
  `testdata/fuzz/` for any bug fixed during implementation.

---

## Acceptance Examples

- AE1. Mid-stream equivalence. **Covers R5, R6, R12.**
  - **Given:** a randomized stream of N commands run into engine A (journaled to
    a WAL), with a snapshot taken at applied `Seq` S (0 < S < N).
  - **When:** engine B restores that snapshot, then replays WAL records
    `(S, N]`.
  - **Then:** `engineState(B)` equals `engineState(A)` and equals a full replay
    of `[0, N]`, under the completeness-extended equality.

- AE2. Untriggered stop survives restore. **Covers R5, R13.**
  - **Given:** a snapshot taken while a Stop order sits untriggered in the
    matcher's stop table.
  - **When:** the engine is restored and the market later reaches the trigger
    price via tail replay.
  - **Then:** the stop activates exactly as it would under full replay from 0 —
    same activation `Seq`, same resulting fills.

- AE3. Reservation integrity after restore. **Covers R5, R8.**
  - **Given:** a snapshot with resting orders whose funds are reserved in the
    ledger.
  - **When:** the engine is restored and a `CmdCancel` for one of those orders
    is applied via tail replay.
  - **Then:** the reservation is released correctly (INV-BAL-03 / INV-BAL-07
    hold) — i.e. `Core.open` was faithfully restored.

- AE4. Corrupt snapshot falls back. **Covers R11.**
  - **Given:** the latest snapshot file fails its CRC.
  - **When:** the engine starts.
  - **Then:** the bad snapshot is skipped, recovery replays the WAL from `Seq`
    0, the rebuilt state satisfies every invariant, and the fallback is logged.

---

## Scope Boundaries

### Deferred for later

- **WAL truncation behind snapshots.** Bounding WAL disk by truncating segments
  older than the oldest retained snapshot. Pairs naturally with a "walk back to
  the previous valid snapshot" fallback (below). Deferred because it couples
  snapshot-GC and WAL-GC and removes the always-reachable full-replay guarantee.
- **Walk-back-to-previous-snapshot fallback.** On corruption, try the next-older
  valid snapshot instead of replaying from 0. Needs retention ≥ 2 and aligned
  GC; only earns its keep once WAL truncation lands.
- **Shadow / non-pausing snapshot.** The §6.4 "shadow consumer" that snapshots
  without pausing. v1 uses pause-and-snapshot at a command boundary.
- **Parallel-topology snapshotting.** v1 targets the serial engine; snapshotting
  the parallel topology (quiescing workers at a clean `Seq` boundary) is a
  follow-up.

### Out of this unit of work

- **Tick/lot rejection** and **amend-up / price-change hardening** — the other
  two features from the same review. They are independent matching-layer changes
  and get their own brainstorm.
- Clustering / failover and backup/DR (object-storage archival, PITR) remain
  out of scope per the v1 design.

---

## Dependencies / Assumptions

- Assumes the serial v1 topology (`internal/market` `Core` / `Engine`). The
  snapshot is taken after `Engine.Drain()` at a quiesced command boundary.
- Assumes the canonical state digest remains the equality oracle and can be
  extended (R13) to cover stop table, `Core.open`, and iceberg hidden state
  without changing its layout-independence.
- Reuses the existing snapshot container (`internal/wal/snapshot.go`) and
  `wal.Replay`'s `afterSeq` seek contract; no change to the WAL record/segment
  format, which is itself a durability contract.
- Assumes stops are journaled (already true) — Restore must therefore suppress
  re-triggering during tail replay, consistent with `SuppressStops` in
  `tests/property/recovery_test.go:43`.

---

## Outstanding Questions

### Resolve before planning

- None blocking. Purpose, completeness contract, reconstruction approach, WAL
  retention, and fallback are all pinned in Key Decisions.

### Deferred to planning

- **Cadence defaults.** Exact default for "every N commands" and whether cadence
  is time-aware in addition to count-based. Direction set (R9); the numbers are
  a planning/config call.
- **Per-section codec layout.** The byte layout of each snapshot section (book,
  ledger, stop table, open-map) within the existing container. Architecture is
  fixed (logical, deterministic, versioned); the exact framing is planning.
- **Where Restore lives.** Whether reconstruction is a method on `Engine` or a
  package-level builder in `internal/market`, and how it reaches book/ledger
  internals without breaking the package layering in `CLAUDE.md`.

---

## Sources / Research

- `internal/wal/snapshot.go:16` — `WriteSnapshot`/`ReadSnapshot` container and
  the "publish only after WAL durable through S" invariant.
- `internal/wal/replay.go:26` — `Replay(dir, afterSeq, fn)`; the seek contract
  Restore's tail replay reuses.
- `internal/orderbook/dump.go:19` — `Book.Dump()` layout-independence, the basis
  for logical reconstruction.
- `internal/balance/dump.go:26` — `Ledger.Dump()` (balances + fees) — what the
  digest currently covers on the ledger side.
- `internal/market/engine.go:37` — `Core.open` reservation map and the
  `Engine` surface (`Drain`, `Seq`, `ApplyJournaled`).
- `tests/property/recovery_test.go` — existing INV-DET-01 / INV-DET-03 recovery
  pattern, `replayInto`, and `SuppressStops`; the template R12 extends.
- `docs/designs/spot-orderbook-engine-design.md:293` — §6.4 Snapshot / §6.5
  Recovery.
- `docs/designs/invariant-fuzz-testing-guide.md:156` — INV-DET-02 definition.
- `docs/plans/2026-06-13-002-feat-invariant-fuzz-harness-plan.md:67` —
  INV-DET-02 deferral and the missing-API rationale.
