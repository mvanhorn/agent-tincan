//go:build !unix

package history

// tryLockSocket always succeeds where flock is not available; a host there
// still refuses a socket that a live host answers on.
func tryLockSocket(string) (func(), bool, error) { return func() {}, true, nil }

// processAlive cannot tell here, so every recorded host counts as running.
func processAlive(pid int) bool { return pid > 0 }
