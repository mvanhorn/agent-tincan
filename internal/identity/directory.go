// Package identity decides which agent sent a request. An agent is a name Matt
// chose, bound to one tailnet node by a one-time invite code. There are no
// keys: Tailscale authenticates the node, and the directory maps node to name.
// A node may carry several agents (claude-code and codex on one laptop); the
// client then names itself and the directory accepts the name only if it is
// bound to the calling node.
package identity

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// InviteTTL is how long an invite code stays valid.
const InviteTTL = 10 * time.Minute

// LocalAdmin is the remote address the relay passes for commands that arrive
// on its local admin socket. It always has admin rights.
const LocalAdmin = "local-admin"

var (
	ErrNotJoined    = errors.New("not a joined agent")
	ErrNotAdmin     = errors.New("admin commands must come from an admin device")
	ErrBadInvite    = errors.New("invite code is invalid, used, or expired")
	ErrUnknownAgent = errors.New("no such agent")
	// ErrAgentAmbiguous means several agents share the calling machine and the
	// request did not say which one it comes from.
	ErrAgentAmbiguous = errors.New("several agents share this machine; the client must name one (point TINCAN_CONFIG at that agent's config)")
	// ErrRebindCheckFailed means re-admitting a rebuilt machine could not
	// check whether the agent's old node is still online. It is a transient
	// failure, not a refusal.
	ErrRebindCheckFailed = errors.New("could not check the agent's old node; try again")
)

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Kinds share the name shape: lowercase words such as hermes or vm-webhook.
func checkKind(kind string) error {
	if kind != "" && !nameRE.MatchString(kind) {
		return fmt.Errorf("agent kind %q must be 1-32 lowercase letters, digits, or dashes", kind)
	}
	return nil
}

// Agent is a joined agent.
type Agent struct {
	Name     string    `json:"name"`
	NodeID   string    `json:"node_id"`
	NodeName string    `json:"node_name"`
	JoinedAt time.Time `json:"joined_at"`
	// Kind is the agent runtime (hermes, codex, ...), empty when unknown.
	Kind string `json:"kind,omitempty"`
	// NodeUser is the Tailscale login that owned the node at join, empty for
	// agents joined before it was recorded.
	NodeUser string `json:"node_user,omitempty"`
}

// Invite is a pending one-time code.
type Invite struct {
	Code    string
	Name    string
	Kind    string // applied to the agent on join when set
	Expires time.Time
}

// Store persists agents and invites. The relay backs it with SQLite; tests
// use NewMemoryStore.
type Store interface {
	PutAgent(ctx context.Context, a Agent) error
	DeleteAgent(ctx context.Context, name string) (bool, error)
	Agents(ctx context.Context) ([]Agent, error)
	// AgentsByNode and AgentByName are lookups for the hot request path.
	// AgentsByNode returns every agent bound to the node, ordered by name.
	AgentsByNode(ctx context.Context, nodeID string) ([]Agent, error)
	AgentByName(ctx context.Context, name string) (Agent, bool, error)
	// SetAgentKind records an agent's runtime kind ("" clears it) and reports
	// whether the agent exists.
	SetAgentKind(ctx context.Context, name, kind string) (bool, error)
	PutInvite(ctx context.Context, inv Invite) error
	// TakeInvite removes and returns the invite, so a code works once.
	TakeInvite(ctx context.Context, code string) (Invite, bool, error)
}

// Config tunes a Directory.
type Config struct {
	// Admins lists the machine names allowed to invite and remove agents.
	Admins []string
	// AdminLogins, when set, also requires an admin node's owning Tailscale
	// login to be listed. Every node on a single-user tailnet shares one login,
	// so this narrows admin rights but cannot replace the machine list.
	AdminLogins []string
	// NoAutoRebind turns off re-admitting rebuilt machines (see ResolveAgent).
	NoAutoRebind bool
	// Now overrides the clock in tests.
	Now func() time.Time
}

// Directory maps tailnet nodes to agent names.
type Directory struct {
	store Store
	who   Resolver
	cfg   Config
	mu    sync.Mutex // serializes join so a name moves atomically
}

// NewDirectory builds a Directory.
func NewDirectory(store Store, who Resolver, cfg Config) *Directory {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Directory{store: store, who: who, cfg: cfg}
}

// Attribute returns the agent name for the node behind remoteAddr when the
// caller does not name itself. It fails with ErrAgentAmbiguous when the node
// carries several agents.
func (d *Directory) Attribute(ctx context.Context, remoteAddr string) (string, error) {
	return d.Resolve(ctx, remoteAddr, "")
}

