---
date: 2026-06-14
topic: cex-order-filters
---

# CEX-Style Per-Market Order Filters

## Summary

Add per-market order-validation filters matching a centralized exchange's
static symbol filters (Binance reference): price tick + min/max, lot step +
min/max (with a separate market-order lot filter), and min/max notional. Every
order is validated at submit time; violations are rejected with a specific
reason and zero state mutation. Filters are mandatory per market, and the book
is guaranteed never to hold a filter-invalid resting order.

## Problem Frame

The engine today accepts any price and quantity expressible at the global
`PriceScale` / `QtyScale`. There is no notion of a price tick, a lot step, a
minimum order size, or a minimum order value — `MarketSpec` carries only the
base and quote asset IDs, and `RejectReason` has no filter-related values. An
off-tick price, a dust-sized quantity, or a sub-cent-value order all rest
cleanly on the book.

For a regulated, money-handling spot exchange this is both a correctness and a
market-quality gap: dust and off-tick orders pollute the price ladder, every
real CEX rejects them, and clients built against CEX conventions expect the
same filter semantics here. The filters are the contract that keeps every
resting order well-formed.

## Key Decisions

- **Reject, never snap.** A violating order is rejected whole with a specific
  reason and no state mutation. The engine never rounds a price to the nearest
  tick or truncates a quantity to the nearest lot — prices and quantities have
  exactly one source of truth, and validation stays deterministic. This is the
  product identity, not a tunable.

- **Static filter set only (no dynamic parity).** v1 implements the static
  per-symbol filters — those computable from config plus deterministic engine
  state. Dynamic-reference filters (price bands, per-account order counts) are
  deferred (see Scope Boundaries).

- **Mandatory per-market config.** Every configured market must declare a
  complete filter block; startup config validation fails loudly if any value is
  missing or non-positive. There are no silently-unfiltered markets.

- **Separate market-order lot filter.** Market orders are validated against
  their own `MARKET_LOT_SIZE` (step / min / max qty), independent of the limit
  `LOT_SIZE`, matching Binance.

- **Market-order notional uses a last-trade reference price.** Because a market
  order carries no price, its min/max-notional check is evaluated against the
  market's last-trade price. This introduces a new deterministic per-market
  last-trade-price value. Before a market's first trade there is no reference,
  and the notional check is skipped (fail-open) — only the lot filter gates such
  orders.

- **The book is always filter-valid.** No accepted order, and no in-place amend,
  may leave a resting order that violates its market's filters. This becomes a
  checked invariant after every command.

- **Reject reasons are per filter group.** A violation yields a group-level
  reason — price, lot (with a distinct market-lot variant), or notional — so
  clients learn why an order was rejected, matching CEX error specificity,
  without exploding the enum into one value per individual rule.

## Requirements

### Filter set & configuration

- R1. Each market declares a static filter block with three groups: price
  (`tickSize`, `minPrice`, `maxPrice`), limit lot (`stepSize`, `minQty`,
  `maxQty`), market lot (`MARKET_LOT_SIZE`: `stepSize`, `minQty`, `maxQty`), and
  notional (`minNotional`, `maxNotional`).
- R2. The filter block is mandatory per market. Startup configuration validation
  rejects the configuration (engine does not start) if any market omits a value
  or supplies a non-positive `tickSize`/`stepSize`/`minQty`, an inverted bound
  (`minPrice > maxPrice`, `minQty > maxQty`, `minNotional > maxNotional`), or a
  `minPrice`/`minQty` that is not itself aligned to its `tickSize`/`stepSize`.
- R3. Filters are read once at startup and are immutable for the engine's
  lifetime (consistent with the existing config-is-startup-only rule).

### Validation behavior

- R4. Order price must satisfy `price % tickSize == 0` and
  `minPrice <= price <= maxPrice`.
- R5. Order quantity must satisfy `qty % stepSize == 0` and
  `minQty <= qty <= maxQty`, against the lot filter applicable to the order
  type (R8/R9).
- R6. Order value must satisfy `minNotional <= price * qty <= maxNotional`.
- R7. A filter violation rejects the order with a specific `RejectReason` and
  produces no state mutation — no reservation, no book insertion, no ledger
  change. Validation runs at submit time, before fund reservation.

### Order-type application

- R8. **Limit** orders are validated against the price filter (R4) on their
  limit price, the limit lot filter (R5), and notional (R6) on
  `limitPrice * qty`.
- R9. **Market** orders skip the price filter (no price), are validated against
  the market lot filter (R5), and against notional (R6) using the last-trade
  reference price (R12). When no reference price exists, the notional check is
  skipped.
- R10. **Stop-limit** orders are validated against the price filter on *both*
  the trigger price and the limit price, the limit lot filter, and notional on
  `limitPrice * qty`. **Stop-market** orders are validated against the price
  filter on the trigger price, the market lot filter, and notional via the
  last-trade reference (as in R9). Stop orders are validated once at submit time,
  not again at activation.
- R11. **Iceberg** orders validate the total quantity against the full lot filter
  (R5) and notional (R6). The display quantity must additionally be a multiple
  of `stepSize` and `>= minQty`, so every replenished visible slice is itself a
  valid lot.

### Reference price

- R12. The engine maintains a deterministic per-market last-trade price, updated
  on each trade, used solely as the reference for market-order notional checks
  (R9, R10). It is part of recoverable state and must survive snapshot/restore
  and replay.

### Amend

- R13. An amend that changes price or increases quantity re-submits through the
  new-order path and therefore inherits full filter validation (R4–R6, R8–R11).
- R14. An amend-down (in-place quantity reduction) re-validates the new quantity
  against the applicable lot filter (R5) and notional (R6); price is unchanged so
  the price filter is unaffected. If the reduced order would violate a filter,
  the amend is rejected and the order retains its prior quantity.

