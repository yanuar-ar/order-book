# order-book

A deterministic, event-sourced **spot order-book matching engine** in Go.

Multi-market (sharded), with a shared balance authority, all eight order types
(Limit, Market, IOC, FOK, Post-Only, Iceberg, Stop, Stop-Limit), price-time
(FIFO) matching, single-node durability (WAL + snapshot + replay), and
configurable maker/taker fees.

> Built in phases. See `docs/plans/` for the implementation plan and
> `docs/designs/spot-orderbook-engine-design.md` for the architecture spec.
> Clustering/failover and backup/DR are out of scope for v1.

## Design principles

- **Determinism is the product.** The same command stream always produces
  byte-identical state. Time enters only through an injected clock, captured
  once per command by the sequencer; nothing else reads the wall clock and no
  `map`-iteration order leaks into command/fill ordering.
- **Event-sourced & single-node durable.** Every sequenced command is journaled
  to the WAL; recovery is a straight replay (snapshot + replay == full replay).
- **Single-writer balance authority.** One ledger owns all funds; reservations
  round up (never under-cover), settlements round down. All money math is
  integer fixed-point — no floats.
- **Allocation-free hot path.** Pointer-free, fixed-size value types flow
  through preallocated arenas and lock-free SPSC rings. Benchmarks gate zero
  allocations in CI.

## Layout

```
cmd/engine       # in-process engine wiring (v1 has no network gateway)
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
internal/market      # engine assembly: shards + serial/parallel topology
internal/platform    # GC suspension, core pinning (build-tagged per OS)
pkg/config           # env-driven configuration (read once at startup)
pkg/logger           # thin slog wrapper (startup/shutdown only)
tests/integration    # cross-package end-to-end tests
tests/property       # randomized invariant + determinism tests
```

Each significant package carries a `CLAUDE.md` describing its responsibility,
constraints, and required tests. Start at the root [`CLAUDE.md`](CLAUDE.md).

## Development

```bash
make lint   # gofmt check + go vet
make test   # unit + integration tests
make race   # race detector on concurrency packages
make bench  # benchmarks with -benchmem (zero-alloc hot path)
make build  # build the engine binary into bin/
```

Requires Go 1.25+. CI runs lint, test, race, and a zero-alloc benchmark gate.

### Load testing

```bash
make loadtest        # live order-book TUI; defaults: 1M TPS, 2m, 100 users
make loadtest-quick  # 10s smoke run
make shardbench CORES="0;1,2"  # parallel matching throughput by core layout
```

## Configuration

Configuration is environment-driven and read once at startup. Copy
[`.env.example`](.env.example) and adjust; every key falls back to a built-in
default. Notable keys: `OB_MARKETS`, `OB_RING_SIZE` (power of two),
`OB_WAL_PATH`, `OB_PRICE_SCALE` / `OB_QTY_SCALE` / `OB_FEE_SCALE` (fixed-point
scales), `OB_MAKER_FEE` / `OB_TAKER_FEE` (rates at the fee scale).

## Testing requirements

Because one bug means real money lost, testing is **mandatory and layered**:

1. **Unit tests** — every suite covers **positive**, **negative**, and **edge
   cases** explicitly. A suite missing any category is incomplete.
2. **Invariant tests** — assert the `INV-*` properties hold **after every
   command**, not just at the end.
3. **Fuzz / property tests** — differential against a slow-but-correct reference
   model, plus Go native `go test -fuzz` and `pgregory.net/rapid` state-machine
   tests with shrinking. Every fixed bug adds a permanent regression seed.

The full correctness contract — every invariant and the fuzz-harness design —
lives in [`docs/designs/invariant-fuzz-testing-guide.md`](docs/designs/invariant-fuzz-testing-guide.md).
