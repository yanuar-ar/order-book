// Package refmodel is a slow-but-obviously-correct reference implementation of
// the spot engine, used as the differential oracle in tests/property. It reuses
// internal/types money helpers (separately unit-tested) so settlement and
// reservation amounts match the engine exactly, but reimplements matching,
// booking, the reservation lifecycle, stops, and self-trade prevention with
// plain slices and maps. It models per-command state plus the cumulative fill
// effect (balances and fees); exact per-fill ordering is checked on the engine
// alone (determinism tests), not here.
package refmodel

import (
	"github.com/yanuar-ar/order-book/internal/types"
)

// MarketSpec maps a market to its base and quote assets.
type MarketSpec struct {
	Base  types.AssetID
	Quote types.AssetID
}

// Config carries the scales, fee rates, and market specs (mirrors balance.Config).
type Config struct {
	QtyScale int64
	FeeScale int64
	MakerFee int64
	TakerFee int64
	Markets  map[types.MarketID]MarketSpec
	// Filters mirrors market.Config.Filters: per-market order filters validated at
	// submit. A market with no entry skips validation (same as the engine).
	Filters map[types.MarketID]types.MarketFilters
}

type acctKey struct {
	acct  types.AccountID
	asset types.AssetID
}

type balance struct {
	available int64
	reserved  int64
}

type reservation struct {
	acct      types.AccountID
	asset     types.AssetID
	remaining int64
	side      types.Side
}

// rorder is one resting order in the model's book.
type rorder struct {
	id        types.OrderID
	acct      types.AccountID
	side      types.Side
	price     types.Price
	remaining types.Qty
	display   types.Qty
	hidden    types.Qty
	peak      types.Qty
	ins       uint64 // insertion sequence for FIFO ordering within a price
}

// stopRec is a pending stop order.
type stopRec struct {
	o order
}

// openMeta mirrors market.Core.open: enough to cancel/amend and detect activation.
type openMeta struct {
	market  types.MarketID
	acct    types.AccountID
	side    types.Side
	ordType types.OrderType
	price   types.Price
	qty     types.Qty
}

// order is the model's internal funded-order shape.
type order struct {
	seq        types.Seq
	market     types.MarketID
	acct       types.AccountID
	id         types.OrderID
	side       types.Side
	ordType    types.OrderType
	tif        types.TIF
	flags      types.Flags
	price      types.Price
	stopPrice  types.Price
	qty        types.Qty
	displayQty types.Qty
	maxQuote   int64
}

// funded renders the model's internal order as a types.FundedOrder, so filter
// validation runs through the exact same code path as the engine.
func (o order) funded() types.FundedOrder {
	return types.FundedOrder{
		Seq: o.seq, Market: o.market, Account: o.acct, OrderID: o.id,
		Side: o.side, OrdType: o.ordType, Tif: o.tif, Flags: o.flags,
		Price: o.price, StopPrice: o.stopPrice, Qty: o.qty, DisplayQty: o.displayQty,
	}
}

func orderFrom(c types.Command) order {
	return order{
		seq: c.Seq, market: c.Market, acct: c.Account, id: c.OrderID,
		side: c.Side, ordType: c.OrdType, tif: c.Tif, flags: c.Flags,
		price: c.Price, stopPrice: c.StopPrice, qty: c.Qty, displayQty: c.DisplayQty,
	}
}

// Model is the reference engine. Not safe for concurrent use.
type Model struct {
	cfg   Config
	bal   map[acctKey]balance
	fees  map[types.AssetID]int64
	res   map[types.OrderID]reservation
	books map[types.MarketID]*[]*rorder
	last  map[types.MarketID]types.Price
	stops map[types.MarketID][]stopRec
	open  map[types.OrderID]openMeta
	queue []order // FIFO stop-activation queue (mirrors the reinject ring)
	ins   uint64
}

// New returns an empty model.
func New(cfg Config) *Model {
	m := &Model{
		cfg:   cfg,
		bal:   map[acctKey]balance{},
		fees:  map[types.AssetID]int64{},
		res:   map[types.OrderID]reservation{},
		books: map[types.MarketID]*[]*rorder{},
		last:  map[types.MarketID]types.Price{},
		stops: map[types.MarketID][]stopRec{},
		open:  map[types.OrderID]openMeta{},
	}
	for mkt := range cfg.Markets {
		s := []*rorder{}
		m.books[mkt] = &s
	}
	return m
}

// ---- balance helpers ----

func (m *Model) avail(a types.AccountID, asset types.AssetID) int64 {
	return m.bal[acctKey{a, asset}].available
}

