// Command throughput answers "how fast can the engine go?" It drives the engine
// at maximum rate with offloaded generation — a producer goroutine fills the
// ingress ring while the engine/control goroutine drains it — so the measured
// rate reflects the engine, not the command generator. Topology is a flag
// (-topology serial|parallel, with -cores for parallel), and run length is
// either a wall-clock window (-duration) or a fixed, reproducible count
// (-n + -rngseed, the deterministic latency-regression mode that replaces the
// old bench).
//
// It renders the same live order-book TUI as cmd/loadtest, plus a final summary
// (throughput, produced/backpressure, engine step-latency percentiles). The
// frame is built on the engine goroutine between its own Step calls — the sole
// book mutator in serial topology, and in parallel the workers are idle between
// the control goroutine's synchronous steps — so the book reads never race the
// matcher.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yanuar-ar/order-book/cmd/internal/harness"
	"github.com/yanuar-ar/order-book/internal/platform"
	"github.com/yanuar-ar/order-book/internal/types"
	"github.com/yanuar-ar/order-book/internal/wal"
)

const latSample = 256 // sample 1 in N processed ops for latency

func newRand(seed int64) *rand.Rand { return rand.New(rand.NewSource(seed)) }

func main() {
	topology := flag.String("topology", "serial", "engine topology: serial | parallel")
	cores := flag.String("cores", "0;1,2", "parallel only: market->worker map ('0;1,2')")
	dur := flag.Duration("duration", 10*time.Second, "run window (ignored when -n > 0)")
	n := flag.Int("n", 0, "fixed command count for a reproducible run (0 = use -duration)")
	warmup := flag.Int("warmup", 0, "commands to process before recording latency")
	rngseed := flag.Int64("rngseed", 1, "producer RNG seed (for -n reproducibility)")
	users := flag.Int("users", 100, "account pool size")
	view := flag.Int("market", 0, "market id to display (0=BTC,1=ETH,2=SOL)")
	levels := flag.Int("levels", 16, "order-book depth levels to show per side")
	durable := flag.Bool("durable", false, "journal to a real WAL (group-commit fsync) instead of the no-op journal — the honest durable ceiling")
	walDir := flag.String("wal", "", "WAL directory for -durable (default: a temp dir, removed on exit)")
	flushCap := flag.Int("flushcap", 0, "group-commit batch ceiling (commands per fsync; 0 = engine default). Bigger amortizes fsync harder on the durable path")
	cpuprofile := flag.String("cpuprofile", "", "write a CPU profile to this path for the measured window")
	flag.Parse()

	var groups [][]types.MarketID
	if *topology == "parallel" {
		groups = harness.ParseCores(*cores)
		// cores for workers + control + producer.
		if want := len(groups) + 2; runtime.GOMAXPROCS(0) < want {
			runtime.GOMAXPROCS(want)
		}
	} else if runtime.GOMAXPROCS(0) < 3 {
		runtime.GOMAXPROCS(3)
	}

	cfg := harness.DefaultConfig()
	cfg.FlushCap = *flushCap
	if *durable {
		dir := *walDir
		if dir == "" {
			d, err := os.MkdirTemp("", "throughput-wal-")
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
		defer w.Close()
		cfg.Journal = w
	}

	eng, cleanup, err := harness.BuildEngine(*topology, groups, cfg)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer cleanup()

	prev := platform.GCOff()
	defer platform.GCOn(prev)

	harness.Fund(eng, *users)
	harness.SeedBook(eng, newRand(*rngseed), *users)

	startSeq := eng.Seq()
	total := *warmup + *n
	title := "spot order-book throughput"
	sub := subLine(*topology, *n, *warmup, *durable, groups)
	h := harness.NewHist()
	var framePtr atomic.Pointer[harness.Frame]
	var stop atomic.Bool
	var produced, backpressure int64

	displayStop := make(chan struct{})
	go harness.DisplayLoop(&framePtr, displayStop)

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			fmt.Println("create cpuprofile:", err)
			return
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			fmt.Println("start cpuprofile:", err)
			return
		}
		defer pprof.StopCPUProfile()
	}

	start := time.Now()

	// Producer: generate base-mid commands as fast as the ingress accepts them.
	prodDone := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer close(prodDone)
		pr := newRand(*rngseed + 1)
		var pid types.OrderID = 1 << 40 // above seeded ids
		for !stop.Load() {
			if *n > 0 && atomic.LoadInt64(&produced) >= int64(total) {
				return
			}
			pid++
			c := harness.GenBaseMid(pr, pid, *users)
			for !eng.Submit(c) {
				atomic.AddInt64(&backpressure, 1)
				if stop.Load() {
					return
				}
			}
			atomic.AddInt64(&produced, 1)
		}
	}()

	// Engine: drain in a tight loop on its own core, owning the histogram (single
	// writer). Sample per-op step latency 1-in-latSample after warmup, and build a
	// TUI frame every 100ms (between Steps: safe book read in both topologies).
	done := make(chan struct{})
	go func() {
		_ = platform.PinCurrentThread(len(groups)) // core above the worker range (no-op on non-Linux)
		defer platform.Unpin()
		frameEvery := 100 * time.Millisecond
		nextFrame := start.Add(frameEvery)
		var processed int64
		var sampleTick int
		for !stop.Load() {
			t0 := time.Now()
			worked := eng.Step()
			if worked {
				processed++
				sampleTick++
				if processed > int64(*warmup) && sampleTick >= latSample {
					h.Record(time.Since(t0).Nanoseconds())
					sampleTick = 0
				}
				if *n > 0 && processed >= int64(total) {
					stop.Store(true)
				}
			}
			if t0.After(nextFrame) {
				framePtr.Store(harness.BuildFrame(eng, types.MarketID(*view), *levels, h, title, sub, processed, t0.Sub(start), atomic.LoadInt64(&backpressure)))
				nextFrame = t0.Add(frameEvery)
			}
		}
		close(done)
	}()

	if *n == 0 {
		time.Sleep(*dur)
		stop.Store(true)
	}
	<-done
	<-prodDone
	elapsed := time.Since(start)
	eng.Drain()
	close(displayStop)
	time.Sleep(120 * time.Millisecond) // let the display goroutine settle

	// Engine and producer goroutines have exited; building the final frame on the
	// main goroutine is race-free.
	processed := int64(eng.Seq() - startSeq)
	final := harness.BuildFrame(eng, types.MarketID(*view), *levels, h, title, sub, processed, elapsed, atomic.LoadInt64(&backpressure))
	harness.Render(final)
	printSummary(*topology, *n, *warmup, *rngseed, *durable, groups, processed, elapsed, atomic.LoadInt64(&produced), atomic.LoadInt64(&backpressure), h)
}

