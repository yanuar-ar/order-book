package market

import (
	"sort"

	"github.com/yanuar-ar/order-book/internal/balance"
	"github.com/yanuar-ar/order-book/internal/matching"
	"github.com/yanuar-ar/order-book/internal/sequencer"
	"github.com/yanuar-ar/order-book/internal/spsc"
	"github.com/yanuar-ar/order-book/internal/types"
	"github.com/yanuar-ar/order-book/internal/wal"
)

// openOrder tracks an order the core has reserved funds for, so cancel/amend
// and reservation release can be applied without querying book internals.
type openOrder struct {
	market  types.MarketID
	account types.AccountID
	side    types.Side
	ordType types.OrderType
	price   types.Price
	qty     types.Qty
}

// Core implements sequencer.Router: it reserves funds, routes funded orders to
// shards, settles fills inline, and manages the reservation lifecycle.
type Core struct {
	shards map[types.MarketID]*Shard
	ledger *balance.Ledger
	open   map[types.OrderID]openOrder
	acks   []types.Ack // captured for tests/publisher
}

// OnSettlement is unused in single-threaded mode (fills settle inline in
// OnCommand); it exists to satisfy the Router interface for the future
// concurrent topology.
func (c *Core) OnSettlement(f types.Fill) { c.ledger.Settle(f) }

// OnCommand processes one sequenced command.
func (c *Core) OnCommand(cmd types.Command) {
	switch cmd.Type {
	case types.CmdDeposit:
		c.ledger.Deposit(cmd.Account, cmd.Asset, cmd.Amount)
		c.ack(cmd, types.AckAccepted, types.ReasonNone)
	case types.CmdWithdraw:
		if c.ledger.Withdraw(cmd.Account, cmd.Asset, cmd.Amount) {
			c.ack(cmd, types.AckAccepted, types.ReasonNone)
		} else {
			c.ack(cmd, types.AckRejected, types.ReasonInsufficientFunds)
		}
	case types.CmdCancel:
		c.cancel(cmd)
	case types.CmdAmend:
		c.amend(cmd)
	case types.CmdNewOrder:
		c.newOrder(cmd)
	}
}

func fundedFrom(cmd types.Command) types.FundedOrder {
	return types.FundedOrder{
		Seq: cmd.Seq, Market: cmd.Market, Account: cmd.Account, OrderID: cmd.OrderID,
		Side: cmd.Side, OrdType: cmd.OrdType, Tif: cmd.Tif, Flags: cmd.Flags,
		Price: cmd.Price, StopPrice: cmd.StopPrice, Qty: cmd.Qty, DisplayQty: cmd.DisplayQty,
	}
}

func (c *Core) newOrder(cmd types.Command) {
	funded := fundedFrom(cmd)
	_, isActivation := c.open[cmd.OrderID]

	if !isActivation {
		if reason, ok := c.ledger.Reserve(funded); !ok {
			c.ack(cmd, types.AckRejected, reason)
			return
		}
	}

	sh := c.shards[cmd.Market]
	if isActivation {
		// Remove the now-activated stop from the shard's pending table. In live
		// mode triggerStops already popped it (no-op here); in replay mode stops
		// are suppressed, so this clears the stale pending entry — keeping live
		// and replayed state identical.
		sh.Cancel(cmd.OrderID)
	}
	res := sh.Submit(funded)

	if res.Pending { // stop / stop-limit stored, reservation held
		c.open[cmd.OrderID] = openOrder{market: cmd.Market, account: cmd.Account, side: cmd.Side, ordType: cmd.OrdType, price: cmd.Price, qty: cmd.Qty}
		return
	}
	if res.Rejected { // post-only cross / FOK unfillable: release reservation
		c.ledger.Release(cmd.OrderID)
		delete(c.open, cmd.OrderID)
		c.ack(cmd, types.AckRejected, res.Reason)
		return
	}

	// Settle fills inline, in match (MatchIndex) order.
	for _, f := range res.Fills {
		c.ledger.Settle(f)
	}
	// Makers fully consumed: release their leftover reservations.
	for _, mid := range res.Filled {
		c.ledger.Release(mid)
		delete(c.open, mid)
	}
	// Aggressor disposition.
	if res.Rested {
		c.open[cmd.OrderID] = openOrder{market: cmd.Market, account: cmd.Account, side: cmd.Side, ordType: types.Limit, price: cmd.Price, qty: res.RestedQty}
	} else {
		c.ledger.Release(cmd.OrderID)
		delete(c.open, cmd.OrderID)
	}
	c.ack(cmd, types.AckAccepted, types.ReasonNone)
}

func (c *Core) cancel(cmd types.Command) {
	oo, ok := c.open[cmd.OrderID]
	if !ok {
		c.ack(cmd, types.AckRejected, types.ReasonUnknownOrder)
		return
	}
	c.shards[oo.market].Cancel(cmd.OrderID)
	c.ledger.Release(cmd.OrderID)
	delete(c.open, cmd.OrderID)
	c.ack(cmd, types.AckCanceled, types.ReasonNone)
}

