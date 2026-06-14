// Package sequencer is the engine's single ordering authority. One goroutine
// assigns the global, contiguous Seq and timestamp, journals each command, and
// routes it onward; it also drains market fills and applies settlement in
// deterministic (aggressor_seq, match_index) order.
//
// Determinism model: every command that receives a Seq is journaled — external
// commands and stop activations alike — so the WAL is a complete, contiguous
// log and replay is a straight re-application (no regeneration). Stop
// re-triggering is suppressed during replay (wired in U8) because the
// activations are already in the log.
package sequencer

import (
	"sort"

	"github.com/yanuar-ar/order-book/internal/spsc"
	"github.com/yanuar-ar/order-book/internal/types"
	"github.com/yanuar-ar/order-book/internal/wal"
)

// ClockFunc returns the current time in nanoseconds. Captured once per command.
type ClockFunc func() int64

// Journal persists sequenced commands. *wal.Writer satisfies it.
//
// Append must not retain Record.Payload beyond the call — the sequencer passes a
// reusable buffer and overwrites it on the next command. A consumer that stores
// records must copy the payload.
type Journal interface {
	Append(wal.Record) error
}

// Router receives sequenced commands (for reservation/routing) and settlements
// (derived from fills, in deterministic order).
type Router interface {
	OnCommand(types.Command)
	OnSettlement(types.Fill)
}

// Sequencer fans in producer command rings, drains market fill rings, assigns
// order, journals, and routes.
type Sequencer struct {
	reinject   *spsc.RingCommand   // priority input for stop activations (U8)
	inputs     []*spsc.RingCommand // external producers (round-robin)
	fills      []*spsc.RingFill    // per-market fill rings
	journaller Journaller          // WAL Append + fsync + durable watermark
	router     Router
	clock      ClockFunc

	seq types.Seq
	rr  int // round-robin cursor over inputs

	// Durable-ack barrier (output-side only — never journaled, never affects Seq,
	// timestamps, or fill order, so replay is byte-identical regardless of cadence).
	// The watermark itself lives on the Journaller (DurableSeq); the sequencer owns
	// only the flush-trigger policy:
	//   unsynced counts records appended since the last flush.
	//   flushCap is the group-commit batch ceiling (see defaultFlushCap).
	unsynced int
	flushCap int

	// fatal latches a terminal WAL-durability failure. Once set, Step is a no-op
	// and Run exits: the WAL is the source of truth, so the engine must not
	// match or release any further output once journaling is broken. No pending
	// ack is released after a fatal latches. Surfaced to the host via Fatal().
	fatal error
}

// defaultFlushCap is the group-commit batch ceiling: a flush fires at the latest
// when this many records have been appended since the last fsync. On the durable
// path this directly sets how many commands amortize one fsync, so it governs
// durable throughput vs durable-ack latency. Config.FlushCap overrides it.
const defaultFlushCap = 64

// Config wires a sequencer.
type Config struct {
	Reinject *spsc.RingCommand
	Inputs   []*spsc.RingCommand
	Fills    []*spsc.RingFill
	// Journal is the raw WAL sink. When Journaller is nil it is wrapped in a
	// SyncJournaller (inline journaling — the default).
	Journal Journal
	// Journaller, when set, overrides the default SyncJournaller — e.g. an
	// AsyncJournaller that fsyncs off the sequencer goroutine. Takes precedence
	// over Journal.
	Journaller Journaller
	Router     Router
	Clock      ClockFunc
	// FlushCap overrides the group-commit batch ceiling (0 -> defaultFlushCap).
	FlushCap int
}

// New returns a sequencer. A nil Reinject ring is allowed (no stop re-injection).
func New(cfg Config) *Sequencer {
	cap := cfg.FlushCap
	if cap <= 0 {
		cap = defaultFlushCap
	}
	j := cfg.Journaller
	if j == nil {
		j = NewSyncJournaller(cfg.Journal)
	}
	return &Sequencer{
		reinject:   cfg.Reinject,
		inputs:     cfg.Inputs,
		fills:      cfg.Fills,
		journaller: j,
		router:     cfg.Router,
		clock:      cfg.Clock,
		flushCap:   cap,
	}
}

// Seq returns the last assigned sequence number.
func (s *Sequencer) Seq() types.Seq { return s.seq }

// DurableSeq returns the highest Seq whose WAL bytes have been fsynced. Output
// (acks) at or below this watermark is safe to release; above it is speculative.
func (s *Sequencer) DurableSeq() types.Seq { return s.journaller.DurableSeq() }

// DrainJournal blocks until the journaller has made every appended command
// durable (DurableSeq catches up to the last appended Seq). For the async
// journaller this waits for its consumer to fsync; for the sync journaller it
// flushes inline. It must be called only when the input rings are quiesced (no
// new commands are being sequenced). A failure latches fatal so a drain-then-
// check caller (the snapshotter) observes it via Fatal().
func (s *Sequencer) DrainJournal() error {
	if s.fatal != nil {
		return s.fatal
	}
	if err := s.journaller.Drain(); err != nil {
		s.fatal = err
		return err
	}
	return nil
}

