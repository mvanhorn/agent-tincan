package mcpserver_test

import (
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
	"github.com/mvanhorn/agent-tincan/internal/wake"
)

func scheduleMesh(t *testing.T) *testrelay.Mesh {
	t.Helper()
	m := testrelay.New(t, relay.Config{MaxWait: 200 * time.Millisecond})
	m.Server.SetWakeNamer(wake.New(wake.Config{
		"muse":    {Method: wake.Schedule, Every: "5m"},
		"grokbot": {Method: wake.Webhook, URL: "http://127.0.0.1:1/hook"},
	}, nil, wake.Options{}))
	return m
}

// AE1 over MCP: list_agents shows a schedule agent's interval; an agent
// that joined moments ago is not overdue.
func TestListAgentsSchedule(t *testing.T) {
	m := scheduleMesh(t)
	out := call(t, session(t, m, "instinct"), "list_agents", nil)
	var muse string
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, "muse:") {
			muse = line
		}
	}
	if !strings.Contains(muse, ", wake=schedule (every 5m), ") || strings.Contains(muse, "overdue") {
		t.Fatalf("list_agents muse = %q in %q", muse, out)
	}
	if !strings.Contains(out, "grokbot: offline, status=offline, wake=webhook, ") {
		t.Fatalf("list_agents = %q", out)
	}
}

// AE2 over MCP: ask with its default wait against a schedule target says
// how often it checks and when to expect a reply; against a webhook target
// the text is unchanged.
func TestAskScheduleTargetOverMCP(t *testing.T) {
	m := scheduleMesh(t)
	inst := session(t, m, "instinct")
	out := call(t, inst, "ask", map[string]any{"to": "muse", "message": "reschedule the dentist"})
	if !strings.HasPrefix(out, "muse checks its inbox every 5m; expect a reply within about 10m.\nNo reply yet from muse.") {
		t.Fatalf("ask muse = %q", out)
	}
	out = call(t, inst, "ask", map[string]any{"to": "grokbot", "message": "status"})
	if !strings.HasPrefix(out, "No reply yet from grokbot.") {
		t.Fatalf("ask grokbot = %q", out)
	}
}

// The list_agents description names every wake method, schedule included.
func TestListAgentsDescriptionNamesSchedule(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	res, err := session(t, m, "instinct").ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.Name == "list_agents" {
			if !strings.Contains(tool.Description, "schedule") {
				t.Fatalf("description = %q", tool.Description)
			}
			return
		}
	}
	t.Fatal("no list_agents tool")
}
