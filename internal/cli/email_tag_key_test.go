package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/relay"
)

// The email-tag-key is created 0600 and 32 bytes on first start, reused on
// restart, kept apart from invite-pepper, and refused when it is unsafe,
// exactly like invite-pepper. Never print the key.
func TestEmailTagKeyCreatedPrivateAndReused(t *testing.T) {
	dir := t.TempDir()
	a, err := loadOrCreateEmailTagKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, relay.EmailTagKeyFile)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 || fi.Size() != 32 || len(a) != 32 {
		t.Fatalf("email-tag-key mode %o size %d len %d, want 0600 and 32 bytes", fi.Mode().Perm(), fi.Size(), len(a))
	}
	b, err := loadOrCreateEmailTagKey(dir)
	if err != nil || !bytes.Equal(a, b) {
		t.Fatalf("restart did not reuse the key (err %v)", err)
	}
	pepper, err := loadOrCreateInvitePepper(dir)
	if err != nil || bytes.Equal(pepper, a) {
		t.Fatalf("invite pepper shares the email tag key (err %v)", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateEmailTagKey(dir); err == nil || !strings.Contains(err.Error(), "email tag key") {
		t.Fatalf("a group-readable key was accepted: %v", err)
	}
}

// The waker gets open asks and tag minting from the live relay.
func TestWakerOptionsCarryRequestMail(t *testing.T) {
	opts := wakerOptions(relayFlags{}, new(relay.Server))
	if opts.OpenAsks == nil || opts.RequestTag == nil {
		t.Fatal("waker OpenAsks or RequestTag callback missing")
	}
}
