# cmd

Executable entry points. v1 has **no network gateway** — commands are submitted
in-process. The benchmark binaries are measurement scaffolds, not production
paths; their shared building blocks live in `cmd/internal/harness`.

## Binaries

- `engine` — wires and runs the in-process engine from `pkg/config` and reports
  readiness. The production assembly.
- `throughput` — **"how fast can it go"**: drives the engine at max rate with
  **offloaded generation** (a producer goroutine fills the ingress ring; the
  engine/control goroutine drains it), so the rate reflects the engine, not the
  generator. `-topology serial|parallel` (+ `-cores` for parallel) makes
  serial-vs-parallel a controlled comparison; run length is `-duration` or
  `-n`+`-rngseed` (the reproducible, deterministic latency-regression mode).
  `-durable` (with optional `-wal <dir>`) journals to a real WAL with
  group-commit fsync — the honest durable ceiling — and `-flushcap` tunes the
  group-commit batch (commands per fsync; bigger amortizes I/O harder at the cost
  of durable-ack latency). Renders the same live order-book TUI as `loadtest`
  plus a final summary with engine step-latency percentiles. The frame is built **on the engine goroutine**
  between its own `Step` calls (the sole book mutator in serial; workers idle
  between the control goroutine's synchronous steps in parallel), so the live
  book reads never race the matcher.
- `loadtest` — **"how does it behave at load X"**: open-loop driver with a live
  order-book TUI (bids/asks/depth/last price) and latency stats.
  `-topology serial|parallel` (+ `-cores`). Pacing is open-loop and
  **coordinated-omission-correct**: command `i` is scheduled at `start + i/rate`
  and latency is measured from that intended time. Live-mid generation and TUI
  depth reads happen between control steps (workers idle), so they don't race
  the matcher in parallel topology.

## Shared kit (`cmd/internal/harness`)

- `engine.go` — the `Engine` interface (satisfied by both `*market.Engine` and
  `*market.ParallelEngine`), `BuildEngine(topology, groups, cfg)` → engine +
  cleanup func, the market/asset spec, `DefaultConfig`, `Fund`, `ParseCores`.
- `gen.go` — the order-flow generators: `GenLiveMid` (reads the live book mid;
  loadtest) and `GenBaseMid` (static base mid, no book reads; throughput's
  producer), plus `SeedBook` and shared `Acct`/`GenQty`.
- `hist.go` — the two-tier latency histogram (fine 10ns / coarse 10µs),
  nearest-rank percentiles.
- `tui.go` — the live order-book renderer (`Frame`, `BuildFrame`, `Render`,
  `DisplayLoop`), parameterized by a title + sub-line so it serves both tools.

## Constraints

- Benches may allocate during setup/generation, but must not perturb the engine
  hot path they measure. Keep generation off the engine's core (see
  `throughput`'s producer/engine split).
- Latency harnesses must stay coordinated-omission-correct: measure from the
  intended schedule time, never from dequeue time.

## Testing (positive / negative / edge)

Helper logic gets unit tests in `cmd/internal/harness`: `hist_test.go`
(histogram/percentiles), `engine_test.go` (`ParseCores`, serial/parallel
build-equivalence), `gen_test.go` (generator determinism + order mix). Cover
**positive** (valid spec → expected parse / correct percentile), **negative**
(unknown topology rejected, malformed `-cores` skipped), **edge** (empty input,
single sample, overflow bucket, boundary percentiles). `main` wiring itself is
exercised via `tests/integration`.
