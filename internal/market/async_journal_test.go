package market

import (
	"reflect"
	"testing"

	"github.com/yanuar-ar/order-book/internal/types"
)

// asyncCfg is parallelCfg with the async journaller enabled (no pin, default
// ring/batch). The no-op journal is fine: the point is that moving journaling
// off-thread does not perturb matching/settlement state.
func asyncCfg() Config {
	c := parallelCfg()
	c.AsyncJournal = true
	c.JournalCore = -1
	return c
}

// ---- U5: async journaller is behavior-transparent ----

// TestAsyncJournalMatchesSyncEngine: the same stream through a sync engine and
// an async-journaller engine yields byte-identical state and identical acks, and
// after Drain the async watermark has caught up to Seq.
func TestAsyncJournalMatchesSyncEngine(t *testing.T) {
	cmds, deposited := buildStream(11, 5000)

	sync := NewEngine(parallelCfg())
	feed(sync, cmds)

	async := NewEngine(asyncCfg())
	feed(async, cmds) // feed's Drain barriers on async durability
	defer async.Close()

	if digestOf(sync) != digestOf(async) {
		t.Fatal("async journaller diverged from sync engine state")
	}
	if !reflect.DeepEqual(sync.Acks(), async.Acks()) {
		t.Fatalf("async acks differ from sync: %d vs %d", len(sync.Acks()), len(async.Acks()))
	}
	if async.DurableSeq() != async.Seq() {
		t.Fatalf("after Drain async durableSeq=%d != Seq=%d", async.DurableSeq(), async.Seq())
	}
	checkInv(t, async, deposited)
}

// TestAsyncParallelMatchesSyncSerial: async journaller composes with the parallel
// topology and still equals the plain serial-sync engine.
func TestAsyncParallelMatchesSyncSerial(t *testing.T) {
	cmds, _ := buildStream(11, 5000)

	sync := NewEngine(parallelCfg())
	feed(sync, cmds)

	par := NewParallelEngine(asyncCfg(), [][]types.MarketID{{m0}, {m1}})
	feed(par, cmds)
	par.Close()

	if digestOf(sync) != digestOf(par) {
		t.Fatal("async parallel diverged from sync serial")
	}
}
