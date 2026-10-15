package wake

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// These tests run examples/cursor-cli/cursor-wake.sh with a fake
// cursor-agent, on the harness in copilotwake_script_test.go.

// fakeCursorAgent answers --version the way cursor-agent does. A run
// records its argv (NUL separated), its CURSOR_CONFIG_DIR, its working
// directory and the workspace's .cursor/mcp.json and .cursor/cli.json, then
// prints FAKE_STDOUT or Cursor's JSON result with FAKE_SID as its session
// id.
const fakeCursorAgent = `#!/bin/sh
if [ "${1:-}" = --version ]; then
  echo "${FAKE_VERSION:-2026.10.14-0f9edde}"
  exit 0
fi
n=$(ls "$ARGV_DIR" 2>/dev/null | grep -c "^run\\." || true)
for a in "$@"; do printf '%s\0' "$a"; done > "$ARGV_DIR/run.$n"
printf '%s\n' "${CURSOR_CONFIG_DIR:-}" > "$ARGV_DIR/env.$n"
pwd -P > "$ARGV_DIR/cwd.$n"
ws=.
prev=
for a in "$@"; do
  [ "$prev" = --workspace ] && ws=$a
  prev=$a
done
cp "$ws/.cursor/mcp.json" "$ARGV_DIR/mcp.$n" 2>/dev/null || true
cp "$ws/.cursor/cli.json" "$ARGV_DIR/cli.$n" 2>/dev/null || true
[ -z "${FAKE_STDERR:-}" ] || echo "$FAKE_STDERR" >&2
if [ -n "${FAKE_STDOUT:-}" ]; then
  echo "$FAKE_STDOUT"
else
  printf '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"%s"}\n' "${FAKE_SID:-0f6c2a1e-9b7d-4c3a-8e5f-1a2b3c4d5e6f}"
fi
exit "${FAKE_EXIT:-0}"
`

func newCursorWake(t *testing.T) *cliWake {
	t.Helper()
	return newCLIWake(t, "cursor-cli/cursor-wake.sh", "CURSOR_AGENT_BIN", "cursor-agent", fakeCursorAgent, "cursor-cli")
}

