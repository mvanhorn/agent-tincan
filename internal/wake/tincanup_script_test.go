package wake

import (
	"errors"
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

// These tests run examples/grokbot/tincan-up.sh against a HOME laid out the
// way a Grok Bot box keeps it, with fake tailscale, tailscaled and tincan
// binaries in their usual places. Fake ps, pgrep and pkill on PATH serve the
// process list from a file, so the tests never see the machine's own
// processes, and a fake sleep keeps the script's waits short.

// fakeTailscale keeps the daemon's backend state in $FAKE_DIR/ts.state (no
// file: nothing answers on the socket). "up" records its argv and stdin and
// brings the node to Running.
const fakeTailscale = `#!/bin/sh
[ "${1:-}" != "${1#--socket=}" ] && shift
case "${1:-}" in
version) echo 1.80.0;;
status)
  [ -f "$FAKE_DIR/ts.state" ] || exit 1
  printf '{"BackendState": "%s"}\n' "$(cat "$FAKE_DIR/ts.state")";;
up)
  for a in "$@"; do printf '%s\0' "$a"; done > "$FAKE_DIR/up.argv"
  case "$*" in *--auth-key=file:/dev/stdin*) cat > "$FAKE_DIR/up.stdin";; esac
  echo Running > "$FAKE_DIR/ts.state";;
ip) echo 100.64.0.5;;
esac
`

// fakeTailscaled records its argv and comes up Stopped when its state dir
// holds an identity, NeedsLogin otherwise.
const fakeTailscaled = `#!/bin/sh
for a in "$@"; do printf '%s\0' "$a"; done > "$FAKE_DIR/tailscaled.argv"
statedir=
for a in "$@"; do case "$a" in --statedir=*) statedir=${a#--statedir=};; esac; done
if [ -s "$statedir/tailscaled.state" ]; then echo Stopped; else echo NeedsLogin; fi > "$FAKE_DIR/ts.state"
`

// fakeTincanUp is tincan: doctor records its TINCAN_CONFIG and passes, and relay records its argv and its
// Tailscale keys (or <unset>), then stays up until it is killed. It records
// nothing under FAKE_NO_RECORD, for a relay the test starts itself.
const fakeTincanUp = `#!/bin/sh
case "${1:-}" in
doctor) printf '%s' "${TINCAN_CONFIG-<unset>}" > "$FAKE_DIR/doctor.config"; exit 0;;
relay)
  if [ -z "${FAKE_NO_RECORD:-}" ]; then
    printf '%s\n%s\n' "${TS_AUTHKEY-<unset>}" "${RELAY_TS_AUTHKEY-<unset>}" > "$FAKE_DIR/relay.env.tmp"
    for a in "$@"; do printf '%s\0' "$a"; done > "$FAKE_DIR/relay.argv"
    mv "$FAKE_DIR/relay.env.tmp" "$FAKE_DIR/relay.env"
  fi
  trap 'exit 0' TERM
  while :; do REAL_SLEEP 0.2; done;;
esac
`

type tincanUpHarness struct {
	t                     *testing.T
	home, fake, path, sh  string
	relayState, relayPID  string
	procs                 string
	tsState, tincan, real string
}

func newTincanUpHarness(t *testing.T) *tincanUpHarness {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("tincan-up.sh is a bash script")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found")
	}
	realPS, err := exec.LookPath("ps")
	if err != nil {
		t.Skip("ps not found")
	}
	realSleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not found")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "examples", "grokbot", "tincan-up.sh"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	h := &tincanUpHarness{t: t, sh: bash, real: script,
		home: filepath.Join(dir, "home"),
		fake: filepath.Join(dir, "fake"),
		path: filepath.Join(dir, "bin"),
	}
	h.relayState = filepath.Join(h.home, ".config", "tincan-relay")
	h.relayPID = filepath.Join(h.home, ".cache", "tincan-relay.pid")
	h.procs = filepath.Join(h.fake, "procs")
	h.tsState = filepath.Join(h.home, ".config", "tailscale", "tailscaled.state")
	h.tincan = filepath.Join(h.home, ".local", "bin", "tincan")
	lib := filepath.Join(h.home, ".local", "lib", "tailscale")
	mkdirs(t, h.fake, h.path, lib, filepath.Dir(h.tincan))
	h.write(filepath.Join(lib, "tailscale"), fakeTailscale, 0o755)
	h.write(filepath.Join(lib, "tailscaled"), fakeTailscaled, 0o755)
	h.write(h.tincan, strings.ReplaceAll(fakeTincanUp, "REAL_SLEEP", realSleep), 0o755)
	h.write(filepath.Join(h.path, "sleep"), "#!/bin/sh\nexit 0\n", 0o755)
	h.write(filepath.Join(h.path, "pkill"), "#!/bin/sh\nexit 1\n", 0o755)
	h.write(filepath.Join(h.path, "pgrep"), "#!/bin/sh\nfor p; do :; done\ngrep -Eq -e \"$p\" \"$FAKE_DIR/procs\"\n", 0o755)
	h.write(filepath.Join(h.path, "ps"), "#!/bin/sh\ncase \" $* \" in *\" -e\"*) cat \"$FAKE_DIR/procs\"; exit 0;; esac\nexec "+realPS+" \"$@\"\n", 0o755)
	h.setProcs()
	t.Cleanup(h.killRelay)
	return h
}

