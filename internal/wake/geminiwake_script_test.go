package wake

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests run examples/gemini-cli/gemini-wake.sh with fake agy and
// gemini binaries that record their argv, the way codexwake_script_test.go
// does for codex, and a fake tincan that records operator notices. The
// shared library comes from examples/lib, where the script finds it in a
// repo checkout.

// fakeGeminiEngine answers --version, records each real run's argv (NUL
// separated, one file per run) and can fail with a message on stderr.
const fakeGeminiEngine = `#!/bin/sh
if [ "${1:-}" = --version ]; then
  echo "${FAKE_VERSION:-1.2.3}"
  exit 0
fi
n=$(ls "$ARGV_DIR" 2>/dev/null | grep -c "^run\\." || true)
for a in "$@"; do printf '%s\0' "$a"; done > "$ARGV_DIR/run.$n"
pwd -P > "$ARGV_DIR/cwd.$n"
if [ -n "${FAKE_STDERR:-}" ]; then
  echo "$FAKE_STDERR" >&2
fi
if [ -n "${FAKE_SLEEP:-}" ]; then
  : > "$ARGV_DIR/sleeping"
  sleep "$FAKE_SLEEP"
fi
echo "${FAKE_STDOUT:-{\"response\":\"done\"}}"
exit "${FAKE_EXIT:-0}"
`

type geminiHarness struct {
	t                                   *testing.T
	home, dir, config, state, argvDir   string
	notices, agyMCP, geminiSettings, wd string
	env                                 []string
}

func newGeminiHarness(t *testing.T) *geminiHarness {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("gemini-wake.sh is a POSIX sh script")
	}
	dir := t.TempDir()
	h := &geminiHarness{t: t, dir: dir,
		home:    filepath.Join(dir, "home"),
		config:  filepath.Join(dir, "home", ".config", "tincan", "gemini-cli.json"),
		state:   filepath.Join(dir, "state"),
		argvDir: filepath.Join(dir, "argv"),
		notices: filepath.Join(dir, "notices"),
		wd:      filepath.Join(dir, "home", "tincan-gemini"),
	}
	h.agyMCP = filepath.Join(h.home, ".gemini", "config", "mcp_config.json")
	h.geminiSettings = filepath.Join(h.home, ".gemini", "settings.json")
	mkdirs(t, filepath.Dir(h.config), h.argvDir, filepath.Dir(h.agyMCP))
	for _, n := range []string{"agy", "gemini"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(fakeGeminiEngine), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "tincan"), []byte(fakeTincan), 0o755); err != nil {
		t.Fatal(err)
	}
	h.servers(h.agyMCP, map[string]any{"agent-tincan": h.tincanEntry(h.config)})
	h.servers(h.geminiSettings, map[string]any{"agent-tincan": h.tincanEntry(h.config)})
	return h
}

func (h *geminiHarness) tincanEntry(config string) map[string]any {
	return map[string]any{"command": "tincan", "args": []string{"mcp"}, "env": map[string]string{"TINCAN_CONFIG": config}, "trust": true}
}

// servers writes an MCP config file holding the given mcpServers map.
func (h *geminiHarness) servers(file string, servers map[string]any) {
	h.t.Helper()
	raw, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		h.t.Fatal(err)
	}
	mkdirs(h.t, filepath.Dir(file))
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

type geminiRun struct {
	err    error
	stdout string
	stderr string
}

func (h *geminiHarness) run(env ...string) geminiRun {
	h.t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", "examples", "gemini-cli", "gemini-wake.sh"))
	if err != nil {
		h.t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", script)
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + h.home,
		"AGY_BIN=" + filepath.Join(h.dir, "agy"),
		"GEMINI_BIN=" + filepath.Join(h.dir, "gemini"),
		"TINCAN_BIN=" + filepath.Join(h.dir, "tincan"),
		"TINCAN_WAKE_STATE_DIR=" + h.state,
		"TINCAN_WAKE_OPERATOR=ops",
		"TINCAN_CONFIG=" + h.config,
		"ARGV_DIR=" + h.argvDir,
		"NOTICES=" + h.notices,
		"TINCAN_WAITING=2",
	}, h.env...)
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	return geminiRun{err: err, stdout: stdout.String(), stderr: stderr.String()}
}

