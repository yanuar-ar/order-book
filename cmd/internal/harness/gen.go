package harness

import (
	"math/rand"

	"github.com/yanuar-ar/order-book/internal/orderbook"
	"github.com/yanuar-ar/order-book/internal/types"
)

const (
	offsetMean   = 50   // mean maker distance from mid, in ticks ($0.50)
	offsetCapTk  = 2000 // cap maker distance at $20.00
	seedDepthTks = 300  // initial ladder spans ±$3.00 around mid
)

// marketWeights routes generated orders BTC 50% / ETH 30% / SOL 20% (design
// §18.3). Index with r.Intn(len(marketWeights)).
var marketWeights = []types.MarketID{0, 0, 0, 0, 0, 1, 1, 1, 2, 2}

// Acct returns a uniformly random account id in [1, users].
func Acct(r *rand.Rand, users int) types.AccountID { return types.AccountID(1 + r.Intn(users)) }

// GenQty returns a fractional base-asset size in 1e-8 units: 0.001 .. ~2.0.
func GenQty(r *rand.Rand) types.Qty { return types.Qty((1 + r.Intn(2000)) * 100_000) }

// makerOffset returns a maker's tick distance from mid, exponentially
// distributed so liquidity clusters near the touch and thins out deeper —
// matching a real crypto book.
func makerOffset(r *rand.Rand) types.Price {
	off := 1 + int(r.ExpFloat64()*offsetMean)
	if off > offsetCapTk {
		off = offsetCapTk
	}
	return types.Price(off)
}

func order(m types.MarketID, a types.AccountID, id types.OrderID, side types.Side, typ types.OrderType, tif types.TIF, price types.Price, qty types.Qty) types.Command {
	return types.Command{Type: types.CmdNewOrder, Market: m, Account: a, OrderID: id, Side: side, OrdType: typ, Tif: tif, Price: price, Qty: qty}
}

// genAround builds one command relative to the supplied mid, following the
// shared order-flow model: mostly makers resting away from the touch (building
// the depth ladder), a minority of takers (market sweep / crossing IOC) that
// consume the touch and move price, and a few cancels that churn liquidity.
// The mid is supplied by the caller so this serves both the live-mid and
// base-mid generators below.
func genAround(r *rand.Rand, id types.OrderID, m types.MarketID, mid types.Price, users int) types.Command {
	a := Acct(r, users)
	side := types.Side(r.Intn(2))
	qty := GenQty(r)

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

// GenLiveMid generates one command against the market's *live* mid, read from
// the book. Use it where the book reflects real trading dynamics and the caller
// can read the book safely — loadtest, between control steps. books is indexed
// by market id.
func GenLiveMid(books *[3]*orderbook.Book, r *rand.Rand, id types.OrderID, users int) types.Command {
	m := marketWeights[r.Intn(len(marketWeights))]
	return genAround(r, id, m, midPrice(books[m], MarketMid[m]), users)
}

// GenBaseMid generates one command against each market's *static* base mid,
// reading no book. Use it on a producer goroutine that runs concurrently with
// the engine — throughput's offloaded generation — where touching the book
// would race the matcher.
func GenBaseMid(r *rand.Rand, id types.OrderID, users int) types.Command {
	m := marketWeights[r.Intn(len(marketWeights))]
	return genAround(r, id, m, MarketMid[m], users)
}

// midPrice estimates the mid from the touch, falling back to the last trade or
// the supplied base mid when a side is empty. Safe to call between control steps
// (no concurrent matcher access).
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

// SeedBook rests a deep initial ladder around each market's mid so the book
// looks like a real one at start: seedDepthTks levels stepping from the touch,
// a couple of resting orders per level per side. Call after Fund.
func SeedBook(e Engine, r *rand.Rand, users int) {
	var id types.OrderID = 1
	for m, mid := range MarketMid {
		for off := types.Price(1); off <= seedDepthTks; off++ {
			for k := 0; k < 2; k++ {
				id++
				e.Submit(order(m, Acct(r, users), id, types.Buy, types.Limit, types.GTC, mid-off, GenQty(r)))
				id++
				e.Submit(order(m, Acct(r, users), id, types.Sell, types.Limit, types.GTC, mid+off, GenQty(r)))
			}
		}
	}
	e.Drain()
}
