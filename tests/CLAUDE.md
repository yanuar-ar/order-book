# tests

Cross-package correctness: end-to-end integration plus the randomized
invariant / differential / fuzz / recovery harness. This is where the engine's
money-safety guarantee is enforced — read
`docs/designs/invariant-fuzz-testing-guide.md` for the invariant taxonomy.

## Layout

- `integration/` — end-to-end order lifecycles across the assembled engine
  (`engine_test.go`) and the parallel topology / ring paths (`concurrent_test.go`,
  run under `make race`).
- `refmodel/` — the **reference model (oracle)**: a slow-but-obviously-correct
  reimplementation of the engine over plain slices/maps. `model.go` +
  `model_match.go` implement all eight order types, cancel, amend-down, stops,
  and self-trade prevention; `state.go` renders a canonical `State` string for
  comparison; `model_test.go` validates the oracle against hand-written examples.
  It reuses `internal/types` money helpers so amounts match the engine exactly,
  but reimplements everything else independently.
- `property/` — the harness, all in `package property`:
  - `invariants.go` — `CheckAllInvariants(engine, netDeposits)`, the composite
    that runs each package's `Verify()` plus the cross-package money/book
    invariants (INV-BAL-03/04, INV-OB-01). `Inspectable` is the read interface
    both `*market.Engine` and `*market.ParallelEngine` satisfy.
  - `generators.go` — `GenBroad` (uniform) and `GenSharp` (adversarial-biased)
    streams over all order types + cancel/amend/withdraw; the shared
    market/asset layout and `engineCfg()`/`modelCfg()`.
  - `differential.go` — `RunDifferential` (serial) and `RunDifferentialParallel`,
    plus the dynamic net-flow tracking (`applyNet`/`feedTrackingNet`) that keeps
    conservation exact across withdrawals.
  - `differential_test.go`, `determinism_test.go`, `recovery_test.go`,
    `adversarial_test.go`, `statemachine_test.go`, `fuzz_test.go`.
  - `testdata/fuzz/` — the permanent regression corpus.
- `fixtures/` — shared test data.

## The three layers of defense

1. **Unit tests** (in each engine package) — positive / negative / edge per
   scenario and order type.
2. **Invariant checks** — `CheckAllInvariants` runs the applicable `INV-*`
   taxonomy **after every command** and returns the first violation with context.
3. **Differential model (oracle)** — `refmodel` run on the identical stream;
   canonical state must match at every step. This is what catches "wrong answer,
   tidy state".

## What's covered

- **Differential loop** — engine vs `refmodel`, broad + sharp generators, serial
  AND parallel (`RunDifferentialParallel`, isolated/shared/default groupings),
  `CheckAllInvariants` each step.
- **Go native fuzz** — `FuzzEngine` decodes bytes → command stream → differential
  loop.
- **Stateful property** — `TestEngineStateMachine` via `pgregory.net/rapid`
  (test-only dep) with automatic shrinking.
- **Determinism** — same-seed byte-identical state (INV-DET-01); fill order by
  `(AggressorSeq, MatchIndex)` (INV-DET-04/INV-MAT-08).
- **Recovery** — full WAL replay equivalence (INV-DET-01), multi-segment
  rollover, torn-tail clean stop (INV-DET-03), metamorphic cancel-all /
  batch-invariance (INV-MET-01/02/04). Mid-stream snapshot equivalence
  (INV-DET-02) is deferred — needs an engine snapshot/restore API.
- **Adversarial seeds** (guide §6) — `adversarial_test.go` has an explicit
  scenario per row (exact-fit / one-short / joint-over balance, cross-market
  double-spend, int64-max overflow, FOK one-short rollback, post-only exact-touch,
  iceberg replenish priority loss, stop cascades, FIFO stress, cancel oddities,
  empty book).

Out of scope (no feature in v1): tick/lot rejection (INV-ARI-07), ClientReqID
idempotency (INV-IDM), amend-up/reprice priority (INV-AMD-02).

## Running

```bash
make property      # full differential + invariants + determinism + recovery + rapid
make differential  # just the engine-vs-model differential checks (verbose)
make fuzz          # coverage-guided fuzz; FUZZTIME=5m to extend
make test          # everything (go test ./...), includes the above Test* funcs
make race          # parallel + ring paths under the race detector
```

## Rules

- Failing fuzz inputs are saved permanently under `property/testdata/fuzz/` —
  **regression seeds are never deleted**.
- Generators are pure functions of a seed: seeded PRNG only, no wall-clock, no
  `map`-iteration order leaking into command order.
- Conservation baselines are tracked dynamically (`feedTrackingNet`) for any
  stream containing withdrawals — never assume deposits == net flow.
- Validate the oracle against hand-written examples (`refmodel/model_test.go`)
  so a buggy oracle can't mask an engine bug.