func (h *tincanUpHarness) write(path, body string, mode os.FileMode) {
	h.t.Helper()
	mkdirs(h.t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		h.t.Fatal(err)
	}
}

// setProcs sets the process list the fake ps and pgrep report.
func (h *tincanUpHarness) setProcs(lines ...string) {
	h.t.Helper()
	var body strings.Builder
	for _, l := range lines {
		body.WriteString(l + "\n")
	}
	h.write(h.procs, body.String(), 0o644)
}

// savedIdentity gives the box's own node the identity a rebuild keeps.
func (h *tincanUpHarness) savedIdentity() {
	h.write(h.tsState, "{\"node\":\"grokbot\"}", 0o600)
}

func (h *tincanUpHarness) killRelay() {
	raw, err := os.ReadFile(h.relayPID)
	if err != nil {
		return
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
}

type tincanUpRun struct {
	code int
	out  string
}

func (h *tincanUpHarness) run(env ...string) tincanUpRun {
	h.t.Helper()
	cmd := exec.Command(h.sh, h.real)
	cmd.Env = append([]string{
		"PATH=" + h.path + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + h.home,
		"FAKE_DIR=" + h.fake,
		"START_RELAY=1",
	}, env...)
	out, err := cmd.CombinedOutput()
	r := tincanUpRun{out: string(out)}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		r.code = ee.ExitCode()
	default:
		h.t.Fatalf("tincan-up.sh: %v\n%s", err, out)
	}
	return r
}

func (h *tincanUpHarness) exists(name string) bool {
	_, err := os.Stat(filepath.Join(h.fake, name))
	return err == nil
}

func (h *tincanUpHarness) argv(name string) []string {
	h.t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.fake, name))
	if err != nil {
		h.t.Fatalf("%s: %v", name, err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
}

// relayEnv waits for the relay the script started to record its keys, and
// returns its TS_AUTHKEY and RELAY_TS_AUTHKEY.
func (h *tincanUpHarness) relayEnv() (tsKey, relayKey string) {
	h.t.Helper()
	p := filepath.Join(h.fake, "relay.env")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		raw, err := os.ReadFile(p)
		if err == nil {
			lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
			if len(lines) == 2 {
				return lines[0], lines[1]
			}
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("the relay was not started (no %s)", p)
		}
	}
}

func TestTincanUpFreshHomeWithoutKeyStartsNothing(t *testing.T) {
	h := newTincanUpHarness(t)
	r := h.run()
	if r.code != 3 {
		t.Fatalf("exit %d, want 3\n%s", r.code, r.out)
	}
	if !strings.Contains(r.out, "TS_AUTHKEY is not set") {
		t.Errorf("output does not say the key is missing:\n%s", r.out)
	}
	if h.exists("relay.argv") || h.exists("relay.env") {
		t.Fatalf("a relay was started on a box that is not on the tailnet\n%s", r.out)
	}
}

