---
date: 2026-06-13
topic: spot-orderbook-engine
---

# Spot Order Book Engine — Requirements

## Summary

Build the spot order-book matching engine described in `docs/designs/spot-orderbook-engine-design.md`: an event-sourced, deterministic, multi-market engine in Go with a shared balance authority and all eight order types. Delivery is split into ~9 dependency-ordered phases, each shipping one component with its tests green before the next begins. Single-node durability (WAL + snapshot + replay) is in scope; clustering, failover, and backup/DR are not.

## Problem Frame

The design doc is a complete implementation spec but a single-shot build of it is hard to verify and easy to derail — matching, balance, durability, and determinism are deeply coupled, and the hot-path performance constraints (zero-alloc, core pinning) are platform-specific. Development and verification happen on macOS arm64, where the Linux pinning/hugepage machinery cannot run, so a build that front-loads performance would block on an unverifiable layer. The work needs an ordering that proves correctness and determinism incrementally on the dev machine, defers platform-bound performance to the end, and leaves out the cluster/backup layers the team has explicitly descoped.

## Key Decisions

- **Conventions from the boilerplate, engine per spec.** Borrow peripheral structure from `boilerplate-gin-dat` (`pkg/config`, `pkg/logger`, the `tests/` tree, `Makefile`, `scripts/`, `.env.example`) but build the engine core exactly per the design doc's package layout. No Gin, GORM, controllers, or services touch the zero-alloc hot path.
- **Single-node durability in, clustering out.** §6 (WAL + snapshot + single-node recovery/replay) is kept — it is the determinism backbone and enables the recovery test. §16 (Raft, cluster, failover, dedup) and §17 (backup/DR, PITR, object storage) are out of scope.
- **Correctness first, performance as the final phase.** Phases 1–8 target correctness, determinism, and a full passing test suite on macOS. Performance hardening (zero-alloc benchmarks, GC-off, core pinning, the 100k-TPS harness) is a dedicated final phase, with core pinning behind Linux build tags and darwin no-op stubs.
- **No network gateway in v1.** The engine is in-process plus a benchmark harness, per the design. There is no HTTP/REST API.
- **Configurable maker/taker fees, fixed at engine config.** Both rates are configurable and `>= 0` (no rebates). Fee rates are not changeable at runtime, so replay reproduces fees deterministically. Each `Fill` must identify which side was the aggressor (taker) so fee attribution is deterministic.
- **Hybrid test layout.** White-box unit tests co-located as `*_test.go`; integration and property/fuzz tests under `tests/`.
- **Strict per-phase gate.** A phase is done only when `go vet`, `go test ./...`, and `go test -race ./...` (concurrency packages) all pass. Red blocks the next phase.

## Requirements

**Engine scope & topology**

- R1. Three independent market shards — BTC/USDT, ETH/USDT, SOL/USDT — over a shared balance authority, with one goroutine per component (single-writer) connected by SPSC ring links. Markets are config-driven, defaulting to these three.
- R2. Spot only: no margin, leverage, or positions.
- R3. All money math is fixed-point `int64` at scale `1e8`. `price * qty` uses a 128-bit intermediate to avoid overflow. Rounding is explicit and reservation rounds in the direction that never under-reserves.

**Order types & matching**

- R4. Matching is strict price-time (FIFO) priority: the aggressor sweeps best opposing levels, FIFO within a level, until filled or price no longer crosses.
- R5. All eight order types are supported with the semantics in design §9: Limit/GTC (rest remainder), Market (no price limit, cancel remainder), IOC (cancel remainder), FOK (full-fill check first, else reject with no execution), Post-Only (reject if it would cross), Iceberg (only `display` visible; on depletion replenish from `hidden` and re-queue at the back, losing time priority), Stop and Stop-Limit (held off-book, triggered by `lastPrice`).
- R6. Amend: a price change or a quantity increase is a cancel/replace — the order gets a new `Seq` and goes to the back of its queue (time priority lost). A quantity decrease is applied in place and keeps time priority. Reserved balance is adjusted in both cases.
- R7. Self-trade prevention: when an aggressor would match against its own resting order, matching of that pair stops and the aggressor's remaining quantity is cancelled (cancel-newest). The resting order is untouched and no fill is emitted for the self-pair.
- R8. Stop/Stop-Limit triggering: each fill updates the market's `lastPrice`; triggered stops (buy-stop on `last >= stopPrice`, sell-stop on `last <= stopPrice`) are activated in deterministic order by re-injecting a new command through the sequencer (new `Seq`), never inline, to avoid unbounded recursion.

