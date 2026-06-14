package property

import (
	"fmt"

	"github.com/yanuar-ar/order-book/internal/market"
	"github.com/yanuar-ar/order-book/internal/types"
	"github.com/yanuar-ar/order-book/tests/refmodel"
)

// Driver is the drive+inspect surface shared by *market.Engine and
// *market.ParallelEngine, letting the differential loop run against either.
type Driver interface {
	Inspectable
	Submit(types.Command) bool
	Step() bool
	Drain()
}

// engineState reads the engine's canonical state into the refmodel.State shape
// so it can be compared against the model's Snapshot via Canonical().
func engineState(e Inspectable) refmodel.State {
	bals, fees := e.Ledger().Dump()
	st := refmodel.State{}
	for _, b := range bals {
		st.Bals = append(st.Bals, refmodel.Bal{Acct: b.Acct, Asset: b.Asset, Available: b.Available, Reserved: b.Reserved})
	}
	for _, f := range fees {
		st.Fees = append(st.Fees, refmodel.Fee{Asset: f.Asset, Amount: f.Amount})
	}
	for _, m := range e.MarketIDs() {
		for _, o := range e.Shard(m).Book().Dump() {
			st.Orders = append(st.Orders, refmodel.Order{Market: m, Side: o.Side, Price: o.Price, ID: o.ID, Remaining: o.Remaining, Display: o.Display})
		}
	}
	return st
}

// RunDifferential drives a fresh serial engine and reference model through the
// stream, asserting after every order command that their canonical states match
// and that CheckAllInvariants holds. Returns the first divergence/violation or
// nil. Decoupled from *testing.T so the fuzz target and state-machine test reuse it.
func RunDifferential(stream Stream) error {
	return runDifferential(market.NewEngine(engineCfg()), stream)
}

// RunDifferentialAsync runs the differential check with the AsyncJournaller wired
// in, proving off-thread journaling is behavior-transparent: the engine still
// matches the reference oracle and holds every invariant after each command.
func RunDifferentialAsync(stream Stream) error {
	cfg := engineCfg()
	cfg.AsyncJournal = true
	cfg.JournalCore = -1
	e := market.NewEngine(cfg)
	defer e.Close()
	return runDifferential(e, stream)
}

// RunDifferentialParallel runs the same check against the ParallelEngine with
// the given worker grouping, proving the parallel topology matches the oracle
// (and therefore the serial engine) across every order type.
func RunDifferentialParallel(stream Stream, groups [][]types.MarketID) error {
	pe := market.NewParallelEngine(engineCfg(), groups)
	defer pe.Close()
	return runDifferential(pe, stream)
}

// drainSubmit submits c and drains the engine, retrying when the ingress ring
// is full (the parallel path can backpressure).
func drainSubmit(e Driver, c types.Command) {
	for !e.Submit(c) {
		e.Step()
	}
	e.Drain()
}

// applyNet submits c and updates the net external-flow map: deposits of amount>0
// always succeed; a withdrawal's effect is read from the engine's own
// available-balance delta (so accepted/rejected withdrawals both stay exact).
// This keeps the conservation check (INV-BAL-04) correct for streams that
// include withdrawals.
func applyNet(net map[types.AssetID]int64, e Driver, c types.Command) {
	switch c.Type {
	case types.CmdDeposit:
		if c.Amount > 0 {
			net[c.Asset] += c.Amount
		}
		drainSubmit(e, c)
	case types.CmdWithdraw:
		before := e.Ledger().Available(c.Account, c.Asset)
		drainSubmit(e, c)
		net[c.Asset] -= before - e.Ledger().Available(c.Account, c.Asset)
	default:
		drainSubmit(e, c)
	}
}

// feedTrackingNet applies a whole stream to e and returns the exact net external
// flow per asset. Used by recovery/metamorphic tests that need the conservation
// baseline for streams that may contain withdrawals.
func feedTrackingNet(e Driver, s Stream) map[types.AssetID]int64 {
	net := map[types.AssetID]int64{}
	for _, c := range s.Deposits {
		applyNet(net, e, c)
	}
	for i := range s.Orders {
		c := s.Orders[i]
		c.Seq = types.Seq(i + 1)
		applyNet(net, e, c)
	}
	return net
}

func runDifferential(e Driver, stream Stream) error {
	mod := refmodel.New(modelCfg())
	net := map[types.AssetID]int64{}

	for _, c := range stream.Deposits {
		applyNet(net, e, c)
		mod.Apply(c)
	}
	for i, c := range stream.Orders {
		c.Seq = types.Seq(i + 1) // monotonic; keeps model stop-ordering aligned with the engine
		applyNet(net, e, c)
		mod.Apply(c)

		eng := engineState(e).Canonical()
		ref := mod.Snapshot().Canonical()
		if eng != ref {
			return fmt.Errorf("state diverged at order step %d (cmd %+v):\n--- engine ---\n%s\n--- model ---\n%s", i, c, eng, ref)
		}
		if err := CheckAllInvariants(e, net); err != nil {
			return fmt.Errorf("invariant violated at order step %d (cmd %+v): %w", i, c, err)
		}
	}
	return nil
}
