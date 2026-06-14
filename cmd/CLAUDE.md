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
  `-journal` selects the journaling path: **`async`** (off-thread fsync — the 1M
  durable path, **the default**), `sync` (inline group-commit fsync), or `none`
  (no WAL, the raw matching ceiling). `-flushcap` tunes the group-commit batch
  (commands per fsync; bigger amortizes I/O harder at the cost of durable-ack
  latency); `-wal <dir>` overrides the temp WAL dir. `-replication off|sync|async`
  adds an **in-process hot standby** (the production posture): the primary's
  ceiling holds (~1M durable-async with a standby following, since `Replicate` is
  non-blocking), but the standby applies every command on the same box and shares
  cores, so this is a lower bound vs a real 2-node deployment. On the dev machine:
  raw ~1.4M, async durable ~1.3M, async durable + sync standby ~1.0M, sync durable
  saturates lower with worse tails.
  `-cpuprofile <path>` writes a CPU profile of the
  measured window (profiles the whole process; read the engine cost under
  `market.(*Engine).Step` and ignore the producer's backpressure-spin in
  `main.func1`). Renders the same live order-book TUI as `loadtest`
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
  the matcher in parallel topology. `-journal async|sync|none` (default `async`,
  + optional `-wal`, `-flushcap`) selects journaling; in the durable modes it
  reports **two SLOs** separately: *internal match latency* (intended→matched) and
  *durable-ack latency* (intended→release watermark, tracked O(1) via `AcksAll`+
  `ReleasedSeq` with a cursor — no per-command rescan). `-replication off|sync|async`
  adds an in-process hot standby; in `sync` the ack SLO becomes
  *durable+replicated-ack* (`ReleasedSeq = min(durableSeq, replicatedSeq)`), so the
  reported tail includes the standby — at a rate the in-process standby can sustain
  (it shares cores) the SLO is bounded; above it the tail blows up, exactly the
  signal a load test should give. Comparing `loadtest` vs
  `loadtest -journal sync` shows async keeping match-latency tails in µs (vs tens
  of ms when fsync blocks the matcher inline) — e.g. at 500k tps, match p99 ~10ms
  async vs ~77ms sync.

## Make targets (async default, sync variant)

Both bench tools cover async and sync via separate targets; the default is the
off-thread (async) journaller. `make throughput` / `make loadtest` journal
durably with the async journaller; `make throughput-sync` / `make loadtest-sync`
use the inline journaller for comparison. Tune the group-commit batch with
`FLUSHCAP=` and load with `TPS=`.

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

`gen_bench_test.go` (`BenchmarkMeasurePathPerCommand`) locks the loadtest
measuring loop's **harness-side** per-command work (live-mid generation + latency
recording) at **0 allocs/op** — so the latency histogram reflects the engine, not
harness churn. (Diagnosis note: loadtest jitter is dominated by deferred items —
OS scheduling on non-pinned platforms, and the engine's unbounded ack buffer at
high load — not the harness; see `docs/brainstorms/2026-06-14-loadtest-jitter-requirements.md`.)