// setFlushCap overrides the group-commit batch ceiling. Test-only seam (used to
// prove the WAL byte stream and Seq assignment are invariant to flush cadence).
func (s *Sequencer) setFlushCap(n int) {
	if n < 1 {
		n = 1
	}
	s.flushCap = n
}

// SetSeq primes the counter to the given watermark. It is used by snapshot
// restore so commands sequenced after a restore continue contiguously from the
// snapshot's Seq. It must only be called while the engine is quiesced (before
// live stepping resumes); it does not journal or route anything.
func (s *Sequencer) SetSeq(seq types.Seq) { s.seq = seq }

// Inject enqueues a synthetic command (a stop activation) for sequencing. It is
// called by market shards; returns false if the re-injection ring is full.
func (s *Sequencer) Inject(c types.Command) bool {
	if s.reinject == nil {
		return false
	}
	return s.reinject.Push(c)
}

// Step performs one deterministic iteration:
//  1. drain all available fills and apply settlement in (aggressor_seq, match_index) order;
//  2. drain all pending stop activations (priority), sequencing each;
//  3. sequence one external command (round-robin).
//
// It reports whether any work was done. Once a fatal WAL-durability failure has
// latched, Step is a no-op and reports no work; the caller surfaces it via
// Fatal().
func (s *Sequencer) Step() bool {
	if s.fatal != nil {
		return false
	}
	did := s.drainFills()
	if s.reinject != nil {
		var c types.Command
		for s.reinject.Pop(&c) {
			if err := s.sequenceAndRoute(&c); err != nil {
				s.fatal = err
				return did
			}
			s.unsynced++
			did = true
		}
	}
	if c, ok := s.pollExternal(); ok {
		if err := s.sequenceAndRoute(&c); err != nil {
			s.fatal = err
			return did
		}
		s.unsynced++
		did = true
	} else if s.unsynced > 0 {
		// Input ring drained with records pending: flush so their acks become
		// durable before the engine goes idle (low latency at light load).
		if err := s.flush(); err != nil {
			s.fatal = err
			return did
		}
		did = true
	} else if err := s.journaller.Fatal(); err != nil {
		// Fully idle, but an async journaller died on its own goroutine: halt so
		// no further output is released above the frozen durableSeq. On the busy
		// path the failure surfaces through Append/flush instead; the sync
		// journaller always reports nil here, so this costs it nothing.
		s.fatal = err
		return did
	}
	if s.unsynced > 0 && s.unsynced >= s.flushCap {
		// Batch ceiling reached under load: amortize the fsync (the LMAX
		// batching effect). flush() zeroes unsynced, so this fires at most once.
		if err := s.flush(); err != nil {
			s.fatal = err
			return did
		}
		did = true
	}
	return did
}

// flush forces WAL durability (via the Journaller) and resets the unsynced
// counter. The Journaller advances its own DurableSeq watermark.
func (s *Sequencer) flush() error {
	if err := s.journaller.Flush(); err != nil {
		return err
	}
	s.unsynced = 0
	return nil
}

// Fatal returns the latched terminal WAL-durability failure, or nil. The host
// run loop checks this after each Step and halts on a non-nil result.
func (s *Sequencer) Fatal() error { return s.fatal }

// Run loops Step until stop is closed or a fatal latches (busy-spin). Used by
// the assembled engine.
func (s *Sequencer) Run(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
			if s.fatal != nil {
				return
			}
			s.Step()
		}
	}
}

func (s *Sequencer) drainFills() bool {
	var batch []types.Fill
	var f types.Fill
	for _, r := range s.fills {
		for r.Pop(&f) {
			batch = append(batch, f)
		}
	}
	if len(batch) == 0 {
		return false
	}
	sort.Slice(batch, func(i, j int) bool {
		if batch[i].AggressorSeq != batch[j].AggressorSeq {
			return batch[i].AggressorSeq < batch[j].AggressorSeq
		}
		return batch[i].MatchIndex < batch[j].MatchIndex
	})
	for _, fl := range batch {
		s.router.OnSettlement(fl)
	}
	return true
}

func (s *Sequencer) pollExternal() (types.Command, bool) {
	n := len(s.inputs)
	for i := 0; i < n; i++ {
		idx := (s.rr + i) % n
		var c types.Command
		if s.inputs[idx].Pop(&c) {
			s.rr = (idx + 1) % n
			return c, true
		}
	}
	return types.Command{}, false
}

// sequenceAndRoute assigns the next Seq + timestamp, journals the command, and
// routes it. A journal failure is returned without routing — so no ack is
// produced for an undurable command — and the caller latches it as fatal.
func (s *Sequencer) sequenceAndRoute(c *types.Command) error {
	s.seq++
	c.Seq = s.seq
	c.TsNanos = s.clock() // wall-clock read happens only here
	if err := s.journaller.Append(*c); err != nil {
		return err
	}
	s.router.OnCommand(*c)
	return nil
}
