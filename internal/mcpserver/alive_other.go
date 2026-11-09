//go:build !unix && !windows

package mcpserver

// LaunchRunning cannot check processes here, so every record without an
// end counts as running.
func LaunchRunning(l Launch) bool { return l.Ended.IsZero() && l.PID > 0 }
