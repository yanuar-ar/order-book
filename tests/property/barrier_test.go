package property

import (
	"testing"

	"github.com/yanuar-ar/order-book/internal/market"
	"github.com/yanuar-ar/order-book/internal/types"
	"github.com/yanuar-ar/order-book/internal/wal"
)

// TestBarrier_WatermarkReachesSeqAndReplayMatches drives a randomized stream
// through a real WAL with the durable-ack barrier active, then asserts the
// watermark reached the final Seq (every ack released), that no released ack
// sits above the watermark, that replay reproduces byte-identical state, and
// that every invariant holds. This exercises the differential/recovery loop
// with the barrier in the path: the watermark is output-side, so the journaled
// stream and replayed state are unaffected by flush cadence (R5/R6, R7).
func TestBarrier_WatermarkReachesSeqAndReplayMatches(t *testing.T) {
	dir := t.TempDir()
	w, err := wal.OpenWriter(dir, 0)
	if err != nil {
		t.Fatalf("open WAL: %v", err)
	}
	cfg := engineCfg()
	cfg.Journal = w
	e := market.NewEngine(cfg)

	net := feedTrackingNet(e, GenSharp(9, 1500))

	if e.DurableSeq() != e.Seq() {
		t.Fatalf("after draining the stream durableSeq=%d != seq=%d (acks not fully released)", e.DurableSeq(), e.Seq())
	}
	for _, a := range e.Acks() {
		if a.Seq > e.DurableSeq() {
			t.Fatalf("released ack Seq %d exceeds durableSeq %d", a.Seq, e.DurableSeq())
		}
	}
	want := engineState(e).Canonical()
	if err := w.Close(); err != nil {
		t.Fatalf("close WAL: %v", err)
	}

	e2 := replayInto(t, dir)
	if got := engineState(e2).Canonical(); got != want {
		t.Fatal("replayed state differs from the barrier-gated original")
	}
	if err := CheckAllInvariants(e2, net); err != nil {
		t.Fatalf("replayed state violates invariants: %v", err)
	}
}

// TestBarrier_ClientReqIDSurvivesWALReplay confirms ClientReqID round-trips
// through the sequencer's journal encode and WAL replay (R9). The field is the
// reserved hook for the deferred dedup-set enforcement; if it failed to persist,
// exactly-once recovery would be impossible to add later without a format break.
func TestBarrier_ClientReqIDSurvivesWALReplay(t *testing.T) {
	dir := t.TempDir()
	w, err := wal.OpenWriter(dir, 0)
	if err != nil {
		t.Fatalf("open WAL: %v", err)
	}
	cfg := engineCfg()
	cfg.Journal = w
	e := market.NewEngine(cfg)

	const reqID uint64 = 0xDEADBEEFCAFE
	if !e.Submit(types.Command{Type: types.CmdDeposit, Account: 1, Asset: genQuote, Amount: 100, ClientReqID: reqID}) {
		t.Fatal("ingress full")
	}
	e.Drain()
	if err := w.Close(); err != nil {
		t.Fatalf("close WAL: %v", err)
	}

	var got uint64
	var seen int
	if err := wal.Replay(dir, 0, func(rec wal.Record) error {
		c, derr := types.DecodeCommand(rec.Payload)
		if derr != nil {
			return derr
		}
		if c.Type == types.CmdDeposit {
			got = c.ClientReqID
			seen++
		}
		return nil
	}); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if seen != 1 {
		t.Fatalf("replayed %d deposit records, want 1", seen)
	}
	if got != reqID {
		t.Fatalf("ClientReqID after replay = %#x, want %#x", got, reqID)
	}
}
