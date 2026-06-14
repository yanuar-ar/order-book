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

// shardOps is the matching surface Core depends on. The serial engine binds it
// to a local *Shard (inline matching); the parallel engine binds it to a remote
// shard that dispatches matching to a worker goroutine. Either way Core drives
// operations in strict Seq order, so behavior is identical.
type shardOps interface {
	Submit(types.FundedOrder) matching.Result
	Cancel(types.OrderID) bool
	AmendDown(types.OrderID, types.Qty) bool
	LastPrice() (types.Price, bool)
}

// Core implements sequencer.Router: it reserves funds, routes funded orders to
// shards, settles fills inline, and manages the reservation lifecycle.
type Core struct {
	shards   map[types.MarketID]shardOps
	ledger   *balance.Ledger
	open     map[types.OrderID]openOrder
	acks     []types.Ack // captured for tests/publisher
	filters  map[types.MarketID]types.MarketFilters
	qtyScale int64
	// degraded is the replication ack-gate mode: when true, acks gate on
	// durability alone (the standby requirement is dropped). It is flipped by the
	// CmdDegradeToSolo / CmdRearm control records, so it is reconstructed
	// deterministically on replay; it is output-side only and is NOT part of the
	// state fingerprint.
	degraded bool
	// syncRep is true only in "sync" replication mode, where acks must wait for the
	// standby (gate on min(durableSeq, replicatedSeq)). In "async" (and "off") mode
	// the replicator still streams but acks release on durability alone — replication
	// is off the critical path with bounded lag. Set once at construction.
	syncRep bool
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
	case types.CmdDegradeToSolo:
		c.degraded = true // drop the replication requirement (output-side only)
		c.ack(cmd, types.AckAccepted, types.ReasonNone)
	case types.CmdRearm:
		c.degraded = false // restore sync gating
		c.ack(cmd, types.AckAccepted, types.ReasonNone)
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
		// Validate static order filters before any reservation or book mutation,
		// so a rejected order leaves zero state change. Stop activations re-enter
		// here with isActivation true and are not re-validated (validated once at
		// submit). Markets without a configured filter set skip validation.
		if f, ok := c.filters[cmd.Market]; ok {
			last, hasLast := c.shards[cmd.Market].LastPrice()
			if reason := f.ValidateNew(funded, c.qtyScale, last, hasLast); reason != types.ReasonNone {
				c.ack(cmd, types.AckRejected, reason)
				return
			}
		}
		// A market buy bounds its spend to the reservable quote budget.
		if funded.OrdType == types.Market && funded.Side == types.Buy {
			funded.MaxQuote = c.ledger.MarketBuyBudget(funded.Account, funded.Market)
		}
		if reason, ok := c.ledger.Reserve(funded); !ok {
			c.ack(cmd, types.AckRejected, reason)
			return
		}
	} else if funded.OrdType == types.Market && funded.Side == types.Buy {
		// Activated stop-market buy reuses its existing reservation's budget.
		funded.MaxQuote = c.ledger.OrderBudget(funded.OrderID)
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
		// Re-validate the reduced quantity so an amend can't leave a resting order
		// off-lot or below the notional floor. Price is unchanged. On reject the
		// order keeps its prior quantity.
		if f, ok := c.filters[oo.market]; ok {
			if reason := f.ValidateAmendDown(oo.price, cmd.Qty, c.qtyScale); reason != types.ReasonNone {
				c.ack(cmd, types.AckRejected, reason)
				return
			}
		}
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
	impls   map[types.MarketID]*Shard // concrete shards for book access
	// journaller is the async journaller goroutine handle (nil for the sync
	// journaller); Close stops it. Durability is forced through the sequencer
	// (SyncJournal), not this handle.
	journaller sequencer.Journaller
	// replicator streams to the hot standby (nil when replication is off); Close
	// stops it. standby is the shadow engine it drives (nil when off), exposed for
	// fingerprint/invariant comparison and promotion.
	replicator sequencer.Replicator
	standby    *Standby
	cfg        Config // retained for the snapshot header (scales, markets)
}

