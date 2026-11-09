//go:build windows

package mcpserver

import (
	"errors"
	"time"

	"golang.org/x/sys/windows"
)

// stillActive is the exit code GetExitCodeProcess gives a running process.
const stillActive = 259

// LaunchRunning reports whether the tincan mcp that wrote l is still
// running. A host can stop tincan mcp with TerminateProcess, which leaves a
// record with no end, and Windows can hand that pid to an unrelated process
// later, so a live pid counts only if its process started no later than the
// record (which tincan mcp writes as it starts).
func LaunchRunning(l Launch) bool {
	if !l.Ended.IsZero() || l.PID <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(l.PID))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED) // cannot tell; the pid is alive
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err == nil && code != stillActive {
		return false
	}
	var created, exited, kernel, user windows.Filetime
	if l.Started.IsZero() || windows.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return true // cannot tell; the pid is alive
	}
	// The same margin as the unix check.
	return !time.Unix(0, created.Nanoseconds()).After(l.Started.Add(2 * time.Second))
}
