// Command shardbench measures parallel shard-matching throughput with a
// configurable market-to-core assignment. Each worker runs in its own
// goroutine pinned to a core and matches a realistic order flow against the
// books for its assigned markets, independently of the other workers.
//
// This isolates the parallelizable hot path (matching). The balance authority
// is a separate single-writer stage (~2.5M reserve+settle/s on one core) and is
// not exercised here; it is not the bottleneck at 1M TPS. Use -cores to place
// markets: e.g. "0;1,2" puts BTC (market 0) alone on worker 0 and ETH+SOL
// (markets 1,2) together on worker 1.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yanuar-ar/order-book/internal/matching"
	"github.com/yanuar-ar/order-book/internal/orderbook"
	"github.com/yanuar-ar/order-book/internal/platform"
	"github.com/yanuar-ar/order-book/internal/types"
)

// Real-world mids in cents (1 tick = $0.01); qty in 1e-8 base units.
var marketMid = map[types.MarketID]types.Price{0: 10_800_000, 1: 400_000, 2: 20_000}
var marketName = map[types.MarketID]string{0: "BTC/USDT", 1: "ETH/USDT", 2: "SOL/USDT"}

const (
	qtyDiv       = 100_000_000
	offsetMean   = 50
	offsetCapTk  = 2000
	seedDepthTks = 300
	latSample    = 512 // sample 1 in N ops for latency
)

type result struct {
	worker  int
	markets []types.MarketID
	ops     int64
	elapsed time.Duration
	samples []time.Duration // sampled latencies
}

func main() {
	cores := flag.String("cores", "0;1,2", "market->worker map; ';' separates workers, ',' separates markets")
	dur := flag.Duration("duration", 10*time.Second, "run duration")
	users := flag.Int("users", 100, "account pool size")
	flag.Parse()

	groups := parseCores(*cores)
	if len(groups) == 0 {
		fmt.Println("no workers parsed from -cores")
		return
	}
	if g := runtime.GOMAXPROCS(0); g < len(groups)+1 {
		runtime.GOMAXPROCS(len(groups) + 1)
	}

	fmt.Printf("running %d workers for %s (GOMAXPROCS=%d)\n", len(groups), *dur, runtime.GOMAXPROCS(0))
	for i, g := range groups {
		names := make([]string, len(g))
		for j, m := range g {
			names[j] = marketName[m]
		}
		fmt.Printf("  worker %d (core %d): %s\n", i, i, strings.Join(names, ", "))
	}

	results := make([]result, len(groups))
	var wg sync.WaitGroup
	for i, g := range groups {
		wg.Add(1)
		go func(idx int, markets []types.MarketID) {
			defer wg.Done()
			results[idx] = runWorker(idx, markets, *dur, *users)
		}(i, g)
	}
	wg.Wait()

	report(results)
}

// runWorker pins to a core and matches as fast as possible (closed-loop) so the
// measured throughput is the worker's sustainable matching ceiling.
func runWorker(idx int, markets []types.MarketID, dur time.Duration, users int) result {
	_ = platform.PinCurrentThread(idx)
	defer platform.Unpin()

	r := rand.New(rand.NewSource(int64(idx) + 1))
	engines := make(map[types.MarketID]*matching.Engine, len(markets))
	for _, m := range markets {
		e := matching.NewEngine(orderbook.New(m, 1<<20), noopSink{}, qtyDiv)
		seedBook(e, m, r, users)
		engines[m] = e
	}

	res := result{worker: idx, markets: markets}
	var id types.OrderID = 1 << 40 // above seeded ids
	deadline := time.Now().Add(dur)
	start := time.Now()
	var ops int64
	for {
		// Process a batch between time checks to keep the clock off the hot path.
		for k := 0; k < 4096; k++ {
			id++
			m := markets[int(id)%len(markets)]
			c := genFunded(r, id, m)
			if ops%latSample == 0 {
				t0 := time.Now()
				engines[m].Submit(c)
				res.samples = append(res.samples, time.Since(t0))
			} else {
				engines[m].Submit(c)
			}
			ops++
		}
		if time.Now().After(deadline) {
			break
		}
	}
	res.elapsed = time.Since(start)
	res.ops = ops
	return res
}

