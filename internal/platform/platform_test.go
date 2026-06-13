package platform

import (
	"runtime/debug"
	"testing"
)

func TestGCOffRestore(t *testing.T) {
	// Establish a known baseline, then verify GCOff reports it and GCOn restores.
	debug.SetGCPercent(100)
	prev := GCOff()
	if prev != 100 {
		t.Fatalf("GCOff returned prev %d, want 100", prev)
	}
	// GCOff set -1; restoring should put 100 back (GCOn returns nothing, so
	// re-read via another SetGCPercent which returns the current value).
	GCOn(prev)
	cur := debug.SetGCPercent(100)
	if cur != 100 {
		t.Fatalf("after GCOn, GC percent = %d, want 100", cur)
	}
}

func TestPinCurrentThreadNoError(t *testing.T) {
	// On darwin this is a no-op; on linux it locks the OS thread. Either way it
	// must not error, and Unpin must be safe to call.
	if err := PinCurrentThread(0); err != nil {
		t.Fatalf("PinCurrentThread: %v", err)
	}
	Unpin()
}
