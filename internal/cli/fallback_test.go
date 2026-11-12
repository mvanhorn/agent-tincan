package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/mcpserver"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// tincan fallback set and clear change the relay's fallback for an agent,
// tincan agents and list_agents both show it, and a teammate that is not an
// admin device is refused.
func TestFallbackCommand(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	ctx := context.Background()
	useConfig(t, client.Config{Relay: m.URL("admin")})
	out, err := run(t, fallbackCmd(), "set", "muse", "instinct")
	if err != nil || out != "Asks \"muse\" leaves untaken now move to \"instinct\".\n" {
		t.Fatalf("set: %q, %v", out, err)
	}
	if out, _ := run(t, agentsCmd()); !strings.Contains(out, "fallback=instinct") {
		t.Fatalf("roster after set:\n%s", out)
	}

	srvT, cliT := mcp.NewInMemoryTransports()
	ss, err := mcpserver.New(m.Client(t, "grokbot"), "test").Connect(ctx, srvT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, cliT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_agents"})
	if err != nil {
		t.Fatal(err)
	}
	if text := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, ", fallback=instinct") {
		t.Fatalf("list_agents:\n%s", text)
	}

	if _, err := run(t, fallbackCmd(), "set", "muse", "muse"); err == nil {
		t.Fatal("an agent was set as its own fallback")
	}
	out, err = run(t, fallbackCmd(), "clear", "muse")
	if err != nil || out != "Asks \"muse\" leaves untaken are no longer moved.\n" {
		t.Fatalf("clear: %q, %v", out, err)
	}
	if out, _ := run(t, agentsCmd()); strings.Contains(out, "fallback=") {
		t.Fatalf("roster after clear:\n%s", out)
	}

	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	if _, err := run(t, fallbackCmd(), "set", "muse", "instinct"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("teammate set: %v", err)
	}
}