func seedBook(e *matching.Engine, m types.MarketID, r *rand.Rand, users int) {
	mid := marketMid[m]
	var id types.OrderID = 1
	for off := types.Price(1); off <= seedDepthTks; off++ {
		for k := 0; k < 2; k++ {
			id++
			e.Submit(funded(m, acct(r, users), id, types.Buy, types.Limit, types.GTC, mid-off, genQty(r)))
			id++
			e.Submit(funded(m, acct(r, users), id, types.Sell, types.Limit, types.GTC, mid+off, genQty(r)))
		}
	}
}

func genFunded(r *rand.Rand, id types.OrderID, m types.MarketID) types.FundedOrder {
	a := acct(r, 100)
	side := types.Side(r.Intn(2))
	qty := genQty(r)
	mid := midPrice(nil, marketMid[m]) // mid drift handled by book; base is fine for gen
	switch n := r.Intn(100); {
	case n < 12: // taker: market sweep
		return funded(m, a, id, side, types.Market, types.GTC, 0, qty)
	case n < 18: // taker: crossing IOC
		px := mid + types.Price(2+r.Intn(5))
		if side == types.Sell {
			px = mid - types.Price(2+r.Intn(5))
		}
		return funded(m, a, id, side, types.Limit, types.IOC, px, qty)
	default: // maker: rest near touch
		off := makerOffset(r)
		px := mid - off
		if side == types.Sell {
			px = mid + off
		}
		if px < 1 {
			px = 1
		}
		return funded(m, a, id, side, types.Limit, types.GTC, px, qty)
	}
}

func funded(m types.MarketID, a types.AccountID, id types.OrderID, side types.Side, typ types.OrderType, tif types.TIF, price types.Price, qty types.Qty) types.FundedOrder {
	return types.FundedOrder{Seq: types.Seq(id), Market: m, Account: a, OrderID: id, Side: side, OrdType: typ, Tif: tif, Price: price, Qty: qty}
}

func acct(r *rand.Rand, users int) types.AccountID { return types.AccountID(1 + r.Intn(users)) }
func genQty(r *rand.Rand) types.Qty                { return types.Qty((1 + r.Intn(2000)) * 100_000) }
func makerOffset(r *rand.Rand) types.Price {
	off := 1 + int(r.ExpFloat64()*offsetMean)
	if off > offsetCapTk {
		off = offsetCapTk
	}
	return types.Price(off)
}

// midPrice falls back to base (the generator does not read the live book to
// keep the hot path branch-light; matching still moves the book itself).
func midPrice(_ *orderbook.Book, base types.Price) types.Price { return base }

type noopSink struct{}

func (noopSink) Emit(types.Command) {}

func report(results []result) {
	fmt.Printf("\n%-22s %14s %12s %10s %10s %10s\n", "worker / markets", "throughput", "p50", "p95", "p99", "max")
	var agg float64
	for _, res := range results {
		names := make([]string, len(res.markets))
		for j, m := range res.markets {
			names[j] = marketName[m]
		}
		tp := float64(res.ops) / res.elapsed.Seconds()
		agg += tp
		p50, p95, p99, mx := pcts(res.samples)
		label := fmt.Sprintf("w%d %s", res.worker, strings.Join(names, "+"))
		fmt.Printf("%-22s %12.0f/s %12s %10s %10s %10s\n", label, tp, dur(p50), dur(p95), dur(p99), dur(mx))
	}
	fmt.Printf("%-22s %12.0f/s\n", "AGGREGATE", agg)
}

func pcts(s []time.Duration) (p50, p95, p99, max time.Duration) {
	if len(s) == 0 {
		return
	}
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	at := func(p float64) time.Duration {
		i := int(p / 100 * float64(len(s)))
		if i >= len(s) {
			i = len(s) - 1
		}
		return s[i]
	}
	return at(50), at(95), at(99), s[len(s)-1]
}

func dur(d time.Duration) string {
	ns := d.Nanoseconds()
	switch {
	case ns >= 1_000_000:
		return fmt.Sprintf("%.2fms", float64(ns)/1e6)
	case ns >= 1_000:
		return fmt.Sprintf("%.2fµs", float64(ns)/1e3)
	default:
		return fmt.Sprintf("%dns", ns)
	}
}

func parseCores(s string) [][]types.MarketID {
	var groups [][]types.MarketID
	for _, grp := range strings.Split(s, ";") {
		grp = strings.TrimSpace(grp)
		if grp == "" {
			continue
		}
		var markets []types.MarketID
		for _, tok := range strings.Split(grp, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(tok))
			if err == nil {
				markets = append(markets, types.MarketID(n))
			}
		}
		if len(markets) > 0 {
			groups = append(groups, markets)
		}
	}
	return groups
}
