package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

func TestAskScheduleCommands(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	out, err := run(t, askCmd(), "muse", "weekly", "report", "--at", "2h", "--every", "1h")
	if err != nil || !strings.Contains(out, "Scheduled ask #") {
		t.Fatalf("ask = %q, %v", out, err)
	}
	if inbox, err := m.Client(t, "muse").Poll(t.Context(), 0); err != nil || len(inbox.Requests) != 0 {
		t.Fatalf("sent at once: %+v, %v", inbox, err)
	}
	out, err = run(t, schedulesCmd())
	if err != nil || !strings.Contains(out, "grokbot to muse") || !strings.Contains(out, "weekly report") {
		t.Fatalf("schedules = %q, %v", out, err)
	}
	all, err := client.Schedules(t.Context(), m.Client(t, "grokbot"), false)
	if err != nil || len(all) != 1 {
		t.Fatalf("list = %+v, %v", all, err)
	}
	out, err = run(t, schedulesCmd(), "--json")
	var got []envelope.Schedule
	if err != nil || json.Unmarshal([]byte(out), &got) != nil || len(got) != 1 || got[0].To != "muse" {
		t.Fatalf("--json = %q, %v", out, err)
	}
	if out, err := run(t, unscheduleCmd(), "not-a-number"); err == nil {
		t.Fatalf("bad id = %q", out)
	}
	out, err = run(t, unscheduleCmd(), fmt.Sprint(all[0].ID))
	if err != nil || !strings.Contains(out, "Cancelled") {
		t.Fatalf("unschedule = %q, %v", out, err)
	}
}

// The relay host usually has no saved config, and the owner stops what
// tincan schedules --all shows with no flags, whoever owns it.
func TestUnscheduleOnRelayHostWithoutFlags(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{})
	serveAdminSocket(t, m)
	sch, err := client.ScheduleAsk(t.Context(), m.Client(t, "grokbot"), client.ScheduledAsk{To: "muse", Body: "x", Every: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, schedulesCmd(), "--all"); err != nil || !strings.Contains(out, "grokbot to muse") {
		t.Fatalf("schedules --all = %q, %v", out, err)
	}
	if out, err := run(t, unscheduleCmd(), fmt.Sprint(sch.ID)); err != nil || !strings.Contains(out, "Cancelled") {
		t.Fatalf("unschedule = %q, %v", out, err)
	}
	if list, err := client.Schedules(t.Context(), m.Client(t, "grokbot"), false); err != nil || len(list) != 0 {
		t.Fatalf("left = %+v, %v", list, err)
	}
}

func TestAskScheduleRefusals(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	for name, args := range map[string][]string{
		"several targets": {"muse,instinct", "x", "--every", "1h"},
		"notify":          {"muse", "x", "--every", "1h", "--notify"},
		"parent":          {"muse", "x", "--every", "1h", "--parent", "abc"},
		"attach":          {"muse", "x", "--every", "1h", "--attach", "/tmp/x"},
		"project, every":  {"muse", "x", "--every", "1h", "--project", "tincan"},
		"project, at":     {"muse", "x", "--at", "2h", "--project", "tincan"},
		"then, every":     {"muse", "x", "y", "--every", "1h", "--then", "instinct"},
		"then, at":        {"muse", "x", "y", "--at", "2h", "--then", "instinct"},
		"too often":       {"muse", "x", "--every", "5m"},
		"empty at":        {"muse", "x", "--at", ""},
		"empty every":     {"muse", "x", "--every", ""},
		"past":            {"muse", "x", "--at", "2001-01-01T00:00:00Z"},
		"bad at":          {"muse", "x", "--at", "tomorrow"},
	} {
		t.Run(name, func(t *testing.T) {
			if out, err := run(t, askCmd(), args...); err == nil {
				t.Fatalf("ask %v = %q", args, out)
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
