package integration

import (
	"testing"

	"github.com/yanuar-ar/order-book/internal/market"
	"github.com/yanuar-ar/order-book/internal/types"
	"github.com/yanuar-ar/order-book/internal/wal"
)

// buildJournaledEngine assembles an engine backed by a real WAL in a temp dir,
// with the inline (sync) or off-thread (async) journaller. The returned close
// func stops the engine (the async journaller goroutine) before closing the WAL.
func buildJournaledEngine(t *testing.T, async bool, maker, taker int64) (e *market.Engine, dir string, closeFn func()) {
	t.Helper()
	dir = t.TempDir()
	w, err := wal.OpenWriter(dir, 0)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	c := cfg(maker, taker)
	c.Journal = w
	if async {
		c.AsyncJournal = true
		c.JournalCore = -1
	}
	e = market.NewEngine(c)
	return e, dir, func() {
		if err := e.Close(); err != nil {
			t.Fatalf("engine close: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("wal close: %v", err)
		}
	}
}

// asyncScenario is a representative multi-type flow that also triggers a stop
// (exercising the reinject → journaller path under async): deposits, a maker, a
// resting buy, a buy-stop, and a trade that triggers it.
func asyncScenario() []types.Command {
	return []types.Command{
		dep(1, usdt, 100000),
		dep(2, btc, 1000),
		dep(3, usdt, 100000),
		ord(mBTC, 2, 200, types.Sell, types.Limit, types.GTC, 106, 5), // maker
		ord(mBTC, 1, 100, types.Buy, types.Limit, types.GTC, 100, 3),  // rests (no cross)
		// buy-stop triggers when last >= 105, then runs as a market buy
		{Type: types.CmdNewOrder, Market: mBTC, Account: 3, OrderID: 300, Side: types.Buy, OrdType: types.Stop, StopPrice: 105, Qty: 2, Tif: types.GTC},
		ord(mBTC, 3, 400, types.Buy, types.Limit, types.GTC, 106, 1), // trades @106 -> triggers the stop
	}
}

// TestSyncAsyncEndToEndEquivalence: the same fully-assembled scenario through the
// sync and async journallers (both on real WALs) yields byte-identical engine
// state — off-thread journaling is behavior-transparent end-to-end.
func TestSyncAsyncEndToEndEquivalence(t *testing.T) {
	stream := asyncScenario()

	eSync, _, closeSync := buildJournaledEngine(t, false, 1, 2)
	defer closeSync()
	run(eSync, stream...)

	eAsync, _, closeAsync := buildJournaledEngine(t, true, 1, 2)
	defer closeAsync()
	run(eAsync, stream...)

	if digest(eSync) != digest(eAsync) {
		t.Fatalf("async diverged from sync:\n--- sync ---\n%s\n--- async ---\n%s", digest(eSync), digest(eAsync))
	}
}

// TestSyncAndAsyncHoldInvariants runs the scenario in both journal modes and
// asserts the global money/book invariants hold in each — the core money-safety
// gate on the assembled engine, for both journaller paths.
func TestSyncAndAsyncHoldInvariants(t *testing.T) {
	for _, async := range []bool{false, true} {
		mode := "sync"
		if async {
			mode = "async"
		}
		t.Run(mode, func(t *testing.T) {
			e, _, closeE := buildJournaledEngine(t, async, 1, 2)
			defer closeE()
			run(e, asyncScenario()...)
			// asyncScenario deposits 200000 USDT (accts 1 + 3) and 1000 BTC (acct 2).
			checkInvariants(t, e, map[types.AssetID]int64{usdt: 200000, btc: 1000})
		})
	}
}

// TestAsyncRecoveryViaReplay: a scenario journaled through the async journaller
// (including the stop activation re-injected into the log) replays from the WAL
// to identical state — recovery is transparent to which journaller wrote the log.
func TestAsyncRecoveryViaReplay(t *testing.T) {
	eA, dir, closeA := buildJournaledEngine(t, true, 1, 2)
	run(eA, asyncScenario()...)
	want := digest(eA)
	closeA() // stop the async journaller (final flush) and close the WAL

	// Replay into a fresh engine with stops suppressed (activations are in the WAL).
	cB := cfg(1, 2)
	cB.SuppressStops = true
	eB := market.NewEngine(cB)
	if err := wal.Replay(dir, 0, func(rec wal.Record) error {
		cmd, derr := types.DecodeCommand(rec.Payload)
		if derr != nil {
			return derr
		}
		eB.ApplyJournaled(cmd)
		return nil
	}); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if digest(eB) != want {
		t.Fatalf("async recovery mismatch:\n--- original ---\n%s\n--- replayed ---\n%s", want, digest(eB))
	}
}
