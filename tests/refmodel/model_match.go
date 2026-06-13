package refmodel

import (
	"sort"

	"github.com/yanuar-ar/order-book/internal/types"
)

// Apply processes one command, fully resolving any stop activations it triggers
// (mirroring the engine's drain of the re-injection ring), so the model's
// post-command state equals the engine's drained state.
func (m *Model) Apply(c types.Command) {
	switch c.Type {
	case types.CmdDeposit:
		if c.Amount > 0 {
			m.move(c.Account, c.Asset, c.Amount, 0)
		}
	case types.CmdWithdraw:
		if c.Amount > 0 && m.avail(c.Account, c.Asset) >= c.Amount {
			m.move(c.Account, c.Asset, -c.Amount, 0)
		}
	case types.CmdCancel:
		m.cancel(c)
	case types.CmdAmend:
		m.amend(c)
	case types.CmdNewOrder:
		m.newOrder(orderFrom(c))
	}
	m.drainActivations()
}

// drainActivations processes queued stop activations FIFO, mirroring the
// sequencer's re-injection ring.
func (m *Model) drainActivations() {
	for len(m.queue) > 0 {
		o := m.queue[0]
		m.queue = m.queue[1:]
		m.newOrder(o)
	}
}

func (m *Model) nextIns() uint64 {
	m.ins++
	return m.ins
}

// ---- command handlers (mirror market.Core) ----

func (m *Model) newOrder(o order) {
	_, isActivation := m.open[o.id]

	if !isActivation {
		// Mirror Core.newOrder: validate filters before reserve. Activations are
		// not re-validated. Markets without a filter set skip validation.
		if f, ok := m.cfg.Filters[o.market]; ok {
			last, hasLast := m.last[o.market]
			if f.ValidateNew(o.funded(), m.cfg.QtyScale, last, hasLast) != types.ReasonNone {
				return
			}
		}
		if o.ordType == types.Market && o.side == types.Buy {
			o.maxQuote = m.marketBuyBudget(o.acct, o.market)
		}
		if !m.reserve(o) {
			return
		}
	} else if o.ordType == types.Market && o.side == types.Buy {
		o.maxQuote = m.orderBudget(o.id)
	}

	if isActivation {
		m.cancelFromBook(o.market, o.id) // clear stale pending stop (no-op if already popped)
	}

	if o.ordType == types.Stop || o.ordType == types.StopLimit {
		m.stops[o.market] = append(m.stops[o.market], stopRec{o: o})
		m.open[o.id] = openMeta{market: o.market, acct: o.acct, side: o.side, ordType: o.ordType, price: o.price, qty: o.qty}
		return
	}

	res := m.submitActive(o)
	if res.rejected {
		m.release(o.id)
		delete(m.open, o.id)
		return
	}
	for _, f := range res.fills {
		m.settle(f)
	}
	for _, mid := range res.filled {
		m.release(mid)
		delete(m.open, mid)
	}
	if res.rested {
		m.open[o.id] = openMeta{market: o.market, acct: o.acct, side: o.side, ordType: types.Limit, price: o.price, qty: res.restedQty}
	} else {
		m.release(o.id)
		delete(m.open, o.id)
	}
}

func (m *Model) cancel(c types.Command) {
	oo, ok := m.open[c.OrderID]
	if !ok {
		return
	}
	m.cancelFromBook(oo.market, c.OrderID)
	m.release(c.OrderID)
	delete(m.open, c.OrderID)
}

func (m *Model) amend(c types.Command) {
	oo, ok := m.open[c.OrderID]
	if !ok {
		return
	}
	if c.Price == oo.price && c.Qty < oo.qty {
		// Mirror Core.amend: re-validate the reduced quantity before applying.
		if f, ok := m.cfg.Filters[oo.market]; ok {
			if f.ValidateAmendDown(oo.price, c.Qty, m.cfg.QtyScale) != types.ReasonNone {
				return
			}
		}
		if m.amendDownBook(oo.market, c.OrderID, c.Qty) {
			m.amendReduce(c.OrderID, oo.side, oo.price, c.Qty)
			oo.qty = c.Qty
			m.open[c.OrderID] = oo
		}
		return
	}
	// Price change or quantity increase: cancel and re-submit (new priority).
	m.cancelFromBook(oo.market, c.OrderID)
	m.release(c.OrderID)
	delete(m.open, c.OrderID)
	repl := orderFrom(c)
	repl.ordType = oo.ordType
	m.newOrder(repl)
}

// ---- matching (mirrors matching.Engine) ----

type matchResult struct {
	fills     []types.Fill
	filled    []types.OrderID
	rested    bool
	restedQty types.Qty
	rejected  bool
	stp       bool
}

