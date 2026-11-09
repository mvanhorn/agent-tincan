package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/relay"
)

// The relay's VAPID key is created on first run, private to its owner,
// and read back unchanged; a key file others can read is refused.
func TestPushKeyFile(t *testing.T) {
	skipOnWindows(t, skipRelayHost)
	dir := t.TempDir()
	p, err := loadOrCreatePusher(dir)
	if err != nil || p == nil {
		t.Fatalf("first run: %v", err)
	}
	path := filepath.Join(dir, relay.PushKeyFile)
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 || !fi.Mode().IsRegular() {
		t.Fatalf("push key mode %v, want a 0600 file", fi.Mode())
	}
	key, _ := os.ReadFile(path)
	if _, err := loadOrCreatePusher(dir); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if again, _ := os.ReadFile(path); !bytes.Equal(key, again) {
		t.Fatal("second run replaced the push key")
	}

	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreatePusher(dir); err == nil || !strings.Contains(err.Error(), "accessible beyond its owner") {
		t.Fatalf("group-readable push key: %v, want a refusal", err)
	}
}

// A symlink in place of the key file is refused rather than followed.
func TestPushKeyFileRefusesSymlink(t *testing.T) {
	skipOnWindows(t, skipRelayHost)
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, bytes.Repeat([]byte{1}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, relay.PushKeyFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreatePusher(dir); err == nil {
		t.Fatal("a symlinked push key was used")
	}
}
