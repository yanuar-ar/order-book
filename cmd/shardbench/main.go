// Command shardbench drives the full parallel engine (sequencer + single-writer
// balance authority + per-worker matching goroutines) at maximum throughput
// with a configurable market-to-core assignment, while rendering a live
// trading-terminal view of one market's order book — bids, asks, depth, last
// price — plus throughput and latency statistics.
//
// Unlike the matching-only microbenchmark it replaces, this exercises the whole
// engine: every command is sequenced, reserved against the shared ledger, and
// settled, with only matching offloaded to the worker cores. So the throughput
// reported here is the honest end-to-end parallel ceiling — bounded by the
// serial control path (the balance authority), not by matching.
//
// Use -cores to place markets on workers: e.g. "0;1,2" puts BTC (market 0)
// alone on worker 0 (core 0) and ETH+SOL (markets 1,2) together on worker 1
// (core 1). Reads of the book for the live view happen between control steps,
// when the workers are idle, so they never race the matchers.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yanuar-ar/order-book/internal/balance"
	"github.com/yanuar-ar/order-book/internal/market"
	"github.com/yanuar-ar/order-book/internal/orderbook"
	"github.com/yanuar-ar/order-book/internal/types"
)

const (
	usdt types.AssetID = 2
	btc  types.AssetID = 1
	eth  types.AssetID = 3
	sol  types.AssetID = 4
)

var marketBase = map[types.MarketID]types.AssetID{0: btc, 1: eth, 2: sol}
var marketName = map[types.MarketID]string{0: "BTC/USDT", 1: "ETH/USDT", 2: "SOL/USDT"}

const (
	priceDiv     = 100.0       // cents -> dollars for display
	qtyDiv       = 100_000_000 // 1e-8 base units -> whole base coin for display
	offsetMean   = 50          // mean maker distance from mid, in ticks ($0.50)
	offsetCapTk  = 2000        // cap maker distance at $20.00
	seedDepthTks = 300         // initial ladder spans ±$3.00 around mid
	latSample    = 256         // sample 1 in N ops for latency (keep the clock off the hot path)
)

// Real-world starting mids (in cents): BTC ~ $108,000, ETH ~ $4,000, SOL ~ $200.
var marketMid = map[types.MarketID]types.Price{0: 10_800_000, 1: 400_000, 2: 20_000}

