package mcpserver_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/mcpserver"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// ask with at or every creates a schedule rather than sending; the two
// schedule tools list and cancel it.
func TestScheduleToolsOverMCP(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	cs := session(t, m, "grokbot")
	out := call(t, cs, "ask", map[string]any{"to": "muse", "message": "weekly report", "every": "1h"})
	if !strings.Contains(out, "Scheduled ask #") || !strings.Contains(out, "every 1h0m0s") {
		t.Fatalf("ask = %q", out)
	}
	if inbox, err := m.Client(t, "muse").Poll(t.Context(), 0); err != nil || len(inbox.Requests) != 0 {
		t.Fatalf("sent at once: %+v, %v", inbox, err)
	}
	list := call(t, cs, "list_schedules", nil)
	if !strings.Contains(list, "grokbot to muse") || !strings.Contains(list, "weekly report") {
		t.Fatalf("list = %q", list)
	}
	schedules, err := client.Schedules(t.Context(), m.Client(t, "grokbot"), false)
	if err != nil || len(schedules) != 1 {
		t.Fatalf("schedules = %+v, %v", schedules, err)
	}
	if out := call(t, cs, "cancel_schedule", map[string]any{"schedule_id": schedules[0].ID}); !strings.Contains(out, "Cancelled") {
		t.Fatalf("cancel = %q", out)
	}
	if list := call(t, cs, "list_schedules", nil); !strings.Contains(list, "No scheduled asks") {
		t.Fatalf("list after cancel = %q", list)
	}
}

// Everything a scheduled ask cannot carry is refused, not dropped.
func TestScheduledAskRefusesWhatItCannotCarry(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	cs := session(t, m, "grokbot")
	for name, extra := range map[string]map[string]any{
		"also":    {"also": []string{"instinct"}},
		"notify":  {"notify": true},
		"parent":  {"parent_id": "abc"},
		"attach":  {"attach": []string{"/tmp/x"}},
		"project": {"project": "tincan"},
		"then":    {"then": map[string]string{"to": "instinct", "message": "y"}},
	} {
		t.Run(name, func(t *testing.T) {
			args := map[string]any{"to": "muse", "message": "x", "every": "1h"}
			maps.Copy(args, extra)
			if out := call(t, cs, "ask", args); !strings.HasPrefix(out, "ERROR:") {
				t.Fatalf("ask = %q", out)
			}
		})
	}
	if inbox, err := m.Client(t, "muse").Poll(t.Context(), 0); err != nil || len(inbox.Requests) != 0 {
		t.Fatalf("sent = %+v, %v", inbox, err)
	}
	if list, err := client.Schedules(t.Context(), m.Client(t, "grokbot"), false); err != nil || len(list) != 0 {
		t.Fatalf("scheduled = %+v, %v", list, err)
	}
}

// An at or every that is there but empty still asked for a schedule, so the
// ask is refused rather than sent now.
func TestScheduledAskEmptyTimesNeverSend(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	cs := session(t, m, "grokbot")
	for name, times := range map[string]map[string]any{
		"empty at":    {"at": ""},
		"empty every": {"every": ""},
		"both empty":  {"at": "", "every": ""},
		"null at":     {"at": nil},
	} {
		t.Run(name, func(t *testing.T) {
			args := map[string]any{"to": "muse", "message": "x", "wait_seconds": 1}
			maps.Copy(args, times)
			if out := call(t, cs, "ask", args); !strings.HasPrefix(out, "ERROR:") {
				t.Fatalf("ask = %q", out)
			}
		})
	}
	if inbox, err := m.Client(t, "muse").Poll(t.Context(), 0); err != nil || len(inbox.Requests) != 0 {
		t.Fatalf("sent = %+v, %v", inbox, err)
	}
}

// An old relay answers capabilities without schedules. The ask tool must
// refuse there, and never fall through to an ordinary send.
func TestScheduledAskRefusedOnOldRelay(t *testing.T) {
	sends := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			_ = json.NewEncoder(w).Encode(client.Capabilities{Rooms: true})
		case "/v1/send":
			sends++
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"r1","to":"muse","body":"x"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	srvT, cliT := mcp.NewInMemoryTransports()
	ss, err := mcpserver.New(client.NewRelayHTTP(srv.URL, srv.Client()), "test").Connect(t.Context(), srvT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil).Connect(t.Context(), cliT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	for _, at := range []string{time.Now().Add(time.Hour).Format(time.RFC3339), ""} {
		out := call(t, cs, "ask", map[string]any{"to": "muse", "message": "x", "at": at, "wait_seconds": 1})
		if !strings.HasPrefix(out, "ERROR:") || !strings.Contains(out, "upgrade the relay") || sends != 0 {
			t.Fatalf("ask at %q = %q, sends %d", at, out, sends)
		}
	}
}
