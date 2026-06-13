package market

import (
	"runtime"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/yanuar-ar/order-book/internal/balance"
	"github.com/yanuar-ar/order-book/internal/matching"
	"github.com/yanuar-ar/order-book/internal/platform"
	"github.com/yanuar-ar/order-book/internal/sequencer"
	"github.com/yanuar-ar/order-book/internal/spsc"
	"github.com/yanuar-ar/order-book/internal/types"
)

// ParallelEngine offloads matching to per-worker goroutines (configurable
// market-to-worker assignment) while keeping sequencing and the balance
// authority single-writer on the control path. Each worker owns its markets'
// books exclusively; the control path owns the ledger, sequencer, and
// reinjection. Control drives operations in strict Seq order, blocking on each
// worker result, so the produced state is identical to the serial Engine —
// verified by digest-equality in tests. The win is that matching runs on the
// worker cores; raw single-market throughput is still bounded by the serial
// balance authority (shared balance cannot be parallelized without optimistic
// concurrency).
type ParallelEngine struct {
	seq     *sequencer.Sequencer
	core    *Core
	ingress *spsc.RingCommand
	impls   map[types.MarketID]*Shard

	workers []*worker
	stop    atomic.Bool
	wg      sync.WaitGroup
}

// matching request kinds dispatched to a worker.
const (
	reqSubmit uint8 = iota
	reqCancel
	reqAmend
	reqLastPrice
)

type wreq struct {
	kind   uint8
	market types.MarketID
	funded types.FundedOrder
	id     types.OrderID
	qty    types.Qty
}

type wresp struct {
	result matching.Result
	ok     bool
	price  types.Price // reqLastPrice result
	acts   []types.Command
}

// collector is a per-worker stop-activation sink; activations are returned to
// the control path, which injects them (single-writer reinjection).
type collector struct{ cmds []types.Command }

func (c *collector) Emit(cmd types.Command) { c.cmds = append(c.cmds, cmd) }
func (c *collector) drain() []types.Command {
	if len(c.cmds) == 0 {
		return nil
	}
	out := c.cmds
	c.cmds = nil
	return out
}

type worker struct {
	reqs   *spsc.Ring[wreq]
	resps  *spsc.Ring[wresp]
	shards map[types.MarketID]*Shard
	coll   *collector
	stop   *atomic.Bool
}

func (w *worker) run(coreIdx int) {
	_ = platform.PinCurrentThread(coreIdx)
	defer platform.Unpin()
	for !w.stop.Load() {
		var req wreq
		if !w.reqs.Pop(&req) {
			runtime.Gosched()
			continue
		}
		sh := w.shards[req.market]
		var resp wresp
		switch req.kind {
		case reqSubmit:
			r := sh.Submit(req.funded)
			// Copy the engine's reused Fills/Filled buffers: they alias the
			// worker's matching engine and would be overwritten on its next
			// Submit before the control path finishes reading the response.
			if len(r.Fills) > 0 {
				r.Fills = append([]types.Fill(nil), r.Fills...)
			}
			if len(r.Filled) > 0 {
				r.Filled = append([]types.OrderID(nil), r.Filled...)
			}
			resp.result = r
			resp.acts = w.coll.drain()
		case reqCancel:
			resp.ok = sh.Cancel(req.id)
		case reqAmend:
			resp.ok = sh.AmendDown(req.id, req.qty)
		case reqLastPrice:
			resp.price, resp.ok = sh.LastPrice()
		}
		for !w.resps.Push(resp) {
			// resp ring is sized so this never blocks in practice (control has
			// at most one op outstanding); spin defensively.
		}
	}
}

// remoteShard implements shardOps by dispatching to a worker and blocking for
// the result. All its methods run on the control goroutine, so its pushes to
// the reinjection ring have a single producer.
type remoteShard struct {
	market   types.MarketID
	reqs     *spsc.Ring[wreq]
	resps    *spsc.Ring[wresp]
	reinject *spsc.RingCommand
}

func (r *remoteShard) call(req wreq) wresp {
	for !r.reqs.Push(req) {
	}
	var resp wresp
	for !r.resps.Pop(&resp) {
	}
	for _, c := range resp.acts {
		for !r.reinject.Push(c) {
		}
	}
	return resp
}

