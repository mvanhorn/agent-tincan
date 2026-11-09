package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

func TestGitRepoName(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "Agent-Tincan")
	sub := filepath.Join(repo, "internal", "cli")
	if err := os.MkdirAll(filepath.Join(repo, ".git", "worktrees", "wt1"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sub, 0700); err != nil {
		t.Fatal(err)
	}
	if got := gitRepoName(sub); got != "Agent-Tincan" {
		t.Fatalf("from a subdirectory = %q", got)
	}

	// A linked worktree answers to its main repository's name.
	worktree := filepath.Join(root, "feature-branch")
	if err := os.MkdirAll(worktree, 0700); err != nil {
		t.Fatal(err)
	}
	link := "gitdir: " + filepath.Join(repo, ".git", "worktrees", "wt1") + "\n"
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte(link), 0600); err != nil {
		t.Fatal(err)
	}
	if got := gitRepoName(worktree); got != "Agent-Tincan" {
		t.Fatalf("from a worktree = %q", got)
	}

	// A submodule's .git file points elsewhere, so it keeps its own name.
	module := filepath.Join(repo, "vendored")
	if err := os.MkdirAll(module, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, ".git"), []byte("gitdir: ../.git/modules/vendored\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := gitRepoName(module); got != "vendored" {
		t.Fatalf("from a submodule = %q", got)
	}

	if got := gitRepoName(t.TempDir()); got != "" {
		t.Fatalf("outside a repository = %q", got)
	}
}

func TestProjectLabel(t *testing.T) {
	for name, want := range map[string]string{
		"":             "",
		"agent-tincan": "agent-tincan",
		"Agent Tincan": "agent-tincan",
		"café":         "caf-",
	} {
		if got := projectLabel(name); got != want {
			t.Errorf("projectLabel(%q) = %q, want %q", name, got, want)
		}
		if err := envelope.ValidProject(projectLabel(name)); err != nil {
			t.Errorf("projectLabel(%q) is invalid: %v", name, err)
		}
	}
	if got := projectLabel(strings.Repeat("x", 100)); len(got) != envelope.MaxProject {
		t.Errorf("long name gives %d bytes", len(got))
	}
}

func TestSessionProject(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "My Repo")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if got, err := sessionProject("", false); got != "my-repo" || err != nil {
		t.Fatalf("derived = %q %v", got, err)
	}
	if got, err := sessionProject("chosen", false); got != "chosen" || err != nil {
		t.Fatalf("flag = %q %v", got, err)
	}
	if got, err := sessionProject("", true); got != "" || err != nil {
		t.Fatalf("off = %q %v", got, err)
	}
	if _, err := sessionProject("Not Valid", false); err == nil {
		t.Fatal("a bad flag was accepted")
	}
}

// tincan ask --project reaches the matching window, tells the sender when
// no such window is live, and refuses a bad label before calling the relay.
func TestAskProjectFlag(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	t.Setenv("TINCAN_CONFIG", filepath.Join(t.TempDir(), "grokbot.json"))
	t.Setenv("TINCAN_RELAY", "")
	t.Setenv("TINCAN_PROXY", "")
	if err := client.SaveConfig(client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"}); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, askCmd(), "muse", "fix it", "--project", "Bad Label", "--wait", "0s"); err == nil {
		t.Fatalf("bad label accepted: %s", out)
	}
	out, err := run(t, askCmd(), "muse", "fix it", "--project", "alpha", "--wait", "0s")
	if err != nil || !strings.Contains(out, "No reply yet") {
		t.Fatalf("ask = %q %v", out, err)
	}
	wrong, right := m.Client(t, "muse"), m.Client(t, "muse")
	wrong.SetProject("beta")
	right.SetProject("alpha")
	if in, err := wrong.Poll(t.Context(), time.Millisecond); err != nil || len(in.Requests) != 0 {
		t.Fatalf("wrong window = %+v %v", in, err)
	}
	if in, err := right.Poll(t.Context(), time.Millisecond); err != nil || len(in.Requests) != 1 {
		t.Fatalf("matching window = %+v %v", in, err)
	}
}
