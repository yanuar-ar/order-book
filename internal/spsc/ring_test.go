package spsc

import (
	"sync"
	"testing"
)

func TestPushPopFIFO(t *testing.T) {
	r := New[int](4)
	for i := 0; i < 4; i++ {
		if !r.Push(i) {
			t.Fatalf("Push(%d) returned false on non-full ring", i)
		}
	}
	for i := 0; i < 4; i++ {
		var out int
		if !r.Pop(&out) {
			t.Fatalf("Pop returned false on non-empty ring")
		}
		if out != i {
			t.Fatalf("FIFO violated: got %d, want %d", out, i)
		}
	}
}

func TestPushReturnsFalseWhenFull(t *testing.T) {
	r := New[int](2)
	if !r.Push(1) || !r.Push(2) {
		t.Fatal("Push failed before full")
	}
	if r.Push(3) {
		t.Fatal("Push succeeded on full ring")
	}
	if r.Len() != 2 {
		t.Fatalf("Len = %d, want 2", r.Len())
	}
}

func TestPopReturnsFalseWhenEmptyAndLeavesOutUntouched(t *testing.T) {
	r := New[int](2)
	out := 99
	if r.Pop(&out) {
		t.Fatal("Pop succeeded on empty ring")
	}
	if out != 99 {
		t.Fatalf("Pop on empty mutated out to %d, want 99", out)
	}
}

func TestWraparoundAcrossMask(t *testing.T) {
	r := New[int](2)
	// Fill, drain, refill repeatedly to push head/tail past the mask boundary.
	for cycle := 0; cycle < 5; cycle++ {
		if !r.Push(cycle*10) || !r.Push(cycle*10+1) {
			t.Fatalf("cycle %d: push failed", cycle)
		}
		var a, b int
		if !r.Pop(&a) || !r.Pop(&b) {
			t.Fatalf("cycle %d: pop failed", cycle)
		}
		if a != cycle*10 || b != cycle*10+1 {
			t.Fatalf("cycle %d: got (%d,%d), want (%d,%d)", cycle, a, b, cycle*10, cycle*10+1)
		}
	}
}

func TestNewPanicsOnNonPowerOfTwo(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New(3) should panic")
		}
	}()
	New[int](3)
}

// TestConcurrentProducerConsumer is the -race target: one producer, one
// consumer, verifying every pushed value arrives exactly once in order.
func TestConcurrentProducerConsumer(t *testing.T) {
	const n = 100_000
	r := New[int](1024)
	var wg sync.WaitGroup
	wg.Add(2)

	go func() { // producer
		defer wg.Done()
		for i := 0; i < n; i++ {
			for !r.Push(i) {
				// spin until space
			}
		}
	}()

	go func() { // consumer
		defer wg.Done()
		want := 0
		var got int
		for want < n {
			if r.Pop(&got) {
				if got != want {
					t.Errorf("out of order: got %d, want %d", got, want)
					return
				}
				want++
			}
		}
	}()

	wg.Wait()
}
