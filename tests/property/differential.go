package property

import (
	"fmt"

	"github.com/yanuar-ar/order-book/internal/market"
	"github.com/yanuar-ar/order-book/internal/types"
	"github.com/yanuar-ar/order-book/tests/refmodel"
)

// engineState reads the engine's canonical state into the refmodel.State shape
// so it can be compared against the model's Snapshot via Canonical().
func engineState(e *market.Engine) refmodel.State {
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

// RunDifferential drives a fresh engine and reference model through the stream,
// asserting after every order command that their canonical states match and
// that CheckAllInvariants holds on the engine. It returns an error describing
// the first divergence or violation, or nil. Decoupled from *testing.T so the
// fuzz target and state-machine test can reuse it.
func RunDifferential(stream Stream) error {
	e := market.NewEngine(engineCfg())
	mod := refmodel.New(modelCfg())

	apply := func(c types.Command) {
		e.Submit(c)
		e.Drain()
		mod.Apply(c)
	}
	for _, c := range stream.Deposits {
		apply(c)
	}
	for i, c := range stream.Orders {
		c.Seq = types.Seq(i + 1) // monotonic; keeps model stop-ordering aligned with the engine
		apply(c)

		eng := engineState(e).Canonical()
		ref := mod.Snapshot().Canonical()
		if eng != ref {
			return fmt.Errorf("state diverged at order step %d (cmd %+v):\n--- engine ---\n%s\n--- model ---\n%s", i, c, eng, ref)
		}
		if err := CheckAllInvariants(e, stream.NetDeposits); err != nil {
			return fmt.Errorf("invariant violated at order step %d (cmd %+v): %w", i, c, err)
		}
	}
	return nil
}
