package cli

import (
	"bytes"
	"encoding/xml"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/history"
)

// Only the flags the operator set reach the service, one --name=value
// each, and a slice flag becomes one argument per element.
func TestRelayServiceArgsCarryOnlyChangedFlags(t *testing.T) {
	cmd := relayInstallCmd()
	if err := cmd.Flags().Parse([]string{"--admin", "macbook,iphone", "--admin-login", "me@example.com", "--wake-grace", "5m", "--no-auto-rebind", "--binary", "/opt/tincan"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"--admin=macbook", "--admin=iphone", "--admin-login=me@example.com", "--no-auto-rebind=true", "--wake-grace=5m0s"}
	if got := relayServiceArgs(cmd.Flags()); !slices.Equal(got, want) {
		t.Fatalf("args = %q, want %q", got, want)
	}
	// The written arguments parse back to the same relay flags.
	back := relayCmd()
	if err := back.Flags().Parse(want); err != nil {
		t.Fatal(err)
	}
	if a, _ := back.Flags().GetStringSlice("admin"); !slices.Equal(a, []string{"macbook", "iphone"}) {
		t.Fatalf("--admin read back as %q", a)
	}
	if empty := relayInstallCmd(); relayServiceArgs(empty.Flags()) != nil {
		t.Fatal("no flags should write no arguments")
	}
}

func TestRelayInstallValidatesLikeTheRelay(t *testing.T) {
	cmd := relayInstallCmd()
	if err := cmd.Flags().Parse([]string{"--urgent-per-hour", "0"}); err != nil {
		t.Fatal(err)
	}
	if err := cmd.RunE(cmd, nil); err == nil || !strings.Contains(err.Error(), "--urgent-per-hour") {
		t.Fatalf("err = %v", err)
	}
}

func TestRelayHasInstallSubcommand(t *testing.T) {
	c, args, err := relayCmd().Find([]string{"install", "--admin", "x"})
	if err != nil || c.Name() != "install" || !slices.Equal(args, []string{"--admin", "x"}) {
		t.Fatalf("find = %v %q %v", c.Name(), args, err)
	}
}

// The plist runs tincan relay with the flags, through the launcher, and
// keeps the relay alive.
func TestRelayServiceDefDarwin(t *testing.T) {
	home := t.TempDir()
	res, err := history.InstallServiceDef(history.ServiceOptions{GOOS: "darwin", Home: home, Binary: "/opt/tin can/tincan", UID: 501, Launcher: "/A/agent-tincan"},
		relayServiceDef([]string{"--admin=macbook", "--state-dir=/Users/o/Library/Application Support/tincan-relay"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "Library", "LaunchAgents", "com.agenttincan.relay.plist"); res.Path != want {
		t.Fatalf("path = %s", res.Path)
	}
	b, _ := os.ReadFile(res.Path)
	if strings.Contains(string(b), "__") {
		t.Fatalf("placeholder left:\n%s", b)
	}
	dec := xml.NewDecoder(bytes.NewReader(b))
	var strs []string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		if cd, ok := tok.(xml.CharData); ok && strings.TrimSpace(string(cd)) != "" {
			strs = append(strs, string(cd))
		}
	}
	joined := strings.Join(strs, "|")
	for _, want := range []string{
		"Label|com.agenttincan.relay|ProgramArguments|/A/agent-tincan|/opt/tin can/tincan|relay|--admin=macbook|--state-dir=/Users/o/Library/Application Support/tincan-relay|EnvironmentVariables",
		"KeepAlive",
		home + "/Library/Logs/tincan-relay.log",
		"AssociatedBundleIdentifiers|com.agenttincan.app",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("plist missing %q:\n%s", want, joined)
		}
	}
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("/usr/bin/plutil", "-lint", res.Path).CombinedOutput(); err != nil {
			t.Fatalf("plutil -lint: %v %s", err, out)
		}
	}
}

