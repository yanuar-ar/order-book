package sequencer

import (
	"runtime"
	"sync/atomic"

	"github.com/yanuar-ar/order-book/internal/platform"
	"github.com/yanuar-ar/order-book/internal/spsc"
	"github.com/yanuar-ar/order-book/internal/types"
)

// defaultReplicationRing is the replicator hand-off ring capacity (power of two).
// Unlike the journal ring it does not bound how far matching runs ahead — a full
// replicator ring does not stall the sequencer; it drops the standby to WAL-tail
// catch-up.
const defaultReplicationRing = 1 << 16

// AsyncReplicator streams sequenced commands to a hot standby off the sequencer
// goroutine — the structural mirror of AsyncJournaller, with one deliberate
// difference: Replicate is NON-BLOCKING. A full ring does not spin (which would
// stall journaling and matching); the command is dropped from the ring and the
// consumer backfills it from the WAL via StandbyLink.Fetch (it is still durable),
// so a slow or dead standby stalls acks only (R11/R13).
//
// The producer (sequencer) advances lastSubmitted on every command. The consumer
// (one goroutine) streams via the link in contiguous Seq order — filling any gap
// left by an overflow drop with a Fetch backfill — and publishes replicatedSeq
// from the link's AckedSeq (only Seqs the standby has durably applied).
type AsyncReplicator struct {
	link StandbyLink
	ring *spsc.RingCommand
	core int // pin target for the consumer; < 0 disables pinning

	replicatedSeq atomic.Uint64 // highest Seq the standby acked — consumer only
	lastSubmitted atomic.Uint64 // highest Seq pushed — producer only
	fatal         atomic.Pointer[asyncFatal]
	stop          atomic.Bool
	done          chan struct{}
}

// NewAsyncReplicator starts the consumer goroutine. ringSize falls back to the
// default when 0; core < 0 disables pinning (tests, non-Linux).
func NewAsyncReplicator(link StandbyLink, ringSize uint64, core int) *AsyncReplicator {
	if ringSize == 0 {
		ringSize = defaultReplicationRing
	}
	r := &AsyncReplicator{
		link: link,
		ring: spsc.NewCommand(ringSize),
		core: core,
		done: make(chan struct{}),
	}
	go r.run()
	return r
}

// Replicate hands a sequenced command to the consumer. It NEVER blocks: a full
// ring drops the command (the WAL still has it; the consumer backfills from
// Fetch), so a slow standby never stalls the sequencer. It returns the latched
// fatal if the consumer has died.
func (r *AsyncReplicator) Replicate(c types.Command) error {
	if f := r.fatal.Load(); f != nil {
		return f.err
	}
	r.ring.Push(c) // best-effort; on overflow the consumer backfills via Fetch
	r.lastSubmitted.Store(uint64(c.Seq))
	return nil
}

// Flush is a no-op hint: the consumer self-drives. It only surfaces a fatal.
func (r *AsyncReplicator) Flush() error {
	if f := r.fatal.Load(); f != nil {
		return f.err
	}
	return nil
}

// Drain blocks until the standby has durably applied every command submitted so
// far, or a fatal latches. Callers quiesce the journaller (DrainJournal) first so
// every submitted command is durable and thus Fetch-eligible.
func (r *AsyncReplicator) Drain() error {
	target := r.lastSubmitted.Load()
	for r.replicatedSeq.Load() < target {
		if f := r.fatal.Load(); f != nil {
			return f.err
		}
		runtime.Gosched()
	}
	if f := r.fatal.Load(); f != nil {
		return f.err
	}
	return nil
}

func (r *AsyncReplicator) ReplicatedSeq() types.Seq { return types.Seq(r.replicatedSeq.Load()) }

func (r *AsyncReplicator) Fatal() error {
	if f := r.fatal.Load(); f != nil {
		return f.err
	}
	return nil
}

// Close stops the consumer (which drains anything it can) and waits for it to
// exit. The producer must have stopped Replicating first.
func (r *AsyncReplicator) Close() error {
	r.stop.Store(true)
	<-r.done
	if f := r.fatal.Load(); f != nil {
		return f.err
	}
	return nil
}

func (r *AsyncReplicator) latch(err error) { r.fatal.Store(&asyncFatal{err: err}) }

// run is the consumer loop. It streams commands in contiguous Seq order. The
// common path pops the next command (Seq == lastSent+1) and sends it. A gap
// (overflow drop) triggers a WAL-tail backfill via Fetch; a duplicate (already
// backfilled) is dropped.
func (r *AsyncReplicator) run() {
	defer close(r.done)
	if r.core >= 0 {
		_ = platform.PinCurrentThread(r.core)
		defer platform.Unpin()
	}
	var lastSent types.Seq
	var c types.Command
	for {
		if r.stop.Load() {
			// Close is abrupt: abandon any catch-up (graceful catch-up is Drain's
			// job). Still surface a dead link so Close reports the failure.
			if f := r.link.Fatal(); f != nil {
				r.latch(f)
			}
			return
		}
		if f := r.link.Fatal(); f != nil {
			r.latch(f)
			return
		}
		if r.ring.Pop(&c) {
			switch {
			case uint64(c.Seq) <= uint64(lastSent):
				// already streamed (via a backfill) — drop the stale ring copy.
			case c.Seq == lastSent+1:
				if err := r.sendOne(c, &lastSent); err != nil {
					r.latch(err)
					return
				}
			default: // c.Seq > lastSent+1: the ring dropped [lastSent+1, c.Seq-1].
				if err := r.catchUp(&lastSent, c.Seq); err != nil {
					r.latch(err)
					return
				}
				if c.Seq == lastSent+1 {
					if err := r.sendOne(c, &lastSent); err != nil {
						r.latch(err)
						return
					}
				}
				// else c is still ahead of durable; drop it — the empty-ring branch
				// re-fetches it once it becomes durable.
			}
			continue
		}
		// Ring empty. If the producer submitted past what we've sent (a dropped
		// tail), catch up from the WAL toward lastSubmitted.
		if uint64(lastSent) < r.lastSubmitted.Load() {
			if err := r.catchUp(&lastSent, types.Seq(r.lastSubmitted.Load())+1); err != nil {
				r.latch(err)
				return
			}
			continue
		}
		runtime.Gosched()
	}
}

// sendOne delivers c to the standby and advances both watermarks. lastSent tracks
// what we have handed the link; replicatedSeq tracks what the standby has acked.
func (r *AsyncReplicator) sendOne(c types.Command, lastSent *types.Seq) error {
	if err := r.link.Send(c); err != nil {
		return err
	}
	*lastSent = c.Seq
	r.replicatedSeq.Store(uint64(r.link.AckedSeq()))
	return nil
}

// catchUp streams durable commands from the WAL (via Fetch) starting just after
// lastSent, until it reaches target (exclusive) or the durable tail runs out. It
// only advances contiguously; a non-contiguous Fetch result stops the run.
func (r *AsyncReplicator) catchUp(lastSent *types.Seq, target types.Seq) error {
	cmds, err := r.link.Fetch(*lastSent)
	if err != nil {
		return err
	}
	for i := range cmds {
		fc := cmds[i]
		if uint64(fc.Seq) <= uint64(*lastSent) {
			continue // older than we've sent
		}
		if fc.Seq != *lastSent+1 || fc.Seq >= target {
			break // non-contiguous, or reached the target boundary
		}
		if err := r.sendOne(fc, lastSent); err != nil {
			return err
		}
	}
	return nil
}