func TestTincanUpRebuiltBoxComesBackWithoutKey(t *testing.T) {
	h := newTincanUpHarness(t)
	h.savedIdentity()
	r := h.run()
	if r.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", r.code, r.out)
	}
	daemon := h.argv("tailscaled.argv")
	if !slices.Contains(daemon, "--statedir="+filepath.Dir(h.tsState)) || !slices.Contains(daemon, "--tun=userspace-networking") {
		t.Errorf("tailscaled argv %q does not use the home-folder state in userspace mode", daemon)
	}
	up := h.argv("up.argv")
	for _, a := range up {
		if strings.HasPrefix(a, "--auth-key") {
			t.Errorf("tailscale up used a key on a node with saved identity: %q", up)
		}
	}
	if h.exists("up.stdin") {
		t.Error("tailscale up read a key from stdin on a node with saved identity")
	}
	h.relayEnv()
	relay := h.argv("relay.argv")
	if want := []string{"relay", "--state-dir", h.relayState}; !slices.Equal(relay[:min(len(relay), 3)], want) {
		t.Errorf("relay argv = %q, want it to start with %q", relay, want)
	}
	for _, a := range relay {
		if a == "--listen" || strings.HasPrefix(a, "--listen=") {
			t.Errorf("relay started with --listen instead of tsnet: %q", relay)
		}
	}
	if !strings.Contains(r.out, "started tincan relay") {
		t.Errorf("output does not report the relay start:\n%s", r.out)
	}
}

func TestTincanUpLeavesRunningRelayAlone(t *testing.T) {
	h := newTincanUpHarness(t)
	h.savedIdentity()
	h.write(filepath.Join(h.fake, "ts.state"), "Running", 0o644)
	// This box's own userspace tailscaled is not the legacy layout.
	h.setProcs(filepath.Join(h.home, ".local", "lib", "tailscale", "tailscaled") +
		" --tun=userspace-networking --statedir=" + filepath.Dir(h.tsState) +
		" --socket=" + filepath.Join(h.home, ".cache", "tailscale", "tailscaled.sock"))
	cmd := exec.Command(h.tincan, "relay", "--state-dir", h.relayState)
	cmd.Env = append(os.Environ(), "FAKE_DIR="+h.fake, "FAKE_NO_RECORD=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h.write(h.relayPID, strconv.Itoa(cmd.Process.Pid)+"\n", 0o644)
	go func() { _ = cmd.Wait() }()

	r := h.run()
	if r.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", r.code, r.out)
	}
	if h.exists("relay.argv") || strings.Contains(r.out, "started tincan relay") {
		t.Fatalf("a second relay was started\n%s", r.out)
	}
	if h.exists("tailscaled.argv") {
		t.Errorf("a second tailscaled was started\n%s", r.out)
	}
}

func TestTincanUpRefusesLegacyLayout(t *testing.T) {
	for name, proc := range map[string]string{
		"listen relay":       "/home/box/.local/bin/tincan relay --listen 100.105.244.112 --admin laptop",
		"system tailscaled":  "/usr/sbin/tailscaled --state=/var/lib/tailscale/tailscaled.state --socket=/run/tailscale/tailscaled.sock --port=41641",
		"default tailscaled": "tailscaled",
	} {
		t.Run(name, func(t *testing.T) {
			h := newTincanUpHarness(t)
			h.savedIdentity()
			h.setProcs("/bin/bash -l", proc)
			r := h.run()
			if r.code != 4 {
				t.Fatalf("exit %d, want 4\n%s", r.code, r.out)
			}
			if !strings.Contains(r.out, "legacy layout") {
				t.Errorf("output does not say legacy layout:\n%s", r.out)
			}
			if h.exists("tailscaled.argv") || h.exists("up.argv") || h.exists("relay.argv") {
				t.Fatalf("started something on a legacy layout\n%s", r.out)
			}
		})
	}
}

func TestTincanUpStripsBoxKeyFromRelay(t *testing.T) {
	h := newTincanUpHarness(t)
	h.savedIdentity()
	r := h.run("TS_AUTHKEY=tskey-auth-box-secret")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", r.code, r.out)
	}
	if ts, _ := h.relayEnv(); ts != "<unset>" {
		t.Fatalf("relay got TS_AUTHKEY %q, want it unset", ts)
	}
}

