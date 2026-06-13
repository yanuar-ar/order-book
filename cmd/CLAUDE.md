# cmd

Executable entry points. v1 has **no network gateway** — commands are submitted
in-process. The bench binaries are measurement scaffolds, not production paths.

## Binaries

- `engine` — wires and runs the in-process engine from `pkg/config` and reports
  readiness. The production assembly.
- `bench` — single-threaded latency/throughput harness: seeds liquidity, drives
  a command stream, reports throughput + internal-latency percentiles.
- `enginebench` — the **honest serial-engine ceiling**: one producer goroutine
  fills the ingress ring, a separate engine goroutine drains it in a tight loop.
  The engine itself is unchanged (still single-writer/deterministic); only
  command generation is offloaded.
- `loadtest` — open-loop load driver with a live order-book TUI (bids/asks/depth/
  last price) and latency stats. Pacing is open-loop and
  **coordinated-omission-correct**: command `i` is scheduled at `start + i/rate`
  and latency is measured from that intended time.
- `shardbench` — parallel shard-matching throughput by core assignment
  (`-cores "0;1,2"`). Isolates the parallelizable matching hot path; the balance
  authority is not exercised here.

## Constraints

- Benches may allocate during setup/generation, but must not perturb the engine
  hot path they measure. Keep generation off the engine's core (see
  `enginebench`).
- Latency harnesses must stay coordinated-omission-correct: measure from the
  intended schedule time, never from dequeue time.

## Testing (positive / negative / edge)

Helper logic gets unit tests: `loadtest/hist_test.go` (histogram/percentiles),
`shardbench/parse_test.go` (core-assignment parsing). Cover **positive** (valid
spec → expected parse / correct percentile), **negative** (malformed `-cores`
string rejected), **edge** (empty input, single bucket, boundary percentiles).
`main` wiring itself is exercised via `tests/integration`.
