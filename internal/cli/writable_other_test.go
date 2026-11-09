//go:build !unix

package cli

import (
	"errors"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/relay"
)

// The relay runs on unix only, so relay-upgrade stops with the error the
// admin already gets for a binary the relay cannot replace.
func TestWritableRefusesWithoutUnixPermissions(t *testing.T) {
	if err := writable(t.TempDir()); !errors.Is(err, relay.ErrUpgradeNotWritable) {
		t.Fatalf("writable = %v, want ErrUpgradeNotWritable", err)
	}
}
