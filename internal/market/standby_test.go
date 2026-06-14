package market

import (
	"bytes"
	"testing"

	"github.com/yanuar-ar/order-book/internal/types"
)

// repCfg is snapCfg with sync replication enabled, so NewEngine builds a standby.
func repCfg() Config {
	c := snapCfg(2)
	c.ReplicationMode = "sync"
	return c
}

// Positive (INV-REP-01 essence): after a stream that exercises deposits, a match,
// an iceberg, and a pending stop, the standby's complete-state fingerprint and
// applied watermark equal the primary's.
func TestStandby_ConvergesToPrimaryFingerprint(t *testing.T) {
	e := NewEngine(repCfg())
	defer e.Close()
	run(t, e,
		dep(2, btc, 100),
		dep(1, usdt, 100000),
		types.Command{Type: types.CmdNewOrder, Market: m0, Account: 2, OrderID: 20, Side: types.Sell, OrdType: types.Limit, Tif: types.GTC, Flags: types.FlagIceberg, Price: 100, Qty: 10, DisplayQty: 3},
		order(m0, 1, 10, types.Buy, types.Limit, 100, 4),
		order(m0, 1, 11, types.Buy, types.Limit, 90, 5),
		types.Command{Type: types.CmdNewOrder, Market: m0, Account: 1, OrderID: 30, Side: types.Buy, OrdType: types.Stop, Tif: types.GTC, StopPrice: 120, Qty: 2},
	)

	s := e.Standby()
	if s == nil {
		t.Fatal("expected a standby with replication enabled")
	}
	if s.Seq() != e.Seq() {
		t.Fatalf("standby Seq = %d, primary Seq = %d", s.Seq(), e.Seq())
	}
	if !bytes.Equal(e.StateFingerprint(), s.Engine().StateFingerprint()) {
		t.Fatal("standby fingerprint differs from primary after convergence")
	}
}

// Edge: replication off builds no standby (behavior-neutral default).
func TestStandby_NoneWhenReplicationOff(t *testing.T) {
	e := NewEngine(snapCfg(2))
	defer e.Close()
	if e.Standby() != nil {
		t.Fatal("expected no standby when replication is off")
	}
}

// Negative (idempotency): ApplyJournaled is not idempotent, so the standby must
// ignore a Seq at or below its applied watermark — a re-delivered command must
// not double-apply.
func TestStandby_IdempotentApply(t *testing.T) {
	s := newStandby(snapCfg(2))
	s.apply(types.Command{Seq: 1, Type: types.CmdDeposit, Account: 1, Asset: usdt, Amount: 100})
	s.apply(types.Command{Seq: 2, Type: types.CmdDeposit, Account: 1, Asset: usdt, Amount: 50})
	fp := s.Engine().StateFingerprint()

	// Re-deliver Seq 2: must be a no-op (would otherwise double the balance).
	s.apply(types.Command{Seq: 2, Type: types.CmdDeposit, Account: 1, Asset: usdt, Amount: 50})
	if s.Seq() != 2 {
		t.Fatalf("applied watermark = %d after duplicate, want 2", s.Seq())
	}
	if !bytes.Equal(fp, s.Engine().StateFingerprint()) {
		t.Fatal("duplicate apply mutated standby state (not idempotent-guarded)")
	}
}
