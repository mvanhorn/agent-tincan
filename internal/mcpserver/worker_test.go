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

// check_inbox points out a request that starts with a teammate's shortcut.
func TestCheckInboxPointsOutAShortcut(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	admin := m.Client(t, "admin")
	if _, err := admin.SetGoodAt(t.Context(), "grokbot", "cc <task>: opens a new Claude Code window"); err != nil {
		t.Skipf("set good-at: %v", err)
	}
	if _, err := m.Client(t, "instinct").Send(t.Context(), "muse", "cc /ce-plan make a donkey plan", envelope.KindAsk, "", false); err != nil {
		t.Fatal(err)
	}
	got := call(t, session(t, m, "muse"), "check_inbox", map[string]any{})
	if !strings.Contains(got, "ask grokbot") || !strings.Contains(got, `"/ce-plan make a donkey plan"`) {
		t.Errorf("check_inbox = %q", got)
	}
}
