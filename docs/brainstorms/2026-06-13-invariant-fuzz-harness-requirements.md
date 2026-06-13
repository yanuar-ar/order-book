---
date: 2026-06-13
topic: invariant-fuzz-harness
---

# Invariant & Fuzz Test Harness — Requirements

## Summary

Build the full §7 correctness harness from `docs/designs/invariant-fuzz-testing-guide.md` over the engine's existing v1 features: a differential reference model (oracle), a complete `CheckAllInvariants` checked after every command, Go native fuzz plus a `rapid` state-machine layer, a permanent regression corpus, and recovery tests (snapshot / replay / torn-tail). It extends the current `tests/property` harness — which today asserts only 3 invariants every 200 commands — into the full §3 taxonomy.

## Problem Frame

The engine moves money: one wrong fill or leaked unit is a real loss. The testing guide defines ~50 invariants and a three-layer defense (unit tables → invariant checks → differential oracle), but only the thinnest slice is built. `tests/property/invariants_test.go` checks non-negative balances (INV-BAL-01), per-asset conservation (INV-BAL-04), and no-crossed-book (INV-OB-01) — and only at 200-command intervals, so a violation can't be pinned to the command that caused it. There is no reference model, so "wrong answer but tidy state" bugs (a wrong fill that still balances) pass silently. Stop, Stop-Limit, Iceberg, Cancel, and Amend are never exercised by the generator at all. The guide's own Appendix calls the reference model "the umbrella that catches the rest" — its absence is the largest gap.

## Key Decisions

- **Oracle matches state + aggregated fill summary, not per-fill ordering.** The reference model asserts equality of per-command state (balances, reserved, canonicalized book contents) plus aggregated fill totals (filled qty per order, fee per asset). It does not reproduce exact `MatchIndex` or per-fill ordering. This keeps the model slow-but-obviously-correct (sorted slices + maps) rather than a second full engine. Exact fill ordering is covered separately — see R-group "Determinism & Recovery".

- **`rapid` is added as a test-only dependency; production stays zero-dep.** `pgregory.net/rapid` provides the state-machine layer with automatic shrinking to a minimal reproducer. The zero-dependency value in `pkg/config` concerns the production hot path; a `_test.go`-only import never links into `go build ./cmd/engine`. `rapid` itself has no transitive dependencies.

- **Two generators share one oracle and one `CheckAllInvariants`.** A *broad* uniform-random generator feeds the differential and determinism runs (easy to trust as unbiased); a *sharp* adversarial-biased generator hunts bugs by frequently producing interesting states (deep books, near-empty balances, prices clustered at mid so orders cross, small-display icebergs, near-trigger stops, cancel/amend of live orders).

- **Invariants are checked via a hybrid `Verify()` surface.** Each package exposes one `Verify() error` that checks its own structural invariants using its internal fields (orderbook: arena/free-list, FIFO links, idIndex; ledger: reserved consistency), returning the first violation with context. `tests/property` composes every `Verify()` plus the cross-package money invariants (INV-BAL-03/04) over the existing `Dump()` APIs into a single `CheckAllInvariants(engine)` entry point the differential loop calls after each command.

- **CI runs short in PR, long at night.** Each PR runs the differential loop, `rapid` defaults, and a short native-fuzz slice as a required gate. A separate nightly workflow runs long fuzz and soak; its findings are reported but do not block PRs.

## Requirements

### Reference model & differential loop

- R1. A reference model implements `Apply(cmd) -> fill summary` and `Snapshot() -> state` for all eight order types plus Cancel and Amend-down, with semantics matching the engine: FOK all-or-nothing with atomic rollback, Post-Only reject-on-cross, Iceberg display/hidden with replenish-to-back, Stop/Stop-Limit trigger on last-trade movement, self-trade prevention (cancel aggressor remainder), and the market-buy `MaxQuote` spend cap.
- R2. The model is validated against hand-written examples before use, so a buggy oracle cannot mask an engine bug.
- R3. A differential loop runs engine and model on an identical command stream and, after every command, asserts: (a) aggregated fill summary matches, (b) canonical state snapshot matches, (c) `CheckAllInvariants` on the engine passes. On mismatch it fails with seed, step index, and the offending command.

### Invariant checking

- R4. `CheckAllInvariants(engine)` implements every applicable §3 invariant (A Balance, B Arithmetic, C Order-book structure, D/E Matching & per-order-type, F Determinism, H Metamorphic) and is invoked after every command in the differential and property loops — not at intervals.
- R5. Each engine package exposes a `Verify() error` covering its own structural invariants: orderbook (INV-OB-02/03/04/05/06/07/08/09 — price ordering, FIFO link integrity, level totals, arena/free-list, idIndex, ID uniqueness, time priority, remaining bounds); ledger (INV-BAL-01 non-negative, reserved consistency). Each returns the first violation with context.
- R6. The cross-package money invariants — INV-BAL-03 (reserved == Σ locked over open orders) and INV-BAL-04 (per-asset conservation including the fee account) — are asserted in `tests/property` against the composed engine. INV-BAL-02/05/06/07/08/09/10 are covered by directed unit tests and the differential loop.
- R7. Arithmetic invariants INV-ARI-01 (no overflow), INV-ARI-02 (reservation rounds up), INV-ARI-03 (settlement rounds down, ≤ reserved), INV-ARI-04 (rounding residue accounted), INV-ARI-05 (scale consistency), INV-ARI-06 (remaining monotonically non-increasing) have directed tests, not just fuzz coverage.

### Generators & corpus

