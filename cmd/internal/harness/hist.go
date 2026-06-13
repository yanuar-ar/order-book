package harness

import "math"

// Bounded-memory two-tier latency histogram. A fine tier (10ns buckets up to
// 300µs) gives sub-µs resolution for the body of the distribution; a coarse tier
// (10µs buckets up to 200ms) captures the tail so high percentiles report real
// values instead of pinning to max. Both tiers are small, so percentile scans
// stay cheap on the hot loop.

const (
	fineWidth   = int64(10)      // 10ns
	fineMax     = int64(300_000) // 300µs
	fineN       = int(fineMax / fineWidth)
	coarseWidth = int64(10_000) // 10µs
	coarseN     = 20_000        // up to 200ms
)

// Hist is the two-tier latency histogram. The zero value is not usable; build it
// with NewHist.
type Hist struct {
	fine   []uint64
	coarse []uint64
	over   uint64
	count  uint64
	sum    uint64
	max    uint64
}

// NewHist allocates a ready-to-use histogram.
func NewHist() *Hist {
	return &Hist{fine: make([]uint64, fineN), coarse: make([]uint64, coarseN)}
}

// Record adds one latency sample in nanoseconds.
func (h *Hist) Record(ns int64) {
	if ns < 0 {
		ns = 0
	}
	h.count++
	h.sum += uint64(ns)
	if uint64(ns) > h.max {
		h.max = uint64(ns)
	}
	if ns < fineMax {
		h.fine[ns/fineWidth]++
		return
	}
	if idx := ns / coarseWidth; int(idx) < coarseN {
		h.coarse[idx]++
		return
	}
	h.over++
}

// Avg returns the mean sample in nanoseconds, or 0 when empty.
func (h *Hist) Avg() int64 {
	if h.count == 0 {
		return 0
	}
	return int64(h.sum / h.count)
}

// Max returns the largest recorded sample in nanoseconds.
func (h *Hist) Max() int64 { return int64(h.max) }

// Pct returns the p-th percentile (p in [0,100]) in nanoseconds, or 0 when
// empty. Values beyond the coarse range report max. Uses the nearest-rank method
// (1-based ceil) so a low percentile on a small sample resolves to a populated
// bucket rather than the first empty one.
func (h *Hist) Pct(p float64) int64 {
	if h.count == 0 {
		return 0
	}
	target := uint64(math.Ceil(p / 100 * float64(h.count)))
	if target == 0 {
		target = 1
	}
	var cum uint64
	for i, c := range h.fine {
		cum += c
		if cum >= target {
			return int64(i)*fineWidth + fineWidth/2
		}
	}
	for i, c := range h.coarse {
		cum += c
		if cum >= target {
			return int64(i)*coarseWidth + coarseWidth/2
		}
	}
	return int64(h.max)
}
