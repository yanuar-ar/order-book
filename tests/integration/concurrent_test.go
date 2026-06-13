package integration

import (
	"math/rand"
	"sync/atomic"
	"testing"

	"github.com/yanuar-ar/order-book/internal/market"
	"github.com/yanuar-ar/order-book/internal/types"
)

// buildStream deterministically produces a deposit prelude + random orders and
// the per-asset deposited totals, for determinism/invariant checks.
func buildStream(seed int64, n int) (deposits, orders []types.Command, deposited map[types.AssetID]int64) {
	r := rand.New(rand.NewSource(seed))
	deposited = map[types.AssetID]int64{}
	bases := map[types.MarketID]types.AssetID{mBTC: btc, mETH: eth, mSOL: sol}
	for a := types.AccountID(1); a <= 6; a++ {
		deposits = append(deposits, dep(a, usdt, 1_000_000))
		deposited[usdt] += 1_000_000
		for _, b := range bases {
			deposits = append(deposits, dep(a, b, 100_000))
			deposited[b] += 100_000
		}
	}
	mkts := []types.MarketID{mBTC, mETH, mSOL}
	var id types.OrderID = 1000
	for i := 0; i < n; i++ {
		id++
		m := mkts[r.Intn(len(mkts))]
		side := types.Side(r.Intn(2))
		typ, tif := types.Limit, types.GTC
		switch x := r.Intn(100); {
		case x < 12:
			typ = types.Market
		case x < 18:
			tif = types.IOC
		}
		orders = append(orders, ord(m, types.AccountID(1+r.Intn(6)), id, side, typ, tif, types.Price(95+r.Intn(11)), types.Qty(1+r.Intn(5))))
	}
	return deposits, orders, deposited
}

// TestConcurrentIngestionMatchesSerial feeds the same stream two ways: serially
// (Submit+Drain on one goroutine) and concurrently (a producer goroutine
// submitting while a separate engine goroutine steps). The final state must be
// identical, proving ingestion concurrency doesn't break determinism — and the
// global invariants must hold. Run under -race to catch data races on the ring.
func TestConcurrentIngestionMatchesSerial(t *testing.T) {
	deposits, orders, deposited := buildStream(7, 4000)
	all := append(append([]types.Command{}, deposits...), orders...)

	// Serial reference.
	serial := market.NewEngine(cfg(1, 2))
	run(serial, all...)

	// Concurrent: producer goroutine submits; this goroutine steps the engine.
	conc := market.NewEngine(cfg(1, 2))
	var producedAll atomic.Bool
	go func() {
		for _, c := range all {
			for !conc.Submit(c) {
				conc_spin()
			}
		}
		producedAll.Store(true)
	}()
	for {
		worked := conc.Step()
		if producedAll.Load() && !worked && !conc.Step() {
			break
		}
	}
	conc.Drain()

	if digest(serial) != digest(conc) {
		t.Fatalf("concurrent ingestion diverged from serial:\n--- serial ---\n%s\n--- concurrent ---\n%s", digest(serial), digest(conc))
	}
	checkInvariants(t, conc, deposited)
}

// conc_spin is a tiny backoff hook (kept trivial; the ring is large enough that
// backpressure is rare in this test).
func conc_spin() {}
