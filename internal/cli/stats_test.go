package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// statsMesh is a mesh where muse has answered one ask after 90 seconds and
// failed another.
func statsMesh(t *testing.T) *testrelay.Mesh {
	t.Helper()
	m := testrelay.New(t, relay.Config{})
	sender := m.Client(t, "grokbot")
	for _, status := range []envelope.Status{envelope.StatusAnswered, envelope.StatusFailed} {
		req, err := sender.Send(t.Context(), "muse", "dummy question", envelope.KindAsk, "", false)
		if err != nil {
			t.Fatal(err)
		}
		m.Store.SetClock(func() time.Time { return time.Now().Add(90 * time.Second) })
		if _, err := m.Store.Reply(t.Context(), req.ID, "muse", envelope.Reply{Status: status, Body: "dummy reply"}); err != nil {
			t.Fatal(err)
		}
	}
	useConfig(t, client.Config{})
	return m
}

func TestStatsJSON(t *testing.T) {
	m := statsMesh(t)
	stdout, _, err := runSplit(t, Root(), "stats", "--json", "--relay", m.URL("instinct"))
	if err != nil {
		t.Fatalf("stats --json: %v", err)
	}
	var got client.StatsReport
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if len(got.Agents) != 3 {
		t.Fatalf("agents = %+v, want all three", got.Agents)
	}
	for _, a := range got.Agents {
		if a.Agent == "muse" && (a.Answered != 1 || a.Failed != 1 || a.Expired != 0) {
			t.Fatalf("muse = %+v", a)
		}
	}
	if strings.Contains(stdout, "dummy") {
		t.Fatalf("stats carry text: %s", stdout)
	}
}

func TestStatsText(t *testing.T) {
	m := statsMesh(t)
	stdout, _, err := runSplit(t, Root(), "stats", "--relay", m.URL("instinct"))
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if !strings.Contains(stdout, "AGENT") || !strings.Contains(stdout, "MEDIAN") {
		t.Fatalf("no header: %s", stdout)
	}
	for _, want := range [][]string{{"muse", "1", "1", "0", "1m"}, {"instinct", "0", "0", "0", "-"}} {
		found := false
		for line := range strings.SplitSeq(stdout, "\n") {
			if slices.Equal(strings.Fields(line), want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("no line %v in:\n%s", want, stdout)
		}
	}
}

func TestStatsOneAgentAndWindow(t *testing.T) {
	m := statsMesh(t)
	stdout, _, err := runSplit(t, Root(), "stats", "muse", "--since", "2d", "--json", "--relay", m.URL("instinct"))
	if err != nil {
		t.Fatalf("stats muse: %v", err)
	}
	var got client.StatsReport
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Agents) != 1 || got.Agents[0].Agent != "muse" || got.Agents[0].Answered != 1 {
		t.Fatalf("agents = %+v", got.Agents)
	}
	if time.Since(got.Since) < 47*time.Hour || time.Since(got.Since) > 49*time.Hour {
		t.Fatalf("since = %v, want two days back", got.Since)
	}
	if _, _, err := runSplit(t, Root(), "stats", "nobody", "--relay", m.URL("instinct")); err == nil || !strings.Contains(err.Error(), "nobody") {
		t.Fatalf("unknown agent err = %v", err)
	}
	if _, _, err := runSplit(t, Root(), "stats", "--since", "soon", "--relay", m.URL("instinct")); err == nil || !strings.Contains(err.Error(), "soon") {
		t.Fatalf("bad window err = %v", err)
	}
}

// A relay that predates the route answers Go's plain 404, which tincan turns
// into an upgrade hint.
func TestStatsOlderRelay(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	useConfig(t, client.Config{})
	_, _, err := runSplit(t, Root(), "stats", "--relay", srv.URL)
	if err == nil || !strings.Contains(err.Error(), "upgrade the relay") {
		t.Fatalf("err = %v, want an upgrade hint", err)
	}
}

// tincan agents shows the record in the same place as list_agents: after the
// roster's facts and before the quoted fields that end the line.
func TestAgentsShowsRecord(t *testing.T) {
	m := statsMesh(t)
	if _, err := m.Client(t, "admin").SetGoodAt(t.Context(), "muse", "phone calls"); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runSplit(t, Root(), "agents", "--relay", m.URL("instinct"))
	if err != nil {
		t.Fatalf("agents: %v", err)
	}
	if want := ` answered=1/2 median=1m good_at="phone calls"`; !strings.Contains(stdout, want) {
		t.Fatalf("agents = %q, want %q", stdout, want)
	}
}
