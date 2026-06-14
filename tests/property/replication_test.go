package property

import (
	"testing"

	"github.com/yanuar-ar/order-book/internal/market"
)

// TestDifferentialReplicatedBroad drives the broad generator through a replicating
// engine: the standby must converge to the primary's fingerprint after every
// command (INV-REP-01/02) while the primary still matches the oracle.
func TestDifferentialReplicatedBroad(t *testing.T) {
	for _, seed := range []int64{1, 2, 3, 20260614} {
		seed := seed
		t.Run("seed="+itoa(seed), func(t *testing.T) {
			if err := RunDifferentialReplicated(GenBroad(seed, 1200)); err != nil {
				t.Fatalf("seed=%d: %v", seed, err)
			}
		})
	}
}

// TestDifferentialReplicatedSharp drives the adversarial generator (dense
// crossing, icebergs, stops, cancel/amend) through the replicating engine.
func TestDifferentialReplicatedSharp(t *testing.T) {
	for _, seed := range []int64{1, 2, 42, 99} {
		seed := seed
		t.Run("seed="+itoa(seed), func(t *testing.T) {
			if err := RunDifferentialReplicated(GenSharp(seed, 1200)); err != nil {
				t.Fatalf("seed=%d: %v", seed, err)
			}
		})
	}
}

// TestReplicationInvariants_NoStandby: the check fails closed when replication is
// off (no standby), so a misconfigured replicated run can never pass vacuously.
func TestReplicationInvariants_NoStandby(t *testing.T) {
	e := market.NewEngine(engineCfg())
	defer e.Close()
	if err := CheckReplicationInvariants(e); err == nil {
		t.Fatal("expected INV-REP failure when no standby is present")
	}
}