// Config wires an engine.
type Config struct {
	Markets  map[types.MarketID]balance.MarketSpec
	Filters  map[types.MarketID]types.MarketFilters
	QtyScale int64
	FeeScale int64
	MakerFee int64
	TakerFee int64
	RingSize uint64
	Journal  sequencer.Journal   // nil -> no-op (in-memory) journal
	Clock    sequencer.ClockFunc // nil -> deterministic counter
	CapHint  int
	// FlushCap overrides the group-commit batch ceiling (0 -> sequencer default).
	// On the durable path it sets how many commands amortize one fsync.
	FlushCap int
	// AsyncJournal moves WAL Append + fsync onto a dedicated journaller goroutine
	// (the LMAX "Journaller") so durability never stalls the matcher. Off by
	// default; the synchronous inline journaller is used when false.
	AsyncJournal bool
	// JournalRing is the async journal ring capacity (power of two; 0 -> default).
	JournalRing uint64
	// JournalBatchCap is the async group-commit ceiling (0 -> default). On the
	// durable path it amortizes the fsync over this many commands.
	JournalBatchCap int
	// JournalCore pins the async journaller goroutine to a core; <= 0 disables
	// pinning (the default, and always a no-op on non-Linux).
	JournalCore int
	// ReplicationMode selects the hot-standby replicator: "off" (the default, no
	// standby), "sync" (acks gate on the standby's replicated watermark), or
	// "async" (stream off the critical path, bounded lag). Wired by buildReplicator
	// (U5); the ack gate consumes the watermark in U3.
	ReplicationMode string
	// ReplicationRing is the replicator hand-off ring capacity (power of two; 0 ->
	// default). ReplicationCore pins the replicator consumer goroutine (<= 0
	// disables, the default).
	ReplicationRing uint64
	ReplicationCore int
	// WALDir is the primary's WAL directory, used by the in-process replicator
	// link to backfill overflow gaps (records the live ring dropped under
	// backpressure are re-read from the WAL). Optional; empty disables backfill.
	WALDir string
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

	ledger := balance.New(balanceConfig(cfg))
	impls := make(map[types.MarketID]*Shard, len(cfg.Markets))
	shards := make(map[types.MarketID]shardOps, len(cfg.Markets))
	for m := range cfg.Markets {
		s := NewShard(m, cfg.CapHint, cfg.QtyScale)
		impls[m] = s
		shards[m] = s
	}
	core := &Core{shards: shards, ledger: ledger, open: make(map[types.OrderID]openOrder, 1024), filters: cfg.Filters, qtyScale: cfg.QtyScale, syncRep: cfg.ReplicationMode == "sync"}

	ingress := spsc.NewCommand(cfg.RingSize)
	reinject := spsc.NewCommand(cfg.RingSize)
	journaller := buildJournaller(cfg)
	replicator, standby := buildReplicator(cfg)
	seq := sequencer.New(sequencer.Config{
		Reinject:   reinject,
		Inputs:     []*spsc.RingCommand{ingress},
		Journal:    cfg.Journal,
		Journaller: journaller, // nil -> sequencer wraps Journal in a SyncJournaller
		Replicator: replicator, // nil -> sequencer uses NopReplicator
		Router:     core,
		Clock:      cfg.Clock,
		FlushCap:   cfg.FlushCap,
	})
	var sink matching.Sink = reinjectSink{seq: seq}
	if cfg.SuppressStops {
		sink = noopSink{}
	}
	for _, s := range impls {
		s.SetSink(sink)
	}
	return &Engine{seq: seq, core: core, ingress: ingress, impls: impls, journaller: journaller, replicator: replicator, standby: standby, cfg: cfg}
}