**Balance & fees**

- R9. The ledger holds `available`/`reserved` per `account|asset`: reserve on order accept, settle on fill, release on cancel; reject on insufficient available. Reservation in advance prevents cross-market double-spend of the same asset.
- R10. Deposit and Withdraw commands adjust `available` (withdraw rejects if insufficient).
- R11. Configurable maker and taker fee rates, both `>= 0`. The aggressor (taker) pays the taker fee; the resting order (maker) pays the maker fee; fees are credited to a per-asset fee account. Reservation includes the worst-case taker fee so an order never under-reserves.
- R12. No balance is ever negative, in any state, under any sequence of commands.

**Durability & determinism**

- R13. The whole engine is a deterministic state machine per the §3 contract: one `Seq` source (sequencer only), timestamp captured once and embedded, no wall-clock or randomness in logic, fills ordered by `(aggressor_seq, match_index)`, settlement applied in that key order.
- R14. WAL records every external command: mmap append, length-prefixed framing with CRC, group-commit (`fdatasync` per N records or X µs), fixed-size segments with a committed-offset published atomically.
- R15. Snapshot is pause-and-snapshot between batches, serializing full state (ledger + all books) and recording the last applied `Seq`.
- R16. Single-node recovery loads the latest snapshot, seeks the WAL to the first record past its `Seq`, verifies CRC (truncate on torn write at the tail), checks `Seq` contiguity (halt on gap — never guess), and replays through the same topology to rebuild identical state.

**Testing & verification**