func (r *remoteShard) Submit(o types.FundedOrder) matching.Result {
	return r.call(wreq{kind: reqSubmit, market: r.market, funded: o}).result
}
func (r *remoteShard) Cancel(id types.OrderID) bool {
	return r.call(wreq{kind: reqCancel, market: r.market, id: id}).ok
}
func (r *remoteShard) AmendDown(id types.OrderID, qty types.Qty) bool {
	return r.call(wreq{kind: reqAmend, market: r.market, id: id, qty: qty}).ok
}
func (r *remoteShard) LastPrice() (types.Price, bool) {
	resp := r.call(wreq{kind: reqLastPrice, market: r.market})
	return resp.price, resp.ok
}

// NewParallelEngine builds the engine with matching offloaded to workers per
// the groups assignment (e.g., [][]MarketID{{0},{1,2}} = market 0 isolated on
// worker 0, markets 1&2 sharing worker 1). Workers are pinned to core = group
// index. A nil/empty groups assigns each market its own worker.
func NewParallelEngine(cfg Config, groups [][]types.MarketID) *ParallelEngine {
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
	if len(groups) == 0 {
		for m := range cfg.Markets {
			groups = append(groups, []types.MarketID{m})
		}
	}

	ledger := balance.New(balance.Config{
		QtyScale: cfg.QtyScale, FeeScale: cfg.FeeScale,
		MakerFee: cfg.MakerFee, TakerFee: cfg.TakerFee, Markets: cfg.Markets,
	})
	ingress := spsc.NewCommand(cfg.RingSize)
	reinject := spsc.NewCommand(cfg.RingSize)

	pe := &ParallelEngine{ingress: ingress, impls: make(map[types.MarketID]*Shard, len(cfg.Markets))}
	coreShards := make(map[types.MarketID]shardOps, len(cfg.Markets))

	for _, g := range groups {
		reqs := spsc.New[wreq](1 << 10)
		resps := spsc.New[wresp](1 << 10)
		coll := &collector{}
		wshards := make(map[types.MarketID]*Shard, len(g))
		for _, m := range g {
			s := NewShard(m, cfg.CapHint, cfg.QtyScale)
			s.SetSink(coll)
			pe.impls[m] = s
			wshards[m] = s
			coreShards[m] = &remoteShard{market: m, reqs: reqs, resps: resps, reinject: reinject}
		}
		pe.workers = append(pe.workers, &worker{reqs: reqs, resps: resps, shards: wshards, coll: coll, stop: &pe.stop})
	}

	pe.core = &Core{shards: coreShards, ledger: ledger, open: make(map[types.OrderID]openOrder, 1024), filters: cfg.Filters, qtyScale: cfg.QtyScale}
	pe.seq = sequencer.New(sequencer.Config{
		Reinject: reinject,
		Inputs:   []*spsc.RingCommand{ingress},
		Journal:  cfg.Journal,
		Router:   pe.core,
		Clock:    cfg.Clock,
	})

	for i, w := range pe.workers {
		pe.wg.Add(1)
		go func(w *worker, core int) {
			defer pe.wg.Done()
			w.run(core)
		}(w, i)
	}
	return pe
}

// Submit pushes a command onto the ingress ring.
func (pe *ParallelEngine) Submit(c types.Command) bool { return pe.ingress.Push(c) }

// Step performs one control tick (sequencing + balance), dispatching matching
// to workers and blocking for results.
func (pe *ParallelEngine) Step() bool { return pe.seq.Step() }

// Drain steps until no control work remains.
func (pe *ParallelEngine) Drain() {
	for pe.seq.Step() {
	}
}

// Ledger exposes the balance ledger (read after Drain/Close for consistency).
func (pe *ParallelEngine) Ledger() *balance.Ledger { return pe.core.ledger }

// Shard returns a market's shard. Read its book only after Drain (control
// quiesced) or Close (workers stopped) to avoid racing the worker.
func (pe *ParallelEngine) Shard(m types.MarketID) *Shard { return pe.impls[m] }

// Acks returns captured acks.
func (pe *ParallelEngine) Acks() []types.Ack { return pe.core.acks }

// Seq returns the last assigned sequence number.
func (pe *ParallelEngine) Seq() types.Seq { return pe.seq.Seq() }

// MarketIDs returns market ids in ascending order.
func (pe *ParallelEngine) MarketIDs() []types.MarketID {
	ids := make([]types.MarketID, 0, len(pe.impls))
	for m := range pe.impls {
		ids = append(ids, m)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// Close stops the worker goroutines and waits for them to exit. After Close,
// books may be read directly without racing.
func (pe *ParallelEngine) Close() {
	pe.stop.Store(true)
	pe.wg.Wait()
}
