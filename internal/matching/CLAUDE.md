# internal/matching

Price-time (FIFO) matching over an order book, supporting all eight order types
plus self-trade prevention.

## Files

- `match.go` — `Engine`: matches "active" orders (Limit, Market, IOC, FOK,
  Post-Only, Iceberg) against one market's book. Reuses per-`Submit` output
  buffers (`fills`, `filled`) — the returned `Result` **aliases** them, so the
  caller must consume the `Result` before the next `Submit` (the serial engine
  does; the parallel worker copies).
- `stops.go` — Stop / Stop-Limit orders held off-book in a stop table, activated
  by trade-price movement and emitted to the `Sink` as new commands (never an
  inline closure — keeps the path allocation-free). Also `StopDump()`/`StopView`,
  a deterministic (by Seq) read view of pending stops so `tests/property` can
  include their off-book reservations in INV-BAL-03.
- `snapshot.go` — `EncodeSnapshot` / `RestoreSnapshot` for the stop table.
  Serializes each pending stop as its raw `FundedOrder` (`StopView` is lossy on
  `Tif`/`Flags`/`DisplayQty`/`MaxQuote`) and restores by setting the `stops` slice
  directly — no ledger calls, no matching.

## Constraints

- **No recursion / no inline cascade** on stop activation: triggered stops
  re-enter as new commands via the `Sink` (`INV-STP-05`). They activate in
  ascending originating-`Seq` order for deterministic replay (`INV-STP-04`).
- Fills execute at the **resting (maker) price**, not the aggressor price
  (`INV-MAT-03`). Best price first, then time within price.
- Market-buy spend is bounded by `FundedOrder.MaxQuote` (`qtyScale`-derived);
  `MaxQuote == 0` means unbounded (limit orders and sells).
- Keep `Submit` allocation-free: append into the reusable buffers, don't return
  freshly allocated slices.

## Testing (positive / negative / edge + invariant)

`match_test.go`, `stops_test.go`, `match_bench_test.go` exist. Per order type,
cover positive / negative / edge AND the relevant `INV-*` (guide §3.D/E):
- **GTC**: fills fully or rests exactly at limit. **IOC**: never rests, releases
  cancelled remainder. **FOK**: all-or-nothing with **byte-identical rollback**
  when not fully fillable (`INV-FOK-02`) — check fillability *before* mutating;
  test the "one unit short" edge. **Market**: sweeps worsening prices, never
  rests, empty opposite book → zero fills + cancel. **Post-Only**: zero taker
  fills; rejected (no state change) if it would cross — test the exact-touch
  boundary. **Iceberg**: replenish queues at the back of the level (loses
  priority, `INV-ICE-03`). **Stop**: immediate-trigger-on-submit, one trade
  triggering many stops, a stop triggering another.
- Cross-cutting: `INV-MAT-01/02/04/05`, self-trade prevention policy, and
  determinism (`INV-MAT-08`).
- `match_bench_test.go` must stay **zero-alloc** (CI gate).
