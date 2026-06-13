# CLAUDE.md — Spot Order-Book Engine

Guidance for AI agents working in this repository. Read this first, then the
nearest package-level `CLAUDE.md` before editing code in that package.

## What this is

A deterministic, event-sourced **spot order-book matching engine** in Go
(`module github.com/yanuar-ar/order-book`, Go 1.25+). Single-node: WAL +
snapshot + replay for durability. Multi-market (sharded), shared balance
authority, price-time (FIFO) matching, eight order types (Limit, Market, IOC,
FOK, Post-Only, Iceberg, Stop, Stop-Limit), configurable maker/taker fees.

> Clustering/failover and backup/DR are **out of scope for v1**.

Authoritative specs live in `docs/`:
- `docs/designs/spot-orderbook-engine-design.md` — architecture.
- `docs/designs/invariant-fuzz-testing-guide.md` — **the correctness contract**
  (every invariant `INV-*` that must never be violated). Read this before
  touching matching, balance, or book code.
- `docs/plans/` — phased implementation plan.

## Core principles (do not break these)

1. **Determinism is the product.** The same command stream must always produce
   byte-identical state (books + ledger). No wall-clock reads, no `map`
   iteration order leaking into command/fill ordering, no `Math.random`-style
   nondeterminism on any path that affects state. Time enters only through the
   injected `ClockFunc`, captured once per command by the sequencer.
2. **The sequencer is the single ordering authority.** Every command that gets a
   `Seq` is journaled to the WAL — external commands *and* stop activations —
   so replay is a straight re-application, never a regeneration.
3. **One bug = real money lost.** All money math is integer fixed-point at a
   configured scale. Reservation rounds **up** (never under-covers); settlement
   rounds **down**. Never introduce floats into value math.
4. **Hot path is allocation-free.** Plain-old-data, pointer-free value types
   flow through preallocated arenas and SPSC rings. Do not add allocations,
   logging, or pointers to the hot path. Benchmarks gate this in CI.
5. **Single-writer balance authority.** The ledger is the sole authority on
   funds; it consumes one tagged event stream so reservations and settlements
   interleave in a single fixed order.

## Testing is MANDATORY — non-negotiable

**This is a financial, money-handling system operating in a sensitive and
regulated domain.** Correctness is a compliance requirement, not a nicety: a
single mispriced fill, leaked unit, or reservation error is real money lost and a
potential regulatory incident. Because of this, the harness below is **required,
not optional** — no change to matching, balance, book, sequencer, or WAL code may
merge without it:

- **Property / invariant tests** — `INV-*` properties asserted after every
  command (`tests/property`, `CheckAllInvariants`).
- **Differential tests** — every change validated against the independent
  reference-model oracle (`tests/refmodel`) over randomized streams.
- **Fuzz tests** — coverage-guided `go test -fuzz` plus the `pgregory.net/rapid`
  state machine, with a permanent regression corpus.

A feature-bearing change that adds engine behavior **without** extending these
three layers is incomplete and must not ship. When in doubt, treat the
`docs/designs/invariant-fuzz-testing-guide.md §7` checklist as the bar.

Every unit test suite you write or touch MUST cover all three categories
explicitly. A suite missing any category is incomplete:

- **Positive** — normal, valid inputs produce the expected result.
- **Negative** — invalid inputs are rejected cleanly (rejection, error,
  no-op) with **no partial state mutation**.
- **Edge cases** — boundaries: zero/empty, exact-fit, off-by-one (e.g. FOK
  one unit short), `int64`-overflow neighborhood, off-tick/off-lot, empty
  opposite book, immediate-trigger stops, `display > hidden` icebergs, duplicate
  IDs, cross-market reuse of the same account.

In addition to unit tests, the engine REQUIRES:

