// Package identity decides which agent sent a request. An agent is a name Matt
// chose, bound to one tailnet node by a one-time invite code. There are no
// keys: Tailscale authenticates the node, and the directory maps node to name.
// A node may carry several agents (claude-code and codex on one laptop); the
// client then names itself and the directory accepts the name only if it is
// bound to the calling node.
package identity

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
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
	// ErrNameTaken means a new-only invite named an agent that is bound to
	// another machine.
	ErrNameTaken = errors.New("the name belongs to an agent on another machine")
	// ErrGuestTier refuses making a guest's name a full teammate, or the
	// other way round, without tincan remove first.
	ErrGuestTier = errors.New("the name is a guest; run tincan remove first to change its tier")
	// ErrGuestNode refuses putting a guest on a machine with any other agent,
	// where a header would choose between them.
	ErrGuestNode = errors.New("a machine runs one guest or only teammates")
	// ErrGuestNeedsLogin refuses a guest invite on a relay that cannot tell
	// a guest's login from the owner's: machine names belong to whoever runs
	// the machine, so the --admin list alone would not keep a guest out.
	ErrGuestNeedsLogin = errors.New("guest invites need the relay to run with --admin-login")
)

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidName reports whether name is an agent name the relay accepts: 1-32
// lowercase letters, digits or dashes, starting with a letter or digit.
func ValidName(name string) bool { return nameRE.MatchString(name) }

// Kinds share the name shape: lowercase words such as hermes or vm-webhook.
func checkKind(kind string) error {
	if kind != "" && !nameRE.MatchString(kind) {
		return fmt.Errorf("agent kind %q must be 1-32 lowercase letters, digits, or dashes", kind)
	}
	return nil
}

// maxGoodAtRunes caps an agent's good-at line so the roster stays scannable.
// A session's working-on line shares the cap.
const maxGoodAtRunes = 120

// checkGoodAt trims line and reports the line to store ("" clears it).
func checkGoodAt(line string) (string, error) {
	return CheckRosterLine("the good-at line", line)
}

// CheckRosterLine trims line and reports the line to store. A line is one
// printable line: valid UTF-8, at most 120 runes, and no control or format
// characters, newlines and tabs included, so text another agent reads on the
// roster cannot break a row in two or hide characters. what names the line in
// errors, such as "the good-at line".
func CheckRosterLine(what, line string) (string, error) {
	line = strings.TrimSpace(line)
	if !utf8.ValidString(line) {
		return "", fmt.Errorf("%s is not valid UTF-8", what)
	}
	if utf8.RuneCountInString(line) > maxGoodAtRunes {
		return "", fmt.Errorf("%s is over %d characters", what, maxGoodAtRunes)
	}
	if strings.IndexFunc(line, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) >= 0 {
		return "", fmt.Errorf("%s contains a line break or other control character", what)
	}
	return line, nil
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
	// GoodAt is the owner-written line saying what the agent is good at,
	// empty when unset. Only SetAgentGoodAt writes it; PutAgent keeps it.
	GoodAt string `json:"good_at,omitempty"`
	// Guest is set on a guest, a narrower tier than a teammate; see
	// GuestTerms. Every rewrite of the row must carry it.
	Guest GuestTerms `json:"guest,omitzero"`
}

// GuestTerms are the limits a guest joined under. A set value is the only
// thing that makes an agent a guest.
type GuestTerms struct {
	Until  time.Time `json:"until,omitzero"`
	Asks   []string  `json:"asks,omitempty"` // the teammates it may ask
	NoHold bool      `json:"no_hold,omitempty"`
	// Ended is set once the sweep has closed out a guest past Until.
	Ended bool `json:"ended,omitempty"`
}

// Set reports whether these are a guest's terms.
func (t GuestTerms) Set() bool { return !t.Until.IsZero() }

// Over reports whether a guest on these terms has run out at now.
func (t GuestTerms) Over(now time.Time) bool { return t.Ended || !now.Before(t.Until) }