// buildJournaller returns an AsyncJournaller when cfg.AsyncJournal is set, else
// nil (the sequencer then defaults to inline SyncJournaller). Shared by the
// serial and parallel assemblies so both topologies journal identically.
func buildJournaller(cfg Config) sequencer.Journaller {
	if !cfg.AsyncJournal {
		return nil
	}
	core := cfg.JournalCore
	if core <= 0 {
		core = -1 // no pin
	}
	return sequencer.NewAsyncJournaller(cfg.Journal, cfg.JournalRing, cfg.JournalBatchCap, core)
}

// Close stops the async journaller goroutine (flushing anything pending) and
// must be called before the host closes the underlying Journal. A no-op for the
// sync journaller. Idempotent only once — call exactly once at shutdown.
func (e *Engine) Close() error {
	var err error
	if e.journaller != nil {
		err = e.journaller.Close()
	}
	if e.replicator != nil {
		if rerr := e.replicator.Close(); rerr != nil && err == nil {
			err = rerr
		}
	}
	return err
}

// Standby returns the hot-standby shadow engine (nil when replication is off),
// for fingerprint/invariant comparison and promotion.
func (e *Engine) Standby() *Standby { return e.standby }

// Degraded reports whether the replication ack-gate is in solo mode (acks gate on
// durability alone). Output-side state; reconstructed deterministically on replay.
func (e *Engine) Degraded() bool { return e.core.degraded }

// balanceConfig derives the ledger config from the engine config.
func balanceConfig(cfg Config) balance.Config {
	return balance.Config{
		QtyScale: cfg.QtyScale, FeeScale: cfg.FeeScale,
		MakerFee: cfg.MakerFee, TakerFee: cfg.TakerFee, Markets: cfg.Markets,
	}
}

// ApplyJournaled applies a command read from the WAL directly to the core,
// preserving its original Seq and bypassing re-sequencing. Used by replay.
func (e *Engine) ApplyJournaled(cmd types.Command) { e.core.OnCommand(cmd) }

// EnableStops re-installs the live stop-activation sink, used after a replay
// (which ran with stops suppressed) to resume normal operation.
func (e *Engine) EnableStops() {
	for _, sh := range e.impls {
		sh.SetSink(reinjectSink{seq: e.seq})
	}
}

// Submit pushes a command onto the ingress ring. Returns false if full.
func (e *Engine) Submit(c types.Command) bool { return e.ingress.Push(c) }

// Step performs one sequencer tick.
func (e *Engine) Step() bool { return e.seq.Step() }

// Drain steps until no work remains (ingress drained, all activations settled),
// then blocks until the journaller has made every appended command durable so
// durableSeq == Seq and drain-then-read callers see every ack. A journaller
// failure during the wait latches Fatal().
func (e *Engine) Drain() {
	for e.seq.Step() {
	}
	_ = e.seq.DrainJournal()
}

// DrainStandby blocks until the hot standby has durably applied every replicated
// command so far (a no-op when replication is off). It is the graceful standby
// catch-up — distinct from Drain (primary durability) and Close (abrupt stop that
// abandons any lag). Call it after Drain at quiesce points where the standby must
// be converged: snapshot, promotion, or a convergence assertion.
func (e *Engine) DrainStandby() error { return e.seq.DrainReplication() }

// Ledger exposes the balance ledger (read access for invariants/tests).
func (e *Engine) Ledger() *balance.Ledger { return e.core.ledger }

// Shard returns the shard for a market.
func (e *Engine) Shard(m types.MarketID) *Shard { return e.impls[m] }

// Filters returns the per-market order filters (read access for invariants).
func (e *Engine) Filters() map[types.MarketID]types.MarketFilters { return e.core.filters }

