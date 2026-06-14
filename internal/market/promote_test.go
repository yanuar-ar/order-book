package market

import (
	"bytes"
	"testing"

	"github.com/yanuar-ar/order-book/internal/types"
)

func epochDep(seq types.Seq, epoch uint64, acct types.AccountID, amt int64) types.Command {
	return types.Command{Seq: seq, Epoch: epoch, Type: types.CmdDeposit, Account: acct, Asset: usdt, Amount: amt}
}

// Negative (INV-REP-04 / AE2): after promotion bumps the term, a revived old
// primary's stale-epoch record is fenced — no Seq consumed, no state mutation.
func TestStandby_PromoteFencesZombie(t *testing.T) {
	s := newStandby(snapCfg(2))
	if !s.apply(epochDep(1, 1, 1, 100)) {
		t.Fatal("seq1 epoch1 should apply")
	}
	if !s.apply(epochDep(2, 1, 1, 50)) {
		t.Fatal("seq2 epoch1 should apply")
	}

	s.Promote() // term 1 -> 2
	if s.Epoch() != 2 {
		t.Fatalf("epoch after promote = %d, want 2", s.Epoch())
	}
	fp := s.Engine().StateFingerprint()

	// Zombie old primary (still term 1) delivers a CRC-valid, contiguous record.
	if s.apply(epochDep(3, 1, 1, 25)) {
		t.Fatal("stale-epoch (zombie) record must be fenced, not applied")
	}
	if s.Seq() != 2 {
		t.Fatalf("applied watermark = %d after fenced record, want 2 (no Seq consumed)", s.Seq())
	}
	if !bytes.Equal(fp, s.Engine().StateFingerprint()) {
		t.Fatal("fenced record mutated state")
	}

	// A record at the new term applies normally.
	if !s.apply(epochDep(3, 2, 1, 25)) {
		t.Fatal("seq3 epoch2 should apply after promotion")
	}
}

// Positive (R10 / INV-REP-05): a promoted standby resumes as the live primary at
// the applied watermark and new term, with state identical to the primary at the
// promotion point, and sequences new live commands contiguously.
func TestStandby_PromoteResumesLive(t *testing.T) {
	e := NewEngine(repCfg())
	run(t, e,
		dep(1, usdt, 100000),
		dep(2, btc, 100),
		order(m0, 2, 20, types.Sell, types.Limit, 100, 5),
		order(m0, 1, 10, types.Buy, types.Limit, 100, 2),
	)
	s := e.Standby()
	primaryFP := e.StateFingerprint()
	primarySeq := e.Seq()
	e.Close() // stop the primary and its replicator before promoting

	live := s.Promote()
	if live.Seq() != primarySeq {
		t.Fatalf("promoted Seq = %d, want %d", live.Seq(), primarySeq)
	}
	if live.Epoch() != 1 {
		t.Fatalf("promoted epoch = %d, want 1", live.Epoch())
	}
	if !bytes.Equal(live.StateFingerprint(), primaryFP) {
		t.Fatal("promoted engine state differs from primary at promotion")
	}

	// The promoted engine is live: it sequences a new order at the new term.
	if !live.Submit(order(m0, 1, 11, types.Buy, types.Limit, 95, 1)) {
		t.Fatal("promoted engine ingress full")
	}
	live.Drain()
	if live.Seq() != primarySeq+1 {
		t.Fatalf("promoted Seq after new order = %d, want %d", live.Seq(), primarySeq+1)
	}
	live.Close()
}