// Invite is a pending one-time code.
//
// Code carries the raw code only at mint time, on the way to the admin that
// asked for it. Everything persisted (see PutInvite) holds only the digest
// produced by hashInviteCode, never the raw code.
type Invite struct {
	Code    string
	Name    string
	Kind    string // applied to the agent on join when set
	Expires time.Time
	// NewOnly marks a code minted by InviteNew: Join refuses it when the
	// name is by then bound to a machine other than MintNode.
	NewOnly bool
	// MintNode is the tailnet node id of the device that minted a NewOnly
	// code, empty when it came from the local admin socket.
	MintNode string
	// Guest is set on a code minted by InviteGuest, and goes on the agent.
	Guest GuestTerms
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
	// SetAgentGoodAt records an agent's owner-written good-at line ("" clears
	// it) and reports whether the agent exists. PutAgent keeps the line an
	// existing agent already has.
	SetAgentGoodAt(ctx context.Context, name, line string) (bool, error)
	// PutInvite stores inv and retires any earlier unredeemed code for the
	// same name, so re-inviting a name leaves only the newest code valid.
	// Callers pass inv.Code already digested (see hashInviteCode); the raw
	// code never reaches the store. Implementations persist the value
	// opaquely — they hold no pepper and perform no hashing.
	PutInvite(ctx context.Context, inv Invite) error
	// TakeInvite removes and returns the invite, so a code works once.
	// Callers pass the digest produced by hashInviteCode, not the raw code.
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
	// InvitePepper is the relay-local secret used to digest invite codes
	// before they are persisted (see hashInviteCode). When empty, NewDirectory
	// generates an ephemeral one: fine for tests and single-process relays,
	// but a production relay must pass a pepper persisted in its state dir so
	// outstanding invites survive a restart.
	InvitePepper []byte
	// Now overrides the clock in tests.
	Now func() time.Time
}

// Directory maps tailnet nodes to agent names.
type Directory struct {
	store        Store
	who          Resolver
	cfg          Config
	invitePepper []byte
	mu           sync.Mutex // serializes join so a name moves atomically
}

// NewDirectory builds a Directory. A configured InvitePepper must be exactly
// 32 bytes; anything else panics, so a misconfigured key fails fast instead
// of silently weakening the digest.
func NewDirectory(store Store, who Resolver, cfg Config) *Directory {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	pepper := cfg.InvitePepper
	if len(pepper) == 0 {
		// Ephemeral pepper: outstanding invites are lost on restart, which is
		// acceptable for tests. Production relays pass a persisted pepper.
		pepper = make([]byte, 32)
		if _, err := rand.Read(pepper); err != nil {
			panic(fmt.Sprintf("identity: cannot generate invite pepper: %v", err))
		}
	} else {
		if len(pepper) != 32 {
			panic(fmt.Sprintf("identity: invite pepper has %d bytes, want 32", len(pepper)))
		}
		// Copy: the Directory must not retain the caller's buffer, which the
		// caller could mutate after minting and silently invalidate
		// outstanding invites.
		pepper = append([]byte(nil), pepper...)
	}
	return &Directory{store: store, who: who, cfg: cfg, invitePepper: pepper}
}

// hashInviteCode returns the hex HMAC-SHA256 of the normalized code under the
// relay's invite pepper. Only this digest is ever persisted; the raw code is
// shown once to the admin that minted it and never stored. The pepper makes
// the 40-bit code space unsearchable to anyone holding only the database.
func hashInviteCode(pepper []byte, code string) string {
	norm := strings.ToUpper(strings.TrimSpace(code))
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte(norm))
	return hex.EncodeToString(mac.Sum(nil))
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
	c, err := d.Caller(ctx, remoteAddr)
	if err != nil {
		return Resolved{}, err
	}
	return d.ResolveCaller(ctx, c, claimed)
}

// Caller is one read of who is behind a call: its node and the node's agents.
// The relay takes it once per call, so its guest gate and its handler judge
// the same caller and a join landing between them changes neither.
type Caller struct {
	Node   Node
	Agents []Agent
}

