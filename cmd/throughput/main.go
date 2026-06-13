// Command throughput answers "how fast can the engine go?" It drives the engine
// at maximum rate with offloaded generation — a producer goroutine fills the
// ingress ring while the engine/control goroutine drains it — so the measured
// rate reflects the engine, not the command generator. Topology is a flag
// (-topology serial|parallel, with -cores for parallel), and run length is
// either a wall-clock window (-duration) or a fixed, reproducible count
// (-n + -rngseed, the deterministic latency-regression mode that replaces the
// old bench). It reports sustained throughput and per-op engine step-latency
// percentiles as text — no live TUI (see cmd/loadtest for that).
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/yanuar-ar/order-book/cmd/internal/harness"
	"github.com/yanuar-ar/order-book/internal/platform"
	"github.com/yanuar-ar/order-book/internal/types"
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

	eng, cleanup, err := harness.BuildEngine(*topology, groups, harness.DefaultConfig())
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
	var stop atomic.Bool
	var produced, backpressure int64

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
	// writer). Sample per-op step latency 1-in-latSample after warmup.
	h := harness.NewHist()
	done := make(chan struct{})
	go func() {
		_ = platform.PinCurrentThread(len(groups)) // core above the worker range (no-op on non-Linux)
		defer platform.Unpin()
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
		}
		close(done)
	}()

	start := time.Now()
	if *n == 0 {
		time.Sleep(*dur)
		stop.Store(true)
	}
	<-done
	<-prodDone
	elapsed := time.Since(start)
	eng.Drain()

	processed := uint64(eng.Seq() - startSeq)

	fmt.Printf("==== throughput (%s topology) ====\n", *topology)
	if *topology == "parallel" {
		fmt.Printf("workers           : %s\n", workerLayout(groups))
	}
	if *n > 0 {
		fmt.Printf("mode              : fixed count n=%d warmup=%d rngseed=%d (reproducible)\n", *n, *warmup, *rngseed)
	} else {
		fmt.Printf("mode              : duration %s\n", elapsed.Round(time.Millisecond))
	}
	fmt.Printf("processed         : %d commands\n", processed)
	fmt.Printf("throughput        : %.0f cmd/s\n", float64(processed)/elapsed.Seconds())
	fmt.Printf("produced          : %d\n", atomic.LoadInt64(&produced))
	fmt.Printf("backpressure      : %d (producer waited for the engine)\n", atomic.LoadInt64(&backpressure))
	fmt.Printf("latency p50/p95/p99/max (engine step): %s / %s / %s / %s\n",
		harness.Dur(h.Pct(50)), harness.Dur(h.Pct(95)), harness.Dur(h.Pct(99)), harness.Dur(h.Max()))
}

func workerLayout(groups [][]types.MarketID) string {
	parts := make([]string, len(groups))
	for i, g := range groups {
		names := make([]string, len(g))
		for j, m := range g {
			names[j] = harness.MarketName[m]
		}
		parts[i] = fmt.Sprintf("core%d:%v", i, names)
	}
	return fmt.Sprint(parts)
}
