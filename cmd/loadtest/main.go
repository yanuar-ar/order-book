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
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yanuar-ar/order-book/cmd/internal/harness"
	"github.com/yanuar-ar/order-book/internal/orderbook"
	"github.com/yanuar-ar/order-book/internal/platform"
	"github.com/yanuar-ar/order-book/internal/types"
	"github.com/yanuar-ar/order-book/internal/wal"
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
	journal := flag.String("journal", "async", "journal mode: async (off-thread fsync, default) | sync (inline fsync) | none (no WAL, match latency only)")
	flushCap := flag.Int("flushcap", 0, "group-commit batch ceiling (commands per fsync; 0 = engine default)")
	walDir := flag.String("wal", "", "WAL directory for durable modes (default: a temp dir, removed on exit)")
	flag.Parse()

	switch *journal {
	case "async", "sync", "none":
	default:
		fmt.Println("invalid -journal:", *journal, "(want async | sync | none)")
		return
	}
	durable := *journal != "none"
	async := *journal == "async"

	var groups [][]types.MarketID
	if *topology == "parallel" {
		groups = harness.ParseCores(*cores)
	}
	cfg := harness.DefaultConfig()
	cfg.FlushCap = *flushCap
	if durable {
		dir := *walDir
		if dir == "" {
			d, err := os.MkdirTemp("", "loadtest-wal-")
			if err != nil {
				fmt.Println("temp WAL dir:", err)
				return
			}
			dir = d
			defer os.RemoveAll(dir)
		}
		w, err := wal.OpenWriter(dir, 0)
		if err != nil {
			fmt.Println("open WAL:", err)
			return
		}
		defer w.Close() // LIFO: runs after cleanup() closes the engine/journaller
		cfg.Journal = w
		if async {
			cfg.AsyncJournal = true
			cfg.JournalBatchCap = *flushCap
		}
	}
	eng, cleanup, err := harness.BuildEngine(*topology, groups, cfg)
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
	eng.Drain() // quiesce setup so the load phase starts from a seeded, durable book

	// Prefetch books so the generation hot path and the TUI never touch a map.
	var books [3]*orderbook.Book
	for m := types.MarketID(0); m < 3; m++ {
		books[m] = eng.Shard(m).Book()
	}

	h := harness.NewHist()          // match latency: intended -> matched (internal SLO)
	hDur := harness.NewHist()       // durable-ack latency: intended -> WAL-durable (production SLO)
	ackCursor := len(eng.AcksAll()) // skip setup (deposit/seed) acks
	var framePtr atomic.Pointer[harness.Frame]
	stop := make(chan struct{})
	go harness.DisplayLoop(&framePtr, stop)

	title := "spot order-book load test"
	sub := topologyLine(*topology, groups) + "  " + journalLabel(durable, async)

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
		c.ClientTsNanos = intended.UnixNano()          // stamp for durable-ack correlation
		if !eng.Submit(c) {
			backpressure++
		}
		eng.Step()
		done := time.Now()
		h.Record(done.Sub(intended).Nanoseconds()) // match latency (coordinated-omission-correct)
		if durable {
			// Durable-ack latency: advance a cursor over the ungated ack log up to
			// the durable watermark (O(1) amortized — no O(prefix) rescan), and
			// record each newly-durable ack from its intended time.
			raw := eng.AcksAll()
			d := eng.DurableSeq()
			nowNs := done.UnixNano()
			for ackCursor < len(raw) && raw[ackCursor].Seq <= d {
				lat := nowNs - raw[ackCursor].ClientTsNanos
				if lat < 0 {
					lat = 0
				}
				hDur.Record(lat)
				ackCursor++
			}
		}
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
	printSummary(final, hDur, durable, async)
}

// journalLabel describes the journaling path for the TUI sub-line and summary.
func journalLabel(durable, async bool) string {
	if !durable {
		return "no-op journal"
	}
	if async {
		return "durable WAL (async, off-thread fsync)"
	}
	return "durable WAL (sync)"
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

func printSummary(f *harness.Frame, hDur *harness.Hist, durable, async bool) {
	fmt.Printf("\n==== load test complete ====\n")
	fmt.Printf("commands processed : %d\n", f.Count)
	fmt.Printf("duration           : %.1fs\n", f.Elapsed.Seconds())
	fmt.Printf("throughput         : %.0f cmd/s\n", f.TPS)
	fmt.Printf("backpressure       : %d\n", f.BP)
	fmt.Printf("journal            : %s\n", journalLabel(durable, async))
	// Match latency (intended -> matched). When not durable this is the only SLO,
	// so it keeps the original "latency ..." labels; when durable it is the
	// internal SLO alongside durable-ack below.
	const w = 24 // pad so the ':' column lines up across both blocks
	label := "latency"
	if durable {
		label = "match" // distinguish the internal SLO from durable-ack below
	}
	fmt.Printf("%-*s: %s\n", w, label+" average", harness.Dur(f.Avg))
	fmt.Printf("%-*s: %s\n", w, label+" median(p50)", harness.Dur(f.P50))
	fmt.Printf("%-*s: %s\n", w, label+" p95", harness.Dur(f.P95))
	fmt.Printf("%-*s: %s\n", w, label+" p99", harness.Dur(f.P99))
	fmt.Printf("%-*s: %s\n", w, label+" max", harness.Dur(f.Mx))
	if durable {
		fmt.Printf("%-*s: %s\n", w, "durable-ack average", harness.Dur(hDur.Avg()))
		fmt.Printf("%-*s: %s\n", w, "durable-ack median(p50)", harness.Dur(hDur.Pct(50)))
		fmt.Printf("%-*s: %s\n", w, "durable-ack p95", harness.Dur(hDur.Pct(95)))
		fmt.Printf("%-*s: %s\n", w, "durable-ack p99", harness.Dur(hDur.Pct(99)))
		fmt.Printf("%-*s: %s\n", w, "durable-ack max", harness.Dur(hDur.Max()))
	}
}
