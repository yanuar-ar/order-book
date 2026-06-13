package harness

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yanuar-ar/order-book/internal/orderbook"
	"github.com/yanuar-ar/order-book/internal/types"
)

const (
	priceDiv = 100.0 // cents -> dollars for display

	red   = "\033[31m"
	green = "\033[32m"
	cyan  = "\033[36m"
	gray  = "\033[90m"
	bold  = "\033[1m"
	reset = "\033[0m"
)

// Frame is one immutable snapshot of the live view, built on the control
// goroutine and handed to the display goroutine via an atomic pointer.
type Frame struct {
	Title    string // e.g. "spot order-book load test"
	SubLine  string // e.g. worker/topology layout; omitted when empty
	Market   string
	Asks     []orderbook.PriceLevel
	Bids     []orderbook.PriceLevel
	Last     types.Price
	HasLast  bool
	BestBid  types.Price
	BestAsk  types.Price
	HasBid   bool
	HasAsk   bool
	Count    int64
	Elapsed  time.Duration
	TPS      float64
	BP       int64
	Avg, P50 int64
	P95, P99 int64
	Mx       int64
}

// BuildFrame captures the current book + stats for one market into a Frame.
// Call it between control steps (book reads must not race the matcher).
func BuildFrame(e Engine, m types.MarketID, levels int, h *Hist, title, subLine string, count int64, elapsed time.Duration, bp int64) *Frame {
	bk := e.Shard(m).Book()
	bid, hasBid := bk.BestBid()
	ask, hasAsk := bk.BestAsk()
	last, hasLast := bk.LastPrice()
	tps := 0.0
	if elapsed > 0 {
		tps = float64(count) / elapsed.Seconds()
	}
	return &Frame{
		Title: title, SubLine: subLine, Market: MarketName[m],
		Asks: bk.Depth(types.Sell, levels), Bids: bk.Depth(types.Buy, levels),
		Last: last, HasLast: hasLast,
		BestBid: bid, HasBid: hasBid, BestAsk: ask, HasAsk: hasAsk,
		Count: count, Elapsed: elapsed, TPS: tps, BP: bp,
		Avg: h.Avg(), P50: h.Pct(50), P95: h.Pct(95), P99: h.Pct(99), Mx: h.Max(),
	}
}

// DisplayLoop renders the latest frame every 100ms until stop is closed.
func DisplayLoop(p *atomic.Pointer[Frame], stop <-chan struct{}) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if f := p.Load(); f != nil {
				Render(f)
			}
		}
	}
}

// Render draws one frame: header, the ask ladder (worst at top), the spread/last
// band, the bid ladder, and the latency line.
func Render(f *Frame) {
	var b strings.Builder
	b.WriteString("\033[H\033[2J") // home + clear
	fmt.Fprintf(&b, "%s%s  %s%s   elapsed %6.1fs   %s%.0f cmd/s%s   cmds %d   backpressure %d\n",
		bold, cyan, f.Title, reset, f.Elapsed.Seconds(), bold, f.TPS, reset, f.Count, f.BP)
	if f.SubLine != "" {
		fmt.Fprintf(&b, "%s%s%s\n", gray, f.SubLine, reset)
	}
	b.WriteByte('\n')

	fmt.Fprintf(&b, "%s%-10s ORDER BOOK%s   (price x qty, depth bar)\n", bold, f.Market, reset)
	maxQty := maxLevelQty(f.Asks, f.Bids)

	for i := len(f.Asks) - 1; i >= 0; i-- {
		lv := f.Asks[i]
		fmt.Fprintf(&b, "%s  %12s  %10s %s%s\n", red, priceStr(lv.Price), qtyStr(lv.Qty), bar(lv.Qty, maxQty), reset)
	}

	spread := "  --"
	if f.HasBid && f.HasAsk {
		spread = priceStr(f.BestAsk - f.BestBid)
	}
	lastStr := "  --"
	if f.HasLast {
		lastStr = priceStr(f.Last)
	}
	fmt.Fprintf(&b, "  %s--------- last %s%s%s%s  spread %s ---------%s\n", bold, cyan, lastStr, reset, bold, spread, reset)

	for _, lv := range f.Bids {
		fmt.Fprintf(&b, "%s  %12s  %10s %s%s\n", green, priceStr(lv.Price), qtyStr(lv.Qty), bar(lv.Qty, maxQty), reset)
	}

	fmt.Fprintf(&b, "\n%slatency%s  avg %s  p50 %s  p95 %s  p99 %s  max %s\n",
		bold, reset, Dur(f.Avg), Dur(f.P50), Dur(f.P95), Dur(f.P99), Dur(f.Mx))
	os.Stdout.WriteString(b.String())
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
func qtyStr(q types.Qty) string     { return fmt.Sprintf("%.4f", float64(q)/QtyDiv) }

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

// Dur formats a nanosecond duration compactly (ms / µs / ns).
func Dur(ns int64) string {
	switch {
	case ns >= 1_000_000:
		return fmt.Sprintf("%.2fms", float64(ns)/1e6)
	case ns >= 1_000:
		return fmt.Sprintf("%.2fµs", float64(ns)/1e3)
	default:
		return fmt.Sprintf("%dns", ns)
	}
}
