package client_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

// AgentInfo embeds Target with its fields at the top level, so the wake
// facts a send carries must not share a JSON name with the roster's own
// wake and online fields. A round trip keeps all four apart.
func TestAgentInfoJSONKeepsWakeAndOnline(t *testing.T) {
	in := client.AgentInfo{Name: "muse", Wake: "channel", Online: true, WakeVia: "socket", Listening: false}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out client.AgentInfo
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Wake != "channel" || !out.Online || out.WakeVia != "socket" || out.Listening {
		t.Fatalf("round trip of %s = %+v", b, out)
	}
	var fields map[string]any
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["wake"] != "channel" || fields["online"] != true || fields["wake_via"] != "socket" {
		t.Fatalf("fields = %v", fields)
	}
}

// The status fields sit beside the embedded Target without colliding, and
// an entry without them (an older relay's) carries none of their keys.
func TestAgentInfoJSONStatusFields(t *testing.T) {
	since := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	in := client.AgentInfo{Name: "codex", Status: client.StatusBusy, BusyFor: "claude-code", BusySince: since, Unanswered: true}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out client.AgentInfo
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != client.StatusBusy || out.BusyFor != "claude-code" || !out.BusySince.Equal(since) || !out.Unanswered {
		t.Fatalf("round trip of %s = %+v", b, out)
	}
	b, err = json.Marshal(client.AgentInfo{Name: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"status"`, `"status_reason"`, `"busy_for"`, `"busy_since"`} {
		if strings.Contains(string(b), key) {
			t.Fatalf("empty status wrote %s: %s", key, b)
		}
	}
}
