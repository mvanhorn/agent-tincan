// Package testrelay runs a real relay behind one httptest server per agent,
// each of which pins the caller's tailnet address, so client-side tests can
// exercise the full HTTP path without a tailnet. Test-only.
package testrelay

import (
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
// machine. JoinOnMachineOf adds more agents to an existing machine.
type Mesh struct {
	Server *relay.Server
	Store  *store.Store
	Dir    *identity.Directory
	urls   map[string]string // agent (or "admin") -> its machine's endpoint
}

var addrs = map[string]string{"grokbot": "100.0.0.2:1", "instinct": "100.0.0.3:1", "muse": "100.0.0.4:1", "admin": "100.0.0.1:1"}

// New starts the mesh.
func New(t *testing.T, cfg relay.Config) *Mesh {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	who := identitytest.New(map[string]identity.Node{
		addrs["admin"]:    {ID: "nMAC", Name: "macbook-pro-44"},
		addrs["grokbot"]:  {ID: "nGROK", Name: "grok-bot"},
		addrs["instinct"]: {ID: "nINST", Name: "instinct"},
		addrs["muse"]:     {ID: "nMUSE", Name: "muse"},
	})
	dir := identity.NewDirectory(st, identity.WithVirtual(who), identity.Config{Admins: []string{"macbook-pro-44"}})
	srv := relay.New(dir, st, cfg)
	srv.SetPreparer(policy.New(st, policy.Config{}))
	m := &Mesh{Server: srv, Store: st, Dir: dir, urls: map[string]string{}}
	h := srv.Handler()
	for name, addr := range addrs {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.RemoteAddr = addr
			h.ServeHTTP(w, r)
		}))
		t.Cleanup(ts.Close)
		m.urls[name] = ts.URL
	}
	for _, name := range []string{"grokbot", "instinct", "muse"} {
		if _, err := m.Client(t, name).Join(t.Context(), m.Invite(t, name)); err != nil {
			t.Fatal(err)
		}
	}
	return m
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
	if name == "admin" {
		cfg.Agent = ""
	}
	r, err := client.NewRelayFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