// Caller reads the caller behind remoteAddr. It changes nothing, so it is
// safe to call ahead of the real resolve.
func (d *Directory) Caller(ctx context.Context, remoteAddr string) (Caller, error) {
	n, err := d.who.WhoIs(ctx, remoteAddr)
	if err != nil {
		return Caller{}, err
	}
	agents, err := d.store.AgentsByNode(ctx, n.ID)
	if err != nil {
		return Caller{}, err
	}
	return Caller{Node: n, Agents: agents}, nil
}

// ResolveCaller is ResolveAgent on a caller already read.
func (d *Directory) ResolveCaller(ctx context.Context, c Caller, claimed string) (Resolved, error) {
	n, agents := c.Node, c.Agents
	if claimed != "" {
		for _, a := range agents {
			if a.Name == claimed {
				return d.resolved(ctx, n, a)
			}
		}
		if !carriesGuest(agents) {
			if res, ok, err := d.readmit(ctx, n, claimed); ok || err != nil {
				return res, err
			}
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
		if carriesGuest(agents) {
			return d.resolved(ctx, n, agents[0])
		}
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

// BoundAgent names the agent behind remoteAddr without changing anything,
// for a caller that may be no agent at all: of the agents bound to the node,
// the one claimed names, or with no claim the only one, unless another agent
// from the same rebuilt machine still waits and the call could be that one's.
// It is "" when none fits or a lookup fails. Unlike ResolveAgent it never
// re-admits a rebuilt machine, so its answer is for display and proves nothing
// beyond that.
func (d *Directory) BoundAgent(ctx context.Context, remoteAddr, claimed string) string {
	n, err := d.Node(ctx, remoteAddr)
	if err != nil {
		return ""
	}
	agents, err := d.store.AgentsByNode(ctx, n.ID)
	if err != nil {
		return ""
	}
	if claimed == "" {
		if len(agents) != 1 {
			return ""
		}
		if waiting, err := d.awaitingReadmit(ctx, n); err != nil || len(waiting) > 0 {
			return ""
		}
		return agents[0].Name
	}
	for _, a := range agents {
		if a.Name == claimed {
			return a.Name
		}
	}
	return ""
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
	if err := d.refuseGuestName(ctx, name); err != nil {
		return "", err
	}
	return d.mint(ctx, Invite{Name: name, Kind: kind})
}

// refuseGuestName is ErrGuestTier when name is a guest.
func (d *Directory) refuseGuestName(ctx context.Context, name string) error {
	a, ok, err := d.store.AgentByName(ctx, name)
	if err != nil {
		return err
	}
	if ok && a.Guest.Set() {
		return fmt.Errorf("%s: %w", name, ErrGuestTier)
	}
	return nil
}

// mint stores inv under a fresh code and returns the raw code. The caller
// has checked admin rights.
func (d *Directory) mint(ctx context.Context, inv Invite) (string, error) {
	if !nameRE.MatchString(inv.Name) {
		return "", fmt.Errorf("agent name %q must be 1-32 lowercase letters, digits, or dashes", inv.Name)
	}
	if err := checkKind(inv.Kind); err != nil {
		return "", err
	}
	code, err := NewCode()
	if err != nil {
		return "", err
	}
	// Persist only the digest; the raw code is returned once to the admin and
	// never stored.
	inv.Code, inv.Expires = hashInviteCode(d.invitePepper, code), d.cfg.Now().Add(InviteTTL)
	if err := d.store.PutInvite(ctx, inv); err != nil {
		return "", err
	}
	return code, nil
}

// InviteNew is InviteKind for a name that must not move an agent: it refuses
// with ErrNameTaken when name is bound to a machine other than the caller's.
// The caller's own agent may be invited again. The local admin socket has no
// machine, so any existing agent refuses it. The code remembers this, and
// Join checks the name again when it is redeemed.
func (d *Directory) InviteNew(ctx context.Context, remoteAddr, name, kind string) (string, error) {
	if err := d.requireAdmin(ctx, remoteAddr); err != nil {
		return "", err
	}
	mintNode := ""
	if remoteAddr != LocalAdmin {
		n, err := d.who.WhoIs(ctx, remoteAddr)
		if err != nil {
			return "", err
		}
		mintNode = n.ID
	}
	a, ok, err := d.store.AgentByName(ctx, name)
	if err != nil {
		return "", err
	}
	if ok && a.Guest.Set() {
		return "", fmt.Errorf("%s: %w", name, ErrGuestTier)
	}
	if err := newOnlyRefusal(a, ok, mintNode); err != nil {
		return "", err
	}
	return d.mint(ctx, Invite{Name: name, Kind: kind, NewOnly: true, MintNode: mintNode})
}

// InviteGuest mints a code that joins the next machine to use it as a guest
// on terms. It is InviteNew with no machine excused: any agent already under
// name refuses it, here and again at Join, so a guest code cannot take a
// teammate's name, even one on the inviting device.
func (d *Directory) InviteGuest(ctx context.Context, remoteAddr, name string, terms GuestTerms) (string, error) {
	if err := d.requireAdmin(ctx, remoteAddr); err != nil {
		return "", err
	}
	if len(d.cfg.AdminLogins) == 0 {
		return "", ErrGuestNeedsLogin
	}
	a, ok, err := d.store.AgentByName(ctx, name)
	if err != nil {
		return "", err
	}
	if err := newOnlyRefusal(a, ok, ""); err != nil {
		return "", err
	}
	return d.mint(ctx, Invite{Name: name, NewOnly: true, Guest: terms})
}

// newOnlyRefusal is ErrNameTaken when agent a (found says it exists) is
// bound to a machine other than mintNode. An empty mintNode (the local admin
// socket) matches no machine.
func newOnlyRefusal(a Agent, found bool, mintNode string) error {
	if found && (mintNode == "" || a.NodeID != mintNode) {
		return fmt.Errorf("%s: %w", a.Name, ErrNameTaken)
	}
	return nil
}

// Join binds the node behind remoteAddr to the invite's name. A node that
// already carries agents gains another name alongside them. Re-inviting an
// existing name moves it (and its kind, unless the invite names a new one) to
// the new machine; other agents on the old machine stay. A code minted by
// InviteNew is refused with ErrNameTaken if the name has since come to be
// bound to a machine other than the minting one; the check runs under the
// same lock as the join, so no other join can slip in between.
func (d *Directory) Join(ctx context.Context, remoteAddr, code string) (string, error) {
	n, err := d.who.WhoIs(ctx, remoteAddr)
	if err != nil {
		return "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	// Look up by digest: the store never sees the raw code.
	inv, ok, err := d.store.TakeInvite(ctx, hashInviteCode(d.invitePepper, code))
	if err != nil {
		return "", err
	}
	if !ok || d.cfg.Now().After(inv.Expires) {
		return "", ErrBadInvite
	}
	prev, found, err := d.Agent(ctx, inv.Name)
	if err != nil {
		return "", err
	}
	if inv.NewOnly {
		if err := newOnlyRefusal(prev, found, inv.MintNode); err != nil {
			return "", err
		}
	}
	if prev.Guest.Set() && !inv.Guest.Set() {
		return "", fmt.Errorf("%s: %w", inv.Name, ErrGuestTier)
	}
	others, err := d.store.AgentsByNode(ctx, n.ID)
	if err != nil {
		return "", err
	}
	for _, o := range others {
		if o.Name != inv.Name && (o.Guest.Set() || inv.Guest.Set()) {
			return "", fmt.Errorf("%s runs %s: %w", n.Name, o.Name, ErrGuestNode)
		}
	}
	kind := prev.Kind
	if inv.Kind != "" {
		kind = inv.Kind
	}
	a := Agent{Name: inv.Name, NodeID: n.ID, NodeName: n.Name, NodeUser: n.User, JoinedAt: d.cfg.Now(), Kind: kind, Guest: inv.Guest}
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

// SetGoodAt records the owner's line saying what an agent is good at (""
// clears it) and returns the line as stored. Only admin devices may set it.
func (d *Directory) SetGoodAt(ctx context.Context, remoteAddr, name, line string) (string, error) {
	if err := d.requireAdmin(ctx, remoteAddr); err != nil {
		return "", err
	}
	line, err := checkGoodAt(line)
	if err != nil {
		return "", err
	}
	// Serialize with Join, as SetKind does.
	d.mu.Lock()
	defer d.mu.Unlock()
	ok, err := d.store.SetAgentGoodAt(ctx, name, line)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%s: %w", name, ErrUnknownAgent)
	}
	return line, nil
}

// Agents lists joined agents.
func (d *Directory) Agents(ctx context.Context) ([]Agent, error) {
	return d.store.Agents(ctx)
}

// Node is the tailnet node behind remoteAddr, as WhoIs reports it. The
// local admin socket is the node LocalAdmin.
func (d *Directory) Node(ctx context.Context, remoteAddr string) (Node, error) {
	if remoteAddr == LocalAdmin {
		return Node{ID: LocalAdmin, Name: LocalAdmin}, nil
	}
	return d.who.WhoIs(ctx, remoteAddr)
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
	return d.adminCaller(ctx, n)
}

// adminCaller is adminNode with the check that needs the store: a node
// carrying a guest is never an admin device, whatever its name or login. A
// failed read says not admin.
func (d *Directory) adminCaller(ctx context.Context, n Node) error {
	if err := d.adminNode(n); err != nil {
		return err
	}
	agents, err := d.store.AgentsByNode(ctx, n.ID)
	if err != nil {
		return fmt.Errorf("%s: %w: %w", n.Name, ErrNotAdmin, err)
	}
	if carriesGuest(agents) {
		return fmt.Errorf("%s carries a guest: %w", n.Name, ErrNotAdmin)
	}
	return nil
}

// carriesGuest reports whether any of a node's agents is a guest.
func carriesGuest(agents []Agent) bool {
	return slices.ContainsFunc(agents, func(a Agent) bool { return a.Guest.Set() })
}

// Guest finds the guest behind c: a node carrying a guest is a guest's node
// for every call, whatever the call claims. guest is true when the node
// carries one, and then a failure means the claim named no guest of this
// node, or named none on a node with several.
func (c Caller) Guest(claimed string) (a Agent, guest bool, err error) {
	if !carriesGuest(c.Agents) {
		return Agent{}, false, nil
	}
	if claimed == "" && len(c.Agents) > 1 {
		return Agent{}, true, fmt.Errorf("%s runs %s: %w", c.Node.Name, strings.Join(agentNames(c.Agents), ", "), ErrAgentAmbiguous)
	}
	for _, a := range c.Agents {
		if a.Guest.Set() && (claimed == "" || a.Name == claimed) {
			return a, true, nil
		}
	}
	return Agent{}, true, fmt.Errorf("%s is not %q: %w", c.Node.Name, claimed, ErrNotJoined)
}

// NodeLookup is an optional Resolver extension that finds a tailnet node
// by its stable id, as the tailnet sees it now.
type NodeLookup interface {
	LookupNode(ctx context.Context, stableID string) (n Node, found bool, err error)
}

// IsAdminNode reports whether the node with stable id, recorded under the
// machine name name, is still an admin device: present on the tailnet and
// passing the same checks as IsAdmin. A resolver that cannot look nodes up
// leaves only the --admin name check. Code acting for a device after its
// request has ended (a push alert) uses this.
func (d *Directory) IsAdminNode(ctx context.Context, id, name string) (bool, error) {
	if id == LocalAdmin {
		return true, nil
	}
	who := d.who
	if v, ok := who.(virtualResolver); ok {
		who = v.Resolver
	}
	lk, ok := who.(NodeLookup)
	if !ok {
		return slices.Contains(d.cfg.Admins, name), nil
	}
	n, found, err := lk.LookupNode(ctx, id)
	if err != nil || !found {
		return false, err
	}
	return d.adminCaller(ctx, n) == nil, nil
}

// adminNode reports why n is not an admin device, nil when it is.
func (d *Directory) adminNode(n Node) error {
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