// Resolve returns the agent behind remoteAddr. WhoIs authenticates the node;
// claimed, when set, picks one of the node's agents and is accepted only if
// that name is bound to the node. With no claim, a node with one agent
// resolves to it and a node with several fails with the choices. A rebuilt
// machine is re-admitted on the way (see ResolveAgent).
func (d *Directory) Resolve(ctx context.Context, remoteAddr, claimed string) (string, error) {
	res, err := d.ResolveAgent(ctx, remoteAddr, claimed)
	return res.Name, err
}

// ResolveAgent is Resolve that also reports a rebind. When the caller's node
// carries no agent, or not the claimed one, it tries to re-admit the node as
// a rebuilt machine before refusing.
func (d *Directory) ResolveAgent(ctx context.Context, remoteAddr, claimed string) (Resolved, error) {
	n, err := d.who.WhoIs(ctx, remoteAddr)
	if err != nil {
		return Resolved{}, err
	}
	agents, err := d.store.AgentsByNode(ctx, n.ID)
	if err != nil {
		return Resolved{}, err
	}
	if claimed != "" {
		for _, a := range agents {
			if a.Name == claimed {
				return d.resolved(ctx, n, a)
			}
		}
		if res, ok, err := d.readmit(ctx, n, claimed); ok || err != nil {
			return res, err
		}
		return Resolved{}, fmt.Errorf("%s is not %q: %w", n.Name, claimed, ErrNotJoined)
	}
	switch len(agents) {
	case 0:
		if res, ok, err := d.readmit(ctx, n, ""); ok || err != nil {
			return res, err
		}
		return Resolved{}, fmt.Errorf("%s: %w", n.Name, ErrNotJoined)
	case 1:
		// Another agent from the same rebuilt machine may still be waiting to
		// be re-admitted, and this nameless call may be its first.
		waiting, err := d.awaitingReadmit(ctx, n)
		if err != nil {
			return Resolved{}, err
		}
		if len(waiting) > 0 {
			return Resolved{}, fmt.Errorf("%s runs %s, and %s from its old machine has not rejoined; each agent must name itself (tincan rejoin --name <agent>): %w",
				n.Name, agents[0].Name, strings.Join(agentNames(waiting), ", "), ErrAgentAmbiguous)
		}
		return d.resolved(ctx, n, agents[0])
	}
	return Resolved{}, fmt.Errorf("%s runs %s: %w", n.Name, strings.Join(agentNames(agents), ", "), ErrAgentAmbiguous)
}

// resolved returns a, which is bound to n. An agent joined before logins
// were recorded gets n's login now, so that a later rebuild of its machine
// can be re-admitted with the login check (see readmit).
func (d *Directory) resolved(ctx context.Context, n Node, a Agent) (Resolved, error) {
	if a.NodeUser == "" && n.User != "" && len(n.Tags) == 0 {
		if err := d.recordLogin(ctx, n, a.Name); err != nil {
			return Resolved{}, err
		}
	}
	return Resolved{Name: a.Name}, nil
}

// recordLogin stores n's login on agent name, provided it is still bound to
// n with no login recorded.
func (d *Directory) recordLogin(ctx context.Context, n Node, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	a, found, err := d.store.AgentByName(ctx, name)
	if err != nil || !found || a.NodeID != n.ID || a.NodeUser != "" {
		return err
	}
	a.NodeUser = n.User
	return d.store.PutAgent(ctx, a)
}

// agentNames returns the names of agents, in order.
func agentNames(agents []Agent) []string {
	names := make([]string, len(agents))
	for i, a := range agents {
		names[i] = a.Name
	}
	return names
}

// Agent returns a joined agent by name.
func (d *Directory) Agent(ctx context.Context, name string) (Agent, bool, error) {
	return d.store.AgentByName(ctx, name)
}

// Has reports whether name is a joined agent.
func (d *Directory) Has(ctx context.Context, name string) (bool, error) {
	_, ok, err := d.store.AgentByName(ctx, name)
	return ok, err
}

// Invite creates a one-time code that joins the next machine to use it as
// name. Only admin devices may invite.
func (d *Directory) Invite(ctx context.Context, remoteAddr, name string) (string, error) {
	return d.InviteKind(ctx, remoteAddr, name, "")
}

