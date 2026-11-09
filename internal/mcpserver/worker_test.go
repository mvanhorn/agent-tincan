package mcpserver_test

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/mcpserver"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// A window's server works on its one request: check_inbox leaves the
// agent's other asks queued, and claim refuses them.
func TestWorkerRequestServerTakesNoOtherRequest(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	asker := m.Client(t, "grokbot")
	mine, err := asker.Send(t.Context(), "muse", "the window's task", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := asker.Send(t.Context(), "muse", "another ask", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	srvT, cliT := mcp.NewInMemoryTransports()
	srv := mcpserver.New(m.Client(t, "muse"), "test", mcpserver.WorkerRequest(mine.ID))
	ss, err := srv.Connect(t.Context(), srvT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil).Connect(t.Context(), cliT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })

	if got := call(t, cs, "check_inbox", map[string]any{}); !strings.Contains(got, "takes no new requests") || strings.Contains(got, "another ask") {
		t.Errorf("check_inbox = %q", got)
	}
	if got := call(t, cs, "claim", map[string]any{"request_id": other.ID}); !strings.HasPrefix(got, "ERROR:") {
		t.Errorf("claim of another request = %q, want an error", got)
	}
	res, err := asker.Get(t.Context(), other.ID, 0)
	if err != nil || res.Status != envelope.StatusQueued {
		t.Errorf("the other ask is %s (%v), want it still queued", res.Status, err)
	}
	if got := call(t, cs, "claim", map[string]any{"request_id": mine.ID}); strings.HasPrefix(got, "ERROR:") {
		t.Errorf("claim of its own request = %q", got)
	}
}
