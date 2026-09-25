package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/identity"
)

// whoamiAs calls whoami from addr as a client running the given build ("" for
// a client that predates the version header).
func whoamiAs(t *testing.T, h *harness, addr, version string) map[string]any {
	t.Helper()
	req := httptest.NewRequest("GET", "/v1/whoami", nil)
	req.RemoteAddr = addr
	if version != "" {
		req.Header.Set(client.VersionHeader, version)
	}
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("whoami from %s: %d %s", addr, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The roster shows the build each agent last called with, taken from the
// client's version header, and the relay's own build beside it. A client
// that sends no header (one that predates it) leaves the agent's entry as it
// was, and a header that does not look like a build name is ignored.
func TestRosterShowsAgentAndRelayVersions(t *testing.T) {
	h := newHarness(t, Config{Version: "0.6.0"})
	h.send(grokAddr, "muse", "hi")
	if a := agentInfo(t, h, macAddr, "grokbot"); a.Version != "" {
		t.Fatalf("version with no header = %q", a.Version)
	}
	whoamiAs(t, h, grokAddr, "0.5.2")
	if a := agentInfo(t, h, macAddr, "grokbot"); a.Version != "0.5.2" {
		t.Fatalf("version = %q, want 0.5.2", a.Version)
	}
	for _, junk := range []string{"0.5.2 evil", "<script>", strings.Repeat("9", 65), " \t"} {
		whoamiAs(t, h, grokAddr, junk)
		if a := agentInfo(t, h, macAddr, "grokbot"); a.Version != "0.5.2" {
			t.Fatalf("after header %q version = %q, want 0.5.2 kept", junk, a.Version)
		}
	}
	h.send(grokAddr, "muse", "again")
	if a := agentInfo(t, h, macAddr, "grokbot"); a.Version != "0.5.2" {
		t.Fatalf("a later call without the header cleared the version: %q", a.Version)
	}
	whoamiAs(t, h, grokAddr, "0.5.2-3-gabcdef-dirty")
	if a := agentInfo(t, h, macAddr, "grokbot"); a.Version != "0.5.2-3-gabcdef-dirty" {
		t.Fatalf("a dev build name was not recorded: %q", a.Version)
	}
	if a := agentInfo(t, h, macAddr, "muse"); a.Version != "" {
		t.Fatalf("muse never called but has version %q", a.Version)
	}

	var roster struct {
		RelayVersion string `json:"relay_version"`
	}
	h.do(macAddr, "GET", "/v1/agents", "", http.StatusOK, &roster)
	if roster.RelayVersion != "0.6.0" {
		t.Fatalf("relay_version = %q", roster.RelayVersion)
	}
	if me := whoamiAs(t, h, grokAddr, ""); me["relay_version"] != "0.6.0" {
		t.Fatalf("whoami relay_version = %v", me["relay_version"])
	}

	// A relay that does not know its build says nothing about it.
	quiet := newHarness(t, Config{})
	rec := quiet.do(macAddr, "GET", "/v1/agents", "", http.StatusOK, nil)
	if strings.Contains(rec.Body.String(), "relay_version") {
		t.Fatalf("relay without a version reported one: %s", rec.Body.String())
	}
}

// The build is kept in the store, so a restarted relay's roster still shows
// it before the agent calls again.
func TestAgentVersionSurvivesRestart(t *testing.T) {
	h := newHarness(t, Config{})
	whoamiAs(t, h, grokAddr, "0.5.2")
	srv := New(identity.NewDirectory(h.st, h.who, identity.Config{Admins: []string{"macbook-pro-44"}}), h.st, Config{})
	h2 := &harness{t: t, srv: srv, h: srv.Handler(), st: h.st, who: h.who}
	if a := agentInfo(t, h2, macAddr, "grokbot"); a.Version != "0.5.2" {
		t.Fatalf("version after restart = %q", a.Version)
	}
}

// Removing an agent forgets what the relay remembered about the name, so an
// agent joined later under it starts clean.
func TestRemoveForgetsAgentVersionAndActivity(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_790_000_000, 0)}
	h := newHarness(t, Config{Now: clk.Now})
	whoamiAs(t, h, museAddr, "0.5.1")
	h.do(museAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
	h.do(macAddr, "POST", "/v1/admin/remove", `{"name":"muse"}`, http.StatusOK, nil)
	var inv struct{ Code string }
	h.do(macAddr, "POST", "/v1/admin/invite", `{"name":"muse"}`, http.StatusOK, &inv)
	h.do(museAddr, "POST", "/v1/join", `{"code":"`+inv.Code+`"}`, http.StatusOK, nil)
	a := agentInfo(t, h, macAddr, "muse")
	if a.Version != "" || !a.LastActive.IsZero() || !a.LastPoll.IsZero() || a.Online {
		t.Fatalf("rejoined muse still carries the removed agent's state: %+v", a)
	}
}

// Two builds calling under one name (an old listen or MCP process next to an
// upgraded CLI) write the store at most once a minute, not on every
// alternating call; the roster still shows the latest build at once.
func TestAlternatingBuildsThrottleVersionWrites(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_790_000_000, 0)}
	h := newHarness(t, Config{Now: clk.Now})
	stored := func() string {
		vs, err := h.st.AgentVersions(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return vs["grokbot"]
	}
	whoamiAs(t, h, grokAddr, "0.5.1")
	if got := stored(); got != "0.5.1" {
		t.Fatalf("first build not stored: %q", got)
	}
	whoamiAs(t, h, grokAddr, "0.5.3")
	whoamiAs(t, h, grokAddr, "0.5.1")
	whoamiAs(t, h, grokAddr, "0.5.3")
	if got := stored(); got != "0.5.1" {
		t.Fatalf("store rewritten within a minute: %q", got)
	}
	if a := agentInfo(t, h, macAddr, "grokbot"); a.Version != "0.5.3" {
		t.Fatalf("roster shows %q, want the latest build", a.Version)
	}
	clk.mu.Lock()
	clk.t = clk.t.Add(persistEvery)
	clk.mu.Unlock()
	whoamiAs(t, h, grokAddr, "0.5.3")
	if got := stored(); got != "0.5.3" {
		t.Fatalf("store did not catch up after a minute: %q", got)
	}
}
