package relay_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
	"github.com/mvanhorn/agent-tincan/internal/policy"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// hostMesh is a mesh that can invite guests, with the admin device's client.
type hostMesh struct {
	*testrelay.Mesh
	admin *client.Relay
}

func newHostMesh(t *testing.T) *hostMesh {
	t.Helper()
	m := testrelay.NewWithAdminLogin(t, relay.Config{})
	return &hostMesh{Mesh: m, admin: m.Client(t, "admin")}
}

// set names agent a task host from the admin device and returns the
// warnings the relay answered with.
func (m *hostMesh) set(t *testing.T, agent string) []string {
	t.Helper()
	warnings, err := m.admin.SetTaskHost(t.Context(), agent, true)
	if err != nil {
		t.Fatalf("set %s as task host: %v", agent, err)
	}
	return warnings
}

// entry is agent's roster entry as c reads it.
func (m *hostMesh) entry(t *testing.T, c *client.Relay, agent string) client.AgentInfo {
	t.Helper()
	agents, err := c.Agents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(agents, func(a client.AgentInfo) bool { return a.Name == agent })
	if i < 0 {
		t.Fatalf("%s is not on the roster", agent)
	}
	return agents[i]
}

// stored reports whether agent's row carries the task host mark.
func (m *hostMesh) stored(t *testing.T, agent string) bool {
	t.Helper()
	a, ok, err := m.Dir.Agent(t.Context(), agent)
	if err != nil || !ok {
		t.Fatalf("agent %s: %v (found %v)", agent, err, ok)
	}
	return a.TaskHost
}

// hostAudit returns the details of the task host audit rows about agent,
// oldest first.
func (m *hostMesh) hostAudit(t *testing.T, agent string) []string {
	t.Helper()
	events, err := m.Store.AuditEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		if e.Event == "task-host" && e.Actor == agent {
			out = append(out, e.Detail)
		}
	}
	return out
}

// gate puts the relay under an approval.json with body.
func (m *hostMesh) gate(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "approval.json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := policy.LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{Approval: a}))
}

func (m *hostMesh) kind(t *testing.T, agent, kind string) {
	t.Helper()
	if err := m.admin.SetKind(t.Context(), agent, kind); err != nil {
		t.Fatalf("kind %s for %s: %v", kind, agent, err)
	}
}

// warned reports whether one of warnings contains every part.
func warned(warnings []string, parts ...string) bool {
	return slices.ContainsFunc(warnings, func(w string) bool {
		for _, p := range parts {
			if !strings.Contains(w, p) {
				return false
			}
		}
		return true
	})
}

const taskHostPath = "/v1/agents/grokbot/task-host"

