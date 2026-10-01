//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestInvitePepperRejectsFIFOWithoutWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invite-pepper")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := readInvitePepperFile(path); err == nil || got != nil {
		t.Fatal("FIFO was accepted as a regular file")
	}
}

func TestInvitePepperOpenDoesNotFollowSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "fixture")
	if err := os.WriteFile(target, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "invite-pepper")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	f, err := openInvitePepperFile(path)
	if f != nil {
		f.Close()
	}
	if err == nil {
		t.Fatal("open followed the final-component symlink")
	}
}