func (m *Model) move(a types.AccountID, asset types.AssetID, dAvail, dRes int64) {
	k := acctKey{a, asset}
	b := m.bal[k]
	b.available += dAvail
	b.reserved += dRes
	m.bal[k] = b
}

// ---- reservation lifecycle (mirrors balance.Ledger) ----

func (m *Model) reserve(o order) bool {
	if _, dup := m.res[o.id]; dup {
		return false
	}
	spec := m.cfg.Markets[o.market]
	if o.side == types.Buy {
		var cost int64
		if o.ordType == types.Market || o.ordType == types.Stop {
			cost = m.avail(o.acct, spec.Quote)
			if cost <= 0 {
				return false
			}
		} else {
			notional, ok := types.Notional(o.price, o.qty, m.cfg.QtyScale, true)
			if !ok {
				return false
			}
			fee, ok := types.Fee(notional, m.cfg.TakerFee, m.cfg.FeeScale, true)
			if !ok {
				return false
			}
			cost = notional + fee
		}
		if m.avail(o.acct, spec.Quote) < cost {
			return false
		}
		m.move(o.acct, spec.Quote, -cost, cost)
		m.res[o.id] = reservation{acct: o.acct, asset: spec.Quote, remaining: cost, side: types.Buy}
		return true
	}
	need := int64(o.qty)
	if m.avail(o.acct, spec.Base) < need {
		return false
	}
	m.move(o.acct, spec.Base, -need, need)
	m.res[o.id] = reservation{acct: o.acct, asset: spec.Base, remaining: need, side: types.Sell}
	return true
}

func (m *Model) settle(f types.Fill) {
	spec := m.cfg.Markets[f.Market]
	notional, _ := types.Notional(f.Price, f.Qty, m.cfg.QtyScale, false)
	buyerRate, sellerRate := m.cfg.MakerFee, m.cfg.TakerFee
	if f.Taker == types.Buy {
		buyerRate, sellerRate = m.cfg.TakerFee, m.cfg.MakerFee
	}
	buyerFee, _ := types.Fee(notional, buyerRate, m.cfg.FeeScale, false)
	sellerFee, _ := types.Fee(notional, sellerRate, m.cfg.FeeScale, false)

	m.move(f.BuyAccount, spec.Quote, 0, -(notional + buyerFee))
	m.move(f.BuyAccount, spec.Base, int64(f.Qty), 0)
	if r, ok := m.res[f.BuyOrder]; ok {
		r.remaining -= notional + buyerFee
		m.res[f.BuyOrder] = r
	}

	m.move(f.SellAccount, spec.Base, 0, -int64(f.Qty))
	m.move(f.SellAccount, spec.Quote, notional-sellerFee, 0)
	if r, ok := m.res[f.SellOrder]; ok {
		r.remaining -= int64(f.Qty)
		m.res[f.SellOrder] = r
	}

	m.fees[spec.Quote] += buyerFee + sellerFee
}

func (m *Model) budgetFromQuote(q int64) int64 {
	if q <= 0 {
		return 0
	}
	b, ok := types.MulDiv(q, m.cfg.FeeScale, m.cfg.FeeScale+m.cfg.TakerFee, false)
	if !ok {
		return q
	}
	return b
}

func (m *Model) marketBuyBudget(acct types.AccountID, market types.MarketID) int64 {
	return m.budgetFromQuote(m.avail(acct, m.cfg.Markets[market].Quote))
}

func (m *Model) orderBudget(id types.OrderID) int64 {
	r, ok := m.res[id]
	if !ok {
		return 0
	}
	return m.budgetFromQuote(r.remaining)
}

func (m *Model) amendReduce(id types.OrderID, side types.Side, price types.Price, newQty types.Qty) {
	r, ok := m.res[id]
	if !ok {
		return
	}
	var newReq int64
	if side == types.Buy {
		notional, ok := types.Notional(price, newQty, m.cfg.QtyScale, true)
		if !ok {
			return
		}
		fee, _ := types.Fee(notional, m.cfg.TakerFee, m.cfg.FeeScale, true)
		newReq = notional + fee
	} else {
		newReq = int64(newQty)
	}
	if r.remaining > newReq {
		rel := r.remaining - newReq
		m.move(r.acct, r.asset, rel, -rel)
		r.remaining = newReq
		m.res[id] = r
	}
}

func (m *Model) release(id types.OrderID) {
	r, ok := m.res[id]
	if !ok {
		return
	}
	if r.remaining > 0 {
		m.move(r.acct, r.asset, r.remaining, -r.remaining)
	}
	delete(m.res, id)
}