- **Invariant tests** — assert the relevant `INV-*` properties from the testing
  guide hold **after every command**, not just at the end. The strongest:
  `INV-BAL-04` (per-asset value conservation), `INV-BAL-03` (reserved == Σ
  locked), `INV-BAL-02` (cannot spend beyond balance), `INV-OB-01` (no crossed
  book), `INV-OB-05` (arena/free-list integrity).
- **Fuzz / property tests** — differential against a slow-but-obviously-correct
  reference model (the oracle catches "wrong answer but tidy state"), plus Go
  native `go test -fuzz` and/or `pgregory.net/rapid` state-machine tests with
  shrinking. Every fixed bug **must** add a permanent regression seed under
  `testdata/fuzz/` — historical bugs never silently return.

When you change behavior that an `INV-*` covers, update or add the assertion.
Follow the checklist in `docs/designs/invariant-fuzz-testing-guide.md §7`.

### Test conventions

- Table-driven tests; one table per scenario, named after the invariant where
  applicable (e.g. `TestInvBal02_NoSpendBeyondBalance`).
- Deterministic PRNG with a **logged seed** — every failure must be 100%
  reproducible by replaying the seed.
- `*_bench_test.go` benchmarks must stay zero-alloc; CI gates this on
  `internal/spsc`, `internal/matching`, `internal/balance`.

## Build & verify

```bash
make lint         # gofmt check + go vet  (run before every commit)
make test         # unit + integration + property/differential tests
make race         # race detector on concurrency packages
make bench        # benchmarks with -benchmem (watch the alloc count)
make build        # build ./cmd/engine into bin/
make property     # full differential + invariants + determinism + recovery + rapid
make differential # just the engine-vs-reference-model differential checks
make fuzz         # coverage-guided native fuzz (FUZZTIME=30s default; FUZZTIME=5m to extend)
```

CI (`.github/workflows/ci.yml`) runs lint, test, race, the zero-alloc benchmark
gate, and a short fuzz slice; `nightly.yml` runs long fuzz + a `-race` soak.
Keep all green. Run `make lint test` locally before claiming work is done.

The engine itself is **zero-dependency**; the only third-party module is
`pgregory.net/rapid`, imported solely from `tests/property/*_test.go` for the
state-machine layer, so it never links into `go build ./cmd/engine`.

## Layout

```
cmd/engine       # in-process engine wiring: startup recovery, snapshot cadence,
                 #   graceful shutdown (v1 has no network gateway)
cmd/bench        # single-thread latency/throughput harness
cmd/enginebench  # honest serial-engine throughput ceiling (offloaded generation)
cmd/loadtest     # open-loop load driver with a live order-book TUI
cmd/shardbench   # parallel shard-matching throughput by core assignment
internal/types       # POD value types, fixed-point money math, WAL codec
internal/orderbook   # per-market limit book (arena + intrusive FIFO levels)
internal/matching    # price-time matching, all order types, stop table
internal/balance     # single-writer ledger / balance authority
internal/sequencer   # global ordering authority + WAL journaling + settlement
internal/spsc        # lock-free SPSC ring buffer
internal/wal         # segmented write-ahead log, snapshot, replay
internal/market      # engine assembly: shards + serial/parallel topology;
                     #   Engine.Snapshot/Restore, Snapshotter, Recover
internal/platform    # GC suspension, core pinning (build-tagged per OS)
pkg/config           # env-driven configuration (read once at startup)
pkg/logger           # thin slog wrapper (startup/shutdown only)
tests/integration    # cross-package end-to-end tests
tests/property       # randomized invariant + determinism tests
```

## House rules

- `internal/` is private to this module; `pkg/` is the only intended-public
  surface. Don't add cross-package coupling that breaks the layering above
  (types ← orderbook ← matching ← market; balance, sequencer, wal are wired by
  market).
- Keep doc comments in the existing voice: a package-level comment stating the
  package's job and its determinism/allocation constraints.
- Don't log on the hot path. Config and logging are startup-only.
- Match surrounding code style; run `gofmt`. The repo must stay `gofmt`-clean.