func crosses(aggSide types.Side, aggPrice types.Price, ordType types.OrderType, restingPrice types.Price) bool {
	if ordType == types.Market {
		return true
	}
	if aggSide == types.Buy {
		return aggPrice >= restingPrice
	}
	return aggPrice <= restingPrice
}

// submitActive matches an active order then triggers stops, mirroring
// matching.Engine.Submit.
func (m *Model) submitActive(o order) matchResult {
	res := m.matchActive(o)
	m.triggerStops(o.market)
	return res
}

func (m *Model) matchActive(o order) matchResult {
	opp := o.side.Opposite()
	res := matchResult{}

	if o.flags.Has(types.FlagPostOnly) {
		if front := m.frontResting(o.market, opp); front != nil && crosses(o.side, o.price, o.ordType, front.price) {
			return matchResult{rejected: true}
		}
		m.restOrder(o, o.qty)
		return matchResult{rested: true, restedQty: o.qty}
	}

	if o.tif == types.FOK {
		pred := func(p types.Price) bool { return crosses(o.side, o.price, o.ordType, p) }
		if m.matchableQty(o.market, opp, o.acct, pred, o.qty) < o.qty {
			return matchResult{rejected: true}
		}
	}

	remaining := o.qty
	var matchIdx uint32
	var spentQuote int64
	for remaining > 0 {
		front := m.frontResting(o.market, opp)
		if front == nil || !crosses(o.side, o.price, o.ordType, front.price) {
			break
		}
		if front.acct == o.acct {
			res.stp = true
			remaining = 0
			break
		}
		x := remaining
		if front.display < x {
			x = front.display
		}
		if o.ordType == types.Market && o.side == types.Buy {
			q, ok := types.Notional(front.price, x, m.cfg.QtyScale, true)
			if !ok || spentQuote+q > o.maxQuote {
				rem := o.maxQuote - spentQuote
				if rem <= 0 {
					break
				}
				maxX, ok2 := types.MulDiv(rem, m.cfg.QtyScale, int64(front.price), false)
				if !ok2 || maxX <= 0 {
					break
				}
				if types.Qty(maxX) < x {
					x = types.Qty(maxX)
				}
				q, _ = types.Notional(front.price, x, m.cfg.QtyScale, true)
			}
			spentQuote += q
		}
		res.fills = append(res.fills, m.buildFill(o, front, x, matchIdx))
		matchIdx++
		remaining -= x
		m.last[o.market] = front.price
		if m.consumeFront(o.market, front, x) {
			res.filled = append(res.filled, front.id)
		}
	}

	switch {
	case res.stp:
	case o.ordType == types.Market:
	case o.tif == types.IOC || o.tif == types.FOK:
	default:
		if remaining > 0 {
			m.restOrder(o, remaining)
			res.rested = true
			res.restedQty = remaining
		}
	}
	return res
}

func (m *Model) buildFill(o order, front *rorder, qty types.Qty, idx uint32) types.Fill {
	f := types.Fill{AggressorSeq: o.seq, MatchIndex: idx, Taker: o.side, Market: o.market, Price: front.price, Qty: qty}
	if o.side == types.Buy {
		f.BuyOrder, f.BuyAccount = o.id, o.acct
		f.SellOrder, f.SellAccount = front.id, front.acct
	} else {
		f.SellOrder, f.SellAccount = o.id, o.acct
		f.BuyOrder, f.BuyAccount = front.id, front.acct
	}
	return f
}

// ---- model book operations (mirror orderbook.Book) ----

func (m *Model) frontResting(market types.MarketID, side types.Side) *rorder {
	var best *rorder
	for _, ro := range *m.books[market] {
		if ro.side != side {
			continue
		}
		if best == nil {
			best = ro
			continue
		}
		better := false
		if side == types.Buy { // best bid: highest price, then oldest
			better = ro.price > best.price || (ro.price == best.price && ro.ins < best.ins)
		} else { // best ask: lowest price, then oldest
			better = ro.price < best.price || (ro.price == best.price && ro.ins < best.ins)
		}
		if better {
			best = ro
		}
	}
	return best
}

func (m *Model) matchableQty(market types.MarketID, restSide types.Side, own types.AccountID, crosses func(types.Price) bool, capAt types.Qty) types.Qty {
	var side []*rorder
	for _, ro := range *m.books[market] {
		if ro.side == restSide {
			side = append(side, ro)
		}
	}
	sort.Slice(side, func(i, j int) bool {
		a, b := side[i], side[j]
		if a.price != b.price {
			if restSide == types.Buy {
				return a.price > b.price // best bid first
			}
			return a.price < b.price // best ask first
		}
		return a.ins < b.ins
	})
	var sum types.Qty
	for _, ro := range side {
		if !crosses(ro.price) {
			break
		}
		if ro.acct == own {
			return sum
		}
		sum += ro.remaining
		if sum >= capAt {
			return sum
		}
	}
	return sum
}

