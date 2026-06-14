---
title: "refactor: Allocation-free, jitter-clean loadtest measurement path"
date: 2026-06-14
type: refactor
origin: docs/brainstorms/2026-06-14-loadtest-jitter-requirements.md
depth: lightweight
---

# refactor: Allocation-free, jitter-clean loadtest measurement path

## Summary

Make `cmd/loadtest`'s coordinated-omission-correct latency measurement reflect
the **engine**, not the harness. The measuring goroutine's per-command and
per-100ms-frame paths become allocation-free, periodic frame-capture cost is not
attributed to engine latency, and the report states honestly that on a non-pinned
platform (Darwin dev) the tail floor is OS-scheduling-bound. Engine ack-buffer
bounding and Linux core-pinning are **out of scope** — deferred per origin to the
gateway and Linux-perf phases respectively.

## Problem Frame

The loadtest match-latency tail swings 4 orders of magnitude (p50 ~705ns, max
5–60ms) and varies wildly run-to-run. Diagnosis (see origin: `docs/brainstorms/2026-06-14-loadtest-jitter-requirements.md`)
established, and reading the code confirmed:

- The **heavy TUI work** (`Sprintf`, `strings.Builder`, terminal write) already
  runs off the measuring goroutine — it lives in `Render`, called by
  `DisplayLoop` (`cmd/internal/harness/tui.go:70,87`). Not a measuring-path cost.
- What remains on the measuring goroutine: per-100ms `BuildFrame`
  (`tui.go:50`) calls `Book.Depth` twice, and `Book.Depth`
  (`internal/orderbook/dump.go:45`) **allocates a fresh slice each call**; plus
  per-command generation (`GenLiveMid`, `cmd/internal/harness/gen.go:80`).
- With GC disabled during the session (`platform.GCOff`), these allocations
  never free → the heap bloats, which is one driver of the growing tail.
- The dominant **absolute** floor on Darwin is OS scheduling (no core-pinning;
  `internal/platform/pin_darwin.go` is a no-op) — not fixable here.

So the achievable win now is an **honest, allocation-clean measurement** that is
ready for a clean read on Linux — not a dramatic drop in Darwin's absolute tail.

## Requirements

Traceable to origin (`docs/brainstorms/2026-06-14-loadtest-jitter-requirements.md`).

- **R1.** No allocating or heavy work runs on the measuring goroutine for TUI
  frame production; only a bounded, cheap state capture stays on it. (origin R1)
- **R2.** The per-command and per-frame measuring paths allocate nothing in
  steady state. (origin R2)
- **R3.** No harness-overhead iteration (frame capture) is attributed to engine
  latency in the histogram. (origin R3)
- **R4.** The report documents the Darwin OS-scheduling floor so the numbers are
  not read as the engine's true tail. (origin success criteria, honesty note)

## Key Technical Decisions

- **Keep the existing capture/render split; only make the capture side clean.**
  `Render` (heavy) already runs on `DisplayLoop`. The fix targets `BuildFrame`
  (capture) — making it allocation-free — rather than restructuring the rendering.
- **A minimal, read-only `DepthInto` accessor on the book, not the ack-buffer.**
  Allocation-free depth capture needs the book to fill a caller-owned buffer
  instead of returning a new slice. This is an additive, read-only accessor — it
  does not touch the engine ack buffer (`core.acks`), which stays deferred to the
  gateway phase. Chosen over copying `Depth`'s result (still allocates) or
  pre-sizing a slice the harness can't reuse across frames.
- **Compensate for known harness cost; never hide real engine slip.** For R3,
  exclude only the frame-capture iteration from the histogram. Do **not** reset
  the pacing baseline in a way that masks genuine engine fall-behind — coordinated
  omission correctness for real slip must be preserved.
- **Honesty over vanity numbers.** The summary states the Darwin floor and points
  at the deferred levers (ack buffer → gateway, pinning → Linux), rather than
  chasing absolute numbers that require deferred work.

