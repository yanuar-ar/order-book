# order-book

A deterministic, event-sourced **spot order-book matching engine** in Go.

Multi-market (sharded), with a shared balance authority, all eight order types
(Limit, Market, IOC, FOK, Post-Only, Iceberg, Stop, Stop-Limit), price-time
(FIFO) matching, single-node durability (WAL + snapshot + replay), and
configurable maker/taker fees.

> Built in phases. See `docs/plans/` for the implementation plan and
> `docs/designs/spot-orderbook-engine-design.md` for the architecture spec.
> Clustering/failover and backup/DR are out of scope for v1.

## Layout

```
cmd/engine     # in-process engine wiring
cmd/bench      # open-loop load harness (perf phase)
internal/      # types, spsc, orderbook, matching, balance, wal, sequencer, market, platform
pkg/           # config, logger
tests/         # integration, property/fuzz, fixtures
```

## Development

```bash
make lint   # gofmt check + go vet
make test   # unit + integration tests
make race   # race detector on concurrency packages
make build  # build the engine binary
```

Requires Go 1.25+.
