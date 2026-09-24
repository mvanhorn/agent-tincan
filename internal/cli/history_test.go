package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/history"
)

// historyEnv points the history command at copies of the history fixtures
// and pins "now" to the fixtures' today.
func historyEnv(t *testing.T) (codexHome, claudeDir string) {
	t.Helper()
	base := t.TempDir()
	codexHome = filepath.Join(base, "codex")
	claudeDir = filepath.Join(base, "claude")
	if err := os.CopyFS(codexHome, os.DirFS("../history/testdata/codex")); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(claudeDir, os.DirFS("../history/testdata/claudecode")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	t.Setenv("TINCAN_HISTORY_SCRATCH", "/Users/matt/.config/tincan/history-scratch")
	old := historyNow
	historyNow = func() time.Time { return time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { historyNow = old })
	return codexHome, claudeDir
}

func TestHistoryCodexLatestText(t *testing.T) {
	historyEnv(t)
	out, err := run(t, Root(), "history", "codex")
	if err != nil {
		t.Fatalf("history codex: %v\n%s", err, out)
	}
	for _, want := range []string{"make the ears bigger like this sketch", "/Users/matt/Documents/Codex/fox-logo", "Fox logo for landing page", "Ears enlarged."} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "tincan wake") || strings.Contains(out, "extract a query") {
		t.Fatalf("exec or scratch thread leaked into latest:\n%s", out)
	}
}

func TestHistoryCodexListAllJSON(t *testing.T) {
	historyEnv(t)
	out, err := run(t, Root(), "history", "codex", "--list", "3", "--all", "--json")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var convs []history.Conversation
	if err := json.Unmarshal([]byte(out), &convs); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if len(convs) != 3 {
		t.Fatalf("got %d, want 3", len(convs))
	}
	var cwds []string
	for _, c := range convs {
		cwds = append(cwds, c.Cwd)
	}
	if !strings.Contains(strings.Join(cwds, ","), "/Users/matt/tincan-codex") {
		t.Fatalf("--all list should include the exec wake's cwd: %v", cwds)
	}
}

func TestHistoryListText(t *testing.T) {
	historyEnv(t)
	out, err := run(t, Root(), "history", "codex", "--list", "2")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "01a0c000-0000-7000-8000-00000000000a") || !strings.Contains(out, "/Users/matt/Documents/Codex/fox-logo") {
		t.Fatalf("list output:\n%s", out)
	}
}

func TestHistorySearchWritesImages(t *testing.T) {
	historyEnv(t)
	dir := filepath.Join(t.TempDir(), "imgs")
	out, err := run(t, Root(), "history", "codex", "--search", "fox logo", "--images-dir", dir, "--json")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var convs []history.Conversation
	if err := json.Unmarshal([]byte(out), &convs); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	var paths []string
	for _, c := range convs {
		for _, m := range c.Messages {
			for _, im := range m.Images {
				paths = append(paths, im.Path)
			}
		}
	}
	if len(paths) < 2 {
		t.Fatalf("want the attached and generated images, got %v", paths)
	}
	for _, p := range paths {
		if filepath.Dir(p) != dir {
			t.Fatalf("image %s not in %s", p, dir)
		}
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("image %s: %v %v", p, st, err)
		}
	}
	if strings.Contains(out, `"data"`) || strings.Contains(out, "base64") {
		t.Fatal("image bytes leaked into JSON output")
	}
}

func TestHistoryClaudeCodeSearchAndID(t *testing.T) {
	historyEnv(t)
	out, err := run(t, Root(), "history", "claude-code", "--search", "pelicans")
	if err != nil || !strings.Contains(out, "plain string prompt about pelicans") {
		t.Fatalf("search: %v\n%s", err, out)
	}
	out, err = run(t, Root(), "history", "claude-code", "--id", "11111111-1111-4111-8111-111111111111")
	if err != nil || !strings.Contains(out, "now write tests for the relay") || !strings.Contains(out, "summarize the relay design") {
		t.Fatalf("id: %v\n%s", err, out)
	}
}

func TestHistoryBadArgs(t *testing.T) {
	historyEnv(t)
	cases := [][]string{
		{"history", "gemini"},
		{"history"},
		{"history", "codex", "--latest", "--search", "fox"},
		{"history", "codex", "--id", "../../etc"},
		{"history", "codex", "--list", "0"},
	}
	for _, args := range cases {
		if out, err := run(t, Root(), args...); err == nil {
			t.Errorf("%v: want error, got output:\n%s", args, out)
		}
	}
}