## Implementation Units

### U1. Allocation-free per-command measuring path

- **Goal:** the per-command loop (generate → submit → step → record → ack-drain)
  allocates nothing in steady state, so a long run does not bloat the heap under
  `GCOff`. (R2)
- **Requirements:** R2.
- **Dependencies:** none.
- **Files:** `cmd/loadtest/main.go`, `cmd/internal/harness/gen.go`, new bench in
  `cmd/internal/harness/gen_bench_test.go` (or extend an existing `*_test.go`).
- **Approach:** add a benchmark over the measuring step (`GenLiveMid` + a no-op
  or in-memory engine `Submit`/`Step` + `h.Record` + the ack-drain cursor),
  observe allocs, and drive to zero. `GenLiveMid` returns a value `types.Command`;
  confirm it and the ack-drain (`AcksAll()` header read + cursor walk) add no
  per-call allocation. The ack-drain already advances an O(1) cursor — keep it.
- **Execution note:** characterization-first — add the zero-alloc benchmark and
  observe current allocs before changing generation.
- **Patterns to follow:** `GenBaseMid` (`cmd/internal/harness/gen.go:89`, the
  static alloc-free generator) and the zero-alloc bench style in
  `internal/sequencer/sequencer_bench_test.go`; generator determinism tests in
  `cmd/internal/harness/gen_test.go`.
- **Test scenarios:**
  - Bench: steady-state per-command measuring path → **0 allocs/op** (`-benchmem`).
  - Determinism preserved: same seed → identical command stream (existing
    `gen_test.go` assertion still passes).
  - Edge: ack-drain with no newly-durable acks → records nothing, allocates
    nothing.
- **Verification:** `go test -bench -benchmem` on the new bench shows 0 allocs/op;
  generator determinism tests green.

### U2. Allocation-free, double-buffered frame capture

- **Goal:** the per-100ms `BuildFrame` capture on the measuring goroutine
  allocates nothing and stays µs-bounded. (R1, R2)
- **Requirements:** R1, R2.
- **Dependencies:** none (independent of U1).
- **Files:** `internal/orderbook/dump.go` (add `DepthInto`), `cmd/internal/harness/tui.go`
  (`BuildFrame` + double-buffer), `cmd/loadtest/main.go`, `cmd/throughput/main.go`
  (shares `BuildFrame`), `internal/orderbook/dump_test.go`, harness frame bench.
- **Approach:** add a read-only `DepthInto(side, n, buf []PriceLevel) []PriceLevel`
  to the book that appends into a caller-owned (reused) buffer. In the harness,
  double-buffer two `Frame` structs with reusable `Asks`/`Bids` backing slices;
  the measuring goroutine fills the inactive buffer via `DepthInto` and publishes
  it through the existing `atomic.Pointer[Frame]`; `DisplayLoop`/`Render` consume
  the published frame unchanged. Book reads stay on the measuring goroutine (sole
  mutator in serial; workers idle between control steps in parallel — the existing
  invariant). Percentile/avg scalars already snapshot into `Frame` fields; leave
  them (µs, no alloc).
- **Technical design (directional):** capture path becomes
  `fill(frame[i].Asks = book.DepthInto(Sell, n, frame[i].Asks[:0]); …); pub.Store(&frame[i]); i ^= 1`.
- **Patterns to follow:** existing `Frame` + `atomic.Pointer` hand-off
  (`tui.go:25-67`), `Book.Depth` (`dump.go:45`) as the content contract.
- **Test scenarios:**
  - Bench: frame capture (`DepthInto` ×2 + fill reusable `Frame`) → **0 allocs/op**.
  - Fidelity: `DepthInto(side, n, buf)` returns the same levels as `Depth(side, n)`
    for the same book state (top-of-book ordering, price + visible qty).
  - Edge: empty book → empty ladders, no panic, no alloc; `buf` reused across
    calls is reset (`[:0]`) and does not leak stale levels.
  - No tearing: a consumer reading the published frame never sees a half-filled
    ladder (double-buffer swaps only after fill completes).
