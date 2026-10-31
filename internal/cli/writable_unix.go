//go:build unix

package cli

import (
	"fmt"
	"path/filepath"
	"syscall"

	"github.com/mvanhorn/agent-tincan/internal/relay"
)

// writable refuses an upgrade the relay user could not finish: it must be
// able to write both its binary and the directory holding it.
func writable(exe string) error {
	const wOK = 0x2
	for _, p := range []string{exe, filepath.Dir(exe)} {
		if err := syscall.Access(p, wOK); err != nil {
			return fmt.Errorf("%w: %s is not writable by the relay user (%v). Self-upgrade needs the relay user to own its binary; "+
				"for a root-owned binary, keep upgrading by hand", relay.ErrUpgradeNotWritable, p, err)
		}
	}
	return nil
}