// InviteKind is Invite that also records the agent's kind, applied when the
// code is used. An empty kind leaves the agent's kind as it was.
func (d *Directory) InviteKind(ctx context.Context, remoteAddr, name, kind string) (string, error) {
	if err := d.requireAdmin(ctx, remoteAddr); err != nil {
		return "", err
	}
	if !nameRE.MatchString(name) {
		return "", fmt.Errorf("agent name %q must be 1-32 lowercase letters, digits, or dashes", name)
	}
	if err := checkKind(kind); err != nil {
		return "", err
	}
	code, err := NewCode()
	if err != nil {
		return "", err
	}
	inv := Invite{Code: code, Name: name, Kind: kind, Expires: d.cfg.Now().Add(InviteTTL)}
	if err := d.store.PutInvite(ctx, inv); err != nil {
		return "", err
	}
	return code, nil
}

// Join binds the node behind remoteAddr to the invite's name. A node that
// already carries agents gains another name alongside them. Re-inviting an
// existing name moves it (and its kind, unless the invite names a new one) to
// the new machine; other agents on the old machine stay.
func (d *Directory) Join(ctx context.Context, remoteAddr, code string) (string, error) {
	n, err := d.who.WhoIs(ctx, remoteAddr)
	if err != nil {
		return "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	inv, ok, err := d.store.TakeInvite(ctx, strings.ToUpper(strings.TrimSpace(code)))
	if err != nil {
		return "", err
	}
	if !ok || d.cfg.Now().After(inv.Expires) {
		return "", ErrBadInvite
	}
	prev, _, err := d.Agent(ctx, inv.Name)
	if err != nil {
		return "", err
	}
	kind := prev.Kind
	if inv.Kind != "" {
		kind = inv.Kind
	}
	a := Agent{Name: inv.Name, NodeID: n.ID, NodeName: n.Name, NodeUser: n.User, JoinedAt: d.cfg.Now(), Kind: kind}
	if err := d.store.PutAgent(ctx, a); err != nil {
		return "", err
	}
	return a.Name, nil
}

// Remove unbinds an agent. The relay cancels its queued requests.
func (d *Directory) Remove(ctx context.Context, remoteAddr, name string) error {
	if err := d.requireAdmin(ctx, remoteAddr); err != nil {
		return err
	}
	ok, err := d.store.DeleteAgent(ctx, name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s: %w", name, ErrUnknownAgent)
	}
	return nil
}

// SetKind records an agent's runtime kind ("" clears it). Only admin devices
// may set it.
func (d *Directory) SetKind(ctx context.Context, remoteAddr, name, kind string) error {
	if err := d.requireAdmin(ctx, remoteAddr); err != nil {
		return err
	}
	if err := checkKind(kind); err != nil {
		return err
	}
	// Join rewrites the whole agent row, so a kind set while it runs would
	// be lost.
	d.mu.Lock()
	defer d.mu.Unlock()
	ok, err := d.store.SetAgentKind(ctx, name, kind)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s: %w", name, ErrUnknownAgent)
	}
	return nil
}

// Agents lists joined agents.
func (d *Directory) Agents(ctx context.Context) ([]Agent, error) {
	return d.store.Agents(ctx)
}

// IsAdmin reports whether remoteAddr may run admin commands.
func (d *Directory) IsAdmin(ctx context.Context, remoteAddr string) bool {
	return d.requireAdmin(ctx, remoteAddr) == nil
}

func (d *Directory) requireAdmin(ctx context.Context, remoteAddr string) error {
	if remoteAddr == LocalAdmin {
		return nil
	}
	n, err := d.who.WhoIs(ctx, remoteAddr)
	if err != nil {
		return err
	}
	// Admin = named in --admin, untagged (tagged nodes are services, never
	// people), and, when --admin-login is set, owned by a listed login.
	if !slices.Contains(d.cfg.Admins, n.Name) {
		return fmt.Errorf("%s: %w", n.Name, ErrNotAdmin)
	}
	if len(n.Tags) > 0 {
		return fmt.Errorf("%s is tagged %s: %w", n.Name, strings.Join(n.Tags, ","), ErrNotAdmin)
	}
	if len(d.cfg.AdminLogins) > 0 && !slices.Contains(d.cfg.AdminLogins, n.User) {
		return fmt.Errorf("%s is owned by %q: %w", n.Name, n.User, ErrNotAdmin)
	}
	return nil
}

// codeAlphabet drops 0, 1, I, and O so codes read cleanly aloud.
const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// NewCode returns a random one-time code shaped XXXX-XXXX.
func NewCode() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, 0, 9)
	for i, c := range b {
		if i == 4 {
			out = append(out, '-')
		}
		out = append(out, codeAlphabet[int(c)%len(codeAlphabet)])
	}
	return string(out), nil
}