- **Verification:** frame-capture bench 0 allocs/op; `DepthInto` fidelity test
  matches `Depth`; loadtest/throughput TUI still renders correctly by manual run.

### U3. Don't attribute frame-capture cost to engine latency + honest floor note

- **Goal:** the periodic frame-capture iteration is not recorded as engine
  latency, and the summary states the Darwin OS floor. (R3, R4)
- **Requirements:** R3, R4.
- **Dependencies:** U2 (capture is cheap/alloc-free first, so exclusion is the
  only residual).
- **Files:** `cmd/loadtest/main.go`.
- **Approach:** when an iteration performs a frame capture, skip `h.Record` for
  that one command (it carries known harness overhead). Do not otherwise alter
  the open-loop `intended = start + i*interval` schedule — genuine engine
  fall-behind must still inflate later samples (coordinated-omission correctness).
  Add one summary line noting that on non-pinned platforms the tail floor is
  OS-scheduling-bound, and that the engine ack-buffer (gateway) and Linux
  core-pinning are the deferred levers for the true floor.
- **Patterns to follow:** the existing measuring loop and `printSummary`
  (`cmd/loadtest/main.go`).
- **Test scenarios:**
  - Recorded histogram sample count == processed − frame-captures (±1), i.e.
    capture iterations are excluded.
  - Coordinated-omission preserved: exclusion drops only capture iterations, not
    samples from genuine slow steps (assert a forced-slow step is still recorded).
  - Summary prints the floor note line.
- **Verification:** unit assertion on excluded count; manual run shows the note;
  `make lint test` green.

## Scope Boundaries

**In scope:** `cmd/loadtest`, `cmd/internal/harness` (`gen.go`, `tui.go`), and a
minimal read-only `DepthInto` accessor in `internal/orderbook`.

### Deferred to Follow-Up Work

- `cmd/throughput` measures step latency (not loop latency) and already isolates
  the frame build from its recording, so it needs no separate work — it inherits
  U2's allocation-free capture from the shared harness.

### Deferred (from origin — separate phases)

- **Engine ack-buffer bounding** (`core.acks` unbounded growth → `memmove` inside
  `Step` at high load; production leak — `cmd/engine` never drains). The correct
  fix is an ack output channel with a real consumer; it belongs with the
  **gateway/publisher** work. Until then, the high-load ack-memmove jitter remains.
- **Real core-pinning/affinity** (`SchedSetaffinity`, isolated cores) for a clean
  tail floor → **Linux perf phase**. On Darwin the absolute numbers stay
  OS-scheduling-bound regardless of this cleanup.

## Risks & Dependencies

- **Double-buffer reuse vs. a slow renderer.** If `DisplayLoop` lagged a full
  100ms cycle, the measuring goroutine could refill a buffer still being read.
  Mitigation: render is fast and cadence is 100ms; use ≥2 buffers and, if needed,
  skip a swap when the prior frame is unconsumed. Low risk.
- **`DepthInto` touches `internal/orderbook`.** Keep it strictly read-only and
  additive (no change to `Depth` or book state); covered by a fidelity test
  against `Depth` and the existing book tests.
- **No external dependencies.** All work is within `cmd/` + a read-only
  `internal/orderbook` accessor.

## Sources & Research

- Origin: `docs/brainstorms/2026-06-14-loadtest-jitter-requirements.md` (verified
  diagnosis + deferral decisions).
- Code: `cmd/internal/harness/tui.go` (`BuildFrame`/`Render`/`DisplayLoop`),
  `cmd/internal/harness/gen.go` (`GenLiveMid`/`GenBaseMid`),
  `cmd/loadtest/main.go` (measuring loop), `internal/orderbook/dump.go`
  (`Depth`/`PriceLevel`), `internal/platform/pin_darwin.go` (no-op pinning),
  `internal/platform/gc.go` (`GCOff`).
