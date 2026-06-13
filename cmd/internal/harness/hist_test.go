package harness

import "testing"

func TestHist_PercentilesAcrossTiers(t *testing.T) {
	h := NewHist()
	// 90 samples at 100ns (fine tier), 10 at 50ms (coarse tier).
	for i := 0; i < 90; i++ {
		h.Record(100)
	}
	for i := 0; i < 10; i++ {
		h.Record(50_000_000)
	}
	// p50 lands in the fine tier near 100ns (bucket midpoint = 100/10*10 + 5 = 105).
	if p50 := h.Pct(50); p50 < 100 || p50 > 110 {
		t.Errorf("p50 = %dns, want ~105ns (fine tier)", p50)
	}
	// p99 lands in the coarse tier near 50ms.
	if p99 := h.Pct(99); p99 < 49_000_000 || p99 > 51_000_000 {
		t.Errorf("p99 = %dns, want ~50ms (coarse tier)", p99)
	}
	if avg := h.Avg(); avg <= 100 {
		t.Errorf("avg = %dns, want pulled up by the coarse-tier tail", avg)
	}
}

func TestHist_EmptyIsZero(t *testing.T) {
	h := NewHist()
	if h.Pct(50) != 0 || h.Avg() != 0 || h.Max() != 0 {
		t.Fatalf("empty histogram should report zeros, got p50=%d avg=%d max=%d", h.Pct(50), h.Avg(), h.Max())
	}
}

func TestHist_SingleSample(t *testing.T) {
	h := NewHist()
	h.Record(2_500) // 2.5µs -> fine bucket midpoint 2505ns
	if p := h.Pct(50); p < 2_500 || p > 2_510 {
		t.Errorf("single-sample p50 = %dns, want ~2505ns", p)
	}
	if h.Max() != 2_500 {
		t.Errorf("max = %dns, want 2500", h.Max())
	}
}

func TestHist_OverflowPinsToMax(t *testing.T) {
	h := NewHist()
	h.Record(500_000_000) // 500ms, beyond the coarse range (200ms)
	if h.Max() != 500_000_000 {
		t.Errorf("max = %d, want 500000000", h.Max())
	}
	if p := h.Pct(99); p != 500_000_000 {
		t.Errorf("p99 beyond coarse range = %d, want max 500000000", p)
	}
}
