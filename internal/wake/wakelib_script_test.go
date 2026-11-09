package wake

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests source examples/lib/tincan-wake-lib.sh from a small driver
// script, the way a per-CLI wake script does, and run it against a fake CLI
// and a fake tincan that only record what they were asked to do.

// fakeCLI answers --version (after FAKE_VERSION_SLEEP seconds, if set),
// records each real run, and can sleep (with a child of its own, like a CLI
// running an MCP server), start a child that ignores TERM (FAKE_STUBBORN),
// leave a TERM-ignoring child running after it exits (FAKE_LEAVE=1, or
// FAKE_LEAVE=pgrp to put that child in a process group of its own, like a
// daemonized MCP server), or fail.
const fakeCLI = `#!/bin/sh
if [ "${1:-}" = --version ]; then
  [ -z "${FAKE_VERSION_SLEEP:-}" ] || sleep "$FAKE_VERSION_SLEEP"
  echo "${FAKE_VERSION:-Fake CLI 1.2.3}"
  exit 0
fi
echo run >> "$RUNS"
echo $$ > "$STATE_OUT/cli.pid"
if [ -n "${FAKE_STUBBORN:-}" ]; then
  sh -c 'trap "" TERM; while :; do sleep 1; done' </dev/null >/dev/null 2>&1 &
  echo $! > "$STATE_OUT/child.pid"
  sleep 60 &
  wait
fi
if [ -n "${FAKE_LEAVE:-}" ]; then
  if [ "$FAKE_LEAVE" = pgrp ]; then
    perl -e 'setpgrp(0, 0); $SIG{TERM} = "IGNORE"; sleep 60' </dev/null >/dev/null 2>&1 &
  else
    sh -c 'trap "" TERM; while :; do sleep 1; done' </dev/null >/dev/null 2>&1 &
  fi
  echo $! > "$STATE_OUT/child.pid"
  sleep 2
fi
if [ -n "${FAKE_SLEEP:-}" ]; then
  sleep "$FAKE_SLEEP" &
  echo $! > "$STATE_OUT/child.pid"
  wait
fi
exit "${FAKE_EXIT:-0}"
`

// fakeTincan records each call's argv on one line, and can hang
// (FAKE_NOTIFY_SLEEP seconds) first. It lifts any file-size limit the
// driver set, so a test that fills the disk still sees the notice.
const fakeTincan = `#!/bin/sh
ulimit -S -f unlimited 2>/dev/null || true
[ -z "${FAKE_NOTIFY_SLEEP:-}" ] || sleep "$FAKE_NOTIFY_SLEEP"
printf '%s\n' "$*" >> "$NOTICES"
`

// driver is a minimal per-CLI wake script on the library.
const driver = `set -eu
. "$TINCAN_WAKE_LIB"
tincan_wake_init fake
tincan_wake_begin
tincan_wake_check_binary "$FAKE_BIN" '^Fake CLI '
fake_mcp_list() {
  [ -z "${FAKE_LIST_FAIL:-}" ] || return 1
  cat "$MCP_LISTING"
}
tincan_wake_check_identity fake_mcp_list
tincan_wake_run "$FAKE_BIN" run "drain the inbox"
`

type libHarness struct {
	t                       *testing.T
	dir, state, config, bin string
	listing, runs, notices  string
	lib                     string
	env                     []string
}

// signalPID sends sig to pid. A signal of 0 only checks that pid is alive.
func signalPID(pid int, sig os.Signal) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(sig)
}

