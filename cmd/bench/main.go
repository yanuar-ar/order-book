// Command bench is the load harness for the spot engine. It seeds liquidity and
// drives a configurable command stream through the engine, reporting throughput
// and internal-latency percentiles.
//
// v1 measures the single-threaded engine's per-command processing latency and
// throughput. Coordinated-omission correction and an HdrHistogram backend (per
// the design's §18) belong with the concurrent shard-goroutine topology and are
// noted as future work; this harness establishes the measurement scaffold.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"sort"
	"time"

	"github.com/yanuar-ar/order-book/internal/balance"
	"github.com/yanuar-ar/order-book/internal/market"
	"github.com/yanuar-ar/order-book/internal/platform"
	"github.com/yanuar-ar/order-book/internal/types"
)

const (
	usdt types.AssetID = 2
	btc  types.AssetID = 1
	eth  types.AssetID = 3
	sol  types.AssetID = 4
)

var marketBase = map[types.MarketID]types.AssetID{0: btc, 1: eth, 2: sol}

// marketWeights matches the design's load model (BTC 50%, ETH 30%, SOL 20%).
var marketWeights = []types.MarketID{0, 0, 0, 0, 0, 1, 1, 1, 2, 2}

func main() {
	n := flag.Int("n", 200_000, "number of measured commands")
	warmup := flag.Int("warmup", 20_000, "warmup commands (discarded)")
	accounts := flag.Int("accounts", 1000, "account pool size")
	seedLiquidity := flag.Int("seed", 200, "resting orders seeded per side per market")
	rngSeed := flag.Int64("rngseed", 1, "RNG seed")
	flag.Parse()

	specs := map[types.MarketID]balance.MarketSpec{}
	for m, base := range marketBase {
		specs[m] = balance.MarketSpec{Base: base, Quote: usdt}
	}
	eng := market.NewEngine(market.Config{
		Markets: specs, QtyScale: 1, FeeScale: 100, MakerFee: 1, TakerFee: 2,
		RingSize: 1 << 16, CapHint: 1 << 16,
	})

	r := rand.New(rand.NewSource(*rngSeed))

	// Setup: fund accounts and seed liquidity. GC off for the session.
	prev := platform.GCOff()
	defer platform.GCOn(prev)

	for a := 1; a <= *accounts; a++ {
		eng.Submit(types.Command{Type: types.CmdDeposit, Account: types.AccountID(a), Asset: usdt, Amount: 1 << 40})
		for _, base := range marketBase {
			eng.Submit(types.Command{Type: types.CmdDeposit, Account: types.AccountID(a), Asset: base, Amount: 1 << 32})
		}
	}
	eng.Drain()

	var id types.OrderID = 1
	for m := range marketBase {
		for i := 0; i < *seedLiquidity; i++ {
			id++
			eng.Submit(types.Command{Type: types.CmdNewOrder, Market: m, Account: types.AccountID(1 + r.Intn(*accounts)), OrderID: id, Side: types.Buy, OrdType: types.Limit, Tif: types.GTC, Price: types.Price(90 + r.Intn(10)), Qty: types.Qty(1 + r.Intn(5))})
			id++
			eng.Submit(types.Command{Type: types.CmdNewOrder, Market: m, Account: types.AccountID(1 + r.Intn(*accounts)), OrderID: id, Side: types.Sell, OrdType: types.Limit, Tif: types.GTC, Price: types.Price(101 + r.Intn(10)), Qty: types.Qty(1 + r.Intn(5))})
		}
	}
	eng.Drain()

	// Measured run: process one command per Step, timing each.
	total := *warmup + *n
	lat := make([]int64, 0, *n)
	start := time.Now()
	for i := 0; i < total; i++ {
		id++
		c := genOrder(r, id)
		t0 := time.Now()
		eng.Submit(c)
		eng.Step()
		d := time.Since(t0).Nanoseconds()
		if i >= *warmup {
			lat = append(lat, d)
		}
	}
	elapsed := time.Since(start)
	eng.Drain()

	report(*n, elapsed, lat)
}

func genOrder(r *rand.Rand, id types.OrderID) types.Command {
	m := marketWeights[r.Intn(len(marketWeights))]
	side := types.Side(r.Intn(2))
	ot, tif := types.Limit, types.GTC
	switch n := r.Intn(100); {
	case n < 10:
		ot = types.Market
	case n < 15:
		tif = types.IOC
	case n < 20:
		tif = types.FOK
	}
	return types.Command{
		Type: types.CmdNewOrder, Market: m, Account: types.AccountID(1 + r.Intn(1000)),
		OrderID: id, Side: side, OrdType: ot, Tif: tif,
		Price: types.Price(95 + r.Intn(11)), Qty: types.Qty(1 + r.Intn(5)),
	}
}

func report(n int, elapsed time.Duration, lat []int64) {
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(p float64) int64 {
		if len(lat) == 0 {
			return 0
		}
		idx := int(p / 100 * float64(len(lat)))
		if idx >= len(lat) {
			idx = len(lat) - 1
		}
		return lat[idx]
	}
	throughput := float64(n) / elapsed.Seconds()
	fmt.Printf("commands measured : %d\n", n)
	fmt.Printf("elapsed           : %s\n", elapsed)
	fmt.Printf("throughput        : %.0f cmd/s\n", throughput)
	fmt.Printf("latency p50       : %d ns\n", pct(50))
	fmt.Printf("latency p99       : %d ns\n", pct(99))
	fmt.Printf("latency p99.9     : %d ns\n", pct(99.9))
	if len(lat) > 0 {
		fmt.Printf("latency max       : %d ns\n", lat[len(lat)-1])
	}
}
