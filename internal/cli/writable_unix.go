//go:build unix

package cli

import (
	"fmt"
	"path/filepath"
	"syscall"

	"github.com/mvanhorn/agent-tincan/internal/relay"
)

const (
	accessX = 0x1
	accessW = 0x2
)

// writable refuses an upgrade the relay user could not finish: it must be
// able to write both its binary and the directory holding it.
func writable(exe string) error {
	if err := syscall.Access(exe, accessW); err != nil {
		return notWritable(exe, err)
	}
	dir := filepath.Dir(exe)
	if err := dirWritable(dir); err != nil {
		return notWritable(dir, err)
	}
	return nil
}

func notWritable(p string, err error) error {
	return fmt.Errorf("%w: %s is not writable by the relay user (%v). Self-upgrade needs the relay user to own its binary; "+
		"for a root-owned binary, keep upgrading by hand", relay.ErrUpgradeNotWritable, p, err)
}

// dirWritable needs search permission too: creating a file in a folder
// takes both.
func dirWritable(dir string) error {
	return syscall.Access(dir, accessW|accessX)
}