// An admin device names grokbot a task host. The roster shows the mark to
// the owner and to a teammate, on grokbot alone, and the audit log records
// the grant. Clearing removes the mark from both and is recorded too.
func TestTaskHostSetShowsOnRosterAndClears(t *testing.T) {
	m := newHostMesh(t)
	ctx := t.Context()
	muse := m.Client(t, "muse")
	if e := m.entry(t, m.admin, "grokbot"); e.TaskHost || e.TaskHostField() != "" {
		t.Fatalf("grokbot before any set = %+v", e)
	}
	var out struct {
		Name     string   `json:"name"`
		TaskHost bool     `json:"task_host"`
		Warnings []string `json:"warnings"`
	}
	if err := m.admin.Raw(ctx, "PUT", taskHostPath, map[string]bool{"task_host": true}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Name != "grokbot" || !out.TaskHost {
		t.Fatalf("answer to the set = %+v", out)
	}
	for who, c := range map[string]*client.Relay{"the owner": m.admin, "a teammate": muse} {
		if e := m.entry(t, c, "grokbot"); !e.TaskHost || e.TaskHostSuspended != "" || e.TaskHostField() != "task_host=yes" {
			t.Errorf("grokbot as %s reads it = %+v, field %q", who, e, e.TaskHostField())
		}
		if e := m.entry(t, c, "instinct"); e.TaskHost || e.TaskHostField() != "" {
			t.Errorf("instinct as %s reads it = %+v", who, e)
		}
	}
	if !m.stored(t, "grokbot") {
		t.Fatal("the mark was not stored")
	}
	if got := m.hostAudit(t, "grokbot"); len(got) != 1 || got[0] != `{"task_host":true}` {
		t.Fatalf("audit after the set = %v", got)
	}

	if warnings, err := m.admin.SetTaskHost(ctx, "grokbot", false); err != nil || len(warnings) != 0 {
		t.Fatalf("clear = %v, %v", warnings, err)
	}
	for who, c := range map[string]*client.Relay{"the owner": m.admin, "a teammate": muse} {
		if e := m.entry(t, c, "grokbot"); e.TaskHost || e.TaskHostField() != "" {
			t.Errorf("grokbot after the clear, as %s reads it = %+v", who, e)
		}
	}
	if m.stored(t, "grokbot") {
		t.Fatal("the mark is still stored after the clear")
	}
	if got := m.hostAudit(t, "grokbot"); len(got) != 2 || got[1] != `{"task_host":false}` {
		t.Fatalf("audit after the clear = %v", got)
	}

	// Several agents may be hosts at once.
	m.set(t, "grokbot")
	m.set(t, "instinct")
	if !m.stored(t, "grokbot") || !m.stored(t, "instinct") {
		t.Fatal("two agents could not both be task hosts")
	}
}

// Naming and clearing a task host are the owner's: a joined agent is
// refused with the admin error, for itself and for a teammate, a guest is
// refused by the guest gate, and nothing changes. A body that does not say
// which way to set the mark is refused instead of read as a clear.
func TestTaskHostRouteIsAdminOnly(t *testing.T) {
	m := newHostMesh(t)
	ctx := t.Context()
	guest := m.JoinGuest(t, "friend", client.GuestInvite{Ask: []string{"muse"}, ForSeconds: 3600})
	grok := m.Client(t, "grokbot")
	for _, path := range []string{taskHostPath, "/v1/agents/muse/task-host"} {
		if err := grok.Raw(ctx, "PUT", path, map[string]bool{"task_host": true}, nil); !refusedWith(err, http.StatusForbidden, identity.ErrNotAdmin.Error()) {
			t.Errorf("grokbot set %s: %v", path, err)
		}
	}
	if err := guest.Raw(ctx, "PUT", taskHostPath, map[string]bool{"task_host": true}, nil); !refusedWith(err, http.StatusForbidden, refusedRoute) {
		t.Errorf("a guest's set: %v", err)
	}
	if m.stored(t, "grokbot") || m.stored(t, "muse") {
		t.Fatal("a refused set stored the mark")
	}

	m.set(t, "grokbot")
	if err := grok.Raw(ctx, "PUT", taskHostPath, map[string]bool{"task_host": false}, nil); !refusedWith(err, http.StatusForbidden, identity.ErrNotAdmin.Error()) {
		t.Errorf("grokbot cleared itself: %v", err)
	}
	if err := guest.Raw(ctx, "PUT", taskHostPath, map[string]bool{"task_host": false}, nil); !refusedWith(err, http.StatusForbidden, refusedRoute) {
		t.Errorf("a guest's clear: %v", err)
	}
	if err := m.admin.Raw(ctx, "PUT", taskHostPath, map[string]string{}, nil); !refusedWith(err, http.StatusBadRequest, "task_host is required") {
		t.Errorf("a body with no task_host: %v", err)
	}
	if !m.stored(t, "grokbot") {
		t.Fatal("a refused clear removed the mark")
	}
	if got := m.hostAudit(t, "grokbot"); len(got) != 1 {
		t.Fatalf("refused calls wrote audit rows: %v", got)
	}
}

// An agent that can never be a host is refused with its reason, and the
// refusal stores nothing: a guest, a phone, the history, notes and pass
// services, a council, an agent driven through a web chat page, and an agent
// approval.json gates. An unknown name is a JSON 404.
func TestTaskHostRefusesIneligibleAgents(t *testing.T) {
	m := newHostMesh(t)
	ctx := t.Context()
	m.JoinGuest(t, "friend", client.GuestInvite{Ask: []string{"muse"}, ForSeconds: 3600})
	refused := func(agent, reason string) {
		t.Helper()
		_, err := m.admin.SetTaskHost(ctx, agent, true)
		if !refusedWith(err, http.StatusBadRequest, agent+" cannot be a task host: "+reason) {
			t.Fatalf("set %s: %v, want 400 with %q", agent, err, reason)
		}
		if m.stored(t, agent) {
			t.Fatalf("a refused set marked %s", agent)
		}
		if got := m.hostAudit(t, agent); len(got) != 0 {
			t.Fatalf("a refused set of %s wrote audit rows: %v", agent, got)
		}
	}
	refused("friend", "it is a guest")
	for kind, reason := range map[string]string{
		onboard.KindPhone:      "a phone agent asks and takes no requests",
		onboard.KindHistory:    "history is a service agent",
		onboard.KindNotes:      "notes is a service agent",
		onboard.KindPass:       "pass is a service agent",
		onboard.KindCouncil:    "asks to a council agent are held for the owner's approval",
		onboard.KindGeminiWeb:  "a gemini-web agent is driven through a web chat page and has no task tools",
		onboard.KindCopilotWeb: "a copilot-web agent is driven through a web chat page and has no task tools",
		onboard.KindDotWeb:     "a dot-web agent is driven through a web chat page and has no task tools",
	} {
		m.kind(t, "muse", kind)
		refused("muse", reason)
	}
	m.kind(t, "muse", "")
	m.gate(t, `{"gate":{"muse":{"from":["instinct"]}}}`)
	refused("muse", "approval.json holds asks to muse")
	// The gate names muse alone, so a teammate it leaves alone is still named.
	m.set(t, "instinct")

	if _, err := m.admin.SetTaskHost(ctx, "nobody", true); !refusedWith(err, http.StatusNotFound, identity.ErrUnknownAgent.Error()) {
		t.Fatalf("unknown agent: %v", err)
	}
}

// wakes names a wake method per agent; "none" for the rest.
type wakes map[string]string

func (w wakes) WakeMethod(agent string) string {
	if m := w[agent]; m != "" {
		return m
	}
	return "none"
}

// The answer to a set warns the owner of what the grant costs. An agent
// that reaches the relay through the MCP gateway sends task text to its
// vendor. An agent whose wake cannot start a turn (command, none) passes
// news on only when the owner next speaks to it. A host a guest may ask can
// carry task text into its replies to that guest, and each such guest is
// named. An agent none of these fit is named with no warning.
func TestTaskHostSetWarns(t *testing.T) {
	m := newHostMesh(t)
	m.Server.SetWakeNamer(wakes{"grokbot": "webhook", "instinct": "command", "scout": "channel"})
	m.JoinOnMachineOf(t, "instinct", "scout")
	const vendor, wake, guest = "through the MCP gateway", "cannot start a turn", "can carry task text"

	if w := m.set(t, "grokbot"); len(w) != 0 {
		t.Fatalf("grokbot, woken by webhook with no guest: warnings = %v", w)
	}

	// A gateway kind, and an agent bound to the gateway whatever its kind.
	m.kind(t, "grokbot", onboard.KindSesame)
	if w := m.set(t, "grokbot"); len(w) != 1 || !warned(w, "grokbot", vendor, "every task") {
		t.Fatalf("gateway kind: warnings = %v", w)
	}
	if err := m.Dir.BindVirtual(t.Context(), "relayed", false); err != nil {
		t.Fatal(err)
	}
	m.Server.SetWakeNamer(wakes{"grokbot": "webhook", "instinct": "command", "scout": "channel", "relayed": "channel"})
	if w := m.set(t, "relayed"); len(w) != 1 || !warned(w, "relayed", vendor) {
		t.Fatalf("gateway agent: warnings = %v", w)
	}

	// Wake methods that cannot start a turn.
	if w := m.set(t, "instinct"); len(w) != 1 || !warned(w, "instinct", "command", wake, "when the owner next speaks to it") {
		t.Fatalf("command wake: warnings = %v", w)
	}
	if w := m.set(t, "muse"); len(w) != 1 || !warned(w, "muse", "none", wake) {
		t.Fatalf("no wake: warnings = %v", w)
	}

	// Guests whose ask lists name the host, each by name.
	m.JoinGuest(t, "friend", client.GuestInvite{Ask: []string{"scout", "muse"}, ForSeconds: 3600})
	w := m.set(t, "scout")
	if len(w) != 1 || !warned(w, "friend", "scout", guest) {
		t.Fatalf("host on a guest's ask list: warnings = %v", w)
	}
	if w := m.set(t, "muse"); len(w) != 2 || !warned(w, wake) || !warned(w, "friend", "muse", guest) {
		t.Fatalf("no wake and on a guest's ask list: warnings = %v", w)
	}
	if w := m.set(t, "grokbot"); warned(w, guest) {
		t.Fatalf("grokbot is on no guest's ask list: warnings = %v", w)
	}
}

// A host that approval.json begins to gate keeps its stored mark and is
// shown on the roster as suspended, with the reason, to the owner and to a
// teammate. When the gate goes, it is a host again with nothing set anew.
func TestTaskHostSuspendedWhenItStopsBeingEligible(t *testing.T) {
	m := newHostMesh(t)
	m.set(t, "muse")
	m.gate(t, `{"gate":{"muse":{"from":["instinct"]}}}`)
	const reason = "approval.json holds asks to muse"
	for who, c := range map[string]*client.Relay{"the owner": m.admin, "a teammate": m.Client(t, "grokbot")} {
		e := m.entry(t, c, "muse")
		if !e.TaskHost || e.TaskHostSuspended != reason {
			t.Errorf("muse as %s reads it = %+v, want a suspended host with %q", who, e, reason)
		}
		if got := e.TaskHostField(); got != `task_host="suspended: `+reason+`"` {
			t.Errorf("muse's roster field as %s reads it = %s", who, got)
		}
	}
	if !m.stored(t, "muse") {
		t.Fatal("the gate removed the stored mark")
	}
	// An agent that is not a host shows no suspension, gated or not.
	m.gate(t, `{"gate":{"muse":{"from":["instinct"]},"instinct":{"from":["muse"]}}}`)
	if e := m.entry(t, m.admin, "instinct"); e.TaskHost || e.TaskHostSuspended != "" {
		t.Fatalf("instinct, gated and never a host = %+v", e)
	}

	// A kind that can never host suspends a host the same way.
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{}))
	if e := m.entry(t, m.admin, "muse"); !e.TaskHost || e.TaskHostSuspended != "" || e.TaskHostField() != "task_host=yes" {
		t.Fatalf("muse once the gate is gone = %+v", e)
	}
	m.kind(t, "muse", onboard.KindNotes)
	if e := m.entry(t, m.admin, "muse"); !e.TaskHost || e.TaskHostSuspended != "notes is a service agent" {
		t.Fatalf("muse as a notes agent = %+v", e)
	}

	// The owner can always clear a suspended host: only naming one checks
	// eligibility.
	if _, err := m.admin.SetTaskHost(t.Context(), "muse", false); err != nil {
		t.Fatalf("clear a suspended host: %v", err)
	}
	if m.stored(t, "muse") {
		t.Fatal("the clear left the stored mark")
	}
	if e := m.entry(t, m.admin, "muse"); e.TaskHost || e.TaskHostSuspended != "" {
		t.Fatalf("muse after the clear = %+v", e)
	}
	if got := m.hostAudit(t, "muse"); len(got) != 2 || got[1] != `{"task_host":false}` {
		t.Fatalf("audit after the clear = %v", got)
	}
}