// runs returns the argv of every engine run so far, in order.
func (h *geminiHarness) runs() []wakeRun {
	h.t.Helper()
	var out []wakeRun
	for i := 0; ; i++ {
		raw, err := os.ReadFile(filepath.Join(h.argvDir, "run."+strconv.Itoa(i)))
		if err != nil {
			return out
		}
		out = append(out, wakeRun{argv: strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")})
	}
}

func (h *geminiHarness) noticeCount() int {
	raw, err := os.ReadFile(h.notices)
	if err != nil {
		return 0
	}
	return strings.Count(string(raw), "\n")
}

func (h *geminiHarness) backoff() bool {
	_, err := os.Stat(filepath.Join(h.state, "backoff"))
	return err == nil
}

func has(argv []string, a string) bool {
	return slices.Contains(argv, a)
}

func checkDrainPrompt(t *testing.T, p string) {
	t.Helper()
	for _, want := range []string{"2 Agent Tincan item(s) waiting", "check_inbox", "call reply", "until it returns nothing waiting"} {
		if !strings.Contains(p, want) {
			t.Errorf("drain prompt lacks %q:\n%s", want, p)
		}
	}
}

// The default engine is Antigravity: headless, JSON output, tool approval
// skipped, the drain prompt, run in the wake's workdir. agy has no
// documented sandbox, so it only runs when the owner opts in to running it
// unconfined.
func TestGeminiWakeAgyEngineArgv(t *testing.T) {
	h := newGeminiHarness(t)
	r := h.run("TINCAN_GEMINI_ALLOW_UNCONFINED=1")
	if r.err != nil {
		t.Fatalf("wake: %v\nstderr:\n%s", r.err, r.stderr)
	}
	runs := h.runs()
	if len(runs) != 1 {
		t.Fatalf("agy ran %d times, want 1\nstderr:\n%s", len(runs), r.stderr)
	}
	a := runs[0]
	if got := a.flagValues("--output-format"); len(got) != 1 || got[0] != "json" {
		t.Fatalf("--output-format = %q, want json: %q", got, a.argv)
	}
	if !has(a.argv, "--dangerously-skip-permissions") {
		t.Fatalf("agy argv lacks --dangerously-skip-permissions: %q", a.argv)
	}
	if got := a.flagValues("-p"); len(got) != 1 {
		t.Fatalf("agy argv lacks one -p prompt: %q", a.argv)
	}
	checkDrainPrompt(t, a.prompt())
	cwd, _ := os.ReadFile(filepath.Join(h.argvDir, "cwd.0"))
	if strings.TrimSpace(string(cwd)) != canonical(t, h.wd) {
		t.Fatalf("agy ran in %q, want the workdir %q", strings.TrimSpace(string(cwd)), h.wd)
	}
	if !strings.Contains(r.stdout, `"response":"done"`) {
		t.Fatalf("engine output not passed through to the listener log:\n%s", r.stdout)
	}
	if !strings.Contains(r.stderr, "unconfined") {
		t.Fatalf("an unconfined agy run is not called out in the log:\n%s", r.stderr)
	}
}

// Without the opt-in, the agy engine refuses once, clearly, and backs off.
func TestGeminiWakeAgyRefusesUnconfinedByDefault(t *testing.T) {
	h := newGeminiHarness(t)
	r := h.run()
	if r.err == nil || len(h.runs()) != 0 {
		t.Fatalf("agy ran without the unconfined opt-in: %v\nstderr:\n%s", r.err, r.stderr)
	}
	if !strings.Contains(r.stderr, "TINCAN_GEMINI_ALLOW_UNCONFINED") || !h.backoff() || h.noticeCount() != 1 {
		t.Fatalf("refusal: backoff %v, notices %d\nstderr:\n%s", h.backoff(), h.noticeCount(), r.stderr)
	}
	// It says where the opt-in goes and how to clear the backoff it sets.
	for _, want := range []string{"tincan listen --exec", "restart the listener", "/backoff", "Set up with a Google account (no API key)"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("refusal does not say %q:\n%s", want, r.stderr)
		}
	}
}

// The Gemini CLI engine runs with an API key: yolo approval, JSON output,
// its sandbox, only the tincan server allowed, and operator write roots as
// include directories, plus the attachments directory tincan mcp saves to
// (attachments/<agent> beside TINCAN_CONFIG), made private.
func TestGeminiWakeGeminiEngineArgv(t *testing.T) {
	h := newGeminiHarness(t)
	if err := os.WriteFile(h.config, []byte(`{"relay":"http://relay","agent":"gemini-cli"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(h.home, "code", "proj")
	outside := filepath.Join(h.dir, "outside")
	mkdirs(t, proj, outside)
	r := h.run("TINCAN_GEMINI_ENGINE=gemini", "GEMINI_API_KEY=dummy-key",
		"TINCAN_GEMINI_WRITE_ROOTS="+proj+":"+outside)
	if r.err != nil {
		t.Fatalf("wake: %v\nstderr:\n%s", r.err, r.stderr)
	}
	runs := h.runs()
	if len(runs) != 1 {
		t.Fatalf("gemini ran %d times, want 1\nstderr:\n%s", len(runs), r.stderr)
	}
	a := runs[0]
	if !has(a.argv, "--approval-mode=yolo") {
		t.Fatalf("gemini argv lacks --approval-mode=yolo: %q", a.argv)
	}
	if !has(a.argv, "--sandbox") {
		t.Fatalf("gemini argv lacks --sandbox: %q", a.argv)
	}
	if got := a.flagValues("--output-format"); len(got) != 1 || got[0] != "json" {
		t.Fatalf("--output-format = %q, want json", got)
	}
	if got := a.flagValues("--allowed-mcp-server-names"); strings.Join(got, ",") != "agent-tincan" {
		t.Fatalf("--allowed-mcp-server-names = %q, want only the tincan server", got)
	}
	att := filepath.Join(filepath.Dir(h.config), "attachments", "gemini-cli")
	if got := a.flagValues("--include-directories"); strings.Join(got, ",") != canonical(t, proj)+","+canonical(t, att) {
		t.Fatalf("--include-directories = %q, want %s and %s\nstderr:\n%s", got, proj, att, r.stderr)
	}
	if fi, err := os.Stat(att); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("attachments dir %s: %v, mode %v; want 0700", att, err, fi)
	}
	if !strings.Contains(r.stderr, outside) {
		t.Fatalf("refused write root not reported:\n%s", r.stderr)
	}
	for _, bad := range []string{"--yolo", "--dangerously-skip-permissions"} {
		if has(a.argv, bad) {
			t.Fatalf("gemini argv has %s: %q", bad, a.argv)
		}
	}
	checkDrainPrompt(t, a.prompt())
}

// The gemini engine without GEMINI_API_KEY fails once with a clear message,
// then backs off quietly with requests left queued.
func TestGeminiWakeMissingAPIKeyFailsOnce(t *testing.T) {
	h := newGeminiHarness(t)
	r := h.run("TINCAN_GEMINI_ENGINE=gemini")
	if r.err == nil || len(h.runs()) != 0 {
		t.Fatalf("gemini ran without an API key: %v\nstderr:\n%s", r.err, r.stderr)
	}
	if !strings.Contains(r.stderr, "GEMINI_API_KEY") || !h.backoff() || h.noticeCount() != 1 {
		t.Fatalf("missing key: backoff %v, notices %d\nstderr:\n%s", h.backoff(), h.noticeCount(), r.stderr)
	}
	r = h.run("TINCAN_GEMINI_ENGINE=gemini")
	if r.err != nil || len(h.runs()) != 0 || h.noticeCount() != 1 {
		t.Fatalf("second nudge: %v, runs %d, notices %d\nstderr:\n%s", r.err, len(h.runs()), h.noticeCount(), r.stderr)
	}
}

// An expired agy login backs off at once (a retry cannot log in) and tells
// the operator once; the requests stay queued.
func TestGeminiWakeAgyAuthFailureBacksOff(t *testing.T) {
	h := newGeminiHarness(t)
	h.env = []string{"TINCAN_GEMINI_ALLOW_UNCONFINED=1"}
	r := h.run("FAKE_EXIT=1", "FAKE_STDERR=Error: authentication required. Run agy to log in.")
	if r.err == nil {
		t.Fatalf("auth failure exited 0\nstderr:\n%s", r.stderr)
	}
	if !h.backoff() || h.noticeCount() != 1 {
		t.Fatalf("auth failure: backoff %v, notices %d\nstderr:\n%s", h.backoff(), h.noticeCount(), r.stderr)
	}
	raw, _ := os.ReadFile(h.notices)
	if !strings.Contains(string(raw), "log in") {
		t.Fatalf("notice does not say the login needs the owner: %q", raw)
	}
	// Queued: the next nudge neither runs agy nor claims anything.
	if r := h.run(); r.err != nil || len(h.runs()) != 1 || h.noticeCount() != 1 {
		t.Fatalf("nudge during backoff: %v, runs %d, notices %d\nstderr:\n%s", r.err, len(h.runs()), h.noticeCount(), r.stderr)
	}
	// A plain failure is only counted, not an immediate backoff.
	h2 := newGeminiHarness(t)
	if r := h2.run("TINCAN_GEMINI_ALLOW_UNCONFINED=1", "FAKE_EXIT=1", "FAKE_STDERR=something else broke"); r.err == nil || h2.backoff() || h2.noticeCount() != 0 {
		t.Fatalf("plain failure: %v, backoff %v, notices %d", r.err, h2.backoff(), h2.noticeCount())
	}
	// Gemini CLI exits 41 when it cannot authenticate (a revoked key).
	h3 := newGeminiHarness(t)
	if r := h3.run("TINCAN_GEMINI_ENGINE=gemini", "GEMINI_API_KEY=dummy", "FAKE_EXIT=41"); r.err == nil || !h3.backoff() || h3.noticeCount() != 1 || !strings.Contains(r.stderr, "GEMINI_API_KEY") {
		t.Fatalf("gemini auth failure: %v, backoff %v, notices %d\nstderr:\n%s", r.err, h3.backoff(), h3.noticeCount(), r.stderr)
	}
}

// The identity check reads the chosen engine's own MCP config: a second
// tincan server, or a tincan server under another teammate's config, stops
// the wake before the engine runs.
func TestGeminiWakeIdentityRefusal(t *testing.T) {
	type tc struct {
		name, engine string
		file         func(h *geminiHarness) string
		servers      func(h *geminiHarness) map[string]any
		want         string
	}
	second := func(h *geminiHarness) map[string]any {
		return map[string]any{"agent-tincan": h.tincanEntry(h.config), "tincan-2": h.tincanEntry(h.config)}
	}
	other := func(h *geminiHarness) map[string]any {
		return map[string]any{"agent-tincan": h.tincanEntry(filepath.Join(h.home, ".config", "tincan", "codex.json"))}
	}
	agyFile := func(h *geminiHarness) string { return h.agyMCP }
	geminiFile := func(h *geminiHarness) string { return h.geminiSettings }
	workspace := func(h *geminiHarness) string { return filepath.Join(h.wd, ".gemini", "settings.json") }
	cases := []tc{
		{"agy second server", "agy", agyFile, second, "2 agent-tincan servers"},
		{"agy other config", "agy", agyFile, other, "codex.json"},
		{"gemini second server", "gemini", geminiFile, second, "2 agent-tincan servers"},
		{"gemini other config", "gemini", geminiFile, other, "codex.json"},
		{"gemini workspace settings add a server", "gemini", workspace, func(h *geminiHarness) map[string]any {
			return map[string]any{"github": map[string]any{"command": "gh", "args": []string{"mcp"}}}
		}, "github"},
	}
	for _, tool := range []string{"jq", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Logf("%s not on PATH; skipping its listing", tool)
			continue
		}
		for _, c := range cases {
			t.Run(tool+"/"+c.name, func(t *testing.T) {
				h := newGeminiHarness(t)
				h.servers(c.file(h), c.servers(h))
				r := h.run("TINCAN_GEMINI_ENGINE="+c.engine, "GEMINI_API_KEY=dummy", "TINCAN_GEMINI_ALLOW_UNCONFINED=1", "TINCAN_GEMINI_JSON_TOOL="+tool)
				if r.err == nil || len(h.runs()) != 0 {
					t.Fatalf("engine ran with a bad MCP config: %v\nstderr:\n%s", r.err, r.stderr)
				}
				if !strings.Contains(r.stderr, c.want) || !h.backoff() || h.noticeCount() != 1 {
					t.Fatalf("refusal lacks %q, backoff %v, notices %d\nstderr:\n%s", c.want, h.backoff(), h.noticeCount(), r.stderr)
				}
				// The same config with a correct entry runs.
				ok := newGeminiHarness(t)
				if r := ok.run("TINCAN_GEMINI_ENGINE="+c.engine, "GEMINI_API_KEY=dummy", "TINCAN_GEMINI_ALLOW_UNCONFINED=1", "TINCAN_GEMINI_JSON_TOOL="+tool); r.err != nil || len(ok.runs()) != 1 {
					t.Fatalf("correct config with %s: %v\nstderr:\n%s", tool, r.err, r.stderr)
				}
			})
		}
	}
}

// Each engine reads only its own config: a broken agy config does not stop
// the gemini engine, and the MCP config location can be pointed elsewhere.
func TestGeminiWakeReadsOnlyTheChosenEngineConfig(t *testing.T) {
	h := newGeminiHarness(t)
	h.servers(h.agyMCP, map[string]any{"a-tincan": h.tincanEntry(h.config), "b-tincan": h.tincanEntry(h.config)})
	if r := h.run("TINCAN_GEMINI_ENGINE=gemini", "GEMINI_API_KEY=dummy"); r.err != nil || len(h.runs()) != 1 {
		t.Fatalf("gemini engine judged by agy's config: %v\nstderr:\n%s", r.err, r.stderr)
	}
	moved := filepath.Join(h.dir, "agy-mcp.json")
	h.servers(moved, map[string]any{"agent-tincan": h.tincanEntry(h.config)})
	if r := h.run("TINCAN_GEMINI_ALLOW_UNCONFINED=1", "TINCAN_AGY_MCP_CONFIG="+moved); r.err != nil || len(h.runs()) != 2 {
		t.Fatalf("agy with TINCAN_AGY_MCP_CONFIG: %v\nstderr:\n%s", r.err, r.stderr)
	}
}

func TestGeminiWakeUnknownEngine(t *testing.T) {
	h := newGeminiHarness(t)
	r := h.run("TINCAN_GEMINI_ENGINE=bard")
	if r.err == nil || len(h.runs()) != 0 || !strings.Contains(r.stderr, "TINCAN_GEMINI_ENGINE") {
		t.Fatalf("unknown engine: %v, runs %d\nstderr:\n%s", r.err, len(h.runs()), r.stderr)
	}
}

// Each JSON reader finds the attachments directory the same way tincan does:
// the config's agent name, or "default" when the config has none or names
// something that is not a plain name.
func TestGeminiWakeAttachmentDirAgentName(t *testing.T) {
	for _, tool := range []string{"jq", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Logf("%s not on PATH; skipping", tool)
			continue
		}
		for _, c := range []struct{ config, want string }{
			{`{"agent":"gem-2"}`, "gem-2"},
			{`{"relay":"http://relay"}`, "default"},
			{`{"agent":"../escape"}`, "default"},
			{"", "default"}, // no config file yet
		} {
			t.Run(tool+"/"+c.want, func(t *testing.T) {
				h := newGeminiHarness(t)
				if c.config != "" {
					if err := os.WriteFile(h.config, []byte(c.config), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				r := h.run("TINCAN_GEMINI_ENGINE=gemini", "GEMINI_API_KEY=dummy", "TINCAN_GEMINI_JSON_TOOL="+tool)
				if r.err != nil || len(h.runs()) != 1 {
					t.Fatalf("wake: %v\nstderr:\n%s", r.err, r.stderr)
				}
				att := canonical(t, filepath.Join(filepath.Dir(h.config), "attachments", c.want))
				if got := h.runs()[0].flagValues("--include-directories"); !slices.Contains(got, att) {
					t.Fatalf("--include-directories = %q, want %s", got, att)
				}
			})
		}
	}
}

// The model's words go to stdout, so a login phrase there is not an auth
// failure: a plain non-zero exit whose stdout says "not logged in" is only
// counted, not an immediate backoff.
func TestGeminiWakeAuthPhraseOnStdoutIsNotAuthFailure(t *testing.T) {
	for _, engine := range []string{"agy", "gemini"} {
		t.Run(engine, func(t *testing.T) {
			h := newGeminiHarness(t)
			r := h.run("TINCAN_GEMINI_ENGINE="+engine, "GEMINI_API_KEY=dummy", "TINCAN_GEMINI_ALLOW_UNCONFINED=1",
				"FAKE_EXIT=1", `FAKE_STDOUT={"response":"The user is not logged in to the dashboard."}`)
			if r.err == nil || len(h.runs()) != 1 {
				t.Fatalf("wake: %v, runs %d\nstderr:\n%s", r.err, len(h.runs()), r.stderr)
			}
			if h.backoff() || h.noticeCount() != 0 {
				t.Fatalf("stdout auth phrase backed off: backoff %v, notices %d\nstderr:\n%s", h.backoff(), h.noticeCount(), r.stderr)
			}
			if !strings.Contains(r.stdout, "not logged in") {
				t.Fatalf("engine stdout not passed through:\n%s", r.stdout)
			}
		})
	}
}

// With neither jq nor python3 on PATH the wake cannot read the MCP config,
// so it refuses cleanly, naming both, before the engine runs.
func TestGeminiWakeNoJSONToolRefuses(t *testing.T) {
	h := newGeminiHarness(t)
	bin := filepath.Join(h.dir, "bin")
	mkdirs(t, bin)
	for _, tool := range []string{"sh", "dirname", "basename", "mkdir", "chmod", "cat", "awk", "tr", "grep",
		"date", "find", "rm", "mv", "head", "sleep", "ls", "perl", "pgrep", "setsid"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			continue
		}
		if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	r := h.run("PATH="+bin, "TINCAN_GEMINI_ENGINE=gemini", "GEMINI_API_KEY=dummy")
	if r.err == nil || len(h.runs()) != 0 {
		t.Fatalf("engine ran with no JSON reader: %v\nstderr:\n%s", r.err, r.stderr)
	}
	if !strings.Contains(r.stderr, "jq or python3") || !h.backoff() || h.noticeCount() != 1 {
		t.Fatalf("refusal: backoff %v, notices %d\nstderr:\n%s", h.backoff(), h.noticeCount(), r.stderr)
	}
}

// --allowed-mcp-server-names names the tincan server the identity check
// found, whatever it is called, and not an allowed server that only
// mentions tincan in its arguments.
func TestGeminiWakeAllowedServerNamesFollowTheTincanServer(t *testing.T) {
	for _, tool := range []string{"jq", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Logf("%s not on PATH; skipping", tool)
			continue
		}
		t.Run(tool, func(t *testing.T) {
			h := newGeminiHarness(t)
			bus := h.tincanEntry(h.config)
			bus["command"] = "/usr/local/bin/tincan"
			h.servers(h.geminiSettings, map[string]any{
				"files":    map[string]any{"command": "npx", "args": []string{"server-filesystem", "/Users/x/code/agent-tincan"}},
				"team-bus": bus,
			})
			r := h.run("TINCAN_GEMINI_ENGINE=gemini", "GEMINI_API_KEY=dummy", "TINCAN_WAKE_ALLOWED_SERVERS=files", "TINCAN_GEMINI_JSON_TOOL="+tool)
			if r.err != nil || len(h.runs()) != 1 {
				t.Fatalf("wake: %v\nstderr:\n%s", r.err, r.stderr)
			}
			if got := h.runs()[0].flagValues("--allowed-mcp-server-names"); strings.Join(got, ",") != "team-bus,files" {
				t.Fatalf("--allowed-mcp-server-names = %q, want team-bus then files", got)
			}
		})
	}
}

// A wake stopped mid-run (the listener stopping) removes the engine's
// captured output from its state directory along with the lock: the
// response and diagnostics are not left behind.
func TestGeminiWakeSignalRemovesCapturedOutput(t *testing.T) {
	h := newGeminiHarness(t)
	script, err := filepath.Abs(filepath.Join("..", "..", "examples", "gemini-cli", "gemini-wake.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", script)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + h.home,
		"AGY_BIN=" + filepath.Join(h.dir, "agy"),
		"TINCAN_BIN=" + filepath.Join(h.dir, "tincan"),
		"TINCAN_WAKE_STATE_DIR=" + h.state,
		"TINCAN_WAKE_OPERATOR=ops",
		"TINCAN_CONFIG=" + h.config,
		"ARGV_DIR=" + h.argvDir,
		"NOTICES=" + h.notices,
		"TINCAN_GEMINI_ALLOW_UNCONFINED=1",
		"FAKE_STDERR=private diagnostics",
		"FAKE_SLEEP=30",
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	sleeping := filepath.Join(h.argvDir, "sleeping")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sleeping); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	captured, _ := filepath.Glob(filepath.Join(h.state, "err.*"))
	if len(captured) == 0 {
		t.Fatalf("engine never started, or its output is not captured in the state directory\nstderr:\n%s", stderr.String())
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	for _, pat := range []string{"out.*", "err.*"} {
		if left, _ := filepath.Glob(filepath.Join(h.state, pat)); len(left) != 0 {
			t.Fatalf("captured output left behind after a SIGTERM: %v\nstderr:\n%s", left, stderr.String())
		}
	}
	if _, err := os.Stat(filepath.Join(h.state, "lock")); err == nil {
		t.Fatalf("lock left behind after a SIGTERM\nstderr:\n%s", stderr.String())
	}
}

// A listen nudge for a relay upgrade alone (TINCAN_WAITING=0) goes to the
// listener's log and starts no engine run; with items waiting, the run
// still happens and the notice is logged too.
func TestGeminiWakeUpgradeNoticeAlone(t *testing.T) {
	h := newGeminiHarness(t)
	r := h.run("TINCAN_GEMINI_ALLOW_UNCONFINED=1", "TINCAN_WAITING=0", "TINCAN_UPGRADE_AVAILABLE=9.9.9")
	if r.err != nil {
		t.Fatalf("wake: %v\nstderr:\n%s", r.err, r.stderr)
	}
	if n := len(h.runs()); n != 0 {
		t.Fatalf("engine ran %d times for an upgrade notice alone\nstderr:\n%s", n, r.stderr)
	}
	if !strings.Contains(r.stderr, "tincan 9.9.9 is available from the relay") || !strings.Contains(r.stderr, "tincan upgrade") {
		t.Fatalf("upgrade notice not logged:\n%s", r.stderr)
	}
	if h.backoff() || h.noticeCount() != 0 {
		t.Fatalf("backoff %v, notices %d; want neither", h.backoff(), h.noticeCount())
	}
	r = h.run("TINCAN_GEMINI_ALLOW_UNCONFINED=1", "TINCAN_UPGRADE_AVAILABLE=9.9.9")
	if r.err != nil || len(h.runs()) != 1 || !strings.Contains(r.stderr, "tincan 9.9.9 is available from the relay") {
		t.Fatalf("with work waiting: %v, %d runs\nstderr:\n%s", r.err, len(h.runs()), r.stderr)
	}
}

// Each run's session id, from the engine's JSON output, is recorded with
// the workdir in a private list beside the teammate's config
// (<name>.wake-sessions), so the history agent can leave wake runs out.
func TestGeminiWakeRecordsSessions(t *testing.T) {
	for _, engine := range [][]string{
		{"TINCAN_GEMINI_ALLOW_UNCONFINED=1"},
		{"TINCAN_GEMINI_ENGINE=gemini", "GEMINI_API_KEY=dummy-key"},
	} {
		t.Run(engine[0], func(t *testing.T) {
			h := newGeminiHarness(t)
			sid := "3f2b8c1d-6a4e-4f7b-9c0d-2e1f3a4b5c6d"
			r := h.run(append(engine, `FAKE_STDOUT={"session_id": "`+sid+`", "response":"done"}`)...)
			if r.err != nil {
				t.Fatalf("wake: %v\nstderr:\n%s", r.err, r.stderr)
			}
			sessions := filepath.Join(filepath.Dir(h.config), "gemini-cli.wake-sessions")
			st, err := os.Stat(sessions)
			if err != nil {
				t.Fatal(err)
			}
			if st.Mode().Perm() != 0o600 {
				t.Errorf("session list mode %v, want 0600", st.Mode().Perm())
			}
			list, _ := os.ReadFile(sessions)
			if string(list) != "workdir "+canonical(t, h.wd)+"\n"+sid+"\n" {
				t.Fatalf("session list:\n%s\nwant the workdir and %s", list, sid)
			}
		})
	}
}
