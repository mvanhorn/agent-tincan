package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// tincan agents --json prints the relay's roster document, node fields and
// all when the caller is an admin device.
func TestAgentsJSONPrintsRoster(t *testing.T) {
	m := testrelay.New(t, relay.Config{Version: "v9.9.9"})
	useConfig(t, client.Config{})
	stdout, _, err := runSplit(t, Root(), "agents", "--json", "--relay", m.URL("admin"))
	if err != nil {
		t.Fatalf("agents --json: %v", err)
	}
	var ro client.Roster
	if err := json.Unmarshal([]byte(stdout), &ro); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if ro.RelayVersion != "v9.9.9" || len(ro.Agents) != 3 {
		t.Fatalf("roster = %+v", ro)
	}
	for _, a := range ro.Agents {
		if a.Name == "grokbot" && (a.NodeID != "nGROK" || a.NodeName != "grok-bot" || a.NodeUser != testrelay.Login) {
			t.Errorf("grokbot = %+v, want its node", a)
		}
	}
}

// tincan agents shows a session's shared title with the same working_on
// field, in the same place before good_at, as list_agents does.
func TestAgentsShowsWorkingOn(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	if _, err := m.Client(t, "admin").SetGoodAt(t.Context(), "muse", "phone calls"); err != nil {
		t.Fatal(err)
	}
	muse := m.Client(t, "muse")
	muse.SetSession("window-a")
	if err := muse.ReportWorkingOn(t.Context(), "Fix flaky relay test, wake=none"); err != nil {
		t.Fatal(err)
	}
	useConfig(t, client.Config{})
	stdout, _, err := runSplit(t, Root(), "agents", "--relay", m.URL("grokbot"))
	if err != nil {
		t.Fatalf("agents: %v", err)
	}
	var line string
	for l := range strings.SplitSeq(stdout, "\n") {
		if strings.HasPrefix(l, "muse ") {
			line = l
		}
	}
	if want := ` working_on="Fix flaky relay test, wake=none" good_at="phone calls"`; !strings.HasSuffix(line, want) {
		t.Fatalf("agents muse = %q, want it to end %q", line, want)
	}
}

// tincan agents ends the caller's own line with you, after good_at, and
// leaves every other line alone. A roster from an older relay, with no self
// field, renders as it always did.
func TestAgentsMarksCaller(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	if _, err := m.Client(t, "admin").SetGoodAt(t.Context(), "muse", "phone calls"); err != nil {
		t.Fatal(err)
	}
	useConfig(t, client.Config{})
	stdout, _, err := runSplit(t, Root(), "agents", "--relay", m.URL("muse"))
	if err != nil {
		t.Fatalf("agents: %v", err)
	}
	var mine, others int
	for l := range strings.SplitSeq(strings.TrimSpace(stdout), "\n") {
		switch {
		case strings.HasPrefix(l, "muse ") && strings.HasSuffix(l, ` good_at="phone calls" you`):
			mine++
		case strings.HasSuffix(l, " you"):
			others++
		}
	}
	if mine != 1 || others != 0 {
		t.Fatalf("agents = %q, want only muse's line to end in you", stdout)
	}

	var ro client.Roster
	if err := json.Unmarshal([]byte(`{"agents":[{"name":"muse","online":false,"wake":"none","good_at":"phone calls"}]}`), &ro); err != nil {
		t.Fatal(err)
	}
	if got := formatRoster(ro, time.Now()); strings.Contains(got, "you") {
		t.Fatalf("older roster rendered %q, want no mark", got)
	}
}