// End to end on Linux: the unit runs the relay with the flags given,
// relative directories made absolute, never TS_AUTHKEY; install says how to
// join the tailnet only while the node has no state.
func TestRelayInstallLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("writes a systemd user unit")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("TS_AUTHKEY", "tskey-auth-secret")
	t.Chdir(home)

	run := func() string {
		cmd := relayInstallCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"--admin", "macbook", "--state-dir", "relay-state", "--binary", "/opt/tincan"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	out := run()
	unit := filepath.Join(home, ".config", "systemd", "user", "tincan-relay.service")
	b, err := os.ReadFile(unit)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(home, "relay-state")
	if want := `ExecStart="/opt/tincan" relay "--admin=macbook" "--state-dir=` + state + `"` + "\n"; !strings.Contains(string(b), want) {
		t.Fatalf("unit missing %q:\n%s", want, b)
	}
	if !strings.Contains(string(b), "Restart=always") {
		t.Fatalf("unit does not restart:\n%s", b)
	}
	if strings.Contains(string(b), "tskey") {
		t.Fatalf("unit holds the auth key:\n%s", b)
	}
	for _, want := range []string{"relay service definition: " + unit + " (not started)", "has not joined your tailnet", "TS_AUTHKEY=tskey-auth-... /opt/tincan relay --admin=macbook --state-dir=" + state, "systemctl --user enable --now tincan-relay.service"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "secret") {
		t.Fatalf("output shows the auth key:\n%s", out)
	}

	if err := os.MkdirAll(filepath.Join(state, "tsnet"), 0o700); err != nil {
		t.Fatal(err)
	}
	if out := run(); !strings.Contains(out, "has not joined") {
		t.Fatalf("a tsnet dir with no node state has not joined:\n%s", out)
	}
	if err := os.WriteFile(filepath.Join(state, "tsnet", "tailscaled.state"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := run(); strings.Contains(out, "has not joined") {
		t.Fatalf("join hint for a node that has state:\n%s", out)
	}
}

func TestRelayNeedsJoin(t *testing.T) {
	dir := t.TempDir()
	if !relayNeedsJoin(relayFlags{stateDir: dir}) {
		t.Fatal("a tsnet relay with no state needs to join")
	}
	if relayNeedsJoin(relayFlags{stateDir: dir, listen: "100.64.0.1"}) {
		t.Fatal("--listen uses the host's tailscaled")
	}
	if err := os.Mkdir(filepath.Join(dir, "tsnet"), 0o700); err != nil {
		t.Fatal(err)
	}
	if !relayNeedsJoin(relayFlags{stateDir: dir}) {
		t.Fatal("a tsnet dir left by a start that never logged in has not joined")
	}
	if err := os.WriteFile(filepath.Join(dir, "tsnet", "tailscaled.state"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if relayNeedsJoin(relayFlags{stateDir: dir}) {
		t.Fatal("a relay with tsnet state has joined")
	}
	if !relayNeedsJoin(relayFlags{stateDir: dir, gateway: true}) {
		t.Fatal("the Funnel gateway node has its own state")
	}
}

func TestShellJoinQuotes(t *testing.T) {
	got := shellJoin([]string{"/opt/tincan", "relay", "--state-dir=/Users/o/Library/Application Support/x", "--admin=it's"})
	want := `/opt/tincan relay '--state-dir=/Users/o/Library/Application Support/x' '--admin=it'\''s'`
	if got != want {
		t.Fatalf("got %s", got)
	}
}

// Without --state-dir the service still names the state dir install
// resolved, so systemd or launchd cannot pick another.
func TestRelayInstallAlwaysWritesStateDir(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("writes a systemd user unit")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := filepath.Join(home, "elsewhere")
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("PATH", t.TempDir())
	cmd := relayInstallCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"--binary", "/opt/tincan"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(home, ".config", "systemd", "user", "tincan-relay.service"))
	if err != nil {
		t.Fatal(err)
	}
	if want := `relay "--state-dir=` + filepath.Join(cfg, "tincan-relay") + `"` + "\n"; !strings.Contains(string(b), want) {
		t.Fatalf("unit missing %q:\n%s", want, b)
	}
}

// A slice value holding a comma or a quote stays one element when the
// relay reads the argument back.
func TestRelayServiceArgsQuoteSliceValues(t *testing.T) {
	cmd := relayInstallCmd()
	// The command line reads slice flags as CSV.
	if err := cmd.Flags().Set("admin-login", `"a,b""c",plain`); err != nil {
		t.Fatal(err)
	}
	args := relayServiceArgs(cmd.Flags())
	back := relayCmd()
	if err := back.Flags().Parse(args); err != nil {
		t.Fatalf("parse %q: %v", args, err)
	}
	if got, _ := back.Flags().GetStringSlice("admin-login"); !slices.Equal(got, []string{`a,b"c`, "plain"}) {
		t.Fatalf("read back %q from %q", got, args)
	}
}

func TestRelayInstallRefusesNewlines(t *testing.T) {
	if err := checkServiceArgs([]string{"--admin=a\nb"}); err == nil {
		t.Fatal("a newline reached the service definition")
	}
	if err := checkServiceArgs([]string{"--admin=a b"}); err != nil {
		t.Fatal(err)
	}
}
