package cli

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// On the relay machine, admin commands need no flags: the local admin
// socket in the default state dir makes this machine the admin.
func TestAdminRelayUsesLocalSocket(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if r, err := adminRelay("", "http://tincan-relay"); err != nil || r.Base() != "http://tincan-relay" {
		t.Fatalf("explicit relay: %v %v", r, err)
	}
	dir := defaultStateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Unix socket paths are length-limited; listen from inside the dir.
	wd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", "admin.sock")
	if err != nil {
		t.Skipf("unix socket unavailable: %v", err)
	}
	defer ln.Close()
	r, err := adminRelay("", "")
	if err != nil || r.Base() != "http://tincan-admin" {
		t.Fatalf("want the local admin socket, got %v %v", r, err)
	}
	if r, _ := adminRelay("", "http://tincan-relay"); r.Base() != "http://tincan-relay" {
		t.Fatal("an explicit --relay must win over the local socket")
	}
}
