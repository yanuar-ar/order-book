---
title: "Reduce loadtest measurement jitter (harness cleanup)"
date: 2026-06-14
type: improvement
status: ready-for-plan
scope: lightweight
---

# Reduce loadtest measurement jitter

## Problem

`cmd/loadtest` reports a match-latency tail that swings wildly: p50 ~705ns but
p99 from 325µs to 25ms and max from 3.65ms to 60ms across runs of the **same**
config. The concern: is the engine matcher jittery, or is the harness polluting
its own measurement? This brainstorm diagnosed the sources and scopes the fix.

## Diagnosis (verified)

Experiments run on the dev machine (Apple M2, Darwin, async journaling). The
compound value of this doc is the diagnosis — so a re-run later doesn't start
from zero.

1. **GC is NOT the steady-state cause.** A pre-built binary with `GODEBUG=gctrace=1`
   shows only ~5 GC cycles, all in the first ~25ms (startup ramp during `Fund` +
   `SeedBook`), then none — the in-code `platform.GCOff()` works. The "20 cycles"
   seen earlier came from `GODEBUG=gctrace=1 go run`, which conflates the **`go
   build` compiler's** GC with the program's. Always profile a pre-built binary.

2. **Harness cause (in scope): `BuildFrame` on the measuring goroutine.**
   `cmd/internal/harness/tui.go` `BuildFrame` does allocating, format-heavy work
   (`fmt.Sprintf` per level, `strings.Builder`, `strings.Repeat`) and runs on the
   **latency-measuring goroutine** every 100ms (`cmd/loadtest/main.go`). Its cost
   delays the next paced command, inflating that command's coordinated-omission
   latency. Load-independent (appears even at 50k tps).

3. **Engine cause (DEFERRED → gateway): unbounded `core.acks`.** `internal/market`
   appends every ack to a slice that nothing trims; at high load the `append`
   realloc/`memmove` happens **inside `Step`**, spiking match latency, and the max
   grows with command count (1.5s→2.15ms, 6s→8.56ms). Verified: **no consumer
   exists in v1** — `cmd/engine` never reads `Acks()`, so it leaks in production
   too. The correct fix is an ack output channel with a real consumer, which
   belongs with the **gateway/publisher** work (out of scope here).

4. **Platform cause (DEFERRED → Linux): OS scheduling floor on Darwin.** Same
   config varies 3.65ms↔60ms run-to-run depending on machine load — the
   measuring goroutine competes with other processes because Darwin core-pinning
   is a no-op (`internal/platform/pin_darwin.go`). Disabling Go async-preemption
   (`GODEBUG=asyncpreemptoff=1`) only moved max 60ms→37ms with p99 unchanged, so
   preemption is minor; **OS descheduling dominates**. Not fixable in the harness
   — needs Linux + `SchedSetaffinity` + isolated cores (also currently deferred in
   `internal/platform/pin_linux.go`).

## Goal

`cmd/loadtest`'s reported latency reflects the **engine**, not the harness's own
TUI/format overhead. The measuring goroutine does engine work (pace → generate →
submit → step → record) and nothing else heavy.

## Requirements (in scope)

- **R1.** TUI frame production must not run allocating/format-heavy work on the
  latency-measuring goroutine. The heavy snapshot/format/render moves off the
  measuring path; if any state read must stay on the measuring goroutine for
  book-read safety (serial topology: the measuring goroutine is the sole book
  mutator), it is a **bounded, cheap** copy (top-N levels), not the full
  `Sprintf` render.
- **R2.** The per-command measuring hot path allocates nothing (so harness work
  doesn't add GC/heap pressure that the engine itself avoids).
- **R3.** No harness-overhead iteration is attributed to engine latency in the
  histogram (whichever approach R1 takes must not leave a periodic TUI spike in
  the recorded distribution).

## Success criteria

- The periodic ~100ms TUI spike no longer appears in the match-latency histogram.
- The measuring goroutine is allocation-free per command (verifiable).
- On a quiet machine, the loadtest match tail drops toward the engine's true
  cost; the residual Darwin OS floor is documented, not hidden.

## Scope boundaries

**In scope:** `cmd/loadtest` + `cmd/internal/harness` measurement-path cleanup.

**Deferred — gateway phase:** bounding the engine ack buffer (`core.acks`). The
proper fix is an ack output channel with a real consumer (the LMAX output
disruptor); it only makes sense once the gateway/publisher exists. Until then,
the high-load ack-memmove jitter and the production ack leak remain known debt.

**Deferred — Linux perf phase:** real core-pinning/affinity (`SchedSetaffinity`)
and isolated cores for a clean tail floor. On Darwin the absolute numbers stay
noisy regardless of this harness cleanup; clean tail measurement requires Linux.

## Assumptions / open questions

- Whether R1 is best served by (a) a cheap on-thread snapshot + off-thread
  render, or (b) excluding the frame-build iteration from recording, is an
  implementation choice for `/ce-plan`. (a) is preferred as it keeps full
  measurement coverage; (b) is the minimal fallback.
- The dev-machine numbers are environmental; real validation of any improvement
  should compare runs on the **same** machine state, ideally on Linux.

## Sources

- Harness: `cmd/loadtest/main.go` (measuring loop + `framePtr.Store(BuildFrame(...))`),
  `cmd/internal/harness/tui.go` (`BuildFrame`, `Sprintf`/`strings.Builder`).
- Engine ack buffer: `internal/market/engine.go` (`core.acks` append in `Core.ack`,
  `Acks()`/`AcksAll()`); no consumer in `cmd/engine/main.go`.
- Platform: `internal/platform/pin_darwin.go` (no-op), `pin_linux.go` (affinity
  deferred), `gc.go` (`GCOff`).
- Diagnosis experiments (2026-06-14): duration scaling, `gctrace` on built binary
  vs `go run`, low-vs-high load, `asyncpreemptoff=1`.
