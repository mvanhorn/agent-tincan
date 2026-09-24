package history

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// A fresh user with no Codex or Claude Code history gets a plain "no history
// found" rather than a raw file-not-found error.
func TestNoLocalHistoryIsAFriendlyError(t *testing.T) {
	home := t.TempDir()
	for _, tc := range []struct {
		r    Reader
		want string
	}{
		{&Codex{Home: filepath.Join(home, ".codex")}, "no Codex history found in " + filepath.Join(home, ".codex")},
		{&ClaudeCode{Root: filepath.Join(home, ".claude", "projects")}, "no Claude Code history found in " + filepath.Join(home, ".claude", "projects")},
	} {
		_, err := tc.r.Read(context.Background(), Query{Source: tc.r.Source(), Mode: ModeLatest, Count: 1}, Options{})
		if err == nil || !errors.Is(err, ErrNoHistory) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.r.Source(), err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "no such file") {
			t.Errorf("%s: raw ENOENT leaked: %v", tc.r.Source(), err)
		}
		if got := readFailure(Query{Source: tc.r.Source()}, err); !strings.Contains(got, "No ") || !strings.Contains(got, "history was found") {
			t.Errorf("%s: reply = %q", tc.r.Source(), got)
		}
	}
}

// The query step asks about the owner, whoever that is, so a customer's
// questions in the first person or with their own name still classify.
func TestExtractInstructionsNameNoOne(t *testing.T) {
	if strings.Contains(extractInstructions, "Matt") {
		t.Fatal("extractor prompt names a specific person")
	}
	for _, want := range []string{"the owner", `"I"`, "their name"} {
		if !strings.Contains(extractInstructions, want) {
			t.Errorf("extractor prompt missing %q", want)
		}
	}
}

// The service PATH starts with where codex and claude were found, then the
// common per-user bins, then the OS defaults. The Linux unit carries no
// macOS paths.
func TestServicePathPerOS(t *testing.T) {
	home := "/home/sam"
	linux := servicePath("linux", home, "/home/sam/.nvm/versions/node/v22/bin", "/opt/claude/bin")
	if strings.Contains(linux, "homebrew") || strings.Contains(linux, "/Library") {
		t.Fatalf("linux PATH has macOS paths: %s", linux)
	}
	for _, want := range []string{"/home/sam/.nvm/versions/node/v22/bin:/opt/claude/bin:", home + "/.local/bin", home + "/.npm-global/bin", "/usr/local/bin:/usr/bin:/bin"} {
		if !strings.Contains(linux, want) {
			t.Errorf("linux PATH %q missing %q", linux, want)
		}
	}
	mac := servicePath("darwin", "/Users/sam", "", "")
	for _, want := range []string{"/Users/sam/.local/bin", "/opt/homebrew/bin", "/usr/bin:/bin"} {
		if !strings.Contains(mac, want) {
			t.Errorf("darwin PATH %q missing %q", mac, want)
		}
	}
	// Unsafe directories are skipped, and nothing repeats.
	if got := servicePath("linux", home, "/bad:dir", "/usr/bin"); strings.Contains(got, "/bad") || strings.Count(got, "/usr/bin:") != 1 {
		t.Fatalf("PATH = %s", got)
	}
}
