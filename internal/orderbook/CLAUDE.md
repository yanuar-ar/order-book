# internal/orderbook

A per-market limit order book. Orders live in a **preallocated arena** addressed
by `uint32` indices (no pointers in hot data), linked into **intrusive FIFO
lists per price level**. Cancelled slots recycle through a free-list.

## Files

- `book.go` — `Book`, `orderNode`, `level`, arena + free-list, price-level map
  with a per-side sorted price slice for best-price traversal, `AmendDown`.
- `consume.go` — quantity consumption / fill application against levels.
- `match_support.go` — helpers the matcher calls into (best price, level walk).
- `dump.go` — deterministic state dump for snapshots/invariant checks.
- `snapshot.go` — `EncodeSnapshot` / `RestoreSnapshot` and `InsertRestored`.
  Carries all four quantity fields (`remaining`/`display`/`hidden`/`peak`) plus
  `lastPrice`: a mid-refill iceberg cannot round-trip through `Insert` (which
  derives `peak = display`), so restore sets the fields directly. Re-resting in
  `Dump` (FIFO) order reproduces time priority.

`NilIdx (0xFFFFFFFF)` marks an absent slot (list terminator / empty level).

## Constraints

- Pointer-free hot data: address orders by arena index, never by pointer.
- The bounded tick-ladder optimization is **deferred** (current best-price path
  uses a sorted slice + map) — don't assume O(1) ladder access.
- `AmendDown` reduces quantity **in place, keeping FIFO priority**; any size
  increase or price change is cancel+reinsert semantics (loses priority).
- Iceberg orders track `display` (visible) vs `hidden`; `level.totalQty` counts
  **display only** — market data must never reveal hidden quantity.

## Testing (positive / negative / edge + invariant)

`Book.Verify()` (`verify.go`) implements the structural invariants below and
returns the first violation with context; `tests/property` composes it into
`CheckAllInvariants` and runs it after every command. `verify_test.go` exercises
it positive/negative/edge by injecting one corruption per invariant.

`book_test.go`, `dump_test.go` exist. Assert the structural invariants
(`docs/designs/invariant-fuzz-testing-guide.md §3.C`) after every mutation:
- `INV-OB-03` FIFO list integrity (head.prev/tail.next == Nil, links reciprocal,
  no cycles, traversal length == count).
- `INV-OB-04` `level.totalQty == Σ display`.
- `INV-OB-05` arena/free-list: every slot reachable-from-a-level XOR on the
  free-list — **no leaked slots over long runs**.
- `INV-OB-06` id index == exactly the open orders. `INV-OB-09` `0 < remaining ≤ original`.
- **Negative/edge**: cancel of unknown/already-removed id is a no-op
  (`INV-CXL-03`); amend below filled qty; thousands of same-price orders
  (FIFO stress); empty-book operations.
