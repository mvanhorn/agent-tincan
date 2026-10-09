package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// Instinct's sandbox is rebuilt and comes back as a new Tailscale node with
// no config. tincan rejoin re-admits it without an invite, saves the config
// (with the relay key and addresses, so a later relay move is followed
// without any further command), and the next command works and still sees
// the request queued during the rebuild.
func TestRejoinSavesConfigAndNextCommandWorks(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	m.Server.SetURLs([]string{"http://tincan-relay"})
	if _, err := m.Client(t, "grokbot").Ask(t.Context(), "instinct", "summarize the report", "", 0, false); err != nil {
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
	if cfg.RelayKey == "" || cfg.RelayInfoAt.IsZero() || !slices.Equal(cfg.RelayURLs, []string{"http://tincan-relay"}) {
		t.Fatalf("rejoin left the relay info out: %+v", cfg)
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

// join and rejoin with --relay at a different relay drop the old relay's
// key and addresses, so a failed learnRelayInfo cannot leave the new address
// paired with the old key. The same relay (a trailing slash aside) keeps
// them.
func TestSetRelayDropsOldRelayInfo(t *testing.T) {
	at := time.Now().UTC()
	saved := client.Config{Relay: "http://old-relay", Agent: "grokbot", RelayKey: "k-old", RelayURLs: []string{"http://old-relay"}, RelayInfoAt: at}

	cfg := saved
	setRelay(&cfg, "http://old-relay/")
	if cfg.Relay != "http://old-relay/" || cfg.RelayKey != "k-old" || len(cfg.RelayURLs) != 1 || !cfg.RelayInfoAt.Equal(at) {
		t.Fatalf("same relay lost its info: %+v", cfg)
	}

	cfg = saved
	setRelay(&cfg, "http://new-relay")
	if cfg.Relay != "http://new-relay" || cfg.RelayKey != "" || cfg.RelayURLs != nil || !cfg.RelayInfoAt.IsZero() || cfg.Agent != "grokbot" {
		t.Fatalf("new relay kept the old relay's info: %+v", cfg)
	}
}

// rejoin --relay at a new relay that hands out no key (older than
// 0.5.0-rc12) leaves no key in the config, not the old relay's.
func TestRejoinAtNewRelayDropsOldKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "grokbot"})
	}))
	t.Cleanup(srv.Close)
	useConfig(t, client.Config{Relay: "http://old-relay", Agent: "grokbot", RelayKey: "k-old", RelayURLs: []string{"http://old-relay"}, RelayInfoAt: time.Now().UTC()})
	if out, err := run(t, rejoinCmd(), "--relay", srv.URL); err != nil {
		t.Fatalf("rejoin: %v\n%s", err, out)
	}
	cfg, err := client.LoadConfig()
	if err != nil || cfg.Relay != srv.URL || cfg.Agent != "grokbot" {
		t.Fatalf("config = %+v, %v", cfg, err)
	}
	if cfg.RelayKey != "" || cfg.RelayURLs != nil || !cfg.RelayInfoAt.IsZero() {
		t.Fatalf("the new relay's config kept the old relay's info: %+v", cfg)
	}
}

// rejoin with --proxy-credentials-from-env saves the proxy without its
// password, as join does.
func TestRejoinProxyCredentialsFromEnvSavesNoPassword(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{})
	proxy := forwardProxy(t, "rejoin-secret")
	host := strings.TrimPrefix(proxy.URL, "http://")
	if _, err := run(t, Root(), "rejoin", "--relay", m.URL("muse"), "--proxy", "http://u:rejoin-secret@"+host, "--proxy-credentials-from-env"); err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	cfg, err := client.LoadConfig()
	if err != nil || cfg.Agent != "muse" || cfg.Proxy != "http://"+host || !cfg.ProxyCredentialsFromEnv {
		t.Fatalf("saved config = %+v, %v", cfg, err)
	}
}

// The migration the 407 error asks for: client.json holds a proxy password
// that has since expired, and rejoin --proxy-credentials-from-env (with no
// --proxy) must dial with the current shell's password from HTTPS_PROXY,
// not the saved one, then save the proxy with no password at all.
func TestRejoinProxyCredentialsFromEnvReplacesExpiredSavedPassword(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	proxy := forwardProxy(t, "fresh-secret")
	host := strings.TrimPrefix(proxy.URL, "http://")
	hostname, _, _ := strings.Cut(host, ":")
	useConfig(t, client.Config{Relay: m.URL("muse"), Proxy: "http://u:expired-secret@" + host, Agent: "muse"})
	t.Setenv("HTTPS_PROXY", "http://u:fresh-secret@"+hostname+":3128")
	out, err := run(t, Root(), "rejoin", "--proxy-credentials-from-env")
	if err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	cfg, err := client.LoadConfig()
	if err != nil || cfg.Agent != "muse" || cfg.Proxy != "http://"+host || !cfg.ProxyCredentialsFromEnv {
		t.Fatalf("saved config = %+v, %v", cfg, err)
	}
	raw, err := os.ReadFile(client.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"expired-secret", "fresh-secret"} {
		if strings.Contains(string(raw)+out, secret) {
			t.Fatalf("a proxy password was saved or printed: %s\n%s", raw, out)
		}
	}
}
