package relay_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// adminSetupRoutes are the page, its files, and the data the setup
// console reads. /v1/agents is read by joined agents too, so it is
// covered separately below.
var adminSetupRoutes = []string{"/admin/", "/admin/app.js", "/admin/style.css", "/v1/admin/urls", "/v1/admin/kinds", "/v1/admin/peers"}

// An admin device loads the setup console and its data; a joined agent and
// a stranger get 403 on every route, static files included.
func TestAdminPageAdminOnly(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	admin := hardened(t, m, "admin")
	code, body, hdr := browserCall(t, "GET", admin, "/admin/", "127.0.0.1", "", map[string]string{"Sec-Fetch-Site": "none", "Sec-Fetch-Mode": "navigate"})
	if code != http.StatusOK || !strings.Contains(body, "<html") {
		t.Fatalf("admin opening /admin/: %d %.200s", code, body)
	}
	csp := hdr.Get("Content-Security-Policy")
	for _, d := range []string{"script-src 'self'", "connect-src 'self'", "frame-ancestors 'none'", "default-src 'none'"} {
		if !strings.Contains(csp, d) {
			t.Errorf("page CSP %q lacks %q", csp, d)
		}
	}
	for _, p := range adminSetupRoutes {
		if code, body, _ := browserCall(t, "GET", admin, p, "127.0.0.1", "", sameSite); code != http.StatusOK {
			t.Errorf("admin GET %s: %d %.200s", p, code, body)
		}
	}
	if code, _, _ := browserCall(t, "GET", admin, "/admin/nope.js", "127.0.0.1", "", sameSite); code != http.StatusNotFound {
		t.Errorf("missing static file: %d, want 404", code)
	}

	for who, base := range map[string]string{"joined agent": hardened(t, m, "grokbot"), "stranger": hardened(t, m, "stranger")} {
		for _, p := range adminSetupRoutes {
			if code, _, _ := browserCall(t, "GET", base, p, "127.0.0.1", "", sameSite); code != http.StatusForbidden {
				t.Errorf("%s GET %s: %d, want 403", who, p, code)
			}
		}
		if code, _, _ := browserCall(t, "GET", base, "/admin/", "127.0.0.1", "", map[string]string{"Sec-Fetch-Site": "none"}); code != http.StatusForbidden {
			t.Errorf("%s opening /admin/: %d, want 403", who, code)
		}
	}
	// The roster stays readable to joined agents; strangers get nothing.
	if code, _, _ := browserCall(t, "GET", hardened(t, m, "grokbot"), "/v1/agents", "127.0.0.1", "", sameSite); code != http.StatusOK {
		t.Errorf("joined agent GET /v1/agents: %d, want 200", code)
	}
	if code, _, _ := browserCall(t, "GET", hardened(t, m, "stranger"), "/v1/agents", "127.0.0.1", "", sameSite); code != http.StatusForbidden {
		t.Errorf("stranger GET /v1/agents: %d, want 403", code)
	}
}

