package property

import (
	"bytes"
	"fmt"

	"github.com/yanuar-ar/order-book/internal/market"
)

// CheckReplicationInvariants asserts the INV-REP-* properties on a replicating
// engine at a quiesced boundary (the caller has drained, so the standby has
// caught up). It is the replication analogue of CheckAllInvariants and is run
// after every command by the replicated differential loop.
//
//   - INV-REP-02 (prefix ordering): the standby's applied watermark never exceeds
//     the primary's Seq, and equals it once drained (no gaps, no reorder).
//   - INV-REP-01 (standby equivalence): the standby's complete-state fingerprint
//     equals the primary's at the shared Seq.
//   - INV-REP-03 (ack safety): no confirmed (released) ack exceeds
//     min(durableSeq, replicatedSeq) while in sync mode and not degraded — the
//     core "no confirmed order lost on failover" contract.
//
// INV-REP-04 (epoch fencing) is asserted directly in the promotion unit tests
// (a stale-epoch record consumes no Seq and mutates nothing), and INV-REP-05/06
// (post-promotion validity / catch-up convergence) in the chaos suite — they are
// not expressible as a per-command check on a single healthy stream.
func CheckReplicationInvariants(e *market.Engine) error {
	s := e.Standby()
	if s == nil {
		return fmt.Errorf("INV-REP: replication enabled but engine exposes no standby")
	}
	if s.Seq() > e.Seq() {
		return fmt.Errorf("INV-REP-02: standby Seq %d exceeds primary Seq %d", s.Seq(), e.Seq())
	}
	if s.Seq() != e.Seq() {
		return fmt.Errorf("INV-REP-02: standby Seq %d != primary Seq %d after drain (gap)", s.Seq(), e.Seq())
	}
	if !bytes.Equal(e.StateFingerprint(), s.Engine().StateFingerprint()) {
		return fmt.Errorf("INV-REP-01: standby fingerprint differs from primary at Seq %d", e.Seq())
	}
	// INV-REP-03: confirmed ⊆ min(durableSeq, replicatedSeq) in sync mode. Computed
	// against the watermarks independently (not via ReleasedSeq) so a gate that
	// wrongly released above the min is caught.
	if !e.Degraded() {
		gate := e.DurableSeq()
		if s.Seq() < gate {
			gate = s.Seq()
		}
		acks := e.Acks()
		if n := len(acks); n > 0 && acks[n-1].Seq > gate {
			return fmt.Errorf("INV-REP-03: confirmed ack Seq %d exceeds min(durableSeq=%d, replicatedSeq=%d)", acks[n-1].Seq, e.DurableSeq(), s.Seq())
		}
	}
	return nil
}
