// Command loadtest answers "how does the engine behave at load X?" It paces
// commands open-loop at a target rate and measures latency from each command's
// intended schedule time (coordinated-omission-correct: a system that falls
// behind shows growing latency rather than hiding it), while rendering a live
// trading-terminal view of one market's order book. Topology is a flag
// (-topology serial|parallel, with -cores for parallel). For "how fast can it
// go" instead, use cmd/throughput.
//
// Generation reads the live book mid and the TUI reads book depth; both happen
// between control steps, when the engine (and, in parallel topology, the
// synchronous workers) are idle, so they never race the matcher.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yanuar-ar/order-book/cmd/internal/harness"
	"github.com/yanuar-ar/order-book/internal/orderbook"
	"github.com/yanuar-ar/order-book/internal/platform"
	"github.com/yanuar-ar/order-book/internal/types"
)

func newRand(seed int64) *rand.Rand { return rand.New(rand.NewSource(seed)) }

func main() {
	tps := flag.Int("tps", 100_000, "target throughput (commands/second)")
	dur := flag.Duration("duration", 2*time.Minute, "test duration")
	users := flag.Int("users", 100, "number of trading accounts")
	view := flag.Int("market", 0, "market id to display (0=BTC,1=ETH,2=SOL)")
	levels := flag.Int("levels", 16, "order-book depth levels to show per side")
	topology := flag.String("topology", "serial", "engine topology: serial | parallel")
	cores := flag.String("cores", "0;1,2", "parallel only: market->worker map ('0;1,2')")
	flag.Parse()

	var groups [][]types.MarketID
	if *topology == "parallel" {
		groups = harness.ParseCores(*cores)
	}
	eng, cleanup, err := harness.BuildEngine(*topology, groups, harness.DefaultConfig())
	if err != nil {
		println(err.Error())
		return
	}
	defer cleanup()

	prev := platform.GCOff()
	defer platform.GCOn(prev)

	r := newRand(1)
	harness.Fund(eng, *users)
	harness.SeedBook(eng, r, *users)

	// Prefetch books so the generation hot path and the TUI never touch a map.
	var books [3]*orderbook.Book
	for m := types.MarketID(0); m < 3; m++ {
		books[m] = eng.Shard(m).Book()
	}

	h := harness.NewHist()
	var framePtr atomic.Pointer[harness.Frame]
	stop := make(chan struct{})
	go harness.DisplayLoop(&framePtr, stop)

	title := "spot order-book load test"
	sub := topologyLine(*topology, groups)

	interval := time.Duration(int64(time.Second) / int64(*tps))
	start := time.Now()
	deadline := start.Add(*dur)
	frameEvery := 100 * time.Millisecond
	nextFrame := start
	var id types.OrderID = 1 << 32 // above seeded ids
	var i, backpressure int64

	for {
		intended := start.Add(time.Duration(i) * interval)
		for time.Now().Before(intended) {
			// busy-wait: time.Sleep is too coarse for ~10µs pacing
		}
		id++
		c := harness.GenLiveMid(&books, r, id, *users) // book read: between steps, workers idle
		if !eng.Submit(c) {
			backpressure++
		}
		eng.Step()
		done := time.Now()
		h.Record(done.Sub(intended).Nanoseconds()) // measured from intended (coordinated-omission-correct)
		i++

		if done.After(nextFrame) {
			framePtr.Store(harness.BuildFrame(eng, types.MarketID(*view), *levels, h, title, sub, i, done.Sub(start), backpressure))
			nextFrame = done.Add(frameEvery)
		}
		if done.After(deadline) {
			break
		}
	}
	eng.Drain()
	close(stop)
	time.Sleep(120 * time.Millisecond) // let the display goroutine settle

	final := harness.BuildFrame(eng, types.MarketID(*view), *levels, h, title, sub, i, time.Since(start), backpressure)
	harness.Render(final)
	printSummary(final)
}

// topologyLine is the TUI sub-line: a plain note for serial, the worker layout
// for parallel.
func topologyLine(topology string, groups [][]types.MarketID) string {
	if topology != "parallel" {
		return "topology serial"
	}
	parts := make([]string, len(groups))
	for i, g := range groups {
		names := make([]string, len(g))
		for j, m := range g {
			names[j] = harness.MarketName[m]
		}
		parts[i] = fmt.Sprintf("core%d:%s", i, strings.Join(names, "+"))
	}
	return "topology parallel  " + strings.Join(parts, "  ")
}

func printSummary(f *harness.Frame) {
	fmt.Printf("\n==== load test complete ====\n")
	fmt.Printf("commands processed : %d\n", f.Count)
	fmt.Printf("duration           : %.1fs\n", f.Elapsed.Seconds())
	fmt.Printf("throughput         : %.0f cmd/s\n", f.TPS)
	fmt.Printf("backpressure       : %d\n", f.BP)
	fmt.Printf("latency average    : %s\n", harness.Dur(f.Avg))
	fmt.Printf("latency median(p50): %s\n", harness.Dur(f.P50))
	fmt.Printf("latency p95        : %s\n", harness.Dur(f.P95))
	fmt.Printf("latency p99        : %s\n", harness.Dur(f.P99))
	fmt.Printf("latency max        : %s\n", harness.Dur(f.Mx))
}