func TestTincanUpRelayKeyOnlyForFirstLogin(t *testing.T) {
	const key = "tskey-auth-relay-secret"
	t.Run("no relay identity", func(t *testing.T) {
		h := newTincanUpHarness(t)
		h.savedIdentity()
		r := h.run("TS_AUTHKEY=tskey-auth-box-secret", "RELAY_TS_AUTHKEY="+key)
		if r.code != 0 {
			t.Fatalf("exit %d, want 0\n%s", r.code, r.out)
		}
		ts, relayKey := h.relayEnv()
		if ts != key {
			t.Fatalf("relay TS_AUTHKEY = %q, want RELAY_TS_AUTHKEY", ts)
		}
		if relayKey != "<unset>" {
			t.Errorf("relay still sees RELAY_TS_AUTHKEY %q", relayKey)
		}
		for _, a := range h.argv("relay.argv") {
			if strings.Contains(a, key) {
				t.Errorf("relay key on the relay's command line: %q", a)
			}
		}
		if strings.Contains(r.out, key) {
			t.Errorf("output shows the relay key:\n%s", r.out)
		}
	})
	t.Run("relay identity saved", func(t *testing.T) {
		h := newTincanUpHarness(t)
		h.savedIdentity()
		h.write(filepath.Join(h.relayState, "tsnet", "tailscaled.state"), "{\"node\":\"tincan-relay\"}", 0o600)
		r := h.run("RELAY_TS_AUTHKEY=" + key)
		if r.code != 0 {
			t.Fatalf("exit %d, want 0\n%s", r.code, r.out)
		}
		ts, relayKey := h.relayEnv()
		if ts != "<unset>" || relayKey != "<unset>" {
			t.Fatalf("relay with saved identity got keys TS_AUTHKEY=%q RELAY_TS_AUTHKEY=%q, want none", ts, relayKey)
		}
	})
}

// workspaceLayout lays out a host where only /workspace survives (Hark): the
// Tailscale binaries, node state, socket and tincan live under a workspace
// folder outside HOME, and the lock and log stay in a local cache folder.
type workspaceLayout struct {
	lib, bin, stateDir, sock, cache string
}

func (h *tincanUpHarness) workspace() workspaceLayout {
	h.t.Helper()
	ws := filepath.Join(filepath.Dir(h.home), "workspace")
	w := workspaceLayout{
		lib:      filepath.Join(ws, "lib", "tailscale"),
		bin:      filepath.Join(ws, "bin"),
		stateDir: filepath.Join(ws, "tailscale", "state"),
		sock:     filepath.Join(ws, "tailscale", "tailscaled.sock"),
		cache:    filepath.Join(h.home, ".cache", "hark"),
	}
	h.write(filepath.Join(w.lib, "tailscale"), fakeTailscale, 0o755)
	h.write(filepath.Join(w.lib, "tailscaled"), fakeTailscaled, 0o755)
	raw, err := os.ReadFile(h.tincan)
	if err != nil {
		h.t.Fatal(err)
	}
	h.write(filepath.Join(w.bin, "tincan"), string(raw), 0o755)
	return w
}

func (w workspaceLayout) env(extra ...string) []string {
	return append([]string{
		"START_RELAY=0",
		"TAILSCALE_LIB=" + w.lib,
		"TAILSCALE_BIN=" + w.bin,
		"TAILSCALE_STATEDIR=" + w.stateDir,
		"TAILSCALE_CACHE=" + w.cache,
		"TS_SOCKET=" + w.sock,
		"TS_HOSTNAME=hark-workspace",
		"TS_TAGS=tag:hark",
	}, extra...)
}

