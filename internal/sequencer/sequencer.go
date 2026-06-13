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
	reinject *spsc.RingCommand   // priority input for stop activations (U8)
	inputs   []*spsc.RingCommand // external producers (round-robin)
	fills    []*spsc.RingFill    // per-market fill rings
	journal  Journal
	router   Router
	clock    ClockFunc

	seq types.Seq
	rr  int // round-robin cursor over inputs

	// fatal latches a terminal WAL-durability failure. Once set, Step is a no-op
	// and Run exits: the WAL is the source of truth, so the engine must not
	// match or release any further output once journaling is broken. No pending
	// ack is released after a fatal latches. Surfaced to the host via Fatal().
	fatal error
}

// Config wires a sequencer.
type Config struct {
	Reinject *spsc.RingCommand
	Inputs   []*spsc.RingCommand
	Fills    []*spsc.RingFill
	Journal  Journal
	Router   Router
	Clock    ClockFunc
}

// New returns a sequencer. A nil Reinject ring is allowed (no stop re-injection).
func New(cfg Config) *Sequencer {
	return &Sequencer{
		reinject: cfg.Reinject,
		inputs:   cfg.Inputs,
		fills:    cfg.Fills,
		journal:  cfg.Journal,
		router:   cfg.Router,
		clock:    cfg.Clock,
	}
}

// Seq returns the last assigned sequence number.
func (s *Sequencer) Seq() types.Seq { return s.seq }

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
			did = true
		}
	}
	if c, ok := s.pollExternal(); ok {
		if err := s.sequenceAndRoute(&c); err != nil {
			s.fatal = err
			return did
		}
		did = true
	}
	return did
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
	if err := s.journal.Append(wal.Record{
		Seq:     uint64(s.seq),
		TsNanos: c.TsNanos,
		Type:    uint16(c.Type),
		Payload: types.EncodeCommand(*c),
	}); err != nil {
		return err
	}
	s.router.OnCommand(*c)
	return nil
}
