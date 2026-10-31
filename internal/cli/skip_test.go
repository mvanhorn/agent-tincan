package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Reasons a single test does not run on Windows, where tincan is a client.
const (
	skipRelayHost  = "the relay runs on Linux and macOS only"
	skipNoShell    = "the command runs through sh -c, so it refuses on Windows"
	skipSocketWake = "socket wake needs unix sockets and file locks"
	skipPOSIXPaths = "the fixture records POSIX paths, which are not absolute on Windows"
	skipNoService  = "web install writes a launchd or systemd definition"
)

// skipOnWindows skips a single test that cannot hold on Windows and states
// why, so the Windows CI job still runs the rest of this package.
func skipOnWindows(t *testing.T, reason string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip(reason)
	}
}

// setHome points the test's home at home on every platform: Windows reads
// USERPROFILE for it and AppData for the config dir.
func setHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("AppData", filepath.Join(home, ".config"))
}

// hasMode reports whether fi has unix mode want. Windows keeps no such bits,
// so it holds there.
func hasMode(fi os.FileInfo, want os.FileMode) bool {
	return runtime.GOOS == "windows" || fi.Mode().Perm() == want
}

// fakeTincan writes a stand-in tincan binary in dir that doctor can find and
// returns its path, written so it can go in hand-written JSON or YAML:
// LookPath needs .exe on Windows, and a backslash would not parse.
func fakeTincan(t *testing.T, dir string) string {
	t.Helper()
	name := "tincan"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(path)
}