// Removing a host writes an audit row for the cleared mark, and an agent
// invited afterwards under the same name is not a host. Removing an agent
// that was not a host writes no such row.
func TestTaskHostClearedWhenTheHostIsRemoved(t *testing.T) {
	m := newHostMesh(t)
	ctx := t.Context()
	m.set(t, "grokbot")
	if err := m.admin.Remove(ctx, "grokbot"); err != nil {
		t.Fatal(err)
	}
	if got := m.hostAudit(t, "grokbot"); len(got) != 2 || got[1] != `{"reason":"removed","task_host":false}` {
		t.Fatalf("audit after the removal = %v", got)
	}
	if _, err := m.Client(t, "grokbot").Join(ctx, m.Invite(t, "grokbot")); err != nil {
		t.Fatal(err)
	}
	if m.stored(t, "grokbot") {
		t.Fatal("the agent invited under a removed host's name is a host")
	}
	if e := m.entry(t, m.admin, "grokbot"); e.TaskHost {
		t.Fatalf("roster entry of the new agent = %+v", e)
	}
	if got := m.hostAudit(t, "grokbot"); len(got) != 2 {
		t.Fatalf("the new join wrote another task host row: %v", got)
	}

	if err := m.admin.Remove(ctx, "instinct"); err != nil {
		t.Fatal(err)
	}
	if got := m.hostAudit(t, "instinct"); len(got) != 0 {
		t.Fatalf("removing an agent that was no host wrote: %v", got)
	}
}

