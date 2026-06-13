package property

import (
	"testing"

	"github.com/yanuar-ar/order-book/internal/market"
	"github.com/yanuar-ar/order-book/internal/matching"
	"github.com/yanuar-ar/order-book/internal/orderbook"
	"github.com/yanuar-ar/order-book/internal/types"
)

// runToDigest applies a stream to a fresh engine and returns its canonical state.
func runToDigest(s Stream) string {
	e := market.NewEngine(engineCfg())
	for _, c := range s.Deposits {
		e.Submit(c)
	}
	for i, c := range s.Orders {
		c.Seq = types.Seq(i + 1)
		e.Submit(c)
	}
	e.Drain()
	return engineState(e).Canonical()
}

func runToAcks(s Stream) []types.Ack {
	e := market.NewEngine(engineCfg())
	for _, c := range s.Deposits {
		e.Submit(c)
	}
	for i, c := range s.Orders {
		c.Seq = types.Seq(i + 1)
		e.Submit(c)
	}
	e.Drain()
	return append([]types.Ack(nil), e.Acks()...)
}

// TestSameSeedIdenticalState_NewGenerators asserts INV-DET-01 over the broad and
// sharp generators: the same seed run twice yields byte-identical state.
func TestSameSeedIdenticalState_NewGenerators(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream Stream
	}{
		{"broad", GenBroad(7, 1500)},
		{"sharp", GenSharp(7, 1500)},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if runToDigest(tc.stream) != runToDigest(tc.stream) {
				t.Fatal("same-seed runs diverged: engine is not deterministic")
			}
		})
	}
}

// TestSameSeedIdenticalAcks asserts that command (and stop-activation)
// processing order is deterministic: the ack stream is identical across runs.
func TestSameSeedIdenticalAcks(t *testing.T) {
	s := GenSharp(13, 1500)
	a1, a2 := runToAcks(s), runToAcks(s)
	if len(a1) != len(a2) {
		t.Fatalf("ack count differs: %d vs %d", len(a1), len(a2))
	}
	for i := range a1 {
		if a1[i] != a2[i] {
			t.Fatalf("ack %d differs: %+v vs %+v", i, a1[i], a2[i])
		}
	}
}

type detSink struct{}

func (detSink) Emit(types.Command) {}

// sweepFills builds a three-level resting book and two aggressors that sweep it,
// returning every fill in emission order.
func sweepFills() []types.Fill {
	e := matching.NewEngine(orderbook.New(0, 64), detSink{}, 1)
	var fills []types.Fill
	submit := func(o types.FundedOrder) {
		fills = append(fills, e.Submit(o).Fills...) // append copies the values out of the reused buffer
	}
	// Resting sells at three ascending levels, distinct accounts.
	submit(types.FundedOrder{Seq: 1, Account: 10, OrderID: 1, Side: types.Sell, OrdType: types.Limit, Tif: types.GTC, Price: 100, Qty: 2})
	submit(types.FundedOrder{Seq: 2, Account: 11, OrderID: 2, Side: types.Sell, OrdType: types.Limit, Tif: types.GTC, Price: 101, Qty: 2})
	submit(types.FundedOrder{Seq: 3, Account: 12, OrderID: 3, Side: types.Sell, OrdType: types.Limit, Tif: types.GTC, Price: 102, Qty: 2})
	// Aggressor 1 sweeps two-and-a-bit levels; aggressor 2 takes the rest.
	submit(types.FundedOrder{Seq: 10, Account: 20, OrderID: 10, Side: types.Buy, OrdType: types.Limit, Tif: types.GTC, Price: 102, Qty: 5})
	submit(types.FundedOrder{Seq: 11, Account: 21, OrderID: 11, Side: types.Buy, OrdType: types.Limit, Tif: types.GTC, Price: 102, Qty: 1})
	return fills
}

// TestFillOrderByAggressorSeqMatchIndex asserts INV-DET-04 / INV-MAT-08: fills
// form a total order by (AggressorSeq, MatchIndex) — non-decreasing aggressor
// seq, and MatchIndex restarting at 0 and incrementing within each aggressor —
// and that the sequence is identical across identical runs.
func TestFillOrderByAggressorSeqMatchIndex(t *testing.T) {
	fills := sweepFills()
	if len(fills) == 0 {
		t.Fatal("expected fills from the sweep")
	}
	var lastSeq types.Seq
	var wantIdx uint32
	for i, f := range fills {
		if f.AggressorSeq < lastSeq {
			t.Fatalf("fill %d: aggressor seq decreased (%d after %d)", i, f.AggressorSeq, lastSeq)
		}
		if f.AggressorSeq != lastSeq {
			lastSeq = f.AggressorSeq
			wantIdx = 0
		}
		if f.MatchIndex != wantIdx {
			t.Fatalf("fill %d: MatchIndex = %d, want %d for aggressor seq %d", i, f.MatchIndex, wantIdx, f.AggressorSeq)
		}
		wantIdx++
	}

	// Determinism: a second identical run yields the identical fill sequence.
	again := sweepFills()
	if len(again) != len(fills) {
		t.Fatalf("fill count differs across runs: %d vs %d", len(again), len(fills))
	}
	for i := range fills {
		if fills[i] != again[i] {
			t.Fatalf("fill %d differs across runs: %+v vs %+v", i, fills[i], again[i])
		}
	}
}
