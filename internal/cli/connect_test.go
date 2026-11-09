package cli

import (
	"regexp"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/gateway"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

const gatewayBase = "https://tincan-gateway.example.ts.net"

// gatewayMesh is a test relay with the MCP gateway's connector enabled, as a
// relay started with --chatgpt-gateway has.
func gatewayMesh(t *testing.T) *testrelay.Mesh {
	t.Helper()
	m := testrelay.New(t, relay.Config{})
	oauth, err := gateway.NewOAuth(m.Store.DB(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m.Server.SetConnector(gateway.Connector{Dir: m.Dir, OAuth: oauth, Base: gatewayBase})
	useConfig(t, client.Config{})
	return m
}

var loginCode = regexp.MustCompile(`(?m)^     [A-Z0-9]{4}-[A-Z0-9]{4}$`)

func connectOut(t *testing.T, m *testrelay.Mesh, name string) string {
	t.Helper()
	out, err := run(t, connectCmd(), name, "--relay", m.URL("admin"))
	if err != nil {
		t.Fatalf("connect %s: %v\n%s", name, err, out)
	}
	if !strings.Contains(out, "\n     "+gatewayBase+"/mcp\n") || !loginCode.MatchString(out) {
		t.Fatalf("connect %s lacks the URL or the code:\n%s", name, out)
	}
	return out
}

// tincan connect sesame prints Sesame's custom-app steps around the gateway
// URL and the one-time code.
func TestConnectSesamePrintsSesameSteps(t *testing.T) {
	m := gatewayMesh(t)
	out := connectOut(t, m, "sesame")
	for _, want := range []string{`"sesame" is ready to connect.`, "In Sesame", "Apps", "Add custom app", "Continue to authorization", "one-time code (valid 10 minutes)"} {
		if !strings.Contains(out, want) {
			t.Errorf("connect sesame missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ChatGPT") {
		t.Errorf("connect sesame names ChatGPT:\n%s", out)
	}
}

// tincan connect chatgpt keeps today's ChatGPT steps word for word.
func TestConnectChatGPTKeepsChatGPTSteps(t *testing.T) {
	m := gatewayMesh(t)
	out := connectOut(t, m, "chatgpt")
	code := strings.TrimSpace(loginCode.FindString(out))
	want := `"chatgpt" is ready to connect.
1. In ChatGPT: Settings > Apps > Advanced settings, turn on Developer mode.
2. Create a connector with this URL:
     ` + gatewayBase + `/mcp
3. When ChatGPT opens the login page, enter this one-time code (valid 10 minutes):
     ` + code + "\n"
	if out != want {
		t.Errorf("connect chatgpt =\n%s\nwant\n%s", out, want)
	}
}

// Any other name gets steps that name no product, whatever kind it has.
func TestConnectOtherNamePrintsNeutralSteps(t *testing.T) {
	m := gatewayMesh(t)
	out := connectOut(t, m, "miles")
	if _, err := run(t, kindCmd(), "miles", "sesame", "--relay", m.URL("admin")); err != nil {
		t.Fatal(err)
	}
	again := connectOut(t, m, "miles")
	for _, o := range []string{out, again} {
		for _, bad := range []string{"ChatGPT", "Sesame"} {
			if strings.Contains(o, bad) {
				t.Errorf("connect miles names %s:\n%s", bad, o)
			}
		}
		for _, want := range []string{`"miles" is ready to connect.`, "MCP", "login page", "one-time code (valid 10 minutes)"} {
			if !strings.Contains(o, want) {
				t.Errorf("connect miles missing %q:\n%s", want, o)
			}
		}
	}
}

// Connecting sesame stores the sesame kind, so tincan agents shows it with no
// tincan kind step; a kind the owner set beforehand is kept on reconnect, and
// a personal name gets no kind.
func TestConnectStoresRuntimeKind(t *testing.T) {
	m := gatewayMesh(t)
	connectOut(t, m, "sesame")
	connectOut(t, m, "miles")
	agents, err := run(t, agentsCmd(), "--relay", m.URL("admin"))
	if err != nil {
		t.Fatal(err)
	}
	if line := agentLine(agents, "sesame"); !strings.Contains(line, "kind=sesame") {
		t.Errorf("sesame after connect: %q\n%s", line, agents)
	}
	if line := agentLine(agents, "miles"); strings.Contains(line, "kind=") {
		t.Errorf("miles after connect has a kind: %q", line)
	}

	if _, err := run(t, kindCmd(), "sesame", "generic", "--relay", m.URL("admin")); err != nil {
		t.Fatal(err)
	}
	connectOut(t, m, "sesame")
	agents, _ = run(t, agentsCmd(), "--relay", m.URL("admin"))
	if line := agentLine(agents, "sesame"); !strings.Contains(line, "kind=generic") {
		t.Errorf("reconnect replaced the owner's kind: %q", line)
	}
}

func agentLine(agents, name string) string {
	for l := range strings.SplitSeq(agents, "\n") {
		if strings.HasPrefix(l, name+" ") {
			return l
		}
	}
	return ""
}

// With the gateway off, connect names the MCP gateway and the flag that
// starts it without implying the connector must be ChatGPT.
func TestConnectWithoutGatewayNamesTheFlag(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{})
	_, err := run(t, connectCmd(), "sesame", "--relay", m.URL("admin"))
	if err == nil {
		t.Fatal("connect without a gateway succeeded")
	}
	msg := err.Error()
	if !strings.Contains(msg, "MCP gateway is not enabled") || !strings.Contains(msg, "--chatgpt-gateway") || strings.Contains(msg, "ChatGPT") {
		t.Errorf("error = %q", msg)
	}
}

// Connecting a name a real tailnet agent holds is refused and names
// --replace; the agent keeps its node, and a product-scoped name connects.
// With --replace the name moves to the gateway and keeps its stored kind.
func TestConnectRefusesRealAgentName(t *testing.T) {
	m := gatewayMesh(t)
	m.JoinOnMachineOf(t, "instinct", "claude-code")
	if _, err := run(t, kindCmd(), "claude-code", "claude-code", "--relay", m.URL("admin")); err != nil {
		t.Fatal(err)
	}
	_, err := run(t, connectCmd(), "claude-code", "--relay", m.URL("admin"))
	if err == nil || !strings.Contains(err.Error(), "--replace") {
		t.Fatalf("connect claude-code: %v", err)
	}
	if a, _, _ := m.Dir.Agent(t.Context(), "claude-code"); a.NodeID != "nINST" {
		t.Fatalf("refused connect changed the node: %+v", a)
	}
	connectOut(t, m, "claude-ai")

	if _, err := run(t, connectCmd(), "claude-code", "--replace", "--relay", m.URL("admin")); err != nil {
		t.Fatalf("connect --replace: %v", err)
	}
	a, _, _ := m.Dir.Agent(t.Context(), "claude-code")
	if a.NodeID != "virtual:claude-code" || a.Kind != "claude-code" {
		t.Fatalf("after --replace: %+v", a)
	}
}

// Reconnecting a name that is already on the gateway needs no --replace.
func TestReconnectVirtualNameNeedsNoReplace(t *testing.T) {
	m := gatewayMesh(t)
	connectOut(t, m, "claude-ai")
	connectOut(t, m, "claude-ai")
}

// An older CLI sends only the name. It gets the refusal, not a takeover.
func TestConnectWithoutReplaceFieldIsRefused(t *testing.T) {
	m := gatewayMesh(t)
	err := m.Client(t, "admin").Raw(t.Context(), "POST", "/v1/admin/connect", map[string]string{"name": "muse"}, nil)
	if err == nil || !strings.Contains(err.Error(), "--replace") {
		t.Fatalf("connect muse without replace: %v", err)
	}
	if a, _, _ := m.Dir.Agent(t.Context(), "muse"); a.NodeID != "nMUSE" {
		t.Fatalf("muse lost its node: %+v", a)
	}
}

// tincan connect --guest prints the card to hand over and the one-time code,
// and warns about a listed teammate no card serves.
func TestConnectGuestPrintsCards(t *testing.T) {
	m := gatewayMesh(t)
	out, err := run(t, connectCmd(), "pal", "--guest", "--ask", "muse,instinct", "--for", "3d", "--relay", m.URL("admin"))
	if err != nil {
		t.Fatalf("connect --guest: %v\n%s", err, out)
	}
	for _, want := range []string{gatewayBase + "/a2a/muse/.well-known/agent-card.json", gatewayBase + "/a2a/instinct/.well-known/agent-card.json", "muse, instinct", "tincan held", "a2a.json"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if !loginCode.MatchString(out) {
		t.Errorf("no code:\n%s", out)
	}
	a, ok, err := m.Dir.Agent(t.Context(), "pal")
	if err != nil || !ok || !a.Guest.Set() || len(a.Guest.Asks) != 2 || a.Guest.NoHold {
		t.Fatalf("pal = %+v %v %v", a, ok, err)
	}
}

func TestConnectGuestFlags(t *testing.T) {
	m := gatewayMesh(t)
	for name, args := range map[string][]string{
		"no ask":            {"pal", "--guest", "--for", "3d"},
		"no term":           {"pal", "--guest", "--ask", "muse"},
		"ask alone":         {"pal", "--ask", "muse"},
		"for alone":         {"pal", "--for", "3d"},
		"too long":          {"pal", "--guest", "--ask", "muse", "--for", "31d"},
		"guest+replace":     {"pal", "--guest", "--ask", "muse", "--for", "3d", "--replace"},
		"unknown ask":       {"pal", "--guest", "--ask", "nobody", "--for", "3d"},
		"a teammate's name": {"muse", "--guest", "--ask", "instinct", "--for", "3d"},
	} {
		if out, err := run(t, connectCmd(), append(args, "--relay", m.URL("admin"))...); err == nil {
			t.Errorf("%s: accepted:\n%s", name, out)
		}
	}
	if _, ok, _ := m.Dir.Agent(t.Context(), "pal"); ok {
		t.Fatal("a refused connect left pal behind")
	}
}

// A gateway guest's name stays a guest's: connecting it again in either tier
// is refused until tincan remove.
func TestConnectGuestNameKeepsItsTier(t *testing.T) {
	m := gatewayMesh(t)
	guest := []string{"pal", "--guest", "--ask", "muse", "--for", "3d", "--relay", m.URL("admin")}
	if out, err := run(t, connectCmd(), guest...); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := run(t, connectCmd(), guest...); err == nil {
		t.Fatalf("second connect --guest accepted:\n%s", out)
	}
	if out, err := run(t, connectCmd(), "pal", "--relay", m.URL("admin")); err == nil {
		t.Fatalf("plain connect over a guest accepted:\n%s", out)
	}
}