// MarketIDs returns the engine's market IDs in ascending order.
func (e *Engine) MarketIDs() []types.MarketID {
	ids := make([]types.MarketID, 0, len(e.core.shards))
	for m := range e.core.shards {
		ids = append(ids, m)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// Acks returns the durable acks: those at or below the WAL durability watermark.
// Acks above durableSeq are speculative (their command is matched in-memory but
// not yet on disk) and must not be observed until a flush makes them durable.
// After Drain the watermark equals Seq, so every ack is released.
func (e *Engine) Acks() []types.Ack { return releasedAcks(e.core.acks, releaseGate(e.seq, e.core)) }

// releaseGate is the highest Seq whose ack is safe to release. It gates on
// min(durableSeq, replicatedSeq) ONLY in sync replication mode and only while not
// degraded — a command is then confirmed once durable AND replicated. In async
// mode (replicator streams but off the critical path), off mode, or once a
// degrade-to-solo is in effect, the gate is durableSeq alone. The syncRep and
// degraded flags live on the Core (degraded replays deterministically); the
// watermarks live on the shared sequencer, so serial and parallel gate identically.
func releaseGate(seq *sequencer.Sequencer, core *Core) types.Seq {
	if !core.syncRep || core.degraded {
		return seq.DurableSeq()
	}
	return seq.ReleaseSeq()
}

// AcksAll returns every ack appended so far, ungated by durability — for
// benchmark harnesses that track the durable watermark (DurableSeq) themselves
// with their own cursor, avoiding the O(prefix) rescan that Acks() does each
// call. Production callers must use Acks().
func (e *Engine) AcksAll() []types.Ack { return e.core.acks }

// releasedAcks returns the prefix of acks whose Seq is at or below durable. Acks
// are appended in ascending Seq order (one per command, in route order, with
// resting-stop commands simply contributing none), so the durable set is a
// contiguous prefix.
func releasedAcks(acks []types.Ack, durable types.Seq) []types.Ack {
	n := 0
	for n < len(acks) && acks[n].Seq <= durable {
		n++
	}
	return acks[:n]
}

// Seq returns the last assigned sequence number.
func (e *Engine) Seq() types.Seq { return e.seq.Seq() }

// Fatal returns the latched terminal WAL-durability failure, or nil. The host
// run loop checks this after each Step; the snapshotter checks it after Drain.
func (e *Engine) Fatal() error { return e.seq.Fatal() }

// ReplicatorFatal returns a latched standby-replication failure (or nil). It does
// not halt the engine; the host should log it so an operator can intervene
// (degrade-to-solo).
func (e *Engine) ReplicatorFatal() error { return e.seq.ReplicatorFatal() }

// DurableSeq returns the highest Seq whose WAL bytes have been fsynced.
func (e *Engine) DurableSeq() types.Seq { return e.seq.DurableSeq() }

// ReleasedSeq returns the highest Seq whose ack is releasable — the gate Acks()
// uses. It equals DurableSeq when replication is off (or degraded), and
// min(durableSeq, replicatedSeq) in sync mode (a command is released only once
// durable AND replicated). Harnesses cursor on it to measure ack-release latency.
func (e *Engine) ReleasedSeq() types.Seq { return releaseGate(e.seq, e.core) }

// SetSeq primes the sequencer watermark. Used by snapshot restore (before live
// stepping resumes) so post-restore commands continue contiguously.
func (e *Engine) SetSeq(s types.Seq) { e.seq.SetSeq(s) }

// Epoch returns the current leadership term (0 until the first promotion).
func (e *Engine) Epoch() uint64 { return e.seq.Epoch() }

// SetEpoch primes the leadership term. Used by snapshot restore (to the
// snapshot's epoch) and promotion (incremented). Quiesced-only, like SetSeq.
func (e *Engine) SetEpoch(epoch uint64) { e.seq.SetEpoch(epoch) }

// SyncJournal forces durability of journaled records through the current Seq. A
// snapshot must be published only after the WAL is durable through its
// watermark. It routes through the journaller (never touching the WAL writer
// directly) so it is safe under the async journaller, where the writer is owned
// by the consumer goroutine; the in-memory no-op journal has nothing to flush
// and reports success.
func (e *Engine) SyncJournal() error {
	return e.seq.DrainJournal()
}