func newLibHarness(t *testing.T) *libHarness {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("tincan-wake-lib.sh is a POSIX sh library")
	}
	lib, err := filepath.Abs(filepath.Join("..", "..", "examples", "lib", "tincan-wake-lib.sh"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	h := &libHarness{t: t, dir: dir, lib: lib,
		state:   filepath.Join(dir, "state"),
		config:  filepath.Join(dir, "config", "fake.json"),
		bin:     filepath.Join(dir, "fake-cli"),
		listing: filepath.Join(dir, "listing"),
		runs:    filepath.Join(dir, "runs"),
		notices: filepath.Join(dir, "notices"),
	}
	mkdirs(t, filepath.Dir(h.config))
	if err := os.WriteFile(h.bin, []byte(fakeCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tincan"), []byte(fakeTincan), 0o755); err != nil {
		t.Fatal(err)
	}
	h.setListing("agent-tincan\t" + h.config + "\ttincan mcp")
	return h
}

func (h *libHarness) setListing(lines ...string) {
	h.t.Helper()
	if err := os.WriteFile(h.listing, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

type libRun struct {
	err    error
	stderr string
	took   time.Duration
}

// run runs body (the driver by default) with the library sourced from
// TINCAN_WAKE_LIB, the harness environment, and env on top.
func (h *libHarness) run(body string, env ...string) libRun {
	h.t.Helper()
	script := filepath.Join(h.dir, "driver.sh")
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		h.t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", script)
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + h.dir,
		"TINCAN_WAKE_LIB=" + h.lib,
		"TINCAN_WAKE_STATE_DIR=" + h.state,
		"TINCAN_BIN=" + filepath.Join(h.dir, "tincan"),
		"TINCAN_CONFIG=" + h.config,
		"FAKE_BIN=" + h.bin,
		"MCP_LISTING=" + h.listing,
		"RUNS=" + h.runs,
		"NOTICES=" + h.notices,
		"STATE_OUT=" + h.dir,
	}, h.env...)
	cmd.Env = append(cmd.Env, env...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	start := time.Now()
	err := cmd.Run()
	return libRun{err: err, stderr: stderr.String(), took: time.Since(start)}
}

// count returns the number of lines in a recording file (0 if missing).
func (h *libHarness) count(file string) int {
	raw, err := os.ReadFile(file)
	if err != nil {
		return 0
	}
	return strings.Count(string(raw), "\n")
}

func (h *libHarness) exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (h *libHarness) lockDir() string { return filepath.Join(h.state, "lock") }

func exitCode(err error) int {
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

func TestWakeLibRunsCLIAndReleasesLock(t *testing.T) {
	h := newLibHarness(t)
	r := h.run(driver)
	if r.err != nil {
		t.Fatalf("wake: %v\nstderr:\n%s", r.err, r.stderr)
	}
	if n := h.count(h.runs); n != 1 {
		t.Fatalf("CLI ran %d times, want 1\nstderr:\n%s", n, r.stderr)
	}
	if h.exists(h.lockDir()) {
		t.Fatal("lock left behind after a clean run")
	}
	if h.count(h.notices) != 0 {
		t.Fatal("operator notice sent although no operator is configured")
	}
}

// deadPid returns the pid of a process that has exited and been reaped.
func deadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func writeLock(t *testing.T, dir string, pid int) {
	t.Helper()
	mkdirs(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWakeLibBreaksStaleLock(t *testing.T) {
	h := newLibHarness(t)
	writeLock(t, h.lockDir(), deadPid(t))
	r := h.run(driver)
	if r.err != nil {
		t.Fatalf("wake: %v\nstderr:\n%s", r.err, r.stderr)
	}
	if n := h.count(h.runs); n != 1 {
		t.Fatalf("CLI ran %d times after a stale lock, want 1\nstderr:\n%s", n, r.stderr)
	}
	if !strings.Contains(r.stderr, "stale lock") {
		t.Fatalf("breaking the stale lock was not reported:\n%s", r.stderr)
	}
	if h.exists(h.lockDir()) {
		t.Fatal("lock left behind")
	}
}

func TestWakeLibLiveLockExitsWithoutRunning(t *testing.T) {
	h := newLibHarness(t)
	writeLock(t, h.lockDir(), os.Getpid()) // this test process is alive
	r := h.run(driver)
	if r.err != nil {
		t.Fatalf("a busy wake should exit 0: %v\nstderr:\n%s", r.err, r.stderr)
	}
	if n := h.count(h.runs); n != 0 {
		t.Fatalf("CLI ran %d times while another run held the lock", n)
	}
	raw, err := os.ReadFile(filepath.Join(h.lockDir(), "pid"))
	if err != nil || strings.TrimSpace(string(raw)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("the live run's lock was touched: %q, %v", raw, err)
	}
}

func alive(pid int) bool {
	return signalPID(pid, syscall.Signal(0)) == nil
}

func readPid(t *testing.T, p string) int {
	t.Helper()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func TestWakeLibTimeoutKillsCLIAndReleasesLock(t *testing.T) {
	h := newLibHarness(t)
	r := h.run(driver, "TINCAN_WAKE_TIMEOUT=1", "FAKE_SLEEP=60")
	if code := exitCode(r.err); code != 124 {
		t.Fatalf("exit = %d (%v), want 124 for a timeout\nstderr:\n%s", code, r.err, r.stderr)
	}
	if r.took > 20*time.Second {
		t.Fatalf("timed-out wake took %s", r.took)
	}
	if !strings.Contains(r.stderr, "timed out") {
		t.Fatalf("timeout not reported:\n%s", r.stderr)
	}
	if h.exists(h.lockDir()) {
		t.Fatal("lock left behind after a timeout")
	}
	cli, child := readPid(t, filepath.Join(h.dir, "cli.pid")), readPid(t, filepath.Join(h.dir, "child.pid"))
	deadline := time.Now().Add(5 * time.Second)
	for (alive(cli) || alive(child)) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if alive(cli) || alive(child) {
		t.Fatalf("CLI (%d alive=%v) or its child (%d alive=%v) survived the timeout", cli, alive(cli), child, alive(child))
	}
}

func TestWakeLibBacksOffAfterThreeFailures(t *testing.T) {
	h := newLibHarness(t)
	h.env = []string{"FAKE_EXIT=3", "TINCAN_WAKE_OPERATOR=ops"}
	for i := 1; i <= 3; i++ {
		r := h.run(driver)
		if code := exitCode(r.err); code != 3 {
			t.Fatalf("run %d: exit = %d, want the CLI's 3\nstderr:\n%s", i, code, r.stderr)
		}
		if i < 3 && h.count(h.notices) != 0 {
			t.Fatalf("operator notified after %d failures", i)
		}
	}
	if !h.exists(filepath.Join(h.state, "backoff")) {
		t.Fatal("no backoff marker after three failures")
	}
	for i := range 2 {
		r := h.run(driver)
		if r.err != nil {
			t.Fatalf("nudge %d during backoff: %v\nstderr:\n%s", i, r.err, r.stderr)
		}
		if !strings.Contains(r.stderr, "backing off") {
			t.Fatalf("nudge %d during backoff did not say so:\n%s", i, r.stderr)
		}
	}
	if n := h.count(h.runs); n != 3 {
		t.Fatalf("CLI ran %d times, want 3 (none during backoff)", n)
	}
	raw, _ := os.ReadFile(h.notices)
	if n := h.count(h.notices); n != 1 {
		t.Fatalf("operator notices = %d, want 1:\n%s", n, raw)
	}
	if !strings.HasPrefix(string(raw), "ask --notify ops ") {
		t.Fatalf("notice was not a tincan ask --notify to the operator: %q", raw)
	}

	// Once the backoff has run out, a failure backs off again without a
	// second notice, and a success clears it all.
	if err := os.WriteFile(filepath.Join(h.state, "backoff"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := h.run(driver); exitCode(r.err) != 3 || !h.exists(filepath.Join(h.state, "backoff")) {
		t.Fatalf("failure after the backoff ran out: %v, backoff set %v\nstderr:\n%s", r.err, h.exists(filepath.Join(h.state, "backoff")), r.stderr)
	}
	if n := h.count(h.notices); n != 1 {
		t.Fatalf("operator notices = %d after the second backoff, want still 1", n)
	}
	if err := os.WriteFile(filepath.Join(h.state, "backoff"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := h.run(driver, "FAKE_EXIT=0"); r.err != nil {
		t.Fatalf("success after backoff: %v\nstderr:\n%s", r.err, r.stderr)
	}
	for _, f := range []string{"backoff", "failures", "notified"} {
		if h.exists(filepath.Join(h.state, f)) {
			t.Errorf("%s not cleared by a successful run", f)
		}
	}
}

func TestWakeLibIdentityCheckStopsTheWake(t *testing.T) {
	cases := []struct {
		name    string
		listing func(h *libHarness) []string
		env     []string
		want    string
	}{
		{"second tincan server", func(h *libHarness) []string {
			return []string{"agent-tincan\t" + h.config + "\ttincan mcp", "tincan-old\t" + h.config + "\t/usr/local/bin/tincan mcp"}
		}, nil, "2 agent-tincan servers"},
		{"tincan by command only", func(h *libHarness) []string {
			return []string{"agent-tincan\t" + h.config + "\ttincan mcp", "helper\t\t/opt/bin/tincan mcp --channel"}
		}, nil, "2 agent-tincan servers"},
		{"another agent's config", func(h *libHarness) []string {
			return []string{"agent-tincan\t" + filepath.Join(h.dir, "config", "claude-code.json") + "\ttincan mcp"}
		}, nil, "claude-code.json"},
		{"no TINCAN_CONFIG on the entry", func(h *libHarness) []string {
			return []string{"agent-tincan\t\ttincan mcp"}
		}, nil, "TINCAN_CONFIG"},
		{"no tincan server", func(h *libHarness) []string {
			return []string{"github\t\tgh mcp"}
		}, nil, "0 agent-tincan servers"},
		{"extra server not allowed", func(h *libHarness) []string {
			return []string{"agent-tincan\t" + h.config + "\ttincan mcp", "github\t\tgh mcp", "fs\t\tnpx fs"}
		}, []string{"TINCAN_WAKE_ALLOWED_SERVERS=fs"}, "github"},
		{"listing fails", func(h *libHarness) []string { return nil }, []string{"FAKE_LIST_FAIL=1"}, "MCP servers"},
		{"server with no name", func(h *libHarness) []string {
			return []string{"agent-tincan\t" + h.config + "\ttincan mcp", "\t/other/cfg.json\t/usr/local/bin/tincan mcp", "\t\tnpx evil-server"}
		}, nil, "no name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newLibHarness(t)
			if l := c.listing(h); l != nil {
				h.setListing(l...)
			}
			env := append([]string{"TINCAN_WAKE_OPERATOR=ops"}, c.env...)
			r := h.run(driver, env...)
			if r.err == nil {
				t.Fatalf("wake ran with a bad MCP listing\nstderr:\n%s", r.stderr)
			}
			if n := h.count(h.runs); n != 0 {
				t.Fatalf("CLI ran %d times, want 0", n)
			}
			if !strings.Contains(r.stderr, c.want) {
				t.Fatalf("stderr lacks %q:\n%s", c.want, r.stderr)
			}
			if !h.exists(filepath.Join(h.state, "backoff")) {
				t.Fatal("refusal did not set backoff")
			}
			if n := h.count(h.notices); n != 1 {
				t.Fatalf("operator notices = %d, want 1", n)
			}
			if h.exists(h.lockDir()) {
				t.Fatal("lock left behind after a refusal")
			}
			// The next nudge backs off without listing or running anything.
			if r := h.run(driver, env...); r.err != nil || h.count(h.runs) != 0 || h.count(h.notices) != 1 {
				t.Fatalf("nudge after refusal: %v, runs %d, notices %d\nstderr:\n%s", r.err, h.count(h.runs), h.count(h.notices), r.stderr)
			}
		})
	}
}

// Servers the operator allows, and a config path reached through a symlink,
// pass the identity check.
func TestWakeLibIdentityCheckAllows(t *testing.T) {
	h := newLibHarness(t)
	link := filepath.Join(h.dir, "config-link")
	if err := os.Symlink(filepath.Dir(h.config), link); err != nil {
		t.Fatal(err)
	}
	h.setListing("github\t\tgh mcp", "agent-tincan\t"+filepath.Join(link, "fake.json")+"\ttincan mcp", "fs\t\tnpx fs")
	r := h.run(driver, "TINCAN_WAKE_ALLOWED_SERVERS=github, fs")
	if r.err != nil {
		t.Fatalf("wake: %v\nstderr:\n%s", r.err, r.stderr)
	}
	if n := h.count(h.runs); n != 1 {
		t.Fatalf("CLI ran %d times, want 1", n)
	}
}

// A server that only mentions tincan in its arguments (a filesystem server
// on a checkout of this repo) is an ordinary server, governed by the
// allowlist.
func TestWakeLibIdentityTincanInArgsIsOrdinary(t *testing.T) {
	h := newLibHarness(t)
	h.setListing("agent-tincan\t"+h.config+"\ttincan mcp", "fs\t\tnpx @modelcontextprotocol/server-filesystem /Users/me/code/agent-tincan", "", "")
	if r := h.run(driver, "TINCAN_WAKE_ALLOWED_SERVERS=fs"); r.err != nil || h.count(h.runs) != 1 {
		t.Fatalf("allowed fs server: %v, runs %d\nstderr:\n%s", r.err, h.count(h.runs), r.stderr)
	}
	h2 := newLibHarness(t)
	h2.setListing("agent-tincan\t"+h2.config+"\ttincan mcp", "fs\t\tnpx @modelcontextprotocol/server-filesystem /Users/me/code/agent-tincan")
	if r := h2.run(driver); r.err == nil || !strings.Contains(r.stderr, "did not allow: fs") {
		t.Fatalf("unallowed fs server: %v\nstderr:\n%s", r.err, r.stderr)
	}
}

func TestWakeLibBinaryCheck(t *testing.T) {
	h := newLibHarness(t)
	r := h.run(driver, "FAKE_VERSION=grok-cli 0.1 (community)", "TINCAN_WAKE_OPERATOR=ops")
	if r.err == nil || h.count(h.runs) != 0 {
		t.Fatalf("wrong binary ran: %v, runs %d\nstderr:\n%s", r.err, h.count(h.runs), r.stderr)
	}
	if !strings.Contains(r.stderr, "community") || !h.exists(filepath.Join(h.state, "backoff")) || h.count(h.notices) != 1 {
		t.Fatalf("wrong binary: backoff %v, notices %d\nstderr:\n%s", h.exists(filepath.Join(h.state, "backoff")), h.count(h.notices), r.stderr)
	}
}

// The confinement helper is the codex-wake.sh allowed-roots check: only
// canonical paths at or under an allowed root come out, one per line.
func TestWakeLibWriteRoots(t *testing.T) {
	h := newLibHarness(t)
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	proj := filepath.Join(allowed, "proj with space")
	outside := filepath.Join(base, "outside")
	sibling := filepath.Join(base, "allowed-evil")
	mkdirs(t, proj, outside, sibling)
	escape := filepath.Join(allowed, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}
	roots := []string{proj, outside, escape, allowed + "/../outside", sibling, "relative/path", filepath.Join(allowed, "missing"), ""}

	body := `set -eu
. "$TINCAN_WAKE_LIB"
tincan_wake_init fake
roots=$(tincan_wake_write_roots "$ALLOWED" "$WRITE")
set --
while IFS= read -r d; do
  [ -n "$d" ] && set -- "$@" --add-dir "$d"
done <<EOF
$roots
EOF
for a in "$@"; do printf '%s\n' "$a"; done > "$STATE_OUT/argv"
`
	r := h.run(body, "ALLOWED="+allowed, "WRITE="+strings.Join(roots, ":"))
	if r.err != nil {
		t.Fatalf("driver: %v\nstderr:\n%s", r.err, r.stderr)
	}
	raw, err := os.ReadFile(filepath.Join(h.dir, "argv"))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSuffix(string(raw), "\n")
	if want := "--add-dir\n" + canonical(t, proj); got != want {
		t.Fatalf("argv = %q, want %q\nstderr:\n%s", got, want, r.stderr)
	}
	for _, refused := range []string{outside, escape, allowed + "/../outside", sibling, "relative/path"} {
		if !strings.Contains(r.stderr, refused) {
			t.Errorf("refused root %q not reported on stderr:\n%s", refused, r.stderr)
		}
	}
}

// waitDead waits up to five seconds for every pid to be gone.
func waitDead(pids ...int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		live := false
		for _, p := range pids {
			if alive(p) {
				live = true
			}
		}
		if !live {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// A CLI that exits on TERM but leaves a child that ignores TERM: the child
// must not outlive the wake, with a process group (perl or setsid) or with
// the saved-tree fallback.
func TestWakeLibTimeoutKillsTERMIgnoringChild(t *testing.T) {
	for _, mode := range []struct{ name, env string }{
		{"process group", ""},
		{"saved tree", "_TW_NO_PGRP=1"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			h := newLibHarness(t)
			env := []string{"TINCAN_WAKE_TIMEOUT=1", "FAKE_STUBBORN=1"}
			if mode.env != "" {
				env = append(env, mode.env)
			}
			r := h.run(driver, env...)
			child := readPid(t, filepath.Join(h.dir, "child.pid"))
			t.Cleanup(func() { _ = signalPID(child, syscall.SIGKILL) })
			if code := exitCode(r.err); code != 124 {
				t.Fatalf("exit = %d (%v), want 124\nstderr:\n%s", code, r.err, r.stderr)
			}
			if !waitDead(child) {
				t.Fatalf("TERM-ignoring child %d outlived the timed-out wake", child)
			}
			if h.exists(h.lockDir()) {
				t.Fatal("lock left behind after a timeout")
			}
		})
	}
}

// A SIGTERM to the wake itself (the listener stopping) takes the CLI and
// its children with it and removes the lock.
func TestWakeLibSignalCleansUp(t *testing.T) {
	h := newLibHarness(t)
	script := filepath.Join(h.dir, "driver.sh")
	if err := os.WriteFile(script, []byte(driver), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", script)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + h.dir,
		"TINCAN_WAKE_LIB=" + h.lib,
		"TINCAN_WAKE_STATE_DIR=" + h.state,
		"TINCAN_BIN=" + filepath.Join(h.dir, "tincan"),
		"TINCAN_CONFIG=" + h.config,
		"FAKE_BIN=" + h.bin,
		"MCP_LISTING=" + h.listing,
		"RUNS=" + h.runs,
		"NOTICES=" + h.notices,
		"STATE_OUT=" + h.dir,
		"FAKE_STUBBORN=1",
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	childFile := filepath.Join(h.dir, "child.pid")
	deadline := time.Now().Add(10 * time.Second)
	for !h.exists(childFile) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // let the pid files be written in full
	cli, child := readPid(t, filepath.Join(h.dir, "cli.pid")), readPid(t, childFile)
	t.Cleanup(func() { _ = signalPID(child, syscall.SIGKILL); _ = signalPID(cli, syscall.SIGKILL) })
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if !waitDead(cli, child) {
		t.Fatalf("CLI (%d alive=%v) or its child (%d alive=%v) survived a SIGTERM to the wake\nstderr:\n%s", cli, alive(cli), child, alive(child), stderr.String())
	}
	if h.exists(h.lockDir()) {
		t.Fatalf("lock left behind after a SIGTERM\nstderr:\n%s", stderr.String())
	}
}

// A state directory that cannot be created is an error the operator hears
// about, not a run in progress.
func TestWakeLibUnwritableStateDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	h := newLibHarness(t)
	ro := filepath.Join(h.dir, "ro")
	mkdirs(t, ro)
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	env := []string{"TINCAN_WAKE_STATE_DIR=" + filepath.Join(ro, "state"), "TINCAN_WAKE_OPERATOR=ops"}
	r := h.run(driver, env...)
	if code := exitCode(r.err); code != 1 {
		t.Fatalf("exit = %d (%v), want 1\nstderr:\n%s", code, r.err, r.stderr)
	}
	if strings.Contains(r.stderr, "in progress") || !strings.Contains(r.stderr, "state directory") {
		t.Fatalf("state-dir failure not reported as such:\n%s", r.stderr)
	}
	if h.count(h.runs) != 0 {
		t.Fatal("CLI ran without a state directory")
	}
	if n := h.count(h.notices); n != 1 {
		t.Fatalf("operator notices = %d, want 1", n)
	}
	// The notice is sent once until a run succeeds, tracked beside the config.
	if r := h.run(driver, env...); exitCode(r.err) != 1 || h.count(h.notices) != 1 {
		t.Fatalf("second nudge: %v, notices %d\nstderr:\n%s", r.err, h.count(h.notices), r.stderr)
	}
	if r := h.run(driver); r.err != nil {
		t.Fatalf("run with a good state dir: %v\nstderr:\n%s", r.err, r.stderr)
	}
	if h.exists(h.config + ".wake-notified") {
		t.Fatal("successful run left the state-dir notice marker")
	}
}

// A lock whose bound or pid cannot be written (the state directory's disk
// is full, say) would let a later wake break it early, so the wake gives the
// lock back and refuses to run. A file-size limit of zero stands in for the
// full disk: the lock directory can be made, but nothing can be written.
func TestWakeLibUnrecordableLockRefuses(t *testing.T) {
	h := newLibHarness(t)
	body := strings.Replace(driver, "tincan_wake_begin\n",
		"trap '' XFSZ\nulimit -S -f 0\ntincan_wake_begin\nulimit -S -f unlimited\n", 1)
	env := []string{"TINCAN_WAKE_OPERATOR=ops"}
	r := h.run(body, env...)
	if code := exitCode(r.err); code != 1 {
		t.Fatalf("exit = %d (%v), want 1\nstderr:\n%s", code, r.err, r.stderr)
	}
	if strings.Contains(r.stderr, "in progress") || !strings.Contains(r.stderr, "cannot record the lock") {
		t.Fatalf("lock-record failure not reported as such:\n%s", r.stderr)
	}
	if n := h.count(h.runs); n != 0 {
		t.Fatalf("CLI ran %d times without a recorded lock", n)
	}
	if h.exists(h.lockDir()) {
		t.Fatal("unrecorded lock left behind")
	}
	if n := h.count(h.notices); n != 1 {
		t.Fatalf("operator notices = %d, want 1", n)
	}
	// Once writes work again, the next nudge runs and clears the notice.
	if r := h.run(driver, env...); r.err != nil || h.count(h.runs) != 1 {
		t.Fatalf("run after the disk frees up: %v, runs %d\nstderr:\n%s", r.err, h.count(h.runs), r.stderr)
	}
	if h.exists(h.config + ".wake-notified") {
		t.Fatal("successful run left the notice marker")
	}
}

func TestWakeLibPreflightTimeout(t *testing.T) {
	h := newLibHarness(t)
	r := h.run(driver, "FAKE_VERSION_SLEEP=60", "TINCAN_WAKE_PREFLIGHT_TIMEOUT=1", "TINCAN_WAKE_OPERATOR=ops")
	if code := exitCode(r.err); code != 1 {
		t.Fatalf("exit = %d (%v), want 1\nstderr:\n%s", code, r.err, r.stderr)
	}
	if r.took > 20*time.Second {
		t.Fatalf("hanging version probe held the wake for %s", r.took)
	}
	if !strings.Contains(r.stderr, "timed out") || !h.exists(filepath.Join(h.state, "backoff")) || h.count(h.notices) != 1 {
		t.Fatalf("hung probe: backoff %v, notices %d\nstderr:\n%s", h.exists(filepath.Join(h.state, "backoff")), h.count(h.notices), r.stderr)
	}
	if h.count(h.runs) != 0 || h.exists(h.lockDir()) {
		t.Fatalf("runs %d, lock left %v", h.count(h.runs), h.exists(h.lockDir()))
	}
}

func TestWakeLibPreflightTimeoutOnListing(t *testing.T) {
	h := newLibHarness(t)
	body := strings.Replace(driver, `cat "$MCP_LISTING"`, `sleep 60; cat "$MCP_LISTING"`, 1)
	r := h.run(body, "TINCAN_WAKE_PREFLIGHT_TIMEOUT=1")
	if exitCode(r.err) != 1 || r.took > 20*time.Second || !strings.Contains(r.stderr, "timed out") {
		t.Fatalf("hung listing: %v after %s\nstderr:\n%s", r.err, r.took, r.stderr)
	}
}

// Without TINCAN_CONFIG the notice would go out as the default config's
// agent, so it is only logged.
func TestWakeLibNoConfigSendsNoNotice(t *testing.T) {
	h := newLibHarness(t)
	r := h.run(driver, "TINCAN_CONFIG=", "TINCAN_WAKE_OPERATOR=ops")
	if exitCode(r.err) != 1 || !strings.Contains(r.stderr, "TINCAN_CONFIG") {
		t.Fatalf("unset TINCAN_CONFIG: %v\nstderr:\n%s", r.err, r.stderr)
	}
	if n := h.count(h.notices); n != 0 {
		t.Fatalf("operator notices = %d without TINCAN_CONFIG, want 0", n)
	}
}

// A CLI that exits normally but leaves a child running (a daemonized MCP
// server, say) must not leave it behind: the next nudge could start a
// second CLI beside it.
func TestWakeLibCleanExitReapsLeftoverChildren(t *testing.T) {
	for _, mode := range []struct{ name, leave, env string }{
		{"process group", "1", ""},
		{"process group, child in its own group", "pgrp", ""},
		{"saved tree", "1", "_TW_NO_PGRP=1"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			h := newLibHarness(t)
			env := []string{"FAKE_LEAVE=" + mode.leave}
			if mode.env != "" {
				env = append(env, mode.env)
			}
			r := h.run(driver, env...)
			child := readPid(t, filepath.Join(h.dir, "child.pid"))
			t.Cleanup(func() { _ = signalPID(child, syscall.SIGKILL) })
			if r.err != nil {
				t.Fatalf("wake: %v\nstderr:\n%s", r.err, r.stderr)
			}
			if !waitDead(child) {
				t.Fatalf("child %d the CLI left behind outlived the wake\nstderr:\n%s", child, r.stderr)
			}
			if h.exists(h.lockDir()) {
				t.Fatal("lock left behind")
			}
		})
	}
}

// A live run holds the lock through both preflights and the CLI run, so a
// lock whose pid is alive is not broken until it is older than all of them
// together could take.
func TestWakeLibLiveLockBoundCoversPreflights(t *testing.T) {
	env := []string{"TINCAN_WAKE_TIMEOUT=3600", "TINCAN_WAKE_PREFLIGHT_TIMEOUT=300"}
	h := newLibHarness(t)
	writeLock(t, h.lockDir(), os.Getpid())
	// Older than the run timeout plus five minutes, within the run plus
	// three preflight-length steps (two checks and a notice).
	old := time.Now().Add(-70 * time.Minute)
	if err := os.Chtimes(h.lockDir(), old, old); err != nil {
		t.Fatal(err)
	}
	r := h.run(driver, env...)
	if r.err != nil || h.count(h.runs) != 0 {
		t.Fatalf("live lock inside the bound: %v, runs %d\nstderr:\n%s", r.err, h.count(h.runs), r.stderr)
	}
	if strings.Contains(r.stderr, "stale lock") {
		t.Fatalf("a live run's lock was broken early:\n%s", r.stderr)
	}

	// Past every configured timeout, the pid is taken to be reused.
	older := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(h.lockDir(), older, older); err != nil {
		t.Fatal(err)
	}
	r = h.run(driver, env...)
	if r.err != nil || h.count(h.runs) != 1 || !strings.Contains(r.stderr, "stale lock") {
		t.Fatalf("lock past the bound: %v, runs %d\nstderr:\n%s", r.err, h.count(h.runs), r.stderr)
	}
}

// The operator notice is sent while the lock is held, so a hung send is cut
// off like a preflight.
func TestWakeLibHungNoticeTimesOut(t *testing.T) {
	h := newLibHarness(t)
	r := h.run(driver, "FAKE_VERSION=other 1.0", "TINCAN_WAKE_OPERATOR=ops", "TINCAN_WAKE_PREFLIGHT_TIMEOUT=1", "FAKE_NOTIFY_SLEEP=60")
	if exitCode(r.err) != 1 || r.took > 20*time.Second {
		t.Fatalf("hung notice: %v after %s\nstderr:\n%s", r.err, r.took, r.stderr)
	}
	if !strings.Contains(r.stderr, "could not notify") || h.exists(h.lockDir()) {
		t.Fatalf("hung notice: lock left %v\nstderr:\n%s", h.exists(h.lockDir()), r.stderr)
	}
}

// The holder records its own bound in the lock, so a wake configured with
// shorter timeouts does not break a live run's lock early.
func TestWakeLibLiveLockHonorsHolderBound(t *testing.T) {
	h := newLibHarness(t)
	writeLock(t, h.lockDir(), os.Getpid())
	if err := os.WriteFile(filepath.Join(h.lockDir(), "max"), []byte("600\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Far past this invocation's own bound (a few minutes), well inside the
	// holder's ten hours.
	old := time.Now().Add(-70 * time.Minute)
	if err := os.Chtimes(h.lockDir(), old, old); err != nil {
		t.Fatal(err)
	}
	short := []string{"TINCAN_WAKE_TIMEOUT=60", "TINCAN_WAKE_PREFLIGHT_TIMEOUT=1"}
	r := h.run(driver, short...)
	if r.err != nil || h.count(h.runs) != 0 {
		t.Fatalf("live lock inside the holder's bound: %v, runs %d\nstderr:\n%s", r.err, h.count(h.runs), r.stderr)
	}
	if strings.Contains(r.stderr, "stale lock") {
		t.Fatalf("a live run's lock was broken by a shorter-configured wake:\n%s", r.stderr)
	}

	// Past the holder's own bound, the pid is taken to be reused.
	older := time.Now().Add(-11 * time.Hour)
	if err := os.Chtimes(h.lockDir(), older, older); err != nil {
		t.Fatal(err)
	}
	r = h.run(driver, short...)
	if r.err != nil || h.count(h.runs) != 1 || !strings.Contains(r.stderr, "stale lock") {
		t.Fatalf("lock past the holder's bound: %v, runs %d\nstderr:\n%s", r.err, h.count(h.runs), r.stderr)
	}
}

// A run records its bound in the lock it holds.
func TestWakeLibRecordsLockBound(t *testing.T) {
	h := newLibHarness(t)
	r := h.run(`. "$TINCAN_WAKE_LIB"
tincan_wake_init fake
tincan_wake_begin
cat "$TINCAN_WAKE_STATE_DIR/lock/max" >"$STATE_OUT/max.out"
`, "TINCAN_WAKE_TIMEOUT=600", "TINCAN_WAKE_PREFLIGHT_TIMEOUT=30")
	if r.err != nil {
		t.Fatalf("wake: %v\nstderr:\n%s", r.err, r.stderr)
	}
	raw, err := os.ReadFile(filepath.Join(h.dir, "max.out"))
	// (600 + 3*30 + 4*15) seconds is 12.5 minutes, rounded up, plus five.
	if err != nil || strings.TrimSpace(string(raw)) != "18" {
		t.Fatalf("recorded bound = %q, %v; want 18", raw, err)
	}
}
