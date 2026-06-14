package sequencer

import (
	"math"

	"github.com/yanuar-ar/order-book/internal/types"
)

// Replicator streams sequenced commands to a hot standby and tracks the
// replicated watermark. It is the seam between the sequencer (which assigns Seq
// and routes) and the standby link — the exact mirror of the Journaller seam.
//
// The default (NopReplicator) is used when replication is "off": its
// ReplicatedSeq is +inf so the ack gate min(durableSeq, replicatedSeq) collapses
// to durableSeq and nothing changes. The real AsyncReplicator (U5) streams to a
// standby off the sequencer goroutine and publishes the standby-acked watermark.
//
// Unlike Journaller.Append, Replicate is non-blocking: a full ring drops the
// standby to WAL-tail catch-up (U6) rather than spinning, so a slow or dead
// standby stalls acks only — never journaling or matching (R11).
type Replicator interface {
	// Replicate hands one already-sequenced command to the standby stream. It must
	// not retain the command past the call and must not block the sequencer.
	Replicate(c types.Command) error
	// Flush is a hint that a batch boundary was reached (the consumer self-flushes).
	Flush() error
	// Drain blocks until the standby has durably applied every command replicated
	// so far, or a fatal latches. Used at quiesce points (snapshot, promotion).
	Drain() error
	// ReplicatedSeq is the highest Seq the standby has durably applied. In sync
	// mode the ack gate withholds acks above it.
	ReplicatedSeq() types.Seq
	// Fatal returns a latched terminal replication failure, or nil.
	Fatal() error
	// Close stops the replicator, flushing anything pending.
	Close() error
}

// StandbyLink abstracts the transport between the primary and a hot standby. It
// is the seam the AsyncReplicator drives; the in-process implementation (market
// package) applies commands directly to a standby Engine, and a network
// implementation would put bytes on a wire — both behind this interface.
//
// Send delivers one command to the standby. AckedSeq is the highest Seq the
// standby has durably applied (the source of the replicated watermark). Fetch
// backfills a gap: when the live ring overflowed and the replicator dropped a
// command (it is still in the WAL), the consumer asks the link for the missing
// durable commands after a watermark and streams them — so records are never
// lost from the log even though the live ring is lossy under backpressure.
type StandbyLink interface {
	Send(c types.Command) error
	AckedSeq() types.Seq
	// Fetch returns durable commands with Seq > afterSeq, in order, bounded to the
	// primary's durableSeq (a record not yet fsynced is not yet eligible).
	Fetch(afterSeq types.Seq) ([]types.Command, error)
	Fatal() error
	Close() error
}

// NopReplicator is the "off" replicator: it streams nothing and reports an
// +infinite replicated watermark, so min(durableSeq, ReplicatedSeq) == durableSeq
// and the ack gate is unchanged. It is the default, like SyncJournaller is the
// default journaller.
type NopReplicator struct{}

func (NopReplicator) Replicate(types.Command) error { return nil }
func (NopReplicator) Flush() error                  { return nil }
func (NopReplicator) Drain() error                  { return nil }

// ReplicatedSeq is the maximum Seq so the min() ack gate ignores replication.
func (NopReplicator) ReplicatedSeq() types.Seq { return types.Seq(math.MaxUint64) }
func (NopReplicator) Fatal() error             { return nil }
func (NopReplicator) Close() error             { return nil }