// The kinds listing matches the kinds the relay accepts on invite, so the
// console's dropdown never drifts from the server.
func TestAdminKindsMatchInvite(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	admin := hardened(t, m, "admin")
	code, body, _ := browserCall(t, "GET", admin, "/v1/admin/kinds", "127.0.0.1", "", sameSite)
	if code != http.StatusOK {
		t.Fatalf("kinds: %d %s", code, body)
	}
	var out struct {
		Kinds []string `json:"kinds"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Join(out.Kinds, ",") != strings.Join(onboard.Kinds, ",") {
		t.Errorf("kinds %q, want onboard.Kinds %q", out.Kinds, onboard.Kinds)
	}
}

// The roster carries each agent's live admin verdict: the tagged machine
// is admin by tag, an agent joined on the admin machine by name, and the
// rest are not; the relay's admin tag rides alongside the roster.
func TestAdminVerdictOnRoster(t *testing.T) {
	const tag = "tag:tincan-admin"
	m := testrelay.NewWithAdminTag(t, relay.Config{}, tag)
	m.Who.Set(m.Addr("grokbot"), identity.Node{ID: "nGROK", Name: "grok-bot", User: testrelay.Login, Tags: []string{tag}})
	m.JoinOnMachineOf(t, "admin", "laptop-agent")
	admin := hardened(t, m, "admin")
	code, body, _ := browserCall(t, "GET", admin, "/v1/agents", "127.0.0.1", "", sameSite)
	if code != http.StatusOK {
		t.Fatalf("agents: %d %s", code, body)
	}
	var out struct {
		Agents   []client.AgentInfo `json:"agents"`
		AdminTag string             `json:"admin_tag"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.AdminTag != tag {
		t.Errorf("admin_tag: %q, want %q", out.AdminTag, tag)
	}
	byName := map[string]client.AgentInfo{}
	for _, a := range out.Agents {
		byName[a.Name] = a
	}
	adminStr := func(b *bool) string {
		if b == nil {
			return "unknown"
		}
		if *b {
			return "true"
		}
		return "false"
	}
	if got := byName["grokbot"]; got.NodeAdmin == nil || !*got.NodeAdmin || len(got.NodeTags) != 1 || got.NodeTags[0] != tag {
		t.Errorf("grokbot: admin=%s tags=%q, want admin with the tag", adminStr(got.NodeAdmin), got.NodeTags)
	}
	if got := byName["laptop-agent"]; got.NodeAdmin == nil || !*got.NodeAdmin || got.NodeName != "macbook-pro-44" {
		t.Errorf("laptop-agent: admin=%s node=%q, want admin by machine name", adminStr(got.NodeAdmin), got.NodeName)
	}
	// muse's machine answers the check and is not an admin: an explicit
	// false, distinct from a lookup failure, which omits the field.
	if got := byName["muse"]; got.NodeAdmin == nil || *got.NodeAdmin {
		t.Errorf("muse: admin=%s, want an explicit false", adminStr(got.NodeAdmin))
	}
}

// A rename after joining shows the live machine name: the roster carries
// it alongside the saved name, so the console tells the owner to tag the
// name Tailscale shows now.
func TestAdminVerdictShowsLiveNameAfterRename(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	m.Who.Set(m.Addr("grokbot"), identity.Node{ID: "nGROK", Name: "grok-box", User: "someone-else@example.com", Tags: []string{"tag:agent"}})
	admin := hardened(t, m, "admin")
	code, body, _ := browserCall(t, "GET", admin, "/v1/agents", "127.0.0.1", "", sameSite)
	if code != http.StatusOK {
		t.Fatalf("agents: %d %s", code, body)
	}
	var out struct {
		Agents []client.AgentInfo `json:"agents"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	byName := map[string]client.AgentInfo{}
	for _, a := range out.Agents {
		byName[a.Name] = a
	}
	got := byName["grokbot"]
	if got.NodeName != "grok-bot" || got.NodeLiveName != "grok-box" {
		t.Errorf("grokbot: saved=%q live=%q, want saved grok-bot and live grok-box", got.NodeName, got.NodeLiveName)
	}
	if got.NodeUser != testrelay.Login || got.NodeLiveUser != "someone-else@example.com" {
		t.Errorf("grokbot: saved owner=%q live owner=%q", got.NodeUser, got.NodeLiveUser)
	}
}

// The peer list names every tailnet node with its live tags and online
// state, for admin eyes only.
func TestAdminPeersListsTailnet(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	m.Who.Set(m.Addr("grokbot"), identity.Node{ID: "nGROK", Name: "grok-bot", User: testrelay.Login, Tags: []string{"tag:agent"}})
	m.Who.SetOnline("nGROK", true)
	admin := hardened(t, m, "admin")
	code, body, _ := browserCall(t, "GET", admin, "/v1/admin/peers", "127.0.0.1", "", sameSite)
	if code != http.StatusOK {
		t.Fatalf("peers: %d %s", code, body)
	}
	var out struct {
		Peers []struct {
			ID     string   `json:"id"`
			Name   string   `json:"name"`
			Tags   []string `json:"tags"`
			User   string   `json:"user"`
			Online bool     `json:"online"`
			Self   bool     `json:"self"`
		} `json:"peers"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	byName := map[string]int{}
	for i, p := range out.Peers {
		byName[p.Name] = i
	}
	grok, ok := byName["grok-bot"]
	if !ok {
		t.Fatalf("peers: %v, want grok-bot", out.Peers)
	}
	if !out.Peers[grok].Online || len(out.Peers[grok].Tags) != 1 || out.Peers[grok].Tags[0] != "tag:agent" {
		t.Errorf("grok-bot: %+v, want online with the agent tag", out.Peers[grok])
	}
	if _, ok := byName["macbook-pro-44"]; !ok {
		t.Errorf("peers: %v, want the admin machine", out.Peers)
	}
	for who, base := range map[string]string{"joined agent": hardened(t, m, "grokbot"), "stranger": hardened(t, m, "stranger")} {
		if code, _, _ := browserCall(t, "GET", base, "/v1/admin/peers", "127.0.0.1", "", sameSite); code != http.StatusForbidden {
			t.Errorf("%s GET /v1/admin/peers: %d, want 403", who, code)
		}
	}
}

// The console's add-agent flow: mint an invite from the page's own
// requests, then join with the printed code.
func TestAdminInviteJoinFlow(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	admin := hardened(t, m, "admin")
	code, body, _ := browserCall(t, "POST", admin, "/v1/admin/invite", "127.0.0.1", `{"name":"newguy","kind":"codex","new_only":true}`, browserJSON)
	if code != http.StatusOK {
		t.Fatalf("invite: %d %s", code, body)
	}
	var inv struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(body), &inv); err != nil {
		t.Fatal(err)
	}
	if inv.Name != "newguy" || inv.Kind != "codex" || inv.Code == "" {
		t.Fatalf("invite: %+v, want name newguy kind codex and a code", inv)
	}
	stranger := hardened(t, m, "stranger")
	code, body, _ = browserCall(t, "POST", stranger, "/v1/join", "127.0.0.1", `{"code":"`+inv.Code+`"}`, browserJSON)
	if code != http.StatusOK {
		t.Fatalf("join: %d %s", code, body)
	}
}
