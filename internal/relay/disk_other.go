//go:build !unix

package relay

// freeDiskBytes cannot measure free space here; a negative result skips
// the low-disk check.
func freeDiskBytes(string) (int64, error) { return -1, nil }
