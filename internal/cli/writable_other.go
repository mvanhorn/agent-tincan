//go:build !unix

package cli

import (
	"fmt"

	"github.com/mvanhorn/agent-tincan/internal/relay"
)

// writable refuses every upgrade where there are no unix file permissions to
// check. The relay does not run there.
func writable(string) error {
	return fmt.Errorf("%w: relay self-upgrade needs unix file permissions; the relay runs on Linux and macOS", relay.ErrUpgradeNotWritable)
}
