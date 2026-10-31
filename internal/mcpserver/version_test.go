package mcpserver_test

import (
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// list_agents shows the build each agent last called with. The calling
// agent's own call carries it; an agent that has not called shows none.
func TestListAgentsShowsVersions(t *testing.T) {
	t.Cleanup(func() { client.Version = "" })
	client.Version = "0.5.2"
	m := testrelay.New(t, relay.Config{Version: "0.6.0"})
	out := call(t, session(t, m, "grokbot"), "list_agents", nil)
	if !strings.Contains(out, "grokbot: offline, status=offline, wake=none, last seen just now, version=0.5.2, you\n") {
		t.Fatalf("list_agents lacks grokbot's version: %q", out)
	}
	if !strings.Contains(out, "muse: offline, status=offline, wake=none, never seen\n") {
		t.Fatalf("list_agents gave muse a version it never reported: %q", out)
	}
}
