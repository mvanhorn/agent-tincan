// Package testrelay runs a real relay behind one httptest server per agent,
// each of which pins the caller's tailnet address, so client-side tests can
// exercise the full HTTP path without a tailnet. Test-only.
package testrelay

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/identity/identitytest"
	"github.com/mvanhorn/agent-tincan/internal/policy"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// Mesh is a relay with grokbot, instinct, and muse joined, each on its own
// machine. "admin" is an admin device that has not joined; "stranger" is a
// machine that is neither joined nor an admin. JoinOnMachineOf adds more
// agents to an existing machine.
type Mesh struct {
	Server *relay.Server
	Store  *store.Store
	Dir    *identity.Directory
	Who    *identitytest.Resolver
	urls   map[string]string // agent (or "admin") -> its machine's endpoint
	addrs  map[string]string // agent (or "admin") -> its machine's tailnet address
	h      http.Handler
}

var addrs = map[string]string{"grokbot": "100.0.0.2:1", "instinct": "100.0.0.3:1", "muse": "100.0.0.4:1", "admin": "100.0.0.1:1", "stranger": "100.0.0.9:1"}

// New starts the mesh.
func New(t *testing.T, cfg relay.Config) *Mesh {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	who := identitytest.New(map[string]identity.Node{
		addrs["admin"]:    {ID: "nMAC", Name: "macbook-pro-44", User: Login},
		addrs["grokbot"]:  {ID: "nGROK", Name: "grok-bot", User: Login},
		addrs["instinct"]: {ID: "nINST", Name: "instinct", User: Login},
		addrs["muse"]:     {ID: "nMUSE", Name: "muse", User: Login},
		addrs["stranger"]: {ID: "nLAPTOP", Name: "old-laptop", User: Login},
	})
	dir := identity.NewDirectory(st, identity.WithVirtual(who), identity.Config{Admins: []string{"macbook-pro-44"}})
	srv := relay.New(dir, st, cfg)
	srv.SetPreparer(policy.New(st, policy.Config{}))
	m := &Mesh{Server: srv, Store: st, Dir: dir, Who: who, urls: map[string]string{}, addrs: map[string]string{}, h: srv.Handler()}
	for name, addr := range addrs {
		m.urls[name] = m.endpoint(t, addr)
		m.addrs[name] = addr
	}
	for _, name := range []string{"grokbot", "instinct", "muse"} {
		if _, err := m.Client(t, name).Join(t.Context(), m.Invite(t, name)); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// endpoint serves the relay to callers the relay sees at addr.
func (m *Mesh) endpoint(t *testing.T, addr string) string {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = addr
		m.h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

// Login is the tailnet owner every mesh machine belongs to, as on a
// single-person tailnet.
const Login = "owner@example.com"

// Rebuild replaces agent's machine with a new tailnet node called
// machineName, as rebuilding a sandbox does: the old node leaves the tailnet
// and the new one has a new stable id and address. It returns the new
// machine's endpoint; the agent is not re-bound until it calls the relay.
func (m *Mesh) Rebuild(t *testing.T, agent, machineName string) string {
	t.Helper()
	old := m.addrs[agent]
	addr := fmt.Sprintf("100.0.1.%d:1", len(m.addrs)+10)
	m.Who.Remove(old)
	m.Who.Set(addr, identity.Node{ID: "n" + machineName + "-rebuilt", Name: machineName, User: Login})
	for name, a := range m.addrs {
		if a == old {
			m.addrs[name], m.urls[name] = addr, ""
		}
	}
	url := m.endpoint(t, addr)
	for name, a := range m.addrs {
		if a == addr {
			m.urls[name] = url
		}
	}
	return url
}

// Invite mints a join code for name from the admin device.
func (m *Mesh) Invite(t *testing.T, name string) string {
	t.Helper()
	code, err := m.Client(t, "admin").Invite(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

// URL is the endpoint the relay sees as the machine agent name runs on.
func (m *Mesh) URL(name string) string { return m.urls[name] }

// JoinOnMachineOf joins a new agent name on the machine host already runs
// on and returns its client.
func (m *Mesh) JoinOnMachineOf(t *testing.T, host, name string) *client.Relay {
	t.Helper()
	m.urls[name] = m.urls[host]
	m.addrs[name] = m.addrs[host]
	if _, err := m.Client(t, name).Join(t.Context(), m.Invite(t, name)); err != nil {
		t.Fatal(err)
	}
	return m.Client(t, name)
}

// Client returns a relay client that the relay sees as agent name. Like a
// real client with a saved config, it names its agent on every call.
func (m *Mesh) Client(t *testing.T, name string) *client.Relay {
	t.Helper()
	cfg := client.Config{Relay: m.urls[name], Agent: name}
	if name == "admin" || name == "stranger" {
		cfg.Agent = ""
	}
	r, err := client.NewRelayFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
