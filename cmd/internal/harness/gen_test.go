package harness

import (
	"math/rand"
	"testing"

	"github.com/yanuar-ar/order-book/internal/orderbook"
	"github.com/yanuar-ar/order-book/internal/types"
)

func TestGenBaseMid_Deterministic(t *testing.T) {
	r1 := rand.New(rand.NewSource(42))
	r2 := rand.New(rand.NewSource(42))
	for id := types.OrderID(10000); id < 11000; id++ {
		if GenBaseMid(r1, id, 100) != GenBaseMid(r2, id, 100) {
			t.Fatalf("GenBaseMid diverged at id %d for the same seed", id)
		}
	}
}

func TestGenLiveMid_Deterministic(t *testing.T) {
	// Build + seed an engine once, capture its books, then generate against that
	// fixed book state with two same-seed RNGs (commands are not applied, so the
	// book stays constant and only RNG drives divergence).
	e, cleanup, err := BuildEngine("serial", nil, DefaultConfig())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer cleanup()
	Fund(e, 50)
	SeedBook(e, rand.New(rand.NewSource(1)), 50)

	var books [3]*orderbook.Book
	for m := types.MarketID(0); m < 3; m++ {
		books[m] = e.Shard(m).Book()
	}

	r1 := rand.New(rand.NewSource(7))
	r2 := rand.New(rand.NewSource(7))
	for id := types.OrderID(20000); id < 21000; id++ {
		if GenLiveMid(&books, r1, id, 50) != GenLiveMid(&books, r2, id, 50) {
			t.Fatalf("GenLiveMid diverged at id %d for the same seed", id)
		}
	}
}

func TestGenBaseMid_CancelUnderflowIsBenign(t *testing.T) {
	// Early ids generate cancels targeting id - (1..8000), which underflows
	// OrderID. That must produce a well-formed cancel command (a no-op
	// unknown-order cancel at runtime), never a panic.
	r := rand.New(rand.NewSource(3))
	for id := types.OrderID(1); id < 500; id++ {
		c := GenBaseMid(r, id, 10) // must not panic
		if c.Type == types.CmdCancel && c.Market > 2 {
			t.Fatalf("cancel targeted an invalid market %d", c.Market)
		}
	}
}

func TestGenBaseMid_OrderMixRatio(t *testing.T) {
	r := rand.New(rand.NewSource(99))
	const n = 100_000
	var cancels, takers, makers int
	for id := types.OrderID(10000); id < 10000+n; id++ {
		c := GenBaseMid(r, id, 100)
		switch {
		case c.Type == types.CmdCancel:
			cancels++
		case c.OrdType == types.Market || c.Tif == types.IOC:
			takers++
		default:
			makers++
		}
	}
	// Targets from the mix: cancel 8%, taker 18% (market 12% + IOC 6%), maker 74%.
	approx := func(label string, got, wantPct int) {
		want := n * wantPct / 100
		tol := n / 100 // ±1 percentage point
		if got < want-tol || got > want+tol {
			t.Errorf("%s = %d (%.1f%%), want ~%d%%", label, got, 100*float64(got)/n, wantPct)
		}
	}
	approx("cancels", cancels, 8)
	approx("takers", takers, 18)
	approx("makers", makers, 74)
}