func TestTincanUpWorkspaceLayoutComesBackAsSameNode(t *testing.T) {
	h := newTincanUpHarness(t)
	w := h.workspace()
	h.write(filepath.Join(w.stateDir, "tailscaled.state"), "{\"node\":\"hark-workspace\"}", 0o600)
	config := filepath.Join(filepath.Dir(w.bin), "tincan", "client.json")
	r := h.run(w.env("TINCAN_CONFIG=" + config)...)
	if r.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", r.code, r.out)
	}
	daemon := h.argv("tailscaled.argv")
	if !slices.Contains(daemon, "--statedir="+w.stateDir) || !slices.Contains(daemon, "--socket="+w.sock) {
		t.Errorf("tailscaled argv %q does not use the workspace state dir and socket", daemon)
	}
	up := h.argv("up.argv")
	if !slices.Contains(up, "--hostname=hark-workspace") || !slices.Contains(up, "--advertise-tags=tag:hark") {
		t.Errorf("tailscale up argv %q does not keep the Hark name and tag", up)
	}
	for _, a := range up {
		if strings.HasPrefix(a, "--auth-key") {
			t.Errorf("tailscale up used a key on a node with saved identity: %q", up)
		}
	}
	if _, err := os.Stat(filepath.Join(h.home, ".config", "tailscale")); err == nil {
		t.Error("the script created the home-folder state dir in the workspace layout")
	}
	if _, err := os.Stat(filepath.Join(w.bin, "tailscale")); err != nil {
		t.Errorf("no tailscale wrapper in the workspace bin: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(h.fake, "doctor.config"))
	if err != nil || string(raw) != config {
		t.Errorf("tincan doctor ran with TINCAN_CONFIG %q (%v), want %q", raw, err, config)
	}
}

// adoptedDaemon is a tailscaled the host started itself on the configured
// socket with its state in the configured state dir, from a path that would
// otherwise read as a system tailscaled.
func adoptedDaemon(w workspaceLayout) string {
	return "/usr/local/bin/tailscaled --tun=userspace-networking --statedir " + w.stateDir + " --socket=" + w.sock + " --socks5-server=localhost:1055"
}

func TestTincanUpAdoptsDaemonOnItsSocket(t *testing.T) {
	h := newTincanUpHarness(t)
	w := h.workspace()
	h.write(filepath.Join(h.fake, "ts.state"), "Running", 0o644)
	h.setProcs("/bin/bash -l", adoptedDaemon(w))
	r := h.run(w.env()...)
	if r.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", r.code, r.out)
	}
	if h.exists("tailscaled.argv") || h.exists("up.argv") {
		t.Fatalf("started or reconfigured a daemon that was already running on the socket\n%s", r.out)
	}
}

func TestTincanUpAdoptedDaemonLoggedOutNeedsKey(t *testing.T) {
	h := newTincanUpHarness(t)
	w := h.workspace()
	h.write(filepath.Join(h.fake, "ts.state"), "NeedsLogin", 0o644)
	h.setProcs(adoptedDaemon(w))
	r := h.run(w.env()...)
	if r.code != 3 {
		t.Fatalf("exit %d, want 3\n%s", r.code, r.out)
	}
	if h.exists("tailscaled.argv") || h.exists("up.argv") {
		t.Fatalf("started a daemon or ran up with no key\n%s", r.out)
	}
}

func TestTincanUpAdoptedDaemonStillRefusesListenRelay(t *testing.T) {
	h := newTincanUpHarness(t)
	w := h.workspace()
	h.write(filepath.Join(h.fake, "ts.state"), "Running", 0o644)
	h.setProcs(adoptedDaemon(w), "/workspace/bin/tincan relay --listen 100.97.127.52")
	r := h.run(w.env()...)
	if r.code != 4 {
		t.Fatalf("exit %d, want 4\n%s", r.code, r.out)
	}
	if !strings.Contains(r.out, "--listen") {
		t.Errorf("output does not name the --listen relay:\n%s", r.out)
	}
}

// A daemon on the configured socket whose identity lives outside the state dir
// would lose it on the next wipe, so it is refused, not adopted.
func TestTincanUpRefusesSocketDaemonWithStateElsewhere(t *testing.T) {
	for name, state := range map[string]string{
		"no state flag":   "",
		"state elsewhere": " --state=/var/lib/tailscale/tailscaled.state",
	} {
		t.Run(name, func(t *testing.T) {
			h := newTincanUpHarness(t)
			w := h.workspace()
			h.write(filepath.Join(h.fake, "ts.state"), "Running", 0o644)
			h.setProcs("/usr/local/bin/tailscaled --tun=userspace-networking --socket=" + w.sock + state)
			r := h.run(w.env()...)
			if r.code != 4 {
				t.Fatalf("exit %d, want 4\n%s", r.code, r.out)
			}
			if !strings.Contains(r.out, w.stateDir) {
				t.Errorf("output does not name the expected state dir:\n%s", r.out)
			}
		})
	}
	t.Run("state file in the state dir", func(t *testing.T) {
		h := newTincanUpHarness(t)
		w := h.workspace()
		h.write(filepath.Join(h.fake, "ts.state"), "Running", 0o644)
		h.setProcs("/usr/local/bin/tailscaled --state=" + filepath.Join(w.stateDir, "tailscaled.state") + " --socket " + w.sock)
		if r := h.run(w.env()...); r.code != 0 {
			t.Fatalf("exit %d, want 0\n%s", r.code, r.out)
		}
	})
}