func main() {
	cores := flag.String("cores", "0;1,2", "market->worker map; ';' separates workers, ',' separates markets")
	dur := flag.Duration("duration", 10*time.Second, "run duration")
	users := flag.Int("users", 100, "account pool size")
	view := flag.Int("market", 0, "market id to display (0=BTC,1=ETH,2=SOL)")
	levels := flag.Int("levels", 16, "order-book depth levels to show per side")
	flag.Parse()

	groups := parseCores(*cores)
	if len(groups) == 0 {
		fmt.Println("no workers parsed from -cores")
		return
	}
	// One core per worker plus the control goroutine.
	if g := runtime.GOMAXPROCS(0); g < len(groups)+1 {
		runtime.GOMAXPROCS(len(groups) + 1)
	}

	specs := map[types.MarketID]balance.MarketSpec{}
	for m, base := range marketBase {
		specs[m] = balance.MarketSpec{Base: base, Quote: usdt}
	}
	eng := market.NewParallelEngine(market.Config{
		Markets: specs, QtyScale: qtyDiv, FeeScale: 100, MakerFee: 1, TakerFee: 2,
		RingSize: 1 << 16, CapHint: 1 << 20,
	}, groups)
	defer eng.Close()

	r := rand.New(rand.NewSource(1))

	// Fund the user pool generously (USDT in cents, base in 1e-8 units).
	for a := 1; a <= *users; a++ {
		eng.Submit(types.Command{Type: types.CmdDeposit, Account: types.AccountID(a), Asset: usdt, Amount: 1 << 54})
		for _, base := range marketBase {
			eng.Submit(types.Command{Type: types.CmdDeposit, Account: types.AccountID(a), Asset: base, Amount: 1 << 50})
		}
	}
	eng.Drain()
	// Seed a deep ladder around each market's mid so the book looks real at start.
	var id types.OrderID = 1
	for m, mid := range marketMid {
		for off := types.Price(1); off <= seedDepthTks; off++ {
			for k := 0; k < 2; k++ {
				id++
				eng.Submit(order(m, acct(r, *users), id, types.Buy, types.Limit, types.GTC, mid-off, genQty(r)))
				id++
				eng.Submit(order(m, acct(r, *users), id, types.Sell, types.Limit, types.GTC, mid+off, genQty(r)))
			}
		}
	}
	eng.Drain()

	h := newHist()
	var framePtr atomic.Pointer[frame]
	stop := make(chan struct{})
	go displayLoop(&framePtr, stop, groups)

	start := time.Now()
	deadline := start.Add(*dur)
	frameEvery := 100 * time.Millisecond
	nextFrame := start.Add(frameEvery)
	var ops, backpressure int64

	// Closed-loop: push as fast as the control path accepts. A batch between clock
	// reads keeps time.Now off the hot path; latency is sampled 1-in-latSample.
	for {
		for k := 0; k < 1024; k++ {
			id++
			c := genOrder(eng, r, id, *users) // reads the live book between steps (workers idle)
			if ops%latSample == 0 {
				t0 := time.Now()
				if !eng.Submit(c) {
					backpressure++
				}
				eng.Step()
				h.record(time.Since(t0).Nanoseconds())
			} else {
				if !eng.Submit(c) {
					backpressure++
				}
				eng.Step()
			}
			ops++
		}
		now := time.Now()
		if now.After(nextFrame) {
			framePtr.Store(buildFrame(eng, types.MarketID(*view), *levels, h, ops, now.Sub(start), backpressure))
			nextFrame = now.Add(frameEvery)
		}
		if now.After(deadline) {
			break
		}
	}
	eng.Drain()
	close(stop)
	time.Sleep(120 * time.Millisecond)

	final := buildFrame(eng, types.MarketID(*view), *levels, h, ops, time.Since(start), backpressure)
	render(final, groups)
	printSummary(final, groups)
}

// genOrder builds a full command relative to the live mid: mostly makers that
// rest away from the touch and deepen the ladder, a minority of takers (market /
// crossing IOC) that consume the touch and move price, and a few cancels.
func genOrder(eng *market.ParallelEngine, r *rand.Rand, id types.OrderID, users int) types.Command {
	m := types.MarketID(r.Intn(len(marketBase)))
	a := acct(r, users)
	side := types.Side(r.Intn(2))
	qty := genQty(r)
	mid := midPrice(eng.Shard(m).Book(), marketMid[m])

	switch n := r.Intn(100); {
	case n < 8: // cancel a recent order (no-op if already gone)
		return types.Command{Type: types.CmdCancel, Market: m, Account: a, OrderID: id - types.OrderID(1+r.Intn(8000))}
	case n < 20: // taker: market sweep
		return order(m, a, id, side, types.Market, types.GTC, 0, qty)
	case n < 26: // taker: crossing IOC a few ticks through the touch
		px := mid + types.Price(2+r.Intn(5))
		if side == types.Sell {
			px = mid - types.Price(2+r.Intn(5))
		}
		return order(m, a, id, side, types.Limit, types.IOC, px, qty)
	default: // maker: rest away from the touch
		off := makerOffset(r)
		px := mid - off
		if side == types.Sell {
			px = mid + off
		}
		if px < 1 {
			px = 1
		}
		return order(m, a, id, side, types.Limit, types.GTC, px, qty)
	}
}

