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
	return nil
}
