// Command enginebench measures the real (serial, deterministic) engine's
// sustainable throughput when command generation is moved off the engine's
// core. A single producer goroutine fills the ingress ring; a separate engine
// goroutine drains and processes it in a tight loop. This is the honest ceiling
// of the production engine on a realistic deep book — the engine itself is
// unchanged and still single-writer/deterministic; only generation is offloaded.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/yanuar-ar/order-book/internal/balance"
	"github.com/yanuar-ar/order-book/internal/market"
	"github.com/yanuar-ar/order-book/internal/platform"
	"github.com/yanuar-ar/order-book/internal/types"
)

const (
	usdt                     = types.AssetID(2)
	qtyDiv                   = 100_000_000
	offsetMean               = 50
	offsetCapTk              = 2000
	seedDepthTks             = 300
	baseMid      types.Price = 10_800_000 // single market for the ceiling test (BTC ~$108k)
	mkt                      = types.MarketID(0)
	base                     = types.AssetID(1)
)

func main() {
	dur := flag.Duration("duration", 10*time.Second, "run duration")
	users := flag.Int("users", 100, "account pool size")
	flag.Parse()

	if runtime.GOMAXPROCS(0) < 3 {
		runtime.GOMAXPROCS(3)
	}
	eng := market.NewEngine(market.Config{
		Markets:  map[types.MarketID]balance.MarketSpec{mkt: {Base: base, Quote: usdt}},
		QtyScale: qtyDiv, FeeScale: 100, MakerFee: 1, TakerFee: 2,
		RingSize: 1 << 16, CapHint: 1 << 21,
	})
	prev := platform.GCOff()
	defer platform.GCOn(prev)

	r := rand.New(rand.NewSource(1))
	for a := 1; a <= *users; a++ {
		eng.Submit(types.Command{Type: types.CmdDeposit, Account: types.AccountID(a), Asset: usdt, Amount: 1 << 54})
		eng.Submit(types.Command{Type: types.CmdDeposit, Account: types.AccountID(a), Asset: base, Amount: 1 << 50})
	}
	eng.Drain()
	var id types.OrderID = 1
	for off := types.Price(1); off <= seedDepthTks; off++ {
		for k := 0; k < 2; k++ {
			id++
			eng.Submit(funded(id, types.Buy, types.Limit, types.GTC, baseMid-off, genQty(r), acct(r, *users)))
			id++
			eng.Submit(funded(id, types.Sell, types.Limit, types.GTC, baseMid+off, genQty(r), acct(r, *users)))
		}
	}
	eng.Drain()

	startSeq := eng.Seq()
	var stop atomic.Bool

	// Producer goroutine: generate as fast as possible, spinning on backpressure.
	var produced, backpressure int64
	prodDone := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer close(prodDone)
		pr := rand.New(rand.NewSource(2))
		var pid types.OrderID = 1 << 40
		for !stop.Load() {
			pid++
			c := genCmd(pr, pid, *users)
			for !eng.Submit(c) {
				atomic.AddInt64(&backpressure, 1)
				if stop.Load() {
					return
				}
			}
			atomic.AddInt64(&produced, 1)
		}
	}()

	// Engine goroutine: drain + process in a tight loop on its own core.
	done := make(chan struct{})
	go func() {
		_ = platform.PinCurrentThread(1)
		defer platform.Unpin()
		for !stop.Load() {
			for k := 0; k < 4096; k++ {
				eng.Step()
			}
		}
		close(done)
	}()

	start := time.Now()
	time.Sleep(*dur)
	stop.Store(true)
	<-done
	<-prodDone
	elapsed := time.Since(start)
	eng.Drain()

	processed := uint64(eng.Seq() - startSeq)
	fmt.Printf("duration        : %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("processed        : %d commands\n", processed)
	fmt.Printf("engine throughput: %.0f cmd/s\n", float64(processed)/elapsed.Seconds())
	fmt.Printf("produced         : %d\n", atomic.LoadInt64(&produced))
	fmt.Printf("backpressure     : %d (producer waited for the engine)\n", atomic.LoadInt64(&backpressure))
}

func genCmd(r *rand.Rand, id types.OrderID, users int) types.Command {
	a := acct(r, users)
	side := types.Side(r.Intn(2))
	qty := genQty(r)
	switch n := r.Intn(100); {
	case n < 8:
		return types.Command{Type: types.CmdCancel, Market: mkt, Account: a, OrderID: id - types.OrderID(1+r.Intn(8000))}
	case n < 20:
		return cmd(id, side, types.Market, types.GTC, 0, qty, a)
	case n < 26:
		px := baseMid + types.Price(2+r.Intn(5))
		if side == types.Sell {
			px = baseMid - types.Price(2+r.Intn(5))
		}
		return cmd(id, side, types.Limit, types.IOC, px, qty, a)
	default:
		off := makerOffset(r)
		px := baseMid - off
		if side == types.Sell {
			px = baseMid + off
		}
		if px < 1 {
			px = 1
		}
		return cmd(id, side, types.Limit, types.GTC, px, qty, a)
	}
}

func cmd(id types.OrderID, side types.Side, typ types.OrderType, tif types.TIF, price types.Price, qty types.Qty, a types.AccountID) types.Command {
	return types.Command{Type: types.CmdNewOrder, Market: mkt, Account: a, OrderID: id, Side: side, OrdType: typ, Tif: tif, Price: price, Qty: qty}
}

func funded(id types.OrderID, side types.Side, typ types.OrderType, tif types.TIF, price types.Price, qty types.Qty, a types.AccountID) types.Command {
	return cmd(id, side, typ, tif, price, qty, a)
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
