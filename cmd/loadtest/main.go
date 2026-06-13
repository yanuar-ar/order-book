// Command loadtest drives the engine at a target throughput for a fixed
// duration with a pool of trading accounts, while rendering a live
// trading-terminal view of one market's order book (bids, asks, depth, last
// price) and reporting latency statistics (average, median, P95, P99).
//
// Pacing is open-loop: command i is scheduled at start + i/rate and latency is
// measured from that intended time, so a system that falls behind shows growing
// latency rather than hiding it (coordinated-omission-correct).
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yanuar-ar/order-book/internal/balance"
	"github.com/yanuar-ar/order-book/internal/market"
	"github.com/yanuar-ar/order-book/internal/orderbook"
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
var marketName = map[types.MarketID]string{0: "BTC/USDT", 1: "ETH/USDT", 2: "SOL/USDT"}

// market routing weights: BTC 50%, ETH 30%, SOL 20% (design §18.3).
var marketWeights = []types.MarketID{0, 0, 0, 0, 0, 1, 1, 1, 2, 2}

const (
	// Price model: integer ticks where 1 tick = $0.01, base mid = $100.00.
	// This gives a realistic deep ladder and decimal price display.
	baseMid   types.Price = 10_000
	priceDiv              = 100.0 // ticks -> dollars for display
	depthBand             = 60    // maker orders rest up to this many ticks from mid
	maxQtyGen             = 50
)

func main() {
	tps := flag.Int("tps", 100_000, "target throughput (commands/second)")
	dur := flag.Duration("duration", 2*time.Minute, "test duration")
	users := flag.Int("users", 100, "number of trading accounts")
	view := flag.Int("market", 0, "market id to display (0=BTC,1=ETH,2=SOL)")
	levels := flag.Int("levels", 12, "order-book depth levels to show per side")
	flag.Parse()

	specs := map[types.MarketID]balance.MarketSpec{}
	for m, base := range marketBase {
		specs[m] = balance.MarketSpec{Base: base, Quote: usdt}
	}
	eng := market.NewEngine(market.Config{
		Markets: specs, QtyScale: 1, FeeScale: 100, MakerFee: 1, TakerFee: 2,
		RingSize: 1 << 16, CapHint: 1 << 20,
	})

	prev := platform.GCOff()
	defer platform.GCOn(prev)

	r := rand.New(rand.NewSource(1))

	// Fund the user pool generously.
	for a := 1; a <= *users; a++ {
		eng.Submit(types.Command{Type: types.CmdDeposit, Account: types.AccountID(a), Asset: usdt, Amount: 1 << 50})
		for _, base := range marketBase {
			eng.Submit(types.Command{Type: types.CmdDeposit, Account: types.AccountID(a), Asset: base, Amount: 1 << 42})
		}
	}
	eng.Drain()
	// Seed a deep initial ladder around mid so the book looks like a real one
	// at start: many resting levels on each side stepping away from the touch.
	var id types.OrderID = 1
	for m := range marketBase {
		for off := types.Price(1); off <= depthBand; off++ {
			for k := 0; k < 3; k++ { // a few resting orders per price level
				id++
				eng.Submit(order(m, acct(r, *users), id, types.Buy, types.Limit, types.GTC, baseMid-off, types.Qty(1+r.Intn(maxQtyGen))))
				id++
				eng.Submit(order(m, acct(r, *users), id, types.Sell, types.Limit, types.GTC, baseMid+off, types.Qty(1+r.Intn(maxQtyGen))))
			}
		}
	}
	eng.Drain()

	h := newHist()
	var framePtr atomic.Pointer[frame]
	stop := make(chan struct{})
	go displayLoop(&framePtr, stop)

	interval := time.Duration(int64(time.Second) / int64(*tps))
	start := time.Now()
	deadline := start.Add(*dur)
	frameEvery := 100 * time.Millisecond
	nextFrame := start
	var i int64
	var backpressure int64

	for {
		intended := start.Add(time.Duration(i) * interval)
		for time.Now().Before(intended) {
			// busy-wait: time.Sleep is too coarse for ~10µs pacing
		}
		id++
		c := genOrder(eng, r, id, *users)
		if !eng.Submit(c) {
			backpressure++
		}
		eng.Step()
		h.record(time.Since(intended).Nanoseconds())
		i++

		now := time.Now()
		if now.After(nextFrame) {
			framePtr.Store(buildFrame(eng, types.MarketID(*view), *levels, h, i, now.Sub(start), backpressure))
			nextFrame = now.Add(frameEvery)
		}
		if now.After(deadline) {
			break
		}
	}
	eng.Drain()
	close(stop)
	time.Sleep(120 * time.Millisecond) // let the display goroutine settle

	// Final frame + summary printed below the live view.
	final := buildFrame(eng, types.MarketID(*view), *levels, h, i, time.Since(start), backpressure)
	render(final)
	printSummary(final)
}

func acct(r *rand.Rand, users int) types.AccountID { return types.AccountID(1 + r.Intn(users)) }

