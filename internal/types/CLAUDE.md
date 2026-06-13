# internal/types

The engine's plain-old-data value types, fixed-point money math, and the WAL
codec. Everything here is **fixed-size and pointer-free** so values write
directly to the WAL and copy through SPSC rings without allocation.

## Files

- `types.go` — scalar IDs (`Price`, `Qty`, `AccountID`, `OrderID`, `MarketID`,
  `Seq`, …) and the wire structs: `Command` (external, journaled), `Fill`,
  `FundedOrder` (post-reservation envelope). Enums: `Side`, `OrderType`, `TIF`,
  `Flags`, `CmdType`.
- `money.go` — integer fixed-point arithmetic: `MulDiv` (128-bit intermediate
  via `math/bits`), `Notional`, `Fee`. The `ok` return is an overflow signal —
  callers MUST reject the operation when `ok == false`, never wrap.
- `codec.go` — `EncodeCommand`/`DecodeCommand`: stable little-endian byte
  layout. **The byte layout is a durability contract** — changing it breaks
  replay of existing WALs.

## Constraints

- Keep every wire struct fixed-size and pointer-free. No slices, maps, or
  pointers in `Command`/`Fill`/`FundedOrder`.
- Reservation rounds **up** (`roundUp=true`), settlement rounds **down** — see
  `Notional`/`Fee` doc comments. Don't flip these.
- `ClientReqID`/`ClientTsNanos` are correlation-only and MUST NOT affect engine
  logic or determinism.

## Testing (positive / negative / edge + invariant)

`money_test.go`, `codec_test.go` exist. When changing math or layout:
- **Positive**: representative in-range values produce exact expected results.
- **Negative**: negative inputs / `denom <= 0` return `ok == false`.
- **Edge**: values near `int64` max, products that overflow naïvely
  (`INV-ARI-01`), inexact divisions exercising both rounding directions
  (`INV-ARI-02/03`), `q == ^uint64(0)` ceil-overflow guard.
- Codec: round-trip `Decode(Encode(c)) == c` for every `CmdType`; truncated /
  garbage input decodes without panic.