### Correctness harness

- R15. A new invariant — *every resting order satisfies its market's static
  filters* — is asserted after every command, alongside the existing `INV-*`
  set.
- R16. Determinism is preserved: filter evaluation is a pure function of the
  command, the static filter config, and the deterministic last-trade price — no
  wall-clock, no map-iteration order, no floating point. Rejected commands still
  receive a `Seq`, are journaled, and re-reject identically on replay.
- R17. The independent reference model mirrors the filter logic so differential
  tests cover it, and every fixed filter bug adds a permanent regression seed
  under `testdata/fuzz/`. Unit suites cover positive (valid orders accepted),
  negative (each filter violation rejected with no partial mutation), and edge
  (exact-boundary, off-by-one, first-trade-no-reference) cases.

## Acceptance Examples

- AE1. **Covers R4, R7.** Given BTC/USDT `tickSize = 0.01`, when a limit buy is
  submitted at `100.005`, then it is rejected for a price-filter violation and no
  reservation or book change occurs.
- AE2. **Covers R5, R7.** Given `stepSize = 0.001`, when a limit order with
  `qty = 0.0015` is submitted, then it is rejected for a lot-size violation.
- AE3. **Covers R6.** Given `minNotional = 10`, when a limit order priced `1.00`
  for `qty = 5` (`notional = 5`) is submitted, then it is rejected for a notional
  violation.
- AE4. **Covers R6 (boundary).** Given `minNotional = 10` and `maxQty = 100`,
  when an order with `notional` exactly `10` and `qty` exactly `100` is
  submitted and all other filters pass, then it is accepted.
- AE5. **Covers R9.** Given a market with no trades yet, when a market order with
  a valid market-lot quantity is submitted, then the notional check is skipped
  and the order proceeds.
- AE6. **Covers R9, R12.** Given a market whose last-trade price is `100` and
  `minNotional = 10`, when a market buy for `qty = 0.05` (`reference notional =
  5`) is submitted, then it is rejected for a notional violation.
- AE7. **Covers R10.** Given a stop-limit with an off-tick trigger price, when it
  is submitted, then it is rejected even if its limit price is on-tick.
- AE8. **Covers R11.** Given `minQty = 0.01`, when an iceberg with total
  `qty = 1.0` but `displayQty = 0.005` is submitted, then it is rejected because
  the display slice is below `minQty`.
- AE9. **Covers R14.** Given a resting order at price `100` qty `0.2` with
  `minNotional = 10`, when an amend-down to `qty = 0.05` (`notional = 5`) is
  submitted, then the amend is rejected and the order remains at qty `0.2`.
- AE10. **Covers R13.** Given a resting limit order, when an amend changes its
  price to an off-tick value, then the amend is rejected via the new-order
  validation path.

## Scope Boundaries

### Deferred for later

- `PERCENT_PRICE_BY_SIDE` dynamic price bands (reject limit prices too far from a
  reference price).
- `ICEBERG_PARTS` cap on the number of iceberg slices.
- `MAX_NUM_ORDERS` / `MAX_NUM_ALGO_ORDERS` per-account open-order count caps.
- Runtime-mutable filters (filters stay startup-only).

### Outside this feature's identity

- Snap / round-to-valid behavior. Reject-only is the chosen identity; adding a
  rounding mode would create a second source of truth for price and quantity.
- Any change to amend's time-priority semantics. Amend continues to use
  in-place reduce (keeps priority) and cancel-replace for price-change /
  qty-increase (loses priority); this feature only adds filter validation to
  those paths.

## Dependencies / Assumptions

- **Snapshot/restore extension.** The per-market last-trade price (R12) is new
  recoverable state and must be folded into the snapshot/restore + replay
  machinery that recently shipped, or market-order notional checks will diverge
  after recovery.
- **Reference-model parity.** The differential oracle must implement the same
  filter logic (R17); otherwise differential tests cannot cover the feature.
- **Config & test migration.** Because filters are mandatory (R2), every market
  currently in configuration and in the test suite must be updated to declare a
  full filter block, or the engine (and those tests) will fail to start.
- **Assumption — fail-open before first trade.** Skipping the market-order
  notional check when no last-trade reference exists (R9) is assumed acceptable;
  the filtered-out risk is a single below-min-notional market order on a market
  that has never traded.

## Outstanding Questions

### Deferred to planning

- Exact home and update point of the last-trade price (R12), and whether the
  reference should be last-trade vs. a mid/best-bid-ask fallback.
- Whether `maxNotional` should apply to market orders identically to
  `minNotional`, or be market-order-exempt.

## Sources / Research

- `internal/types/types.go` — `CmdType`, `Command` (carries `Price`,
  `StopPrice`, `Qty`, `DisplayQty`), and `RejectReason` (six values, no filter
  reasons) — the reject enum to extend.
- `internal/market/engine.go` — `Core.newOrder` reserves funds before
  `Submit` (the pre-reserve validation hook point, R7); `Core.amend` implements
  the in-place-reduce vs. cancel-replace split (R13/R14).
- `internal/orderbook/book.go` — `Book.AmendDown` reduces qty in place with no
  filter check today (R14 target).
- `internal/balance/ledger.go` — `MarketSpec` holds only `Base`/`Quote`; the
  per-market filter block (R1) extends per-market metadata.
- `pkg/config/config.go` — global `PriceScale`/`QtyScale`; `Markets []string`
  is where mandatory per-market filter config (R1/R2) attaches.
- `docs/designs/invariant-fuzz-testing-guide.md` — invariant + differential +
  fuzz contract the new `INV` (R15) and harness work (R17) plug into.