- R17. Co-located white-box unit tests per package covering design §13.1 (spsc, orderbook, matching per type, balance incl. rounding, wal, sequencer).
- R18. Integration tests under `tests/integration/` covering all §13.2 scenarios: cross-market balance consistency, parallel matching, order-types end-to-end, cancel & release, recovery determinism, and the global invariants check.
- R19. Property/fuzz tests under `tests/property/` generating random load across types/accounts/markets, asserting the §13.2.6 invariants at every step, and asserting same-seed runs produce identical output.
- R20. The recovery-determinism test compares post-replay state by logical deep-equality (all balances including the fee account, every book's levels and FIFO order and best bid/ask, `lastPrice`, final `Seq`), order-independent — not by literal byte-snapshot comparison.
- R21. Per-phase verification gate: `go vet ./...`, `go test ./...`, and `go test -race ./...` (concurrency packages) must pass before the next phase starts. The performance phase additionally requires `-benchmem` showing 0 allocs/op on hot paths.

**Repo & conventions**

- R22. Go module `github.com/yanuar/orderbook`, rooted directly at the repo (no nested `spot-engine/` subdir), with the package layout from design §11 (`cmd/engine`, `cmd/bench`, `internal/{types,spsc,wal,sequencer,balance,orderbook,matching,market,platform}`, `tests/`, `pkg/`, `scripts/`, `Makefile`).
- R23. `pkg/config` is a small struct loaded from env/flags (markets, ring sizes, WAL path, scales, fee rates); `pkg/logger` uses stdlib `log/slog`. No heavy dependencies; the hot path never logs or allocates.
- R24. The performance phase enforces a zero-alloc hot path, `debug.SetGCPercent(-1)` during sessions, core pinning behind Linux build tags (`runtime.LockOSThread` + `SchedSetaffinity`) with darwin no-op stubs, and a `cmd/bench` open-loop load harness with coordinated-omission correction targeting 100k TPS.

## Build Phases

Each phase ends green per R21 before the next begins.

- P0. **Scaffold** — `go.mod` (`github.com/yanuar/orderbook`), repo skeleton, `pkg/config`, `pkg/logger`, `Makefile` (`make test`, `make build`), `.env.example`, `tests/` dirs. Done: builds, empty `make test` runs.
- P1. **types + spsc** — fixed-point types and scaling/rounding helpers (§4), the SPSC ring (§5). Done: ring push/pop/full/empty/wraparound tests; rounding tests; `-race` concurrent producer/consumer test.
- P2. **orderbook** — arena, levels, price ladder, free-list, `idIndex` (§8). Done: insert/cancel/amend, per-level FIFO integrity, best bid/ask updates, free-list recycling, `idIndex` consistency.
- P3. **matching + order types** — price-time core plus all eight types and self-trade prevention (§9, R5–R7). Stop triggering is unit-tested in isolation via an injection callback (full re-injection lands in P6/P7). Done: table-driven tests per type, STP case, amend cases.
- P4. **balance** — `available/reserved` ledger, reserve/settle/release, deposit/withdraw, maker/taker fees and fee account, rounding (R9–R12). Done: reject-on-insufficient, never-negative, conservation incl. fee account, fee rounding tests.
- P5. **wal** — mmap append, CRC framing, segments, replay (§6, R14–R16). Done: append→read round-trip, CRC corruption detection, torn-write truncation, replay rebuilds identical state.
- P6. **sequencer** — global `Seq`, MPSC fan-in (round-robin SPSC), fill ordering and settlement, timestamp capture (§6, R13). Done: monotonic/contiguous `Seq`, deterministic fill-vs-command interleaving; `-race`.
- P7. **market shard + wiring** — wrap orderbook+matching+rings as a shard; wire all components in `cmd/engine` (no core pinning yet); complete stop re-injection through the sequencer. Done: single-process engine runs a scripted command stream end-to-end.
- P8. **integration + property/fuzz** — `tests/integration/` (§13.2 scenarios) and `tests/property/` (§13.3), including recovery determinism via deep-equality (R18–R20). Done: all scenarios and invariants green; same-seed reruns identical.
- P9. **performance** — zero-alloc micro-benchmarks (`-benchmem`), `SetGCPercent(-1)`, Linux-tagged core pinning with darwin stubs, `cmd/bench` open-loop 100k-TPS harness with HdrHistogram and coordinated-omission correction (§14, §18, R24). Done: benchmarks show 0 allocs/op on hot paths; harness reports throughput and latency percentiles.

## Key Flows

- F1. Order lifecycle (happy path)
  - **Trigger:** A `NewOrder` command enters the ingress ring.
  - **Steps:** Sequencer assigns `Seq` + timestamp and journals to WAL → Balance reserves funds (or rejects) and routes a funded order to the correct market shard → shard matches, emitting `Fill`s → fills return to the sequencer for deterministic ordering → Balance settles each fill in `(aggressor_seq, match_index)` order, charging fees → Publisher emits ack/trade/book-update.
  - **Covered by:** R1, R4, R9, R11, R13.
- F2. Recovery
  - **Trigger:** Engine restarts after a stop/crash.
  - **Steps:** Load latest snapshot (state at `Seq = S`) → seek WAL to first record with `Seq > S` → per record verify CRC and `Seq` contiguity (truncate on tail torn write, halt on gap) → replay through the same topology → resume live from the next `Seq`.
  - **Covered by:** R13, R14, R15, R16.

## Acceptance Examples

- AE1. **Covers R5 (FOK).** Given a book with 3 units available at crossing prices, when a FOK order for 5 units arrives, then it is rejected with no execution and the book is unchanged.
- AE2. **Covers R5 (Post-Only).** Given a resting ask at 100, when a Post-Only buy at 100 arrives (would cross), then it is rejected; when a Post-Only buy at 99 arrives, then it rests.
- AE3. **Covers R5 (Iceberg).** Given an iceberg with `display`=2, `hidden`=8, when its visible portion is fully taken, then `display` is replenished from `hidden` and re-queued at the back of the level (time priority lost).
- AE4. **Covers R8 (Stop).** Given a buy-stop at `stopPrice`=105 and `lastPrice`=100, when a trade pushes `lastPrice` to 106, then the stop activates as a new command (new `Seq`) and matches as a market order.
- AE5. **Covers R6 (Amend).** Given a resting order with time priority, when amended to a lower quantity, then it stays in place and keeps priority; when amended to a higher price, then it is re-queued at the back with a new `Seq`.
- AE6. **Covers R7 (STP).** Given account A has a resting bid, when A submits a crossing ask, then matching of that pair stops, A's aggressor remainder is cancelled, the resting bid remains, and no fill is emitted for the pair.
- AE7. **Covers R9 (cross-market balance).** Given account A deposits USDT sufficient for one order, when A places crossing buys in BTC/USDT and ETH/USDT that together exceed the balance, then the second is rejected (the same USDT can't be reserved twice).
- AE8. **Covers R11, R12 (fees, conservation).** Given non-zero maker/taker fees, when a trade settles, then taker and maker fees are credited to the fee account and the total per-asset value across user balances plus the fee account is unchanged by matching.
- AE9. **Covers R16, R20 (recovery determinism).** Given a random load run with snapshot + WAL, when the engine is killed and replayed, then the reconstructed state is logically deep-equal to the pre-kill state across all balances, books, `lastPrice`, and final `Seq`.

## Scope Boundaries

**Deferred for later (out of v1, plausibly later)**

- High availability: Raft replication, odd-node cluster, leader election, automatic failover, `internal/cluster`, `internal/dedup` (design §16).
- Backup & DR: continuous WAL archival to object storage, periodic offsite snapshots, PITR, restore verification (design §17).
- Ledger and order-book scale-out (per-account hash sharding, per-market multi-group Raft).
- Maker rebates (negative maker fees), runtime-mutable fee schedules.
- Non-pausing (shadow consumer) snapshots.

**Outside this product's identity (v1)**

- Network gateway / HTTP/REST API — the engine is in-process plus a benchmark harness.
- Margin, leverage, derivatives, or positions — spot only.
- Gin/GORM/controller-service-repository layering over the hot path.

## Dependencies / Assumptions

- Go 1.25.6 on macOS arm64 is the dev/verification environment; functional and concurrency tests run there. Core pinning, `Mlockall`, and hugepages are Linux-only and are exercised only on Linux (darwin uses no-op stubs).
- Dependencies stay minimal: `golang.org/x/sys` (mmap and Linux scheduling) and an HdrHistogram library for the performance phase. No Viper/Logrus/GORM.
- `price * qty` fits a 128-bit intermediate given per-market price-ladder bounds; the exact 128-bit arithmetic approach is a planning detail.
- Fee rates are fixed engine configuration for the lifetime of a session, so replay reproduces fee computation deterministically.
- The design doc's `Fill` struct is extended (or settlement otherwise receives) an aggressor-side indicator so maker/taker fee attribution is deterministic.

## Success Criteria

- All phases reach the R21 gate green; the full suite (unit + integration + property/fuzz) passes, with `-race` clean on concurrency packages.
- The recovery-determinism test (AE9) passes, and same-seed property runs produce identical output — demonstrating the determinism contract end-to-end.
- The §13.2.6 global invariants hold at every step of the property/fuzz run: no negative balances, reserved equals locked funds across open orders, per-asset value conserved across matching, books internally consistent (no crossed resting levels, intact linked lists).
- Performance phase: hot-path micro-benchmarks show 0 allocs/op, no GC cycles during a measured session, and the `cmd/bench` harness reports sustained throughput with latency percentiles (titik jenuh / headroom) against the §1 targets.

## Outstanding Questions

**Deferred to Planning**

- Exact 128-bit arithmetic mechanism for `price * qty` and the precise rounding implementation for fee reservation.
- WAL group-commit tuning (records-per-sync vs µs window) and segment size defaults.
- Price-ladder bounds and the dense-vs-sparse fallback per market.
- Where the aggressor-side indicator lives (a new `Fill` field vs derived in the shard) and how settlement consumes it.
- Snapshot serialization format (used for persistence; the recovery test compares logically, so canonical encoding is not required for correctness).

## Sources / Research

- `docs/designs/spot-orderbook-engine-design.md` — the authoritative implementation spec; section references throughout (§3 determinism, §4 types, §5 spsc, §6 wal/sequencer/recovery, §7 balance, §8 orderbook, §9 matching/order-types, §11 layout/build-order, §13 testing, §14/§18 performance, §16/§17 descoped HA/DR).
- `github.com/bonarizki-dat/boilerplate-gin-dat` — source of peripheral repo conventions only (`pkg/config`, `pkg/logger`, `tests/` layout, `Makefile`, `scripts/`); its HTTP clean-architecture layering is not applied to the engine.
