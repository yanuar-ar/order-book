// Package platform holds the OS- and runtime-level controls for the engine's
// performance mode: GC suspension and core pinning. Core pinning is
// build-tagged (Linux has real affinity control; darwin is a no-op), so the
// engine builds and runs on the development machine while the production tuning
// lives behind the Linux path.
package platform

import "runtime/debug"

// GCOff disables the garbage collector for the duration of a measured session
// and returns the previous GC percent so it can be restored. The engine targets
// a zero-allocation hot path; GC off removes residual collection jitter.
func GCOff() int {
	return debug.SetGCPercent(-1)
}

// GCOn restores a previously saved GC percent (see GCOff).
func GCOn(prev int) {
	debug.SetGCPercent(prev)
}
