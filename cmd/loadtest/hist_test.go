package main

import (
	"testing"

	"github.com/yanuar-ar/order-book/internal/types"
)

// ---- histogram: positive ----

func TestHistPercentilesAndAvg(t *testing.T) {
	h := newHist()
	for i := 0; i < 1000; i++ {
		h.record(1000) // 1µs each
	}
	if h.avg() != 1000 {
		t.Fatalf("avg = %d, want 1000", h.avg())
	}
	// 1000ns falls in fine bucket 100 -> representative 100*10+5 = 1005.
	if p := h.pct(50); p < 1000 || p > 1010 {
		t.Fatalf("p50 = %d, want ~1005", p)
	}
}

func TestHistMonotonicPercentiles(t *testing.T) {
	h := newHist()
	for i := int64(1); i <= 10000; i++ {
		h.record(i * 10) // spread 10ns..100µs
	}
	p50, p95, p99, mx := h.pct(50), h.pct(95), h.pct(99), int64(h.max)
	if !(p50 <= p95 && p95 <= p99 && p99 <= mx) {
		t.Fatalf("percentiles not monotonic: p50=%d p95=%d p99=%d max=%d", p50, p95, p99, mx)
	}
}

// ---- histogram: edge ----

func TestHistEmpty(t *testing.T) {
	h := newHist()
	if h.avg() != 0 || h.pct(50) != 0 {
		t.Fatalf("empty hist avg/pct = %d/%d, want 0/0", h.avg(), h.pct(50))
	}
}

func TestHistNegativeClampedToZero(t *testing.T) {
	h := newHist()
	h.record(-5)
	if h.max != 0 {
		t.Fatalf("negative recorded as max %d, want clamped to 0", h.max)
	}
}

func TestHistCoarseTierAndOverflow(t *testing.T) {
	h := newHist()
	h.record(500_000)     // 500µs -> coarse tier
	h.record(500_000_000) // 500ms -> beyond coarse -> overflow, counted in max
	if h.over != 1 {
		t.Fatalf("overflow count = %d, want 1", h.over)
	}
	if int64(h.max) != 500_000_000 {
		t.Fatalf("max = %d, want 500000000", h.max)
	}
	// p99 of two samples lands at/above the coarse sample; must not be 0.
	if h.pct(99) <= 0 {
		t.Fatalf("p99 = %d, want > 0", h.pct(99))
	}
}

func TestHistFineCoarseBoundary(t *testing.T) {
	h := newHist()
	h.record(fineMax - 1) // last fine bucket
	h.record(fineMax)     // first coarse bucket
	if h.over != 0 {
		t.Fatalf("boundary values overflowed: over=%d, want 0", h.over)
	}
}

// ---- formatting ----

func TestPriceQtyFormatting(t *testing.T) {
	if got := priceStr(10_800_000); got != "108000.00" {
		t.Fatalf("priceStr = %q, want 108000.00", got)
	}
	if got := qtyStr(types.Qty(150_000_000)); got != "1.5000" {
		t.Fatalf("qtyStr = %q, want 1.5000", got)
	}
}