func order(m types.MarketID, a types.AccountID, id types.OrderID, side types.Side, typ types.OrderType, tif types.TIF, price types.Price, qty types.Qty) types.Command {
	return types.Command{Type: types.CmdNewOrder, Market: m, Account: a, OrderID: id, Side: side, OrdType: typ, Tif: tif, Price: price, Qty: qty}
}

// midPrice estimates the current mid from the touch, falling back to the last
// trade or the base mid when a side is empty.
func midPrice(bk *orderbook.Book) types.Price {
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
	return baseMid
}

// genOrder models real order-book dynamics relative to the live mid: most
// orders are makers that rest away from the touch and build a deep ladder; a
// minority are takers (market / crossing IOC) that consume the touch and move
// the price; a few are cancels that churn resting liquidity.
func genOrder(eng *market.Engine, r *rand.Rand, id types.OrderID, users int) types.Command {
	m := marketWeights[r.Intn(len(marketWeights))]
	a := acct(r, users)
	side := types.Side(r.Intn(2))
	qty := types.Qty(1 + r.Intn(maxQtyGen))
	mid := midPrice(eng.Shard(m).Book())

	switch n := r.Intn(100); {
	case n < 8: // cancel a recent order (no-op if already gone)
		return types.Command{Type: types.CmdCancel, Market: m, Account: a, OrderID: id - types.OrderID(1+r.Intn(8000))}
	case n < 20: // taker: market order sweeps the touch
		return order(m, a, id, side, types.Market, types.GTC, 0, qty)
	case n < 26: // taker: crossing IOC a few ticks through the touch
		px := mid + types.Price(2+r.Intn(5))
		if side == types.Sell {
			px = mid - types.Price(2+r.Intn(5))
		}
		return order(m, a, id, side, types.Limit, types.IOC, px, qty)
	default: // maker: rest away from the touch, building the depth ladder
		off := types.Price(1 + r.Intn(depthBand))
		px := mid - off // bid below mid
		if side == types.Sell {
			px = mid + off // ask above mid
		}
		if px < 1 {
			px = 1
		}
		return order(m, a, id, side, types.Limit, types.GTC, px, qty)
	}
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

func buildFrame(e *market.Engine, m types.MarketID, levels int, h *hist, count int64, elapsed time.Duration, bp int64) *frame {
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

func displayLoop(p *atomic.Pointer[frame], stop <-chan struct{}) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if f := p.Load(); f != nil {
				render(f)
			}
		}
	}
}

const (
	red   = "\033[31m"
	green = "\033[32m"
	cyan  = "\033[36m"
	bold  = "\033[1m"
	reset = "\033[0m"
)

func render(f *frame) {
	var b strings.Builder
	b.WriteString("\033[H\033[2J") // home + clear
	fmt.Fprintf(&b, "%s%s  spot order-book load test%s   elapsed %6.1fs   %s%.0f cmd/s%s   cmds %d   backpressure %d\n\n",
		bold, cyan, reset, f.elapsed.Seconds(), bold, f.tps, reset, f.count, f.bp)

	fmt.Fprintf(&b, "%s%-10s ORDER BOOK%s   (price x qty, depth bar)\n", bold, f.market, reset)
	maxQty := maxLevelQty(f.asks, f.bids)

	// Asks: print worst (highest) at top down to best (lowest) just above the spread.
	for i := len(f.asks) - 1; i >= 0; i-- {
		lv := f.asks[i]
		fmt.Fprintf(&b, "%s  %10s  %6d %s%s\n", red, priceStr(lv.Price), lv.Qty, bar(lv.Qty, maxQty), reset)
	}

	// Spread / last price band.
	spread := "  --"
	if f.hasBid && f.hasAsk {
		spread = priceStr(f.bestAsk - f.bestBid)
	}
	lastStr := "  --"
	if f.hasLast {
		lastStr = priceStr(f.last)
	}
	fmt.Fprintf(&b, "  %s--------- last %s%s%s%s  spread %s ---------%s\n", bold, cyan, lastStr, reset, bold, spread, reset)

	// Bids: best (highest) at top down.
	for _, lv := range f.bids {
		fmt.Fprintf(&b, "%s  %10s  %6d %s%s\n", green, priceStr(lv.Price), lv.Qty, bar(lv.Qty, maxQty), reset)
	}

	fmt.Fprintf(&b, "\n%slatency%s  avg %s  p50 %s  p95 %s  p99 %s  max %s\n",
		bold, reset, dur(f.avg), dur(f.p50), dur(f.p95), dur(f.p99), dur(f.mx))
	os.Stdout.WriteString(b.String())
}

func printSummary(f *frame) {
	fmt.Printf("\n%s==== load test complete ====%s\n", bold, reset)
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

func priceStr(p types.Price) string {
	return fmt.Sprintf("%.2f", float64(p)/priceDiv)
}

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

// ---- bounded-memory two-tier latency histogram ----
//
// A fine tier (10ns buckets up to 300µs) gives sub-µs resolution for the body
// of the distribution; a coarse tier (10µs buckets up to 200ms) captures the
// tail so high percentiles report real values instead of being pinned to max.
// Both tiers are small, so percentile scans stay cheap on the hot loop.

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
	return int64(h.max) // beyond the coarse range
}