func order(m types.MarketID, a types.AccountID, id types.OrderID, side types.Side, typ types.OrderType, tif types.TIF, price types.Price, qty types.Qty) types.Command {
	return types.Command{Type: types.CmdNewOrder, Market: m, Account: a, OrderID: id, Side: side, OrdType: typ, Tif: tif, Price: price, Qty: qty}
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

// midPrice estimates the mid from the touch, falling back to the last trade or
// the market's base mid when a side is empty. Safe to call between control steps.
func midPrice(bk *orderbook.Book, base types.Price) types.Price {
	bid, okb := bk.BestBid()
	ask, oka := bk.BestAsk()
	switch {
	case okb && oka:
		return (bid + ask) / 2
	case okb:
		return bid
	case oka:
		return ask
	}
	if lp, ok := bk.LastPrice(); ok {
		return lp
	}
	return base
}

// ---- live display ----

type frame struct {
	market     string
	asks, bids []orderbook.PriceLevel
	last       types.Price
	hasLast    bool
	bestBid    types.Price
	bestAsk    types.Price
	hasBid     bool
	hasAsk     bool
	count      int64
	elapsed    time.Duration
	tps        float64
	bp         int64
	avg, p50   int64
	p95, p99   int64
	mx         int64
}

func buildFrame(e *market.ParallelEngine, m types.MarketID, levels int, h *hist, count int64, elapsed time.Duration, bp int64) *frame {
	bk := e.Shard(m).Book()
	bid, hasBid := bk.BestBid()
	ask, hasAsk := bk.BestAsk()
	last, hasLast := bk.LastPrice()
	tps := 0.0
	if elapsed > 0 {
		tps = float64(count) / elapsed.Seconds()
	}
	return &frame{
		market: marketName[m],
		asks:   bk.Depth(types.Sell, levels),
		bids:   bk.Depth(types.Buy, levels),
		last:   last, hasLast: hasLast,
		bestBid: bid, hasBid: hasBid,
		bestAsk: ask, hasAsk: hasAsk,
		count: count, elapsed: elapsed, tps: tps, bp: bp,
		avg: h.avg(), p50: h.pct(50), p95: h.pct(95), p99: h.pct(99), mx: int64(h.max),
	}
}

func displayLoop(p *atomic.Pointer[frame], stop <-chan struct{}, groups [][]types.MarketID) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if f := p.Load(); f != nil {
				render(f, groups)
			}
		}
	}
}

const (
	red   = "\033[31m"
	green = "\033[32m"
	cyan  = "\033[36m"
	gray  = "\033[90m"
	bold  = "\033[1m"
	reset = "\033[0m"
)

func workerLayout(groups [][]types.MarketID) string {
	parts := make([]string, len(groups))
	for i, g := range groups {
		names := make([]string, len(g))
		for j, m := range g {
			names[j] = marketName[m]
		}
		parts[i] = fmt.Sprintf("core%d:%s", i, strings.Join(names, "+"))
	}
	return strings.Join(parts, "  ")
}

func render(f *frame, groups [][]types.MarketID) {
	var b strings.Builder
	b.WriteString("\033[H\033[2J") // home + clear
	fmt.Fprintf(&b, "%s%s  parallel engine shardbench%s   elapsed %6.1fs   %s%.0f cmd/s%s   cmds %d   backpressure %d\n",
		bold, cyan, reset, f.elapsed.Seconds(), bold, f.tps, reset, f.count, f.bp)
	fmt.Fprintf(&b, "%sworkers  %s%s\n\n", gray, workerLayout(groups), reset)

	fmt.Fprintf(&b, "%s%-10s ORDER BOOK%s   (price x qty, depth bar)\n", bold, f.market, reset)
	maxQty := maxLevelQty(f.asks, f.bids)

	for i := len(f.asks) - 1; i >= 0; i-- {
		lv := f.asks[i]
		fmt.Fprintf(&b, "%s  %12s  %10s %s%s\n", red, priceStr(lv.Price), qtyStr(lv.Qty), bar(lv.Qty, maxQty), reset)
	}

	spread := "  --"
	if f.hasBid && f.hasAsk {
		spread = priceStr(f.bestAsk - f.bestBid)
	}
	lastStr := "  --"
	if f.hasLast {
		lastStr = priceStr(f.last)
	}
	fmt.Fprintf(&b, "  %s--------- last %s%s%s%s  spread %s ---------%s\n", bold, cyan, lastStr, reset, bold, spread, reset)

	for _, lv := range f.bids {
		fmt.Fprintf(&b, "%s  %12s  %10s %s%s\n", green, priceStr(lv.Price), qtyStr(lv.Qty), bar(lv.Qty, maxQty), reset)
	}

	fmt.Fprintf(&b, "\n%slatency%s (per-op service time)  avg %s  p50 %s  p95 %s  p99 %s  max %s\n",
		bold, reset, dur(f.avg), dur(f.p50), dur(f.p95), dur(f.p99), dur(f.mx))
	os.Stdout.WriteString(b.String())
}

