package client

import (
	"path/filepath"
	"testing"
)

func TestConfigPathExpandsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TINCAN_CONFIG", "~/.config/tincan/codex.json")
	if got, want := ConfigPath(), filepath.Join(home, ".config/tincan/codex.json"); got != want {
		t.Fatalf("ConfigPath() = %q, want %q", got, want)
	}
	t.Setenv("TINCAN_CONFIG", "/abs/agent.json")
	if got := ConfigPath(); got != "/abs/agent.json" {
		t.Fatalf("absolute path changed: %q", got)
	}
}
