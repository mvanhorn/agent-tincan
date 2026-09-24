package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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

func TestHistoryInstallWritesManifest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TINCAN_HISTORY_NATIVE_DIR", filepath.Join(home, "native"))
	out, err := run(t, Root(), "history", "install", "--binary", "/opt/tincan/tincan")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	dir, err := history.NativeManifestDir(runtime.GOOS, home)
	if err != nil {
		t.Skip(err)
	}
	path := filepath.Join(dir, history.NativeHostName+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("manifest not written: %v\n%s", err, out)
	}
	if !strings.Contains(string(b), "chrome-extension://"+history.DefaultExtensionID+"/") || !strings.Contains(out, path) {
		t.Fatalf("manifest:\n%s\noutput:\n%s", b, out)
	}
	out, err = run(t, Root(), "history", "install", "--binary", "/opt/tincan/tincan", "--extension-id", "abcdefghijklmnopabcdefghijklmnop")
	if err != nil {
		t.Fatalf("install with id: %v\n%s", err, out)
	}
	b, _ = os.ReadFile(path)
	if !strings.Contains(string(b), "chrome-extension://abcdefghijklmnopabcdefghijklmnop/") {
		t.Fatalf("custom id not used:\n%s", b)
	}
	if _, err := run(t, Root(), "history", "install", "--extension-id", "NOT-AN-ID"); err == nil {
		t.Fatal("bad extension id accepted")
	}
}

func TestHistoryLiveSourceUnavailable(t *testing.T) {
	historyEnv(t)
	t.Setenv("TINCAN_HISTORY_NATIVE_DIR", t.TempDir())
	for _, src := range []string{"chatgpt", "claude-ai"} {
		out, err := run(t, Root(), "history", src)
		if err == nil || !strings.Contains(err.Error(), "source unavailable: "+src+": ") {
			t.Fatalf("%s: want source unavailable, got %v\n%s", src, err, out)
		}
	}
}

func TestHistoryInstallWritesServiceDefinitionWithoutLoading(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no service definition on " + runtime.GOOS)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TINCAN_HISTORY_NATIVE_DIR", filepath.Join(home, "native"))
	// A PATH with no launchctl or systemctl proves install never runs them.
	t.Setenv("PATH", t.TempDir())
	out, err := run(t, Root(), "history", "install", "--binary", "/opt/tincan/tincan")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	var def, next string
	if runtime.GOOS == "darwin" {
		def = filepath.Join(home, "Library", "LaunchAgents", history.ServiceLabel+".plist")
		next = "launchctl bootstrap gui/"
	} else {
		def = filepath.Join(home, ".config", "systemd", "user", "tincan-history.service")
		next = "systemctl --user enable --now tincan-history.service"
	}
	b, err := os.ReadFile(def)
	if err != nil {
		t.Fatalf("service definition not written: %v\n%s", err, out)
	}
	if !strings.Contains(string(b), "/opt/tincan/tincan") || !strings.Contains(out, def) || !strings.Contains(out, next) {
		t.Fatalf("definition:\n%s\noutput:\n%s", b, out)
	}
	if _, err := run(t, Root(), "history", "install", "--binary", "/opt/tincan/tincan", "--no-service"); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryServeFlagsAndMissingConfig(t *testing.T) {
	out, err := run(t, Root(), "history", "serve", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--config", "--allowlist", "--codex"} {
		if !strings.Contains(out, want) {
			t.Fatalf("serve help missing %s:\n%s", want, out)
		}
	}
	t.Setenv("TINCAN_RELAY", "")
	_, err = run(t, Root(), "history", "serve", "--config", filepath.Join(t.TempDir(), "history.json"))
	if err == nil || !strings.Contains(err.Error(), "no relay configured") {
		t.Fatalf("serve without a config: err = %v", err)
	}
	p := filepath.Join(t.TempDir(), "allow.txt")
	if err := os.WriteFile(p, []byte("not/a name\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "history.json")
	if err := os.WriteFile(cfg, []byte(`{"relay":"http://127.0.0.1:1","agent":"history"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = run(t, Root(), "history", "serve", "--config", cfg, "--allowlist", p)
	if err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("serve with a bad allowlist: err = %v", err)
	}
}

// serveConfig writes a history serve config for relay and agent.
func serveConfig(t *testing.T, relayURL, agent string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"relay": relayURL, "agent": agent})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// whoamiRelay answers /v1/whoami with name and fails the test on any other
// call, so a refusal is proven to happen before any poll or claim.
func whoamiRelay(t *testing.T, name string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/whoami" {
			_ = json.NewEncoder(w).Encode(map[string]string{"name": name})
			return
		}
		t.Errorf("history serve called %s %s before confirming its identity", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A config joined as another agent (TINCAN_CONFIG pointing at codex.json)
// must not poll and claim that agent's inbox.
func TestHistoryServeRefusesConfigForAnotherAgent(t *testing.T) {
	srv := whoamiRelay(t, "codex")
	cfg := serveConfig(t, srv.URL, "codex")
	allow := filepath.Join(t.TempDir(), "allow.txt")
	_, err := run(t, Root(), "history", "serve", "--config", cfg, "--allowlist", allow)
	if err == nil || !strings.Contains(err.Error(), `"codex"`) || !strings.Contains(err.Error(), "history.json") {
		t.Fatalf("serve with a codex config: err = %v", err)
	}
}

// With no agent in the config, the relay's answer decides: a machine the
// relay knows as codex is refused too.
func TestHistoryServeRefusesWhenRelaySaysAnotherAgent(t *testing.T) {
	srv := whoamiRelay(t, "codex")
	cfg := serveConfig(t, srv.URL, "")
	allow := filepath.Join(t.TempDir(), "allow.txt")
	_, err := run(t, Root(), "history", "serve", "--config", cfg, "--allowlist", allow)
	if err == nil || !strings.Contains(err.Error(), `"codex"`) || !strings.Contains(err.Error(), "history.json") {
		t.Fatalf("serve as codex per whoami: err = %v", err)
	}
}

// startupLine runs a serve command against a relay that confirms name and
// stops the command at its first poll, and returns what it printed.
func startupLine(t *testing.T, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/whoami" {
			_ = json.NewEncoder(w).Encode(map[string]string{"name": name})
			return
		}
		cancel()
		http.Error(w, "stopping", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	cmd := Root()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append(args, "--config", serveConfig(t, srv.URL, name)))
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

// The startup line says who may use the agent: everyone by default, "*",
// or the listed names.
func TestServeStartupLineDescribesAllowlist(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.txt")
	star := filepath.Join(dir, "star.txt")
	names := filepath.Join(dir, "names.txt")
	if err := os.WriteFile(star, []byte("*\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(names, []byte("grokbot, codex\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		args []string
	}{
		{"history", []string{"history", "serve"}},
		{"chatgpt-web", []string{"web", "serve", "--site", "chatgpt"}},
		{"claude-web", []string{"web", "serve", "--site", "claude-ai"}},
	}
	for _, c := range cases {
		for allow, want := range map[string]string{
			missing: "allowlist: all joined agents (no file at " + missing + ")",
			star:    "allowlist: all joined agents (* in " + star + ")",
			names:   "allowlist " + names + ": grokbot, codex",
		} {
			out := startupLine(t, c.name, append(slices.Clone(c.args), "--allowlist", allow)...)
			if !strings.Contains(out, want) {
				t.Errorf("%s with %s: output missing %q:\n%s", c.name, filepath.Base(allow), want, out)
			}
		}
	}
}