func printSummary(f *frame, groups [][]types.MarketID) {
	fmt.Printf("\n%s==== shardbench complete (full parallel engine) ====%s\n", bold, reset)
	fmt.Printf("workers            : %s\n", workerLayout(groups))
	fmt.Printf("commands processed : %d\n", f.count)
	fmt.Printf("duration           : %.1fs\n", f.elapsed.Seconds())
	fmt.Printf("throughput         : %.0f cmd/s\n", f.tps)
	fmt.Printf("backpressure       : %d\n", f.bp)
	fmt.Printf("latency average    : %s\n", dur(f.avg))
	fmt.Printf("latency median(p50): %s\n", dur(f.p50))
	fmt.Printf("latency p95        : %s\n", dur(f.p95))
	fmt.Printf("latency p99        : %s\n", dur(f.p99))
	fmt.Printf("latency max        : %s\n", dur(f.mx))
}

func maxLevelQty(a, b []orderbook.PriceLevel) types.Qty {
	var m types.Qty = 1
	for _, l := range a {
		if l.Qty > m {
			m = l.Qty
		}
	}
	for _, l := range b {
		if l.Qty > m {
			m = l.Qty
		}
	}
	return m
}

func priceStr(p types.Price) string { return fmt.Sprintf("%.2f", float64(p)/priceDiv) }
func qtyStr(q types.Qty) string     { return fmt.Sprintf("%.4f", float64(q)/qtyDiv) }

func bar(q, max types.Qty) string {
	const width = 30
	n := int(int64(q) * width / int64(max))
	if n < 0 {
		n = 0
	}
	if n > width {
		n = width
	}
	return strings.Repeat("█", n)
}

func dur(ns int64) string {
	switch {
	case ns >= 1_000_000:
		return fmt.Sprintf("%.2fms", float64(ns)/1e6)
	case ns >= 1_000:
		return fmt.Sprintf("%.2fµs", float64(ns)/1e3)
	default:
		return fmt.Sprintf("%dns", ns)
	}
}

// ---- bounded-memory two-tier latency histogram (see cmd/loadtest) ----

const (
	fineWidth   = int64(10)      // 10ns
	fineMax     = int64(300_000) // 300µs
	fineN       = int(fineMax / fineWidth)
	coarseWidth = int64(10_000) // 10µs
	coarseN     = 20_000        // up to 200ms
)

type hist struct {
	fine   []uint64
	coarse []uint64
	over   uint64
	count  uint64
	sum    uint64
	max    uint64
}

func newHist() *hist {
	return &hist{fine: make([]uint64, fineN), coarse: make([]uint64, coarseN)}
}

func (h *hist) record(ns int64) {
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

func (h *hist) avg() int64 {
	if h.count == 0 {
		return 0
	}
	return int64(h.sum / h.count)
}

func (h *hist) pct(p float64) int64 {
	if h.count == 0 {
		return 0
	}
	target := uint64(p / 100 * float64(h.count))
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
