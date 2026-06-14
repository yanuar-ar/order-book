package sequencer

import (
	"runtime"
	"sync/atomic"

	"github.com/yanuar-ar/order-book/internal/platform"
	"github.com/yanuar-ar/order-book/internal/spsc"
	"github.com/yanuar-ar/order-book/internal/types"
	"github.com/yanuar-ar/order-book/internal/wal"
)

// defaultJournalRing is the async journal ring capacity (power of two). It bounds
// how far speculative matching may run ahead of durability before Append
// backpressures the sequencer.
const defaultJournalRing = 1 << 16

// defaultBatchCap is the async group-commit ceiling: the consumer fsyncs after
// this many records, or whenever the ring drains. Large batches amortize the
// fsync so the consumer keeps pace with the matcher on the durable path.
const defaultBatchCap = 8192

// asyncFatal boxes a latched terminal error so it can be published through an
// atomic.Pointer (store the error before the pointer becomes visible).
type asyncFatal struct{ err error }

// AsyncJournaller moves WAL Append + fsync onto its own goroutine (the LMAX
// "Journaller" consumer) so the sequencer/matcher never blocks on durability.
//
// The sequencer (producer) hands sequenced commands off through an SPSC ring;
// FIFO ordering means the on-disk byte stream is identical to inline journaling
// regardless of the consumer's timing. The consumer (one goroutine) pops,
// appends, and group-commit-fsyncs, then publishes the durable watermark through
// an atomic the engine reads to gate acks. Append backpressures (spins) when the
// ring is full — records are never dropped (the WAL is the source of truth).
type AsyncJournaller struct {
	journal  Journal
	ring     *spsc.RingCommand
	batchCap int
	core     int // pin target for the consumer; < 0 disables pinning

	durableSeq    atomic.Uint64 // highest Seq fsynced — written by the consumer only
	lastSubmitted atomic.Uint64 // highest Seq pushed — written by the producer only
	fatal         atomic.Pointer[asyncFatal]
	stop          atomic.Bool
	done          chan struct{}

	// payloadBuf is owned by the consumer goroutine alone, so encoding allocates
	// nothing and never races the producer.
	payloadBuf [types.CommandSize]byte
}

// NewAsyncJournaller starts the consumer goroutine. ringSize and batchCap fall
// back to defaults when 0/<=0; core < 0 disables pinning (tests, non-Linux).
func NewAsyncJournaller(j Journal, ringSize uint64, batchCap, core int) *AsyncJournaller {
	if ringSize == 0 {
		ringSize = defaultJournalRing
	}
	if batchCap <= 0 {
		batchCap = defaultBatchCap
	}
	aj := &AsyncJournaller{
		journal:  j,
		ring:     spsc.NewCommand(ringSize),
		batchCap: batchCap,
		core:     core,
		done:     make(chan struct{}),
	}
	go aj.run()
	return aj
}

// Append hands a sequenced command to the consumer. It spins when the ring is
// full (backpressure) and returns the latched fatal if the consumer has died, so
// a dead consumer never deadlocks the producer.
func (j *AsyncJournaller) Append(c types.Command) error {
	if f := j.fatal.Load(); f != nil {
		return f.err
	}
	for !j.ring.Push(c) {
		if f := j.fatal.Load(); f != nil {
			return f.err
		}
		runtime.Gosched()
	}
	j.lastSubmitted.Store(uint64(c.Seq))
	return nil
}

// Flush is a no-op hint: the consumer goroutine owns flushing (on its batch cap
// or when the ring drains). It only surfaces a latched fatal.
func (j *AsyncJournaller) Flush() error {
	if f := j.fatal.Load(); f != nil {
		return f.err
	}
	return nil
}

// Drain blocks until every command appended so far is durable, or a fatal
// latches.
func (j *AsyncJournaller) Drain() error {
	target := j.lastSubmitted.Load()
	for j.durableSeq.Load() < target {
		if f := j.fatal.Load(); f != nil {
			return f.err
		}
		runtime.Gosched()
	}
	if f := j.fatal.Load(); f != nil {
		return f.err
	}
	return nil
}

func (j *AsyncJournaller) DurableSeq() types.Seq { return types.Seq(j.durableSeq.Load()) }

// Fatal returns the terminal error latched on the consumer goroutine, or nil.
func (j *AsyncJournaller) Fatal() error {
	if f := j.fatal.Load(); f != nil {
		return f.err
	}
	return nil
}

// Close stops the consumer (which drains the ring and does a final flush) and
// waits for it to exit. It does not close the underlying Journal — the host owns
// that. Safe to call once; the producer must have stopped Appending first.
func (j *AsyncJournaller) Close() error {
	j.stop.Store(true)
	<-j.done
	if f := j.fatal.Load(); f != nil {
		return f.err
	}
	return nil
}

// run is the consumer loop: pop → append → flush on batch cap or ring-drain. On
// stop it drains the ring and does a final flush before exiting. Any Append/Sync
// error latches a fatal and stops the loop.
func (j *AsyncJournaller) run() {
	defer close(j.done)
	if j.core >= 0 {
		_ = platform.PinCurrentThread(j.core)
		defer platform.Unpin()
	}
	var unsynced int
	var last types.Seq
	var c types.Command
	for {
		if j.ring.Pop(&c) {
			if err := j.appendOne(c, &last); err != nil {
				j.fatal.Store(&asyncFatal{err: err})
				return
			}
			unsynced++
			if unsynced >= j.batchCap {
				if err := j.flushTo(last); err != nil {
					j.fatal.Store(&asyncFatal{err: err})
					return
				}
				unsynced = 0
			}
			continue
		}
		// Ring empty: flush any pending batch so its durability (and acks) land
		// promptly at light load, mirroring the inline drain-flush.
		if unsynced > 0 {
			if err := j.flushTo(last); err != nil {
				j.fatal.Store(&asyncFatal{err: err})
				return
			}
			unsynced = 0
			continue
		}
		if j.stop.Load() {
			return // ring drained and nothing pending
		}
		runtime.Gosched()
	}
}

func (j *AsyncJournaller) appendOne(c types.Command, last *types.Seq) error {
	n := types.EncodeCommandInto(j.payloadBuf[:], c)
	if err := j.journal.Append(wal.Record{
		Seq:     uint64(c.Seq),
		TsNanos: c.TsNanos,
		Type:    uint16(c.Type),
		Payload: j.payloadBuf[:n],
	}); err != nil {
		return err
	}
	*last = c.Seq
	return nil
}

// flushTo fsyncs (when the journal supports it) then publishes the watermark.
// The watermark is captured by the caller before this returns, so it never
// over-claims coverage the fsync did not include.
func (j *AsyncJournaller) flushTo(last types.Seq) error {
	if sj, ok := j.journal.(interface{ Sync() error }); ok {
		if err := sj.Sync(); err != nil {
			return err
		}
	}
	j.durableSeq.Store(uint64(last))
	return nil
}
