package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// serveAdminSocket serves the mesh's admin API on the local admin socket of
// this test's home, the way a relay running on this machine does. Unix
// socket paths are length-limited, so the home lives directly under /tmp
// rather than in the test's own temp dir.
func serveAdminSocket(t *testing.T, m *testrelay.Mesh) {
	t.Helper()
	skipOnWindows(t, skipRelayHost)
	home, err := os.MkdirTemp("/tmp", "tincan-host-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	setHome(t, home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	dir := defaultStateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := client.ListenUnix(filepath.Join(dir, "admin.sock"))
	if err != nil {
		t.Skipf("unix socket unavailable: %v", err)
	}
	srv := &http.Server{Handler: m.Server.AdminHandler()}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
}

// The relay host is the team's admin and usually never joins, so it has no
// saved config. tincan agents and tincan onboard there need no flags: they
// read the roster through the local admin socket, like invite and the other
// admin commands, and onboard names the relay by the address it advertises.
func TestAgentsAndOnboardWorkOnRelayHostWithoutFlags(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	m.Server.SetURLs([]string{"http://tincan-relay", "http://100.64.0.1"})
	useConfig(t, client.Config{})
	serveAdminSocket(t, m)

	out, err := run(t, agentsCmd())
	if err != nil {
		t.Fatalf("tincan agents on the relay host: %v", err)
	}
	for _, name := range []string{"grokbot", "instinct", "muse"} {
		if !strings.Contains(out, name) {
			t.Fatalf("agents output lacks %s:\n%s", name, out)
		}
	}

	k := onboardKit(t, "--section", "agents")
	if len(k.Agents) != 3 || k.RelayURL != "http://tincan-relay" {
		t.Fatalf("onboard over the admin socket: %d agents, relay %q", len(k.Agents), k.RelayURL)
	}
	// An explicit --relay still wins over the socket, as for every admin command.
	if k := onboardKit(t, "--section", "agents", "--relay", m.URL("admin")); k.RelayURL != m.URL("admin") || len(k.Agents) != 3 {
		t.Fatalf("onboard --relay beside a local socket: %d agents, relay %q", len(k.Agents), k.RelayURL)
	}
}

// Away from the relay host, with nothing saved, both commands still say
// what to do instead of guessing.
func TestAgentsAndOnboardWithoutRelaySayHow(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	useConfig(t, client.Config{})
	if _, err := run(t, agentsCmd()); err == nil || !strings.Contains(err.Error(), "no relay configured") {
		t.Fatalf("agents with nothing configured: %v", err)
	}
	if _, err := run(t, onboardCmd()); err == nil || !strings.Contains(err.Error(), "--offline") {
		t.Fatalf("onboard with nothing configured: %v", err)
	}
}

// An explicit --socket wins over a saved relay, and when the relay advertises
// no address the kit prints a placeholder for the reader to fill in.
func TestOnboardExplicitSocketAndRelayURLPlaceholder(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{Relay: "http://127.0.0.1:1"})
	serveAdminSocket(t, m)

	k := onboardKit(t, "--section", "agents", "--socket", filepath.Join(defaultStateDir(), "admin.sock"))
	if len(k.Agents) != 3 || k.RelayURL != "<relay URL>" {
		t.Fatalf("onboard --socket with no advertised URLs: %d agents, relay %q", len(k.Agents), k.RelayURL)
	}
}
