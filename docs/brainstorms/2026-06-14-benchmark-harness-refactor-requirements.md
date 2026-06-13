---
date: 2026-06-14
topic: benchmark-harness-refactor
---

# Benchmark Harness Refactor — Requirements

## Summary

Collapse the four `cmd/` benchmark scaffolds into two purpose-named tools over a
shared kit: `throughput` ("how fast can it go") and `loadtest` ("how does it
behave at load X"). Each gains a `-topology serial|parallel` flag so serial and
parallel can be compared under identical conditions. Delivered in phases —
extract the shared kit (A), add the topology flag (B), then collapse and delete
the old binaries (C).

---

## Problem Frame

The repo has four benchmark binaries — `cmd/bench`, `cmd/enginebench`,
`cmd/loadtest`, `cmd/shardbench` — that each pick a point in a three-axis space
(*topology* serial/parallel, *what's measured* throughput-ceiling/latency,
*generation* inline/offloaded) without the axes being legible. Three concrete
problems result:

- **Overlapping, misleading purposes.** `bench` and `enginebench` both read as
  "serial engine throughput," but `bench` generates inline (so its rate is
  generation-bound) while `enginebench` offloads generation (the true ~940k
  ceiling). Same apparent purpose, non-comparable numbers, confusable names.
- **No controlled comparison.** `enginebench` (serial, ~940k, generation
  offloaded) and `shardbench` (parallel, ~327k, generation inline) differ on
  *two* axes at once, so the serial-vs-parallel numbers can't be compared — the
  parallel figure is also penalized by inline generation.
- **Duplication.** The shardbench rewrite (this session) copied ~250 lines of
  TUI, histogram, and generator from `loadtest`; that logic is now triplicated.

A newcomer (or the author six months later) cannot tell which tool to reach for
or trust a cross-tool comparison.

---

## Key Decisions

- **Two purpose-named tools, not four.** `throughput` answers "how fast can it
  go"; `loadtest` answers "how does it behave at load X." Names state the
  question. Chosen over keeping four deduped tools because clarity-of-purpose is
  the primary goal.

- **`-topology serial|parallel` as a flag, not separate binaries.** Topology
  becomes one explicit axis on each tool (with `-cores` for parallel), so the
  serial/parallel choice is visible and isolated rather than encoded in which
  binary you ran.

- **`throughput` offloads generation for both topologies.** A producer goroutine
  fills the ingress ring; the engine/control goroutine drains it. Holding the
  generation method constant across serial and parallel is what makes the two
  numbers a controlled comparison. Consequence: the new `throughput -topology
  parallel` reports a higher, cleaner figure than the current inline shardbench
  (~327k) — that is the intended fix, not a regression.

- **`bench`'s deterministic latency role survives as a flag.** `throughput`
  accepts `-duration` *or* `-n <count>` + `-rngseed` and always reports per-op
  latency percentiles, so `throughput -topology serial -n 200000 -rngseed 1`
  replaces today's `bench`. No third tool. The measured quantity shifts from
  `bench`'s inline service time to engine step-time under saturation — accepted
  as an equivalent latency-regression signal (see Dependencies / Assumptions).

- **Shared kit under `cmd/internal/harness`.** The load generator, two-tier
  latency histogram, live TUI renderer, and the market/asset + topology setup
  move into one Go-cmd-shared package; both tools become thin wiring over it.

- **Phased A → B → C, each shipping green.** A: extract the kit behind the
  existing four binaries (behavior-preserving). B: add `-topology` to the
  throughput-role tool (the comparability fix). C: collapse to `throughput` +
  `loadtest`, delete `bench`/`enginebench`/`shardbench`, rewrite the Makefile.

---

## Requirements

**`throughput` tool**

- R1. `throughput` drives the engine at maximum rate with **offloaded
  generation** (a producer goroutine feeds the ingress ring; the engine/control
  goroutine drains it), so the measured rate reflects the engine, not the
  generator.
- R2. `throughput` accepts `-topology serial|parallel` and, for parallel,
  `-cores` (the market→worker map). Both topologies use the same offloaded
  generation method.
- R3. `throughput` accepts either `-duration <d>` or `-n <count>` (+ `-rngseed`
  for a reproducible stream) as the run-length mode.
- R4. `throughput` reports sustained throughput and per-op latency percentiles
  (avg/p50/p95/p99/max) as a text summary. No live TUI.

**`loadtest` tool**

- R5. `loadtest` paces commands open-loop at `-tps` and measures latency from
  each command's intended time (coordinated-omission-correct).
- R6. `loadtest` accepts `-topology serial|parallel` and, for parallel,
  `-cores`, plus `-market`/`-levels` for the display.
- R7. `loadtest` renders the live order-book TUI (bids/asks/depth/last/spread,
  throughput, latency) and prints a final summary.

**Shared kit**

- R8. A `cmd/internal/harness` package holds the load generator, the latency
  histogram, the TUI renderer, and the market/asset + topology construction,
  with no logic duplicated across the two tools.
- R9. The kit's generator serves both styles the tools need: a live-mid inline
  generator (for `loadtest`'s realism and TUI) and a base-mid generator (for
  `throughput`'s offloaded producer).

**Migration**

- R10. After phase C, `cmd/bench`, `cmd/enginebench`, and `cmd/shardbench` are
  deleted and the Makefile targets are rewritten to the two new tools.
- R11. Each phase (A, B, C) builds, lints, and passes tests on its own before
  the next begins.

---

## Acceptance Examples

- AE1. **Covers R1, R2.** **Given** `throughput -topology serial` and
  `throughput -topology parallel -cores "0;1,2"` on the same workload, **then**
  both numbers come from offloaded generation and differ only by topology — a
  controlled serial-vs-parallel comparison.
- AE2. **Covers R3, R4.** **Given** `throughput -topology serial -n 200000
  -rngseed 1`, **then** the run is reproducible and reports per-op latency
  percentiles — the replacement for today's `bench`.
- AE3. **Covers R5, R7.** **Given** `loadtest -tps 1000000 -topology parallel
  -cores "0;1,2"`, **when** the engine cannot sustain the rate, **then** latency
  grows against the intended schedule (not hidden) and the TUI shows the live
  book and the achieved-vs-target shortfall.
- AE4. **Covers R8.** **Given** the two tools after phase C, **then** the TUI,
  histogram, and generator exist in exactly one place (`cmd/internal/harness`),
  referenced by both.

---

## Scope Boundaries

**Deferred for later**

- Parallelizing the balance authority / control path — the only route past the
  serial throughput ceiling. This refactor measures topologies more clearly; it
  does not change the engine. (Separately deferred "Outside v1" work.)

**Outside this refactor's identity**

- New metric classes beyond throughput + latency (flame graphs, per-market
  breakdowns, allocation tracing) — the zero-alloc gate and `go test -bench`
  already cover allocation.
- HTML/web output for the harnesses — terminal text + TUI only.

---

## Dependencies / Assumptions

- Assumes full freedom to rename/delete binaries and rewrite Makefile targets —
  nothing external (CI, scripts, other people) depends on `bench`/`enginebench`/
  `shardbench` names. Confirmed during dialogue.
- Assumes engine step-time under saturation (what offloaded `throughput`
  measures per op) is an acceptable substitute for `bench`'s inline service-time
  latency for regression purposes — they are semantically close (both exclude
  generation from the timed region) but not identical.
- Assumes `cmd/internal/harness` (Go's cmd-shared convention) is an acceptable
  home; it is internal to the module and not part of the public `pkg/` surface.
- Live book reads for `loadtest`'s parallel mode remain race-safe because the
  `ParallelEngine` control↔worker handshake is synchronous — reads happen
  between control steps when workers are idle (verified under `-race` this
  session).

---

## Sources / Research

- Current harnesses (this session's reads): `cmd/bench/main.go` (serial,
  fixed-count, inline, every-op latency), `cmd/enginebench/main.go` (serial,
  offloaded generation, ~940k ceiling), `cmd/loadtest/main.go` (serial,
  open-loop paced, inline, TUI), `cmd/shardbench/main.go` (parallel full engine,
  inline, TUI, ~327k).
- `cmd/CLAUDE.md` — bench scaffold conventions (coordinated-omission correctness,
  generation off the engine's core, helper unit tests).
- `internal/market/parallel.go` — `ParallelEngine`, synchronous control↔worker
  dispatch (the reason the live book read is race-safe between steps, and the
  reason parallel end-to-end is dispatch-bound).
- Measured baselines observed this session: enginebench ~940k, shardbench (full
  engine, inline) ~327k, loadtest @1M target ~470k.
