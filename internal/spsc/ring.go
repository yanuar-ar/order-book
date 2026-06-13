// Package spsc provides a lock-free single-producer/single-consumer ring
// buffer. Exactly one goroutine may Push and exactly one (distinct) goroutine
// may Pop concurrently. Values are stored by value, so for plain-old-data T the
// hot path is allocation-free.
package spsc

import "sync/atomic"

// Ring is a bounded SPSC queue with power-of-two capacity. head and tail sit on
// separate cache lines (padded) to avoid false sharing between producer and
// consumer.
type Ring[T any] struct {
	_    [64]byte
	head atomic.Uint64 // read by consumer, advanced on Pop
	_    [56]byte
	tail atomic.Uint64 // read by producer, advanced on Push
	_    [56]byte
	buf  []T
	mask uint64
}

// New returns a Ring with the given capacity, which must be a power of two and
// greater than zero. A non-conforming capacity is a startup programming error
// and panics.
func New[T any](capacity uint64) *Ring[T] {
	if capacity == 0 || capacity&(capacity-1) != 0 {
		panic("spsc: capacity must be a power of two")
	}
	return &Ring[T]{buf: make([]T, capacity), mask: capacity - 1}
}

// Cap returns the ring capacity.
func (r *Ring[T]) Cap() uint64 { return uint64(len(r.buf)) }

// Len returns the current number of buffered items. Safe to call from either
// side; the value may be stale by the time it is read.
func (r *Ring[T]) Len() uint64 { return r.tail.Load() - r.head.Load() }

// Push appends v. It returns false (without storing) when the ring is full.
// Only the producer goroutine may call Push.
func (r *Ring[T]) Push(v T) bool {
	t := r.tail.Load()
	if t-r.head.Load() >= uint64(len(r.buf)) {
		return false // full
	}
	r.buf[t&r.mask] = v
	r.tail.Store(t + 1) // publish
	return true
}

// Pop removes the oldest item into *out. It returns false (leaving *out
// untouched) when the ring is empty. Only the consumer goroutine may call Pop.
func (r *Ring[T]) Pop(out *T) bool {
	h := r.head.Load()
	if h == r.tail.Load() {
		return false // empty
	}
	*out = r.buf[h&r.mask]
	r.head.Store(h + 1)
	return true
}
