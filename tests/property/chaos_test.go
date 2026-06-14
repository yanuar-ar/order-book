package property

import (
	"bytes"
	"testing"

	"github.com/yanuar-ar/order-book/internal/market"
)

// replicatedChaosCfg is the production failure-test posture: async journaling
// (the 1M-TPS path) plus a sync hot standby.
func replicatedChaosCfg() market.Config {
	cfg := engineCfg()
	cfg.AsyncJournal = true
	cfg.JournalCore = -1
	cfg.ReplicationMode = "sync"
	cfg.ReplicationCore = -1
	return cfg
}

// TestChaos_PrimaryCrashPromotion (chaos scenario 1): after a stream, the primary
// "crashes" and the standby is promoted. No confirmed order is lost — every
// released ack is covered by the standby's applied watermark (confirmed ⊆
// min(durableSeq, replicatedSeq)) — and the promoted engine resumes with state
// identical to the primary and passes every invariant.
func TestChaos_PrimaryCrashPromotion(t *testing.T) {
	for _, seed := range []int64{1, 7, 42, 99} {
		seed := seed
		t.Run("seed="+itoa(seed), func(t *testing.T) {
			e := market.NewEngine(replicatedChaosCfg())
			net := feedTrackingNet(e, GenSharp(seed, 600)) // drains each command
			_ = e.DrainStandby()                           // converge the standby before crashing

			confirmed := e.Acks()
			s := e.Standby()
			// No confirmed order falls outside what the standby has durably applied.
			for _, a := range confirmed {
				if a.Seq > s.Seq() {
					t.Fatalf("confirmed Seq %d exceeds standby Seq %d — lost on failover", a.Seq, s.Seq())
				}
				if a.Seq > e.DurableSeq() {
					t.Fatalf("confirmed Seq %d exceeds durableSeq %d", a.Seq, e.DurableSeq())
				}
			}

			primaryFP := e.StateFingerprint()
			primarySeq := e.Seq()
			e.Close() // crash the primary

			live := s.Promote()
			if live.Seq() != primarySeq {
				t.Fatalf("promoted Seq = %d, want %d", live.Seq(), primarySeq)
			}
			if !bytes.Equal(live.StateFingerprint(), primaryFP) {
				t.Fatal("promoted engine state differs from primary at crash")
			}
			if err := CheckAllInvariants(live, net); err != nil {
				t.Fatalf("promoted state violates invariants: %v", err)
			}
			live.Close()
		})
	}
}

// TestChaos_StandbyConvergesAcrossAllGenerators (chaos scenario 3, catch-up):
// across both generators the standby tracks the primary command-for-command and
// ends fingerprint-equal — the convergence guarantee a reconnecting standby
// relies on. (Reuses the per-step INV-REP checks in the replicated differential.)
func TestChaos_StandbyConvergesAcrossAllGenerators(t *testing.T) {
	for _, seed := range []int64{3, 11, 2026} {
		seed := seed
		t.Run("broad/seed="+itoa(seed), func(t *testing.T) {
			if err := RunDifferentialReplicated(GenBroad(seed, 800)); err != nil {
				t.Fatalf("%v", err)
			}
		})
		t.Run("sharp/seed="+itoa(seed), func(t *testing.T) {
			if err := RunDifferentialReplicated(GenSharp(seed, 800)); err != nil {
				t.Fatalf("%v", err)
			}
		})
	}
}

// FuzzReplicatedEngine drives coverage-guided byte streams through the replicated
// differential: the primary matches the oracle and the standby converges
// (INV-REP-*) after every command. Failing inputs are saved permanently under
// testdata/fuzz/.
func FuzzReplicatedEngine(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{2, 1, 0, 0, 1, 100, 5, 0, 2, 1, 1, 0, 3, 100, 5, 0})
	f.Add(bytes.Repeat([]byte{3, 2, 1, 0, 5, 100, 2, 0}, 12))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			data = data[:4096]
		}
		if err := RunDifferentialReplicated(decodeStream(data)); err != nil {
			t.Fatal(err)
		}
	})
}
