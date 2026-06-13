package types

import "math/bits"

// MulDiv computes a*b/denom using a 128-bit intermediate so the product never
// overflows before the division. a, b, and denom must be non-negative and
// denom must be positive. When roundUp is true and the division is inexact,
// the quotient is rounded up (toward +infinity); otherwise it is truncated
// (rounded down).
//
// ok is false when the quotient does not fit in a non-negative int64 — the
// caller must treat this as an overflow and reject the operation rather than
// silently wrapping.
func MulDiv(a, b, denom int64, roundUp bool) (result int64, ok bool) {
	if a < 0 || b < 0 || denom <= 0 {
		return 0, false
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	// bits.Div64 panics when hi >= denom (quotient would exceed 64 bits).
	if hi >= uint64(denom) {
		return 0, false
	}
	q, r := bits.Div64(hi, lo, uint64(denom))
	if roundUp && r != 0 {
		if q == ^uint64(0) {
			return 0, false
		}
		q++
	}
	const maxInt64 = uint64(1<<63 - 1)
	if q > maxInt64 {
		return 0, false
	}
	return int64(q), true
}

// Notional returns price*qty/qtyScale: the quote-currency value of qty units at
// price. Settlement rounds down (favoring the engine); reservation rounds up
// (so a buyer's reservation never under-covers the eventual fill).
func Notional(price Price, qty Qty, qtyScale int64, roundUp bool) (int64, bool) {
	return MulDiv(int64(price), int64(qty), qtyScale, roundUp)
}

// Fee returns notional*feeRate/feeScale. Reservation rounds the taker fee up so
// the reservation never under-covers; settlement uses the same direction the
// caller chooses for consistency.
func Fee(notional, feeRate, feeScale int64, roundUp bool) (int64, bool) {
	return MulDiv(notional, feeRate, feeScale, roundUp)
}
