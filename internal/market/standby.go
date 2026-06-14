package market

import (
	"errors"

	"github.com/yanuar-ar/order-book/internal/sequencer"
	"github.com/yanuar-ar/order-book/internal/types"
	"github.com/yanuar-ar/order-book/internal/wal"
)

// fetchCap bounds a single backfill so a standby that is hopelessly behind (e.g.
// sharing cores with the primary under max load) makes incremental progress
// rather than reading the whole WAL tail into one slice — the consumer simply
// fetches again. Without it, sustained overflow allocates unbounded slices.
const fetchCap = 1 << 16

// errFetchCap stops wal.Replay early once a backfill batch is full.
var errFetchCap = errors.New("fetch batch full")

// Standby applies the replicated command stream to a shadow engine kept in
// suppress-stops mode — exactly the replay posture (stop activations are streamed
// from the primary, not regenerated locally, so they apply once). It tracks its
// own applied watermark and ignores any Seq at or below it, because
// ApplyJournaled is not idempotent (re-applying a newOrder double-reserves). On
// promotion the standby's engine becomes the live primary; until then it is a
// passive applier.
type Standby struct {
	eng         *Engine
	lastApplied types.Seq
	epoch       uint64 // highest leadership term held; Promote increments it
}

// newStandby builds the shadow engine from the same config as the primary, but
// with stops suppressed, its own replicator disabled (no chaining), and no
// journal of its own in v1 (a future standby-local WAL is where its own
// durability would live — see the plan).
func newStandby(cfg Config) *Standby {
	scfg := cfg
	scfg.SuppressStops = true
	scfg.ReplicationMode = "off" // the standby does not itself replicate
	scfg.AsyncJournal = false
	scfg.Journal = nil
	scfg.WALDir = ""
	return &Standby{eng: NewEngine(scfg)}
}

// apply delivers one replicated command to the shadow engine. It fences a
// stale-epoch record (a backwards leadership term — a zombie old primary's write
// after this node promoted) and guards against duplicate / already-applied Seqs
// (ApplyJournaled is not idempotent). It reports whether the command was applied.
// In-process delivery is contiguous, so the common case is Seq == lastApplied+1.
func (s *Standby) apply(c types.Command) bool {
	if c.Epoch < s.epoch {
		return false // fenced: stale leadership term (no state mutation)
	}
	if c.Seq <= s.lastApplied {
		return false // already applied — drop the duplicate
	}
	if c.Epoch > s.epoch {
		s.epoch = c.Epoch // adopt a newer term streamed from the primary
	}
	s.eng.ApplyJournaled(c)
	s.lastApplied = c.Seq
	return true
}

// Seq is the standby's applied watermark (the source of the replicated ack).
func (s *Standby) Seq() types.Seq { return s.lastApplied }

// Epoch is the leadership term the standby holds (fences anything below it).
func (s *Standby) Epoch() uint64 { return s.epoch }

// Promote turns the standby into the live primary: it increments the leadership
// term (so a revived old primary's records are fenced), primes the sequencer to
// the applied watermark and the new term, and re-enables stop activations (they
// were suppressed while shadowing, exactly like recovery). It returns the now-live
// engine. The caller must have stopped replicating into this standby first.
func (s *Standby) Promote() *Engine {
	s.epoch++
	s.eng.SetSeq(s.lastApplied)
	s.eng.SetEpoch(s.epoch)
	s.eng.EnableStops()
	return s.eng
}

// Engine exposes the shadow engine for fingerprint/invariant comparison and, on
// promotion, to become the live primary.
func (s *Standby) Engine() *Engine { return s.eng }

// inProcessLink is the v1 StandbyLink: it applies replicated commands directly to
// a local Standby and backfills overflow gaps from the primary's WAL directory.
// A network transport would implement the same seam over a wire (deferred).
type inProcessLink struct {
	standby *Standby
	walDir  string // primary WAL for Fetch backfill; "" disables backfill
}

func (l *inProcessLink) Send(c types.Command) error {
	l.standby.apply(c)
	return nil
}

func (l *inProcessLink) AckedSeq() types.Seq { return l.standby.Seq() }

// Fetch backfills durable commands with Seq > afterSeq from the primary's WAL. It
// is the recovery path for records the live ring dropped under backpressure;
// wal.Replay only yields fsynced records, so this is naturally bounded to the
// primary's durableSeq. With no walDir it returns nothing (overflow is then not
// recoverable — used only where the ring is sized so overflow cannot occur).
func (l *inProcessLink) Fetch(afterSeq types.Seq) ([]types.Command, error) {
	if l.walDir == "" {
		return nil, nil
	}
	out := make([]types.Command, 0, fetchCap)
	err := wal.Replay(l.walDir, uint64(afterSeq), func(rec wal.Record) error {
		c, derr := types.DecodeCommand(rec.Payload)
		if derr != nil {
			return derr
		}
		out = append(out, c)
		if len(out) >= fetchCap {
			return errFetchCap // bounded batch; the consumer fetches again for more
		}
		return nil
	})
	if err != nil && err != errFetchCap {
		return nil, err
	}
	return out, nil
}

func (l *inProcessLink) Fatal() error { return nil }
func (l *inProcessLink) Close() error { return nil }

// buildReplicator returns the replicator and its standby when replication is
// enabled, else (nil, nil) so the sequencer defaults to NopReplicator. Shared by
// the serial and parallel assemblies so both topologies replicate identically.
func buildReplicator(cfg Config) (sequencer.Replicator, *Standby) {
	if cfg.ReplicationMode == "" || cfg.ReplicationMode == "off" {
		return nil, nil
	}
	standby := newStandby(cfg)
	link := &inProcessLink{standby: standby, walDir: cfg.WALDir}
	core := cfg.ReplicationCore
	if core <= 0 {
		core = -1 // no pin
	}
	return sequencer.NewAsyncReplicator(link, cfg.ReplicationRing, core), standby
}
