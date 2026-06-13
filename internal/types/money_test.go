package types

import (
	"math"
	"testing"
)

func TestMulDivExact(t *testing.T) {
	// 2 * 3e8 / 1e8 = 6
	got, ok := MulDiv(2, 300_000_000, 100_000_000, false)
	if !ok || got != 6 {
		t.Fatalf("MulDiv exact = (%d,%v), want (6,true)", got, ok)
	}
}

func TestMulDivRoundingDirection(t *testing.T) {
	// 1 * 1 / 2 = 0.5 -> down=0, up=1
	down, ok1 := MulDiv(1, 1, 2, false)
	up, ok2 := MulDiv(1, 1, 2, true)
	if !ok1 || !ok2 || down != 0 || up != 1 {
		t.Fatalf("rounding: down=(%d,%v) up=(%d,%v), want 0/1", down, ok1, up, ok2)
	}
}

func TestMulDivRoundUpExactDoesNotIncrement(t *testing.T) {
	// 4 / 2 = 2 exactly; round-up must not bump to 3.
	up, ok := MulDiv(4, 1, 2, true)
	if !ok || up != 2 {
		t.Fatalf("round-up exact = (%d,%v), want (2,true)", up, ok)
	}
}

func TestMulDiv128BitIntermediateNoOverflow(t *testing.T) {
	// a*b overflows int64 (each ~3.0e9 scaled) but a*b/scale fits.
	// 9e9 * 9e9 / 1e8 = 8.1e11, well within int64; the product 8.1e19
	// would overflow a naive int64 multiply.
	a := int64(9_000_000_000)
	b := int64(9_000_000_000)
	got, ok := MulDiv(a, b, 100_000_000, false)
	if !ok || got != 810_000_000_000 {
		t.Fatalf("128-bit path = (%d,%v), want (810000000000,true)", got, ok)
	}
}

func TestMulDivQuotientOverflowReportsNotOK(t *testing.T) {
	// price*qty/scale whose quotient exceeds int64 must report ok=false,
	// not panic (this is the bits.Div64 overflow guard).
	got, ok := MulDiv(math.MaxInt64, math.MaxInt64, 1, false)
	if ok {
		t.Fatalf("expected overflow ok=false, got (%d,true)", got)
	}
}

func TestMulDivRejectsNegativeAndZeroDenom(t *testing.T) {
	if _, ok := MulDiv(-1, 2, 100, false); ok {
		t.Error("negative a should be rejected")
	}
	if _, ok := MulDiv(2, 3, 0, false); ok {
		t.Error("zero denom should be rejected")
	}
}

func TestNotionalReservationNeverUnderReserves(t *testing.T) {
	// price=1.5 (1.5e8), qty=1.5 (1.5e8), scale=1e8 -> 2.25 -> 225000000.
	// Pick values that produce a remainder so reserve (up) > settle (down).
	price := Price(150_000_001) // 1.50000001
	qty := Qty(100_000_000)     // 1.0
	scale := int64(100_000_000)
	settle, ok1 := Notional(price, qty, scale, false)
	reserve, ok2 := Notional(price, qty, scale, true)
	if !ok1 || !ok2 {
		t.Fatalf("notional overflow: settle ok=%v reserve ok=%v", ok1, ok2)
	}
	if reserve < settle {
		t.Fatalf("reservation %d under-reserves vs settlement %d", reserve, settle)
	}
}

func TestFeeRoundsUpForReservation(t *testing.T) {
	// notional=1000, rate=1 (1/1e8), scale=1e8 -> 0.00001 -> down=0, up=1.
	down, _ := Fee(1000, 1, 100_000_000, false)
	up, _ := Fee(1000, 1, 100_000_000, true)
	if down != 0 || up != 1 {
		t.Fatalf("fee rounding: down=%d up=%d, want 0/1", down, up)
	}
}
