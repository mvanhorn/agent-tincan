package cli

import (
	"regexp"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

// TINCAN_SESSION names the session for any command; without it a command
// names none and sees every reply, as before sessions.
func TestConnectTakesTincanSession(t *testing.T) {
	t.Setenv("TINCAN_CONFIG", t.TempDir()+"/tincan.json")
	t.Setenv("TINCAN_PROXY", "")
	t.Setenv("TINCAN_RELAY", "http://tincan-relay")
	if err := client.SaveConfig(client.Config{Relay: "http://tincan-relay", Agent: "muse", RelayKey: "k", RelayURLs: []string{"http://tincan-relay"}, RelayInfoAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TINCAN_SESSION", "site")
	r, _, err := connect()
	if err != nil || r.Session() != "site" {
		t.Fatalf("session = %q, %v; want site", r.Session(), err)
	}
	t.Setenv("TINCAN_SESSION", "")
	if r, _, err = connect(); err != nil || r.Session() != "" {
		t.Fatalf("session = %q, %v; want none", r.Session(), err)
	}
}

// Each MCP server process makes its own session id, in the form the relay
// accepts.
func TestNewSessionIsUniqueAndValid(t *testing.T) {
	valid := regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	a, b := newSession(), newSession()
	if a == b || !valid.MatchString(a) || !valid.MatchString(b) {
		t.Fatalf("sessions %q and %q", a, b)
	}
}