- R8. A broad uniform-random generator and a sharp adversarial-biased generator both produce streams over all eight order types plus Cancel and Amend-down, sharing the oracle and `CheckAllInvariants`. Both are pure functions of a seed; every run logs its seed for reproducibility; no wall-clock or map-iteration order leaks into command order.
- R9. Every adversarial scenario in the guide §6 has an explicit hand-written seed test: exact-fit balance, one-unit-short balance, two orders jointly over balance, same account buying across markets over its quote balance, price/qty near `int64` max, inexact-`quote()` rounding, FOK one-unit-short rollback, post-only exact-touch boundary, iceberg replenish priority loss, immediate-trigger stop, one trade triggering many stops, a stop triggering another stop, thousands of same-price orders, cancel of unknown/filled/cancelled IDs, empty opposite book for market/IOC.
- R10. A Go native `FuzzEngine` decodes a `[]byte` into a command stream (tolerating malformed input) and runs the differential loop. A `rapid` state-machine test drives new-order / cancel / amend / deposit operations with `CheckAllInvariants` on every step and automatic shrinking.
- R11. Every fixed bug adds a permanent regression seed under `testdata/fuzz/`; regression seeds are never deleted.

### Determinism & recovery

- R12. Determinism is asserted on the engine alone: two runs of the same seed produce byte-identical state (INV-DET-01), and fill order is deterministic by `(aggressorSeq, matchIndex)` (INV-DET-04, INV-MAT-08). This is where exact ordering — not modeled by the oracle — is verified.
- R13. Recovery tests assert snapshot+replay equivalence — `load snapshot@S + replay (S,N] == replay [0,N]` (INV-DET-02) — and torn-tail crash-consistency: truncating the final WAL record yields a state that still satisfies all A–E invariants (INV-DET-03). These wire a real `*wal.Writer` as `Config.Journal` and replay via `Engine.ApplyJournaled`.
- R14. Metamorphic properties are asserted: cancel-all returns each account to net-deposit ± realized trades (INV-MET-01), an account with no open orders has zero reserved (INV-MET-02), and subset-replay equals full replay (INV-MET-04).

## Acceptance Examples

- AE1. **Covers R3, R4.** **Given** any seeded command stream, **when** the differential loop applies command *i* to both engine and model, **then** fill summary and state snapshot match and `CheckAllInvariants` passes; **and** if not, the failure message names the seed, step *i*, and the command.
- AE2. **Covers R1, R9 (FOK).** **Given** resting liquidity one unit short of a FOK order's quantity at acceptable prices, **when** the FOK is submitted, **then** zero fills occur and book + all balances are byte-identical to the pre-FOK state.
- AE3. **Covers R6 (INV-BAL-03).** **Given** any point in any run, **when** `CheckAllInvariants` runs, **then** each `(account, asset).reserved` equals the sum of locked amounts over that account's open orders (quote for buys, base for sells).
- AE4. **Covers R13 (INV-DET-03).** **Given** a WAL whose final record is truncated mid-write, **when** recovery replays it, **then** replay stops cleanly at the torn tail and the rebuilt state satisfies every A–E invariant.

## Scope Boundaries

- **Tick/lot rejection (INV-ARI-07)** — N/A in v1. The engine has no tick-size/lot-size validation (only a deferred tick-ladder *performance* note); off-tick/off-lot orders are not rejected. Not implemented in this round.
- **ClientReqID idempotency (INV-IDM-01/02)** — N/A in v1. `ClientReqID` is unused by engine logic and no dedup exists; idempotency is deferred alongside failover, which is out of v1 scope.
- **Amend-up / price-change (INV-AMD-02)** — N/A. The engine surface exposes only amend-down (quantity decrease keeping priority); raise-qty / reprice cancel+insert semantics do not exist to test.
- **Implementing new engine features** — out of scope. This round builds the harness over existing behavior; it does not add tick/lot, idempotency, or amend-up.

## Dependencies / Assumptions

- The WAL/journal seam exists: `market.Config.Journal` accepts a `sequencer.Journal` (defaults to no-op) and `Engine.ApplyJournaled(cmd)` replays journaled commands. **Assumption to confirm in planning:** the engine can produce a full-state snapshot (ledger + every book) and restore from it, as R13's INV-DET-02 requires. `internal/wal/snapshot.go` exists but its wiring to live engine state is unverified.
- Checking `CheckAllInvariants` after every command is O(open-orders) per command, i.e. O(n²) per run. Accepted: differential and `rapid` streams are bounded (hundreds to low thousands of commands), where this is affordable.
- Adding `pgregory.net/rapid` introduces the repo's first `go.mod` dependency (test-only). The CI zero-alloc and lint gates are unaffected.

## Outstanding Questions

### Deferred to planning

- Where the reference model and `CheckAllInvariants` live (e.g. a `tests/refmodel` package vs `tests/property` internals) and the exact signature of each package's `Verify()`.
- Native-fuzz `-fuzztime` budget per target for the PR gate, and the nightly soak duration.
- Whether snapshot/restore needs new engine API before R13 can land (follows from the Dependencies assumption above).

## Sources

- `docs/designs/invariant-fuzz-testing-guide.md` — the §3 invariant taxonomy, §4 harness design, §6 adversarial scenarios, §7 checklist this doc implements.
- `tests/property/invariants_test.go` — existing harness (`genStream`, `digest`, partial `checkInvariants`) being extended.
- `internal/matching/match.go` — `Result` fields (`STP`, `Rejected`, `Reason`, `Pending`) and self-trade-prevention policy.
- `internal/market/engine.go` — `Config.Journal`, `ApplyJournaled`, the `amend` path (amend-down only).
- `internal/types/types.go` — `RejectReason` values and `ClientReqID` (unused by engine logic).
