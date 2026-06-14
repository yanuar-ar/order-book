package sequencer

import (
	"github.com/yanuar-ar/order-book/internal/types"
	"github.com/yanuar-ar/order-book/internal/wal"
)

// Journaller persists sequenced commands and tracks the durable watermark. It is
// the seam between the sequencer (which assigns Seq and routes) and the WAL.
//
// The synchronous implementation (SyncJournaller) journals inline on the
// sequencer goroutine — the historical behavior. An asynchronous implementation
// (added later) moves the WAL Append + fsync to its own core so the matcher never
// blocks on durability; the interface is the same so the sequencer is agnostic to
// which one it drives.
type Journaller interface {
	// Append journals one already-sequenced command (encode + durable write). It
	// must not retain the command past the call.
	Append(c types.Command) error
	// Flush forces durability and advances DurableSeq to cover every command
	// appended so far. On the synchronous journaller this fsyncs inline.
	Flush() error
	// DurableSeq is the highest Seq whose bytes are durable (fsynced).
	DurableSeq() types.Seq
}

// SyncJournaller journals inline on the caller's goroutine: Append writes to the
// WAL page cache and Flush fsyncs it. This is exactly the behavior the sequencer
// had before the seam was extracted, and is the default for tests and replay.
type SyncJournaller struct {
	journal Journal
	// payloadBuf is a reusable command-encode buffer so journaling allocates
	// nothing on the hot path. Aliased into the WAL Record's Payload; safe because
	// Journal.Append must not retain it past the call.
	payloadBuf [types.CommandSize]byte
	lastSeq    types.Seq
	durableSeq types.Seq
}

// NewSyncJournaller wraps a Journal (a *wal.Writer, or the no-op in-memory
// journal used by tests and replay).
func NewSyncJournaller(j Journal) *SyncJournaller { return &SyncJournaller{journal: j} }

func (j *SyncJournaller) Append(c types.Command) error {
	n := types.EncodeCommandInto(j.payloadBuf[:], c)
	if err := j.journal.Append(wal.Record{
		Seq:     uint64(c.Seq),
		TsNanos: c.TsNanos,
		Type:    uint16(c.Type),
		Payload: j.payloadBuf[:n],
	}); err != nil {
		return err
	}
	j.lastSeq = c.Seq
	return nil
}

// Flush captures the last-appended Seq before Sync so the watermark never
// over-claims coverage the fsync did not include.
func (j *SyncJournaller) Flush() error {
	last := j.lastSeq
	if err := j.sync(); err != nil {
		return err
	}
	j.durableSeq = last
	return nil
}

// sync fsyncs the journal when it supports it; the no-op in-memory journal
// (tests, replay) exposes no Sync method and reports success — and the watermark
// still advances (nothing to fsync).
func (j *SyncJournaller) sync() error {
	if sj, ok := j.journal.(interface{ Sync() error }); ok {
		return sj.Sync()
	}
	return nil
}

func (j *SyncJournaller) DurableSeq() types.Seq { return j.durableSeq }
