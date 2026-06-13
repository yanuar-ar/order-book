//go:build darwin

package platform

// PinCurrentThread is a no-op on darwin: macOS exposes no thread-to-core
// affinity API. The development machine runs correctness and functional tests
// here; core pinning is exercised only on Linux.
func PinCurrentThread(cpu int) error {
	_ = cpu
	return nil
}

// Unpin is a no-op on darwin.
func Unpin() {}