// The wake runs cursor-agent headless with JSON output in its workspace,
// approving the workspace's MCP servers (which the wake wrote: only
// agent-tincan) and permitting exactly the agent-tincan tools in the
// workspace's cli.json: never --force or --yolo. CURSOR_CONFIG_DIR is a
// private directory in the wake home, so the owner's ~/.cursor allow
// entries never apply. The session id Cursor reports is recorded.
func TestCursorWakeRunsHeadlessWithTincanToolsOnly(t *testing.T) {
	h := newCursorWake(t)
	r := h.run()
	if r.err != nil {
		t.Fatalf("wake: %v\nstderr:\n%s", r.err, r.stderr)
	}
	runs := h.runs()
	if len(runs) != 1 {
		t.Fatalf("cursor-agent ran %d times, want 1\nstderr:\n%s", len(runs), r.stderr)
	}
	a := runs[0]
	for _, f := range []string{"-p", "--approve-mcps", "--trust"} {
		if !slices.Contains(a.argv, f) {
			t.Errorf("argv lacks %s: %q", f, a.argv)
		}
	}
	for _, f := range []string{"--force", "-f", "--yolo"} {
		if slices.Contains(a.argv, f) {
			t.Errorf("argv has %s: %q", f, a.argv)
		}
	}
	if got := a.flagValues("--output-format"); len(got) != 1 || got[0] != "json" {
		t.Errorf("--output-format = %q, want json", got)
	}
	if got := a.flagValues("--workspace"); len(got) != 1 || got[0] != canonical(t, h.workdir) {
		t.Errorf("--workspace = %q, want the workdir", got)
	}
	checkDrainPrompt(t, a.prompt())
	if got := strings.TrimSpace(h.file("cwd.0")); got != canonical(t, h.workdir) {
		t.Fatalf("cursor-agent ran in %q, want the workdir", got)
	}
	h.tincanServer(h.file("mcp.0"))
	var cli struct {
		Permissions struct{ Allow, Deny []string } `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(h.file("cli.0")), &cli); err != nil {
		t.Fatalf("cli.json: %v\n%s", err, h.file("cli.0"))
	}
	want := tincanAllowed(func(tool string) string { return "Mcp(agent-tincan:" + tool + ")" })
	if !slices.Equal(slices.Sorted(slices.Values(cli.Permissions.Allow)), want) || len(cli.Permissions.Deny) != 0 {
		t.Errorf("cli.json permissions = %+v, want allow exactly %q", cli.Permissions, want)
	}
	cfgDir := filepath.Join(h.wakeHome, ".cursor")
	if got := strings.TrimSpace(h.file("env.0")); got != canonical(t, cfgDir) {
		t.Errorf("CURSOR_CONFIG_DIR = %q, want %s in the wake home", got, canonical(t, cfgDir))
	}
	for _, d := range []string{h.wakeHome, cfgDir} {
		if st, err := os.Stat(d); err != nil || st.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v, want mode 0700", d, err)
		}
	}
	h.sessionList("0f6c2a1e-9b7d-4c3a-8e5f-1a2b3c4d5e6f")
	if !strings.Contains(r.stdout, `"result":"done"`) {
		t.Fatalf("cursor-agent's output not passed to the listener log:\n%s", r.stdout)
	}
}

// A workdir set outside the wake home, before either exists, still gets a
// run: the wake creates its wake home too.
func TestCursorWakeWorkdirOutsideTheWakeHome(t *testing.T) {
	h := newCursorWake(t)
	h.workdir = filepath.Join(h.dir, "elsewhere", "work")
	r := h.run("TINCAN_CURSOR_WORKDIR=" + h.workdir)
	if r.err != nil || len(h.runs()) != 1 {
		t.Fatalf("wake: err %v, runs %d\nstderr:\n%s", r.err, len(h.runs()), r.stderr)
	}
	if got := strings.TrimSpace(h.file("cwd.0")); got != canonical(t, h.workdir) {
		t.Fatalf("cursor-agent ran in %q, want the workdir", got)
	}
	if got := strings.TrimSpace(h.file("env.0")); got != canonical(t, filepath.Join(h.wakeHome, ".cursor")) {
		t.Errorf("CURSOR_CONFIG_DIR = %q, want the wake home's", got)
	}
	h.sessionList()
}

// The owner's ~/.cursor does not load in a wake run (CURSOR_CONFIG_DIR is
// the wake's own), so a server listed there does not stop the wake.
func TestCursorWakeIgnoresTheOwnersCursorConfig(t *testing.T) {
	h := newCursorWake(t)
	h.write(filepath.Join(h.home, ".cursor", "mcp.json"), `{"mcpServers":{"files":{"command":"npx","args":["fs"]}}}`)
	h.write(filepath.Join(h.home, ".cursor", "hooks.json"), `{"version":1,"hooks":{}}`)
	if r := h.run(); r.err != nil || len(h.runs()) != 1 {
		t.Fatalf("wake: err %v, runs %d\nstderr:\n%s", r.err, len(h.runs()), r.stderr)
	}
}

// Cursor loads the private config directory's mcp.json too, and
// --approve-mcps approves every server in it, so any server there other
// than the workspace's agent-tincan stops the wake. So does a hooks.json
// in the workspace or the config directory, whose commands would run
// unattended.
func TestCursorWakeIdentityCheck(t *testing.T) {
	for _, tc := range []struct{ name, file, body, want string }{
		{"an unvetted server", ".cursor/mcp.json", `{"mcpServers":{"files":{"command":"npx","args":["fs"]}}}`, "did not allow: files"},
		{"the owner's agent-tincan", ".cursor/mcp.json", `{"mcpServers":{"agent-tincan":{"command":"tincan","args":["mcp"],"env":{"TINCAN_CONFIG":"/Users/owner/.config/tincan/config.json"}}}}`, "2 agent-tincan servers"},
		{"a workspace hook", "work-hooks", `{"version":1,"hooks":{}}`, "hooks.json"},
		{"a config directory hook", ".cursor/hooks.json", `{"version":1,"hooks":{}}`, "hooks.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCursorWake(t)
			if tc.file == "work-hooks" {
				h.write(filepath.Join(h.workdir, ".cursor", "hooks.json"), tc.body)
			} else {
				h.write(filepath.Join(h.wakeHome, tc.file), tc.body)
			}
			h.refused(h.run(), tc.want)
		})
	}
}

func TestCursorWakeAuthFailureBacksOff(t *testing.T) {
	checkAuthBackoff(t, newCursorWake, "Error: not logged in. Run cursor-agent login.")
}

// A wake home that is the owner's home (whose ~/.cursor would then be the
// config directory), or a cursor-agent whose --version is not a Cursor
// build, is refused once, with a backoff.
func TestCursorWakeRefusals(t *testing.T) {
	t.Run("wake home is the owner's home", func(t *testing.T) {
		h := newCursorWake(t)
		h.refused(h.run("TINCAN_CURSOR_WAKE_HOME="+h.home, "TINCAN_CURSOR_WORKDIR="+filepath.Join(h.dir, "work")), "is the owner's home")
	})
	t.Run("garbage version", func(t *testing.T) {
		h := newCursorWake(t)
		h.refused(h.run("FAKE_VERSION=cursor 1.0"), "not the expected tool")
	})
}