// subLine is the TUI sub-line: topology + run mode + (parallel) the worker map.
func subLine(topology string, n, warmup int, durable bool, groups [][]types.MarketID) string {
	mode := "duration"
	if n > 0 {
		mode = fmt.Sprintf("fixed n=%d warmup=%d", n, warmup)
	}
	journal := "no-op journal"
	if durable {
		journal = "durable WAL"
	}
	if topology != "parallel" {
		return "topology serial  " + mode + "  " + journal
	}
	return "topology parallel  " + mode + "  " + journal + "  " + workerLayout(groups)
}

func workerLayout(groups [][]types.MarketID) string {
	parts := make([]string, len(groups))
	for i, g := range groups {
		names := make([]string, len(g))
		for j, m := range g {
			names[j] = harness.MarketName[m]
		}
		parts[i] = fmt.Sprintf("core%d:%s", i, strings.Join(names, "+"))
	}
	return strings.Join(parts, "  ")
}

func printSummary(topology string, n, warmup int, rngseed int64, durable bool, groups [][]types.MarketID, processed int64, elapsed time.Duration, produced, backpressure int64, h *harness.Hist) {
	fmt.Printf("\n==== throughput (%s topology) ====\n", topology)
	if topology == "parallel" {
		fmt.Printf("workers           : %s\n", workerLayout(groups))
	}
	journal := "no-op (no fsync)"
	if durable {
		journal = "durable WAL (group-commit fsync)"
	}
	fmt.Printf("journal           : %s\n", journal)
	if n > 0 {
		fmt.Printf("mode              : fixed count n=%d warmup=%d rngseed=%d (reproducible)\n", n, warmup, rngseed)
	} else {
		fmt.Printf("mode              : duration %s\n", elapsed.Round(time.Millisecond))
	}
	fmt.Printf("processed         : %d commands\n", processed)
	fmt.Printf("throughput        : %.0f cmd/s\n", float64(processed)/elapsed.Seconds())
	fmt.Printf("produced          : %d\n", produced)
	fmt.Printf("backpressure      : %d (producer waited for the engine)\n", backpressure)
	fmt.Printf("latency p50/p95/p99/max (engine step): %s / %s / %s / %s\n",
		harness.Dur(h.Pct(50)), harness.Dur(h.Pct(95)), harness.Dur(h.Pct(99)), harness.Dur(h.Max()))
}
