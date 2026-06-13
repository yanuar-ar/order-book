# tests

Cross-package correctness: end-to-end integration and randomized
invariant/determinism testing. This is where the engine's money-safety guarantee
is actually enforced — read `docs/designs/invariant-fuzz-testing-guide.md`.

## Layout

- `integration/` — `engine_test.go` (end-to-end order lifecycles across the
  assembled engine) and `concurrent_test.go` (parallel topology / ring paths;
  run under `make race`).
- `property/` — `invariants_test.go`: builds a deterministic deposit prelude +
  random order stream from a seed, applies it, and asserts **global invariants
  hold at every step**, plus same-seed determinism.
- `fixtures/` — shared test data / regression seeds.

## The three layers of defense (must all exist)

1. **Unit tests** (in each package) — concrete positive / negative / edge
   examples per scenario and order type.
2. **Invariant checks** — `CheckAllInvariants(state)` runs the full `INV-*`
   taxonomy (guide §3) **after every command**; returns the first violation with
   context.
3. **Differential model (oracle)** — a slow-but-obviously-correct reference
   model run on the identical command stream; assert output + state match at
   every step. This is the only thing that catches "wrong answer, tidy state".

## Required tests here

- **Differential loop** (guide §4.2): engine vs reference model, identical
  random stream, compare events + state + `CheckAllInvariants` each step.
- **Go native fuzz** (`go test -fuzz=FuzzEngine`): coverage-guided byte mutation
  over serialized command streams.
- **Stateful property** (`pgregory.net/rapid`): state-machine with automatic
  shrinking to a minimal reproducer.
- **Determinism** (`INV-DET-01`): two runs of the same seed → byte-identical
  state. **Recovery** (`INV-DET-02/03`): snapshot + replay == full replay, and
  torn-tail recovery still satisfies all invariants.
- **Adversarial seeds** (guide §6): every listed edge has an explicit seed
  (exact-fit balance, FOK one-unit-short, overflow neighborhood, off-tick/lot,
  immediate-trigger stops, cross-market reuse, duplicate `ClientReqID`).

## Rules

- Every failing seed found by fuzzing is saved permanently under
  `testdata/fuzz/` — **regression seeds are never deleted**.
- Generators must be deterministic: seeded PRNG only, no wall-clock, no `map`
  iteration order leaking into command order. Log the seed on every run.
- Validate the reference model itself against hand-written examples so a buggy
  oracle can't mask an engine bug.
