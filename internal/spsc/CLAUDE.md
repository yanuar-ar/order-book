# internal/spsc

A lock-free **single-producer / single-consumer** ring buffer. Exactly one
goroutine may `Push` and exactly one (distinct) goroutine may `Pop`
concurrently. Values stored by value → allocation-free for POD `T`.

## Files

- `ring.go` — generic `Ring[T]`. Power-of-two capacity; `head`/`tail` on
  separate cache lines (padded) to avoid false sharing.
- `concrete.go` — concrete instantiations (`RingCommand`, `RingFill`, …) used on
  the hot path.
- `ring_test.go`, `ring_bench_test.go`.

## Constraints

- **SPSC contract is a hard invariant**: only the producer calls `Push`, only
  the consumer calls `Pop`. Violating it is undefined — do not add a second
  producer or consumer.
- Capacity MUST be a power of two and > 0 (`New` panics otherwise — a startup
  programming error, not a runtime condition).
- `Len()` is safe from either side but may be stale; never use it for
  correctness decisions, only for metrics/heuristics.

## Testing (positive / negative / edge + invariant)

`ring_test.go`, `ring_bench_test.go` exist. Cover:
- **Positive**: FIFO order preserved across push/pop.
- **Negative**: `Push` returns `false` when full (no store); `Pop` returns
  `false` when empty; `New` panics on non-power-of-two / zero capacity.
- **Edge**: wrap-around at the mask boundary; capacity 1; full→drain→refill.
- **Concurrency**: run under `make race` — concurrent producer/consumer
  preserves order and loses no items.
- `ring_bench_test.go` must stay **zero-alloc** (CI gate).