// Redeeming an invite for a host's name, with no removal first, clears the
// mark and writes an audit row: on another machine, where the name moves,
// and on the host's own machine. A join over an agent that was not a host
// writes none.
func TestTaskHostClearedWhenAnInviteIsRedeemedForTheName(t *testing.T) {
	for name, machine := range map[string]string{"on another machine": "stranger", "on the same machine": "grokbot"} {
		t.Run(name, func(t *testing.T) {
			m := newHostMesh(t)
			ctx := t.Context()
			m.set(t, "grokbot")
			joiner, err := client.NewRelayFor(client.Config{Relay: m.URL(machine), Agent: "grokbot"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := joiner.Join(ctx, m.Invite(t, "grokbot")); err != nil {
				t.Fatal(err)
			}
			if m.stored(t, "grokbot") {
				t.Fatal("the joined agent kept the earlier agent's mark")
			}
			if got := m.hostAudit(t, "grokbot"); len(got) != 2 || got[1] != `{"reason":"joined","task_host":false}` {
				t.Fatalf("audit after the join = %v", got)
			}
			if _, err := joiner.Join(ctx, m.Invite(t, "grokbot")); err != nil {
				t.Fatal(err)
			}
			if got := m.hostAudit(t, "grokbot"); len(got) != 2 {
				t.Fatalf("a join over an agent that was no host wrote: %v", got)
			}
		})
	}
}

// An automatic rebind to a rebuilt machine moves the host to a new node,
// which clears the mark, and the audit log says so.
func TestTaskHostClearedByRebindToRebuiltMachine(t *testing.T) {
	m := newHostMesh(t)
	ctx := t.Context()
	// One call from its own node, so the mark is shown to survive ordinary
	// calls before the rebuild.
	if _, err := m.Client(t, "instinct").WhoAmI(ctx); err != nil {
		t.Fatal(err)
	}
	m.set(t, "instinct")
	if _, err := m.Client(t, "instinct").WhoAmI(ctx); err != nil {
		t.Fatal(err)
	}
	if !m.stored(t, "instinct") {
		t.Fatal("an ordinary call from the host's own node cleared the mark")
	}
	rebuilt, err := client.NewRelayFor(client.Config{Relay: m.Rebuild(t, "instinct", "instinct"), Agent: "instinct"})
	if err != nil {
		t.Fatal(err)
	}
	me, err := rebuilt.WhoAmI(ctx)
	if err != nil || me.Name != "instinct" {
		t.Fatalf("call from the rebuilt machine = %+v, %v", me, err)
	}
	if m.stored(t, "instinct") {
		t.Fatal("the rebound agent kept the mark")
	}
	if got := m.hostAudit(t, "instinct"); len(got) != 2 || got[1] != `{"reason":"rebind","task_host":false}` {
		t.Fatalf("audit after the rebind = %v", got)
	}
}

// binder is a connector that binds the name to the gateway and nothing else.
type binder struct{ dir *identity.Directory }

func (b binder) Connect(ctx context.Context, name string, replace bool) (string, string, error) {
	return "code", "https://gateway.example/mcp", b.dir.BindVirtual(ctx, name, replace)
}

func (binder) Revoke(context.Context, string) error { return nil }

// Connecting a host's name to the gateway clears the mark with an audit
// row. Connecting a name that was no host writes none.
func TestTaskHostClearedByGatewayConnect(t *testing.T) {
	m := newHostMesh(t)
	ctx := t.Context()
	m.Server.SetConnector(binder{m.Dir})
	m.set(t, "grokbot")
	connect := func(name string) {
		t.Helper()
		if err := m.admin.Raw(ctx, "POST", "/v1/admin/connect", map[string]any{"name": name, "replace": true}, nil); err != nil {
			t.Fatalf("connect %s: %v", name, err)
		}
	}
	connect("grokbot")
	if m.stored(t, "grokbot") {
		t.Fatal("the connected agent kept the mark")
	}
	if got := m.hostAudit(t, "grokbot"); len(got) != 2 || got[1] != `{"reason":"connected","task_host":false}` {
		t.Fatalf("audit after the connect = %v", got)
	}
	connect("grokbot")
	connect("muse")
	if got := append(m.hostAudit(t, "grokbot")[2:], m.hostAudit(t, "muse")...); len(got) != 0 {
		t.Fatalf("connects of agents that were no hosts wrote: %v", got)
	}
}

// A relay that stores task hosts and takes their decisions says so.
func TestCapabilitiesAdvertiseTaskHosts(t *testing.T) {
	m := newHostMesh(t)
	var caps map[string]any
	if err := m.Client(t, "grokbot").Raw(t.Context(), "GET", "/v1/capabilities", nil, &caps); err != nil || caps["task_hosts"] != true || caps["task_host_decisions"] != true {
		t.Fatalf("capabilities = %v, %v", caps, err)
	}
	typed, err := m.admin.Capabilities(t.Context())
	if err != nil || !typed.TaskHosts || !typed.TaskHostDecisions {
		t.Fatalf("client capabilities = %+v, %v", typed, err)
	}
}
