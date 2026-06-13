# internal/balance

The shared **balance authority**: a single-writer ledger of available/reserved
funds per `(account, asset)`, with maker/taker fees credited to a per-asset fee
account. This is the most money-critical package in the engine.

## Files

- `ledger.go` — `Ledger`, `Balance{Available, Reserved}`, `Config` (scales, fee
  rates, market specs), `reservation` lifecycle.
- `verify.go` — `Ledger.Verify()` (INV-BAL-01 + reservation-consistency, the
  ledger half of INV-BAL-03) and `ReservedOrders()`, which `tests/property` uses
  to cross-check the reserved-order set against open resting orders + stops.
- `event.go` — `BalanceEvent`: the single tagged event stream the ledger
  consumes so reservations (in `Seq` order) and settlements (in fill order)
  interleave in one fixed, deterministic order.
- `dump.go` — deterministic ledger dump for snapshots / invariant checks.
- `snapshot.go` — `EncodeSnapshot` / `Restore`: serialize `bal`+`fees`+`res` and
  rebuild directly. Reservations are serialized **verbatim by key-set** (incl.
  `remaining == 0`, or a settled-but-unreleased order is orphaned); restore sets
  the maps without `Reserve` and reads amounts as opaque int64s (no re-rounding).
- `ledger_test.go`, `ledger_bench_test.go`.

## Constraints

- **Single writer.** Funds are mutated only through the event stream; never
  expose concurrent mutation.
- Reservation rounds **up** (never under-covers the eventual fill); settlement
  rounds **down**. Leftover reservation is released on completion/cancel —
  exactly, no more, no less.
- A buy limit locks quote (`quote(limitPrice, remaining)`); a sell locks base
  (`remaining`). Withdraw can only draw from `available`, never `reserved`.
- Value must be conserved per asset: matching never creates or destroys units;
  rounding residue must land in an accounted place (dust/fee account).

## Testing (positive / negative / edge + invariant)

This package owns the highest-priority invariants (guide Appendix). Assert after
every event:
- `INV-BAL-04` per-asset conservation: `Σ(available+reserved) + feeAccount ==
  netDeposits`. `INV-BAL-03` `reserved == Σ locked(open orders)`.
- `INV-BAL-01` non-negative. `INV-BAL-02` cannot reserve beyond available
  (test: exact-fit, one-unit-short, two orders summing over balance).
- `INV-BAL-06` settled ≤ reserved. `INV-BAL-07` release-on-cancel exact.
  `INV-BAL-08` withdraw ≤ available. `INV-BAL-09` no cross-market double-spend
  (same account, buys in several markets exceeding the quote balance).
- **Negative/edge**: reject over-reservation with no mutation; fee rounding /
  dust; fee conservation (`INV-BAL-10`).
- `ledger_bench_test.go` must stay **zero-alloc** (CI gate).