func (m *Model) consumeFront(market types.MarketID, ro *rorder, qty types.Qty) (removed bool) {
	ro.display -= qty
	ro.remaining -= qty
	if ro.display > 0 {
		return false
	}
	if ro.hidden > 0 {
		refill := ro.peak
		if refill > ro.hidden {
			refill = ro.hidden
		}
		ro.hidden -= refill
		ro.display = refill
		ro.ins = m.nextIns() // re-queue at the tail (lose time priority)
		return false
	}
	m.removeRestingPtr(market, ro)
	return true
}

func (m *Model) restOrder(o order, remaining types.Qty) {
	display := remaining
	if o.flags.Has(types.FlagIceberg) && o.displayQty > 0 && o.displayQty < remaining {
		display = o.displayQty
	}
	book := m.books[o.market]
	*book = append(*book, &rorder{
		id: o.id, acct: o.acct, side: o.side, price: o.price,
		remaining: remaining, display: display, hidden: remaining - display, peak: display,
		ins: m.nextIns(),
	})
}

func (m *Model) removeRestingPtr(market types.MarketID, ro *rorder) {
	book := m.books[market]
	for i, r := range *book {
		if r == ro {
			*book = append((*book)[:i], (*book)[i+1:]...)
			return
		}
	}
}

func (m *Model) cancelFromBook(market types.MarketID, id types.OrderID) bool {
	book := m.books[market]
	for i, ro := range *book {
		if ro.id == id {
			*book = append((*book)[:i], (*book)[i+1:]...)
			return true
		}
	}
	stops := m.stops[market]
	for i := range stops {
		if stops[i].o.id == id {
			m.stops[market] = append(stops[:i], stops[i+1:]...)
			return true
		}
	}
	return false
}

func (m *Model) amendDownBook(market types.MarketID, id types.OrderID, newQty types.Qty) bool {
	for _, ro := range *m.books[market] {
		if ro.id != id {
			continue
		}
		if newQty <= 0 || newQty >= ro.remaining {
			return false
		}
		delta := ro.remaining - newQty
		if delta <= ro.hidden {
			ro.hidden -= delta
		} else {
			visibleDrop := delta - ro.hidden
			ro.hidden = 0
			ro.display -= visibleDrop
		}
		ro.remaining = newQty
		return true
	}
	return false
}

// ---- stops (mirror matching.Engine.triggerStops) ----

func (m *Model) triggerStops(market types.MarketID) {
	last, ok := m.last[market]
	stops := m.stops[market]
	if !ok || len(stops) == 0 {
		return
	}
	var triggered []order
	kept := stops[:0]
	for _, s := range stops {
		o := s.o
		var trig bool
		if o.side == types.Buy {
			trig = last >= o.stopPrice
		} else {
			trig = last <= o.stopPrice
		}
		if trig {
			triggered = append(triggered, o)
		} else {
			kept = append(kept, s)
		}
	}
	m.stops[market] = kept
	if len(triggered) == 0 {
		return
	}
	sort.Slice(triggered, func(i, j int) bool { return triggered[i].seq < triggered[j].seq })
	for _, o := range triggered {
		m.queue = append(m.queue, activationOrder(o))
	}
}

func activationOrder(o order) order {
	a := order{
		seq: o.seq, market: o.market, acct: o.acct, id: o.id, side: o.side,
		tif: o.tif, flags: o.flags, qty: o.qty, displayQty: o.displayQty,
	}
	if o.ordType == types.Stop {
		a.ordType = types.Market
	} else {
		a.ordType = types.Limit
		a.price = o.price
	}
	return a
}

// Snapshot returns the model's canonical state.
func (m *Model) Snapshot() State {
	bals := make([]Bal, 0, len(m.bal))
	for k, b := range m.bal {
		bals = append(bals, Bal{Acct: k.acct, Asset: k.asset, Available: b.available, Reserved: b.reserved})
	}
	fees := make([]Fee, 0, len(m.fees))
	for a, amt := range m.fees {
		fees = append(fees, Fee{Asset: a, Amount: amt})
	}
	var orders []Order
	for mkt, book := range m.books {
		for _, ro := range *book {
			orders = append(orders, Order{Market: mkt, Side: ro.side, Price: ro.price, ID: ro.id, Remaining: ro.remaining, Display: ro.display})
		}
	}
	return State{Bals: bals, Fees: fees, Orders: orders}
}
