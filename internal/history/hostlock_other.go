//go:build !unix

package history

import "os"

// tryLockSocket always succeeds where flock is not available; a host there
// still refuses a socket that a live host answers on.
func tryLockSocket(string) (func(), bool, error) { return func() {}, true, nil }

// processAlive reports whether pid names a running process. On Windows
// os.FindProcess opens the process and fails when there is none.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}
