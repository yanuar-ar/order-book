//go:build linux

package platform

import "runtime"

// PinCurrentThread locks the calling goroutine to its OS thread so the engine's
// hot goroutines stay on dedicated cores.
//
// CPU-affinity assignment (unix.SchedSetaffinity to bind the locked thread to
// the given cpu and isolate it from the scheduler) requires golang.org/x/sys
// and is wired in when that dependency is added; the cpu argument is accepted
// now so callers and tests target the final signature.
func PinCurrentThread(cpu int) error {
	runtime.LockOSThread()
	_ = cpu // affinity binding deferred until golang.org/x/sys is vendored
	return nil
}

// Unpin releases the OS-thread lock.
func Unpin() { runtime.UnlockOSThread() }
