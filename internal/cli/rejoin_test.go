package cli

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// Instinct's sandbox is rebuilt and comes back as a new Tailscale node with
// no config. tincan rejoin re-admits it without an invite, saves the config,
// and the next command works and still sees the request queued during the
// rebuild.
func TestRejoinSavesConfigAndNextCommandWorks(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	if _, err := m.Client(t, "grokbot").Ask(t.Context(), "instinct", "summarize the report", "", 0); err != nil {
		t.Fatal(err)
	}
	url := m.Rebuild(t, "instinct", "instinct")
	useConfig(t, client.Config{})

	out, err := run(t, Root(), "rejoin", "--relay", url)
	if err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	if !strings.Contains(out, `"instinct"`) {
		t.Fatalf("rejoin output = %q", out)
	}
	cfg, err := client.LoadConfig()
	if err != nil || cfg.Relay != url || cfg.Agent != "instinct" {
		t.Fatalf("saved config = %+v, %v", cfg, err)
	}
	out, err = run(t, Root(), "inbox")
	if err != nil || !strings.Contains(out, "summarize the report") {
		t.Fatalf("inbox after rejoin = %q, %v", out, err)
	}
}

func TestRejoinNameAndProxyAreSaved(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{})
	if _, err := run(t, Root(), "rejoin", "--relay", m.URL("muse"), "--name", "muse"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := client.LoadConfig()
	if cfg.Agent != "muse" {
		t.Fatalf("config = %+v", cfg)
	}
	// A wrong name is refused and leaves the config alone.
	if _, err := run(t, Root(), "rejoin", "--relay", m.URL("muse"), "--name", "grokbot"); err == nil {
		t.Fatal("rejoin as another machine's agent should fail")
	}
	if cfg2, _ := client.LoadConfig(); !reflect.DeepEqual(cfg2, cfg) {
		t.Fatalf("config changed on failure: %+v", cfg2)
	}
}

// A machine that was never joined is the one case that needs a person.
func TestRejoinNeverJoinedMachineNeedsInvite(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{})
	_, err := run(t, Root(), "rejoin", "--relay", m.URL("stranger"))
	if err == nil || !strings.Contains(err.Error(), "first-time invite") {
		t.Fatalf("want first-time invite message, got %v", err)
	}
	if strings.Contains(err.Error(), "tincan rejoin --relay") {
		t.Fatalf("rejoin must not suggest itself: %v", err)
	}
	if cfg, _ := client.LoadConfig(); cfg.Relay != "" {
		t.Fatalf("nothing should be saved: %+v", cfg)
	}
}

func TestClientCommandsSuggestRejoin(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{})
	for _, args := range [][]string{{"agents"}, {"inbox"}, {"ask", "muse", "hi"}} {
		_, err := run(t, Root(), args...)
		if err == nil || !strings.Contains(err.Error(), "no relay configured") || !strings.Contains(err.Error(), "tincan rejoin --relay") {
			t.Fatalf("%v with no config: %v", args, err)
		}
	}
	useConfig(t, client.Config{Relay: m.URL("stranger")})
	for _, args := range [][]string{{"agents"}, {"inbox"}, {"get", "r1"}} {
		_, err := run(t, Root(), args...)
		if err == nil || !strings.Contains(err.Error(), "not a joined agent") || !strings.Contains(err.Error(), "tincan rejoin --relay "+m.URL("stranger")) {
			t.Fatalf("%v from an unjoined machine: %v", args, err)
		}
	}
}

func TestRelayNoAutoRebindFlag(t *testing.T) {
	var f relayFlags
	cmd := relayCmd()
	if fl := cmd.Flags().Lookup("no-auto-rebind"); fl == nil || fl.DefValue != "false" {
		t.Fatalf("--no-auto-rebind flag = %+v, want default false (auto rebind on)", fl)
	}
	if f.directoryConfig().NoAutoRebind {
		t.Fatal("auto rebind should be on by default")
	}
	f.noRebind = true
	if !f.directoryConfig().NoAutoRebind {
		t.Fatal("--no-auto-rebind should reach the directory")
	}
}

// The relay refuses a rebuilt machine while its old node is still online
// (identity's rebind check). rejoin says to shut the old machine down, not to
// get an invite. The message crosses HTTP, so it is matched by text.
func TestRejoinErrorOldMachineStillOnline(t *testing.T) {
	cfg := client.Config{Relay: "http://tincan-relay", Agent: "instinct"}
	online := &client.APIError{Code: 403, Message: `instinct-2: "instinct" is still bound to instinct, which is online: not a joined agent`}
	err := rejoinError(online, cfg)
	if !strings.Contains(err.Error(), "old machine online") || strings.Contains(err.Error(), "first-time invite") {
		t.Fatalf("online branch = %v", err)
	}
	if !errors.Is(err, online) {
		t.Fatalf("online branch should wrap the relay error: %v", err)
	}

	// An agent or machine merely named "online" is not the online branch.
	named := &client.APIError{Code: 403, Message: `online-box: not a joined agent`}
	if err := rejoinError(named, cfg); !strings.Contains(err.Error(), "first-time invite") {
		t.Fatalf("never-joined machine named online = %v", err)
	}
}
