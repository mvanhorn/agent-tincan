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
