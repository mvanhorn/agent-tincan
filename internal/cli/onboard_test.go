package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// useConfig points this test at a fresh client config holding cfg (nothing is
// saved when cfg is empty) and clears the env overrides.
func useConfig(t *testing.T, cfg client.Config) {
	t.Helper()
	t.Setenv("TINCAN_CONFIG", filepath.Join(t.TempDir(), "client.json"))
	t.Setenv("TINCAN_RELAY", "")
	t.Setenv("TINCAN_PROXY", "")
	// Admin and roster commands fall back to a local relay's admin socket
	// under the user config dir; point it at an empty dir so a relay running
	// on the test machine is never reached.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if !reflect.DeepEqual(cfg, client.Config{}) {
		if err := client.SaveConfig(cfg); err != nil {
			t.Fatal(err)
		}
	}
}

func run(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func onboardKit(t *testing.T, args ...string) onboard.Kit {
	t.Helper()
	out, err := run(t, onboardCmd(), append([]string{"--json"}, args...)...)
	if err != nil {
		t.Fatalf("onboard --json %v: %v", args, err)
	}
	var k onboard.Kit
	if err := json.Unmarshal([]byte(out), &k); err != nil {
		t.Fatalf("onboard --json is not a Kit: %v: %q", err, out)
	}
	return k
}

func agentNames(k onboard.Kit) []string {
	var names []string
	for _, a := range k.Agents {
		names = append(names, a.Name)
	}
	return names
}

func TestOnboardJSONAgainstRelay(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	k := onboardKit(t, "--operator", "grokbot", "--owner", "Matt", "--kind", "muse=proxy-sandbox")
	if got := agentNames(k); !slices.Equal(got, []string{"grokbot", "instinct", "muse"}) {
		t.Fatalf("agents = %v", got)
	}
	if k.RelayURL != m.URL("grokbot") || k.Host != "grokbot" || k.Owner != "Matt" || k.Operator == "" || len(k.Recipes) == 0 {
		t.Fatalf("kit = relay %q host %q owner %q, %d recipes", k.RelayURL, k.Host, k.Owner, len(k.Recipes))
	}
	if k.Agents[2].Kind != onboard.KindProxySandbox {
		t.Fatalf("--kind override ignored: %+v", k.Agents[2])
	}
	if _, err := run(t, onboardCmd(), "--kind", "muse"); err == nil || !strings.Contains(err.Error(), "name=kind") {
		t.Fatalf("malformed --kind: %v", err)
	}
	if _, err := run(t, onboardCmd(), "--section", "everything"); err == nil {
		t.Fatal("unknown section should fail")
	}
}

func TestOnboardSectionOperator(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	out, err := run(t, onboardCmd(), "--section", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "Operator prompt") || strings.Contains(out, "Agent: ") || strings.Contains(out, "Recipe: ") {
		t.Fatalf("--section operator printed more than the operator prompt:\n%s", out)
	}
	k := onboardKit(t, "--section", "operator")
	if k.Operator == "" || len(k.Agents) != 0 || len(k.Recipes) != 0 {
		t.Fatalf("--json --section operator kit: %+v", k)
	}
	all, err := run(t, onboardCmd())
	if err != nil || !strings.Contains(all, "Agent: muse") || !strings.Contains(all, "Recipe: ") {
		t.Fatalf("default output: %v\n%s", err, all)
	}
}

func TestOnboardWithoutRelayNamesRelayFlag(t *testing.T) {
	useConfig(t, client.Config{})
	_, err := run(t, onboardCmd())
	if err == nil || !strings.Contains(err.Error(), "--relay") {
		t.Fatalf("err = %v, want a message naming --relay", err)
	}
}

// --relay both prints in the kit and is where the roster is read from.
func TestOnboardRelayFlagOverridesConfig(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{})
	k := onboardKit(t, "--relay", m.URL("grokbot"))
	if k.RelayURL != m.URL("grokbot") || len(k.Agents) != 3 {
		t.Fatalf("kit = relay %q, agents %v", k.RelayURL, agentNames(k))
	}
}

// counter is a relay stand-in that counts every request it gets.
type counter struct {
	mu    sync.Mutex
	calls []string
}

func (c *counter) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.calls = append(c.calls, r.Method+" "+r.URL.Path)
		c.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func (c *counter) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.calls)
}