func (c *Core) amend(cmd types.Command) {
	oo, ok := c.open[cmd.OrderID]
	if !ok {
		c.ack(cmd, types.AckRejected, types.ReasonUnknownOrder)
		return
	}
	sh := c.shards[oo.market]
	// Quantity decrease at the same price: amend in place, keep time priority.
	if cmd.Price == oo.price && cmd.Qty < oo.qty {
		if sh.AmendDown(cmd.OrderID, cmd.Qty) {
			c.ledger.AmendReduce(cmd.OrderID, oo.side, oo.price, cmd.Qty)
			oo.qty = cmd.Qty
			c.open[cmd.OrderID] = oo
			c.ack(cmd, types.AckAccepted, types.ReasonNone)
			return
		}
		c.ack(cmd, types.AckRejected, types.ReasonUnknownOrder)
		return
	}
	// Price change or quantity increase: cancel and re-submit (new priority).
	sh.Cancel(cmd.OrderID)
	c.ledger.Release(cmd.OrderID)
	delete(c.open, cmd.OrderID)
	repl := cmd
	repl.Type = types.CmdNewOrder
	repl.OrdType = oo.ordType
	c.newOrder(repl)
}

func (c *Core) ack(cmd types.Command, status types.AckStatus, reason types.RejectReason) {
	c.acks = append(c.acks, types.Ack{Seq: cmd.Seq, OrderID: cmd.OrderID, Account: cmd.Account, Status: status, Reason: reason, ClientTsNanos: cmd.ClientTsNanos})
}

// reinjectSink forwards stop activations to the sequencer's re-injection entry.
type reinjectSink struct{ seq *sequencer.Sequencer }

func (r reinjectSink) Emit(c types.Command) { r.seq.Inject(c) }

// Engine is the assembled single-node engine.
type Engine struct {
	seq     *sequencer.Sequencer
	core    *Core
	ingress *spsc.RingCommand
}

// Config wires an engine.
type Config struct {
	Markets  map[types.MarketID]balance.MarketSpec
	QtyScale int64
	FeeScale int64
	MakerFee int64
	TakerFee int64
	RingSize uint64
	Journal  sequencer.Journal   // nil -> no-op (in-memory) journal
	Clock    sequencer.ClockFunc // nil -> deterministic counter
	CapHint  int
	// SuppressStops installs a no-op activation sink. Set during replay, where
	// stop activations are read from the WAL rather than regenerated.
	SuppressStops bool
}

type noopJournal struct{}

func (noopJournal) Append(wal.Record) error { return nil }

type noopSink struct{}

func (noopSink) Emit(types.Command) {}

func counterClock() sequencer.ClockFunc {
	var n int64
	return func() int64 { n++; return n }
}

// NewEngine assembles the sequencer, balance ledger, and one shard per market.
func NewEngine(cfg Config) *Engine {
	if cfg.RingSize == 0 {
		cfg.RingSize = 1 << 16
	}
	if cfg.CapHint == 0 {
		cfg.CapHint = 1024
	}
	if cfg.Journal == nil {
		cfg.Journal = noopJournal{}
	}
	if cfg.Clock == nil {
		cfg.Clock = counterClock()
	}

	ledger := balance.New(balance.Config{
		QtyScale: cfg.QtyScale, FeeScale: cfg.FeeScale,
		MakerFee: cfg.MakerFee, TakerFee: cfg.TakerFee, Markets: cfg.Markets,
	})
	shards := make(map[types.MarketID]*Shard, len(cfg.Markets))
	for m := range cfg.Markets {
		shards[m] = NewShard(m, cfg.CapHint)
	}
	core := &Core{shards: shards, ledger: ledger, open: make(map[types.OrderID]openOrder, 1024)}

	ingress := spsc.NewCommand(cfg.RingSize)
	reinject := spsc.NewCommand(cfg.RingSize)
	seq := sequencer.New(sequencer.Config{
		Reinject: reinject,
		Inputs:   []*spsc.RingCommand{ingress},
		Journal:  cfg.Journal,
		Router:   core,
		Clock:    cfg.Clock,
	})
	var sink matching.Sink = reinjectSink{seq: seq}
	if cfg.SuppressStops {
		sink = noopSink{}
	}
	for _, sh := range shards {
		sh.SetSink(sink)
	}
	return &Engine{seq: seq, core: core, ingress: ingress}
}

// ApplyJournaled applies a command read from the WAL directly to the core,
// preserving its original Seq and bypassing re-sequencing. Used by replay.
func (e *Engine) ApplyJournaled(cmd types.Command) { e.core.OnCommand(cmd) }

// EnableStops re-installs the live stop-activation sink, used after a replay
// (which ran with stops suppressed) to resume normal operation.
func (e *Engine) EnableStops() {
	for _, sh := range e.core.shards {
		sh.SetSink(reinjectSink{seq: e.seq})
	}
}

// Submit pushes a command onto the ingress ring. Returns false if full.
func (e *Engine) Submit(c types.Command) bool { return e.ingress.Push(c) }

// Step performs one sequencer tick.
func (e *Engine) Step() bool { return e.seq.Step() }

// Drain steps until no work remains (ingress drained, all activations settled).
func (e *Engine) Drain() {
	for e.seq.Step() {
	}
}

// Ledger exposes the balance ledger (read access for invariants/tests).
func (e *Engine) Ledger() *balance.Ledger { return e.core.ledger }

// Shard returns the shard for a market.
func (e *Engine) Shard(m types.MarketID) *Shard { return e.core.shards[m] }

// MarketIDs returns the engine's market IDs in ascending order.
func (e *Engine) MarketIDs() []types.MarketID {
	ids := make([]types.MarketID, 0, len(e.core.shards))
	for m := range e.core.shards {
		ids = append(ids, m)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// Acks returns the acks captured so far.
func (e *Engine) Acks() []types.Ack { return e.core.acks }

// Seq returns the last assigned sequence number.
func (e *Engine) Seq() types.Seq { return e.seq.Seq() }
