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

// TestReservationNeverUnderCoversSettlement is the INV-ARI-02/03 relationship
// across a range of inexact inputs: for every (price, qty, rate), the
// reservation (notional+fee, both ceil) is >= the settlement (notional+fee,
// both floor). A buyer's lock can never under-cover the eventual debit.
func TestReservationNeverUnderCoversSettlement(t *testing.T) {
	const scale = int64(100_000_000)
	prices := []Price{1, 150_000_001, 333_333_333, 999_999_999}
	qtys := []Qty{1, 100_000_000, 250_000_001, 7_777_777}
	rates := []int64{0, 1, 10_000, 1_500_000} // 0, 1e-8, 0.0001, 0.015
	for _, p := range prices {
		for _, q := range qtys {
			nSettle, ok1 := Notional(p, q, scale, false)
			nReserve, ok2 := Notional(p, q, scale, true)
			if !ok1 || !ok2 {
				t.Fatalf("notional overflow p=%d q=%d", p, q)
			}
			if nReserve < nSettle {
				t.Fatalf("notional reserve %d < settle %d (p=%d q=%d)", nReserve, nSettle, p, q)
			}
			for _, rate := range rates {
				fSettle, _ := Fee(nSettle, rate, scale, false)
				fReserve, _ := Fee(nReserve, rate, scale, true)
				if nReserve+fReserve < nSettle+fSettle {
					t.Fatalf("reserve %d under-covers settle %d (p=%d q=%d rate=%d)",
						nReserve+fReserve, nSettle+fSettle, p, q, rate)
				}
			}
		}
	}
}

// TestNotionalOverflowNearInt64Max is the INV-ARI-01 edge: a price*qty whose
// scaled quotient still overflows int64 must report ok=false, never wrap.
func TestNotionalOverflowNearInt64Max(t *testing.T) {
	// price*qty/scale = MaxInt64*MaxInt64/1 -> quotient far exceeds int64.
	if v, ok := Notional(Price(math.MaxInt64), Qty(math.MaxInt64), 1, true); ok {
		t.Fatalf("expected overflow ok=false, got (%d,true)", v)
	}
	// A large-but-fitting case still succeeds (scale brings it back in range).
	if _, ok := Notional(Price(1<<60), Qty(1<<60), math.MaxInt64, false); !ok {
		t.Fatal("expected in-range result to succeed")
	}
}

// TestFeeZeroRateAndNegative covers the fee edges: a zero rate yields zero fee,
// and negative inputs are rejected (ok=false) rather than wrapping.
func TestFeeZeroRateAndNegative(t *testing.T) {
	if f, ok := Fee(1000, 0, 100_000_000, true); !ok || f != 0 {
		t.Fatalf("zero-rate fee = (%d,%v), want (0,true)", f, ok)
	}
	if _, ok := Fee(-1000, 1, 100_000_000, false); ok {
		t.Error("negative notional should be rejected")
	}
}
