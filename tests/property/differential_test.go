package property

import (
	"testing"

	"github.com/yanuar-ar/order-book/internal/types"
)

// TestDifferentialBroad runs the broad uniform generator against the reference
// model over several seeds: engine and model must agree on canonical state and
// CheckAllInvariants after every command.
func TestDifferentialBroad(t *testing.T) {
	for _, seed := range []int64{1, 2, 3, 20260613} {
		seed := seed
		t.Run("seed="+itoa(seed), func(t *testing.T) {
			if err := RunDifferential(GenBroad(seed, 1500)); err != nil {
				t.Fatalf("seed=%d: %v", seed, err)
			}
		})
	}
}

// TestDifferentialSharp runs the adversarial-biased generator (dense crossing
// prices, tight balances, icebergs, stops, frequent cancel/amend) over several
// seeds — the highest bug-finding pressure on the engine/oracle pair.
func TestDifferentialSharp(t *testing.T) {
	for _, seed := range []int64{1, 2, 3, 42, 99} {
		seed := seed
		t.Run("seed="+itoa(seed), func(t *testing.T) {
			if err := RunDifferential(GenSharp(seed, 1500)); err != nil {
				t.Fatalf("seed=%d: %v", seed, err)
			}
		})
	}
}

// TestDifferentialParallel runs the full sharp/broad streams through the
// ParallelEngine (matching offloaded to worker goroutines) and asserts it
// matches the reference model and holds every invariant — covering all eight
// order types, stops, icebergs, and cancel/amend on the concurrent topology,
// which the serial-only differential and the thin internal parallel test miss.
func TestDifferentialParallel(t *testing.T) {
	cases := []struct {
		name   string
		groups [][]types.MarketID
	}{
		{"isolated", [][]types.MarketID{{0}, {1}, {2}}},
		{"shared", [][]types.MarketID{{0}, {1, 2}}},
		{"default", nil},
	}
	for _, gc := range cases {
		gc := gc
		t.Run(gc.name, func(t *testing.T) {
			if err := RunDifferentialParallel(GenSharp(3, 1200), gc.groups); err != nil {
				t.Fatalf("sharp/%s: %v", gc.name, err)
			}
			if err := RunDifferentialParallel(GenBroad(3, 1200), gc.groups); err != nil {
				t.Fatalf("broad/%s: %v", gc.name, err)
			}
		})
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