func TestOnboardOfflineMakesNoNetworkCall(t *testing.T) {
	useConfig(t, client.Config{})
	out, err := run(t, onboardCmd(), "--offline")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Operator prompt") || !strings.Contains(out, "Recipe: ") || strings.Contains(out, "Agent: ") {
		t.Fatalf("--offline output:\n%s", out)
	}

	c := &counter{}
	ts := httptest.NewServer(c.wrap(http.NotFoundHandler()))
	defer ts.Close()
	k := onboardKit(t, "--offline", "--relay", ts.URL)
	if n := c.seen(); len(n) != 0 {
		t.Fatalf("--offline called the relay: %v", n)
	}
	if k.RelayURL != ts.URL || len(k.Agents) != 0 || k.Operator == "" || len(k.Recipes) == 0 {
		t.Fatalf("offline kit: relay %q, agents %v", k.RelayURL, agentNames(k))
	}
}

// Onboarding only reads the roster: one GET /v1/agents per run, nothing else.
func TestOnboardOnlyReadsRoster(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	target, _ := url.Parse(m.URL("grokbot"))
	c := &counter{}
	ts := httptest.NewServer(c.wrap(httputil.NewSingleHostReverseProxy(target)))
	defer ts.Close()
	useConfig(t, client.Config{Relay: ts.URL, Agent: "grokbot"})
	onboardKit(t)
	if _, err := run(t, onboardCmd()); err != nil {
		t.Fatal(err)
	}
	if got := c.seen(); !slices.Equal(got, []string{"GET /v1/agents", "GET /v1/agents"}) {
		t.Fatalf("relay calls = %v, want only roster reads", got)
	}
}

// No caching: a roster change shows up on the next run.
func TestOnboardReflectsRosterChange(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	before, _ := run(t, onboardCmd(), "--section", "agents")
	m.JoinOnMachineOf(t, "muse", "hermes")
	after, _ := run(t, onboardCmd(), "--section", "agents")
	if strings.Contains(before, "Agent: hermes") || !strings.Contains(after, "Agent: hermes (kind hermes") {
		t.Fatalf("before:\n%s\nafter:\n%s", before, after)
	}
}

func TestOnboardFromUnjoinedMachines(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{Relay: m.URL("admin")})
	if got := agentNames(onboardKit(t)); len(got) != 3 {
		t.Fatalf("admin device agents = %v", got)
	}
	out, err := run(t, agentsCmd())
	if err != nil || !strings.Contains(out, "muse") {
		t.Fatalf("admin device roster: %v\n%s", err, out)
	}

	useConfig(t, client.Config{Relay: m.URL("stranger")})
	if _, err := run(t, onboardCmd()); err == nil || !strings.Contains(err.Error(), "not a joined agent") {
		t.Fatalf("unjoined non-admin onboard: %v", err)
	}
}

func TestInviteKindThenKindCommand(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{Relay: m.URL("admin")})
	out, err := run(t, inviteCmd(), "cx", "--kind", "hermes")
	if err != nil {
		t.Fatal(err)
	}
	code := strings.Fields(strings.SplitN(out, "): ", 2)[1])[0]
	if _, err := m.Client(t, "stranger").Join(context.Background(), code); err != nil {
		t.Fatalf("join with %q: %v", code, err)
	}
	if out, _ := run(t, agentsCmd()); !strings.Contains(out, "kind=hermes") {
		t.Fatalf("roster after invite --kind:\n%s", out)
	}
	if _, err := run(t, kindCmd(), "cx", "codex"); err != nil {
		t.Fatal(err)
	}
	if k := onboardKit(t); k.Agents[0].Name != "cx" || k.Agents[0].Kind != onboard.KindCodex {
		t.Fatalf("after tincan kind: %+v", k.Agents[0])
	}
	if _, err := run(t, kindCmd(), "cx", "codx"); err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Fatalf("bad kind: %v", err)
	}

	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	if _, err := run(t, kindCmd(), "cx", "openclaw"); err == nil || !client.IsStatus(err, http.StatusForbidden) {
		t.Fatalf("non-admin kind: %v", err)
	}
	if out, _ := run(t, agentsCmd()); !strings.Contains(out, "kind=codex") {
		t.Fatalf("non-admin changed the kind:\n%s", out)
	}
}
