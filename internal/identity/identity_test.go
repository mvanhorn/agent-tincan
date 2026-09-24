package identity_test

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/identity/identitytest"
)

var (
	macNode      = identity.Node{ID: "nMAC", Name: "macbook-pro-44", User: "mvanhorn@gmail.com"}
	grokNode     = identity.Node{ID: "nGROK", Name: "grok-bot", User: "mvanhorn@gmail.com"}
	instinctNode = identity.Node{ID: "nINST", Name: "e2b.local", User: "mvanhorn@gmail.com"}
	museNode     = identity.Node{ID: "nMUSE", Name: "muse", User: "mvanhorn@gmail.com"}
)

type fixture struct {
	dir   *identity.Directory
	who   *identitytest.Resolver
	clock *time.Time
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	now := time.Unix(1_790_000_000, 0)
	who := identitytest.New(map[string]identity.Node{
		"100.0.0.1:1": macNode, "100.0.0.2:1": grokNode, "100.0.0.3:1": instinctNode, "100.0.0.4:1": museNode,
	})
	dir := identity.NewDirectory(identity.NewMemoryStore(), who, identity.Config{
		Admins: []string{"macbook-pro-44"},
		Now:    func() time.Time { return now },
	})
	return fixture{dir: dir, who: who, clock: &now}
}

func (f fixture) invite(t *testing.T, name string) string {
	t.Helper()
	code, err := f.dir.Invite(context.Background(), "100.0.0.1:1", name)
	if err != nil {
		t.Fatalf("invite %s: %v", name, err)
	}
	return code
}

func TestThreeAgentsJoinAndAreAttributed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, j := range []struct{ name, addr string }{{"grokbot", "100.0.0.2:1"}, {"instinct", "100.0.0.3:1"}, {"muse", "100.0.0.4:1"}} {
		code := f.invite(t, j.name)
		if got, err := f.dir.Join(ctx, j.addr, code); err != nil || got != j.name {
			t.Fatalf("join %s: got %q, %v", j.name, got, err)
		}
	}
	for addr, want := range map[string]string{"100.0.0.2:1": "grokbot", "100.0.0.3:1": "instinct", "100.0.0.4:1": "muse"} {
		if got, err := f.dir.Attribute(ctx, addr); err != nil || got != want {
			t.Errorf("attribute %s: got %q, %v; want %q", addr, got, err, want)
		}
	}
	agents, _ := f.dir.Agents(ctx)
	if len(agents) != 3 {
		t.Fatalf("want 3 agents, got %v", agents)
	}
}

func TestInviteCodeShape(t *testing.T) {
	code := newFixture(t).invite(t, "muse")
	if !regexp.MustCompile(`^[A-Z2-9]{4}-[A-Z2-9]{4}$`).MatchString(code) {
		t.Fatalf("code %q is not XXXX-XXXX", code)
	}
}

func TestInviteCodeIsSingleUse(t *testing.T) {
	f := newFixture(t)
	code := f.invite(t, "muse")
	if _, err := f.dir.Join(context.Background(), "100.0.0.4:1", code); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dir.Join(context.Background(), "100.0.0.3:1", code); !errors.Is(err, identity.ErrBadInvite) {
		t.Fatalf("reused code: want ErrBadInvite, got %v", err)
	}
}

func TestInviteCodeExpires(t *testing.T) {
	f := newFixture(t)
	code := f.invite(t, "muse")
	*f.clock = f.clock.Add(identity.InviteTTL + time.Second)
	if _, err := f.dir.Join(context.Background(), "100.0.0.4:1", code); !errors.Is(err, identity.ErrBadInvite) {
		t.Fatalf("expired code: want ErrBadInvite, got %v", err)
	}
}

// Re-inviting a name retires the earlier unredeemed code for it.
func TestReinviteRetiresOlderCode(t *testing.T) {
	f := newFixture(t)
	old := f.invite(t, "muse")
	other := f.invite(t, "grokbot")
	current := f.invite(t, "muse")
	if _, err := f.dir.Join(context.Background(), "100.0.0.4:1", old); !errors.Is(err, identity.ErrBadInvite) {
		t.Fatalf("older code: want ErrBadInvite, got %v", err)
	}
	if got, err := f.dir.Join(context.Background(), "100.0.0.4:1", current); err != nil || got != "muse" {
		t.Fatalf("current code: %q, %v", got, err)
	}
	if got, err := f.dir.Join(context.Background(), "100.0.0.2:1", other); err != nil || got != "grokbot" {
		t.Fatalf("other name's code: %q, %v", got, err)
	}
}

func TestUnknownCodeRejected(t *testing.T) {
	f := newFixture(t)
	if _, err := f.dir.Join(context.Background(), "100.0.0.4:1", "ABCD-EFGH"); !errors.Is(err, identity.ErrBadInvite) {
		t.Fatalf("want ErrBadInvite, got %v", err)
	}
}

// The plan's naming lesson: store the name Matt chose, never the hostname.
func TestAgentKeepsInvitedNameNotHostname(t *testing.T) {
	f := newFixture(t)
	code := f.invite(t, "instinct")
	if _, err := f.dir.Join(context.Background(), "100.0.0.3:1", code); err != nil {
		t.Fatal(err)
	}
	got, _ := f.dir.Attribute(context.Background(), "100.0.0.3:1")
	if got != "instinct" {
		t.Fatalf("attributed as %q, want instinct (host announces e2b.local)", got)
	}
}

// Every node on the tailnet shares one login, so admin rights come from an
// explicit node list, not from the login.
func TestInviteOnlyFromAdminNodes(t *testing.T) {
	f := newFixture(t)
	if _, err := f.dir.Invite(context.Background(), "100.0.0.2:1", "evil"); !errors.Is(err, identity.ErrNotAdmin) {
		t.Fatalf("invite from grok-bot: want ErrNotAdmin, got %v", err)
	}
	if err := f.dir.Remove(context.Background(), "100.0.0.4:1", "grokbot"); !errors.Is(err, identity.ErrNotAdmin) {
		t.Fatalf("remove from muse: want ErrNotAdmin, got %v", err)
	}
}

func TestLocalAdminBypassesNodeCheck(t *testing.T) {
	f := newFixture(t)
	if _, err := f.dir.Invite(context.Background(), identity.LocalAdmin, "muse"); err != nil {
		t.Fatalf("local admin invite: %v", err)
	}
}

func TestRemoveStopsAttribution(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	code := f.invite(t, "muse")
	f.dir.Join(ctx, "100.0.0.4:1", code)
	if err := f.dir.Remove(ctx, "100.0.0.1:1", "muse"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dir.Attribute(ctx, "100.0.0.4:1"); !errors.Is(err, identity.ErrNotJoined) {
		t.Fatalf("after remove: want ErrNotJoined, got %v", err)
	}
	if err := f.dir.Remove(ctx, "100.0.0.1:1", "muse"); !errors.Is(err, identity.ErrUnknownAgent) {
		t.Fatalf("second remove: want ErrUnknownAgent, got %v", err)
	}
}

// AE5: a tailnet node that never joined is not an agent.
func TestUnjoinedNodeIsRejected(t *testing.T) {
	f := newFixture(t)
	if _, err := f.dir.Attribute(context.Background(), "100.0.0.1:1"); !errors.Is(err, identity.ErrNotJoined) {
		t.Fatalf("want ErrNotJoined, got %v", err)
	}
	if _, err := f.dir.Attribute(context.Background(), "100.9.9.9:1"); err == nil {
		t.Fatal("unknown address should fail WhoIs")
	}
}

func TestReinviteMovesAgentToNewMachine(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.dir.Join(ctx, "100.0.0.3:1", f.invite(t, "instinct"))
	f.dir.Join(ctx, "100.0.0.4:1", f.invite(t, "instinct"))
	if got, _ := f.dir.Attribute(ctx, "100.0.0.4:1"); got != "instinct" {
		t.Fatalf("new machine attributed as %q", got)
	}
	if _, err := f.dir.Attribute(ctx, "100.0.0.3:1"); !errors.Is(err, identity.ErrNotJoined) {
		t.Fatalf("old machine should no longer be instinct, got %v", err)
	}
}

// Several agents can share one machine (claude-code and codex on a laptop).
// Each joins with its own invite and the client names itself per request.
func TestSecondAgentJoinsSameMachine(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if got, err := f.dir.Join(ctx, "100.0.0.4:1", f.invite(t, "claude-code")); err != nil || got != "claude-code" {
		t.Fatalf("first join: %q, %v", got, err)
	}
	// One agent on the node: no claim needed, current behavior preserved.
	if got, err := f.dir.Attribute(ctx, "100.0.0.4:1"); err != nil || got != "claude-code" {
		t.Fatalf("single agent attribute: %q, %v", got, err)
	}
	if got, err := f.dir.Join(ctx, "100.0.0.4:1", f.invite(t, "codex")); err != nil || got != "codex" {
		t.Fatalf("second join on same node: %q, %v", got, err)
	}
	for _, name := range []string{"claude-code", "codex"} {
		if got, err := f.dir.Resolve(ctx, "100.0.0.4:1", name); err != nil || got != name {
			t.Errorf("resolve claim %s: %q, %v", name, got, err)
		}
	}
	_, err := f.dir.Resolve(ctx, "100.0.0.4:1", "")
	if !errors.Is(err, identity.ErrAgentAmbiguous) {
		t.Fatalf("two agents, no claim: want ErrAgentAmbiguous, got %v", err)
	}
	if !strings.Contains(err.Error(), "claude-code") || !strings.Contains(err.Error(), "codex") {
		t.Fatalf("ambiguity error should list both names: %v", err)
	}
}

// A claim is only honored for a name bound to the calling node.
func TestClaimForAnotherNodeIsNotJoined(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.dir.Join(ctx, "100.0.0.2:1", f.invite(t, "grokbot"))
	f.dir.Join(ctx, "100.0.0.4:1", f.invite(t, "muse"))
	if _, err := f.dir.Resolve(ctx, "100.0.0.4:1", "grokbot"); !errors.Is(err, identity.ErrNotJoined) {
		t.Fatalf("claiming another node's agent: want ErrNotJoined, got %v", err)
	}
	if _, err := f.dir.Resolve(ctx, "100.0.0.4:1", "nobody"); !errors.Is(err, identity.ErrNotJoined) {
		t.Fatalf("claiming an unknown agent: want ErrNotJoined, got %v", err)
	}
	if _, err := f.dir.Resolve(ctx, "100.0.0.1:1", "muse"); !errors.Is(err, identity.ErrNotJoined) {
		t.Fatalf("claim from an unjoined node: want ErrNotJoined, got %v", err)
	}
}

// Re-inviting a name still moves it, and only it: other agents on the old
// machine stay put.
func TestReinviteMovesOnlyThatAgent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.dir.Join(ctx, "100.0.0.3:1", f.invite(t, "hermes"))
	f.dir.Join(ctx, "100.0.0.3:1", f.invite(t, "openclaw"))
	if _, err := f.dir.Join(ctx, "100.0.0.4:1", f.invite(t, "openclaw")); err != nil {
		t.Fatal(err)
	}
	if got, err := f.dir.Attribute(ctx, "100.0.0.4:1"); err != nil || got != "openclaw" {
		t.Fatalf("new machine: %q, %v", got, err)
	}
	if got, err := f.dir.Attribute(ctx, "100.0.0.3:1"); err != nil || got != "hermes" {
		t.Fatalf("old machine should keep hermes alone: %q, %v", got, err)
	}
}

func TestRemoveLeavesOtherAgentOnSameMachine(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.dir.Join(ctx, "100.0.0.1:1", f.invite(t, "claude-code"))
	f.dir.Join(ctx, "100.0.0.1:1", f.invite(t, "codex"))
	if err := f.dir.Remove(ctx, "100.0.0.1:1", "codex"); err != nil {
		t.Fatal(err)
	}
	if got, err := f.dir.Attribute(ctx, "100.0.0.1:1"); err != nil || got != "claude-code" {
		t.Fatalf("after removing codex: %q, %v", got, err)
	}
	if _, err := f.dir.Resolve(ctx, "100.0.0.1:1", "codex"); !errors.Is(err, identity.ErrNotJoined) {
		t.Fatalf("removed codex claim: want ErrNotJoined, got %v", err)
	}
}

// An agent's kind survives a move to a new machine.
func TestKindSurvivesMove(t *testing.T) {
	store := identity.NewMemoryStore()
	who := identitytest.New(map[string]identity.Node{"100.0.0.3:1": instinctNode, "100.0.0.4:1": museNode})
	dir := identity.NewDirectory(store, who, identity.Config{})
	ctx := context.Background()
	code, _ := dir.Invite(ctx, identity.LocalAdmin, "hermes")
	dir.Join(ctx, "100.0.0.3:1", code)
	if ok, err := store.SetAgentKind(ctx, "hermes", "hermes"); err != nil || !ok {
		t.Fatalf("set kind: %v, %v", ok, err)
	}
	if ok, _ := store.SetAgentKind(ctx, "nobody", "codex"); ok {
		t.Fatal("setting kind on an unknown agent should report false")
	}
	code, _ = dir.Invite(ctx, identity.LocalAdmin, "hermes")
	dir.Join(ctx, "100.0.0.4:1", code)
	a, _, _ := store.AgentByName(ctx, "hermes")
	if a.NodeID != museNode.ID || a.Kind != "hermes" {
		t.Fatalf("after move: %+v", a)
	}
}

// adminFixture resolves admin-named nodes with varying tags and owners.
func adminFixture(t *testing.T, adminLogins []string) *identity.Directory {
	t.Helper()
	who := identitytest.New(map[string]identity.Node{
		"100.0.1.1:1": {ID: "nA", Name: "macbook-pro-44", User: "mvanhorn@gmail.com"},
		"100.0.1.2:1": {ID: "nB", Name: "macbook-pro-44", User: "mvanhorn@gmail.com", Tags: []string{"tag:agent"}},
		"100.0.1.3:1": {ID: "nC", Name: "macbook-pro-44", User: "someone-else@example.com"},
	})
	return identity.NewDirectory(identity.NewMemoryStore(), who, identity.Config{
		Admins:      []string{"macbook-pro-44"},
		AdminLogins: adminLogins,
	})
}

// A tagged node never has admin rights, even if it carries an admin's name.
func TestTaggedNodeWithAdminNameIsNotAdmin(t *testing.T) {
	dir := adminFixture(t, nil)
	ctx := context.Background()
	if !dir.IsAdmin(ctx, "100.0.1.1:1") {
		t.Fatal("untagged admin-named node should be admin")
	}
	if dir.IsAdmin(ctx, "100.0.1.2:1") {
		t.Fatal("tagged admin-named node must not be admin")
	}
	if _, err := dir.Invite(ctx, "100.0.1.2:1", "evil"); !errors.Is(err, identity.ErrNotAdmin) {
		t.Fatalf("invite from tagged node: want ErrNotAdmin, got %v", err)
	}
}

// With --admin-login set, the node's owning login must also match.
func TestAdminLoginRestrictsOwner(t *testing.T) {
	dir := adminFixture(t, []string{"mvanhorn@gmail.com"})
	ctx := context.Background()
	if !dir.IsAdmin(ctx, "100.0.1.1:1") {
		t.Fatal("admin-named node owned by the listed login should be admin")
	}
	if dir.IsAdmin(ctx, "100.0.1.3:1") {
		t.Fatal("admin-named node owned by another login must not be admin")
	}
	// Without AdminLogins, ownership is not checked.
	if !adminFixture(t, nil).IsAdmin(ctx, "100.0.1.3:1") {
		t.Fatal("without AdminLogins an untagged admin-named node is admin")
	}
}

func TestInvalidNamesRejected(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"", "Muse", "has space", "a/b", "way-too-long-name-for-an-agent-xxxxxxxx"} {
		if _, err := f.dir.Invite(context.Background(), "100.0.0.1:1", name); err == nil {
			t.Errorf("name %q should be rejected", name)
		}
	}
}

// --listen mode must refuse to start when it cannot ask tailscaled WhoIs.
func TestProbeFailsWithoutTailscaled(t *testing.T) {
	r := identity.NewLocalResolverAt(filepath.Join(t.TempDir(), "no-such-tailscaled.sock"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.Probe(ctx); err == nil {
		t.Fatal("probe should fail with no reachable tailscaled")
	}
}

// An invite's kind is applied on join and wins over a kind the name already
// had; an invite without a kind keeps the old one.
func TestInviteKindAppliedOnJoin(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	code, err := f.dir.InviteKind(ctx, "100.0.0.1:1", "hermes", "hermes")
	if err != nil {
		t.Fatal(err)
	}
	f.dir.Join(ctx, "100.0.0.3:1", code)
	if a := agentNamed(t, f.dir, "hermes"); a.Kind != "hermes" {
		t.Fatalf("kind after join = %q", a.Kind)
	}
	f.dir.Join(ctx, "100.0.0.4:1", f.invite(t, "hermes"))
	if a := agentNamed(t, f.dir, "hermes"); a.Kind != "hermes" || a.NodeID != museNode.ID {
		t.Fatalf("kindless re-invite: %+v", a)
	}
	code, _ = f.dir.InviteKind(ctx, "100.0.0.1:1", "hermes", "openclaw")
	f.dir.Join(ctx, "100.0.0.3:1", code)
	if a := agentNamed(t, f.dir, "hermes"); a.Kind != "openclaw" {
		t.Fatalf("kind after re-invite with kind = %q", a.Kind)
	}
	if _, err := f.dir.InviteKind(ctx, "100.0.0.1:1", "x", "Bad Kind"); err == nil {
		t.Fatal("malformed kind should be rejected")
	}
}

func TestSetKindIsAdminOnly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.dir.Join(ctx, "100.0.0.2:1", f.invite(t, "grokbot"))
	if err := f.dir.SetKind(ctx, "100.0.0.2:1", "grokbot", "vm-webhook"); !errors.Is(err, identity.ErrNotAdmin) {
		t.Fatalf("non-admin set kind: %v", err)
	}
	if err := f.dir.SetKind(ctx, "100.0.0.1:1", "grokbot", "vm-webhook"); err != nil {
		t.Fatal(err)
	}
	if a := agentNamed(t, f.dir, "grokbot"); a.Kind != "vm-webhook" {
		t.Fatalf("kind = %q", a.Kind)
	}
	if err := f.dir.SetKind(ctx, identity.LocalAdmin, "nobody", "codex"); !errors.Is(err, identity.ErrUnknownAgent) {
		t.Fatalf("unknown agent: %v", err)
	}
}

// pausingStore runs pause after the first AgentByName read, which Join does
// between taking the invite and writing the agent.
type pausingStore struct {
	*identity.MemoryStore
	pause func()
}

func (p *pausingStore) AgentByName(ctx context.Context, name string) (identity.Agent, bool, error) {
	a, ok, err := p.MemoryStore.AgentByName(ctx, name)
	if p.pause != nil {
		pause := p.pause
		p.pause = nil
		pause()
	}
	return a, ok, err
}

// SetKind waits for a Join in progress, so the kind it sets is not
// overwritten by the kind Join read before it.
func TestSetKindDuringJoinIsNotLost(t *testing.T) {
	ctx := context.Background()
	st := &pausingStore{MemoryStore: identity.NewMemoryStore()}
	who := identitytest.New(map[string]identity.Node{"100.0.0.1:1": macNode, "100.0.0.2:1": grokNode})
	dir := identity.NewDirectory(st, who, identity.Config{Admins: []string{"macbook-pro-44"}})
	st.PutAgent(ctx, identity.Agent{Name: "grokbot", NodeID: "nOLD", NodeName: "old", Kind: "codex"})
	code, err := dir.Invite(ctx, identity.LocalAdmin, "grokbot")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	st.pause = func() {
		go func() { done <- dir.SetKind(ctx, identity.LocalAdmin, "grokbot", "vm-webhook") }()
		select {
		case err := <-done:
			done <- err // SetKind finished inside Join: the race this guards
		case <-time.After(50 * time.Millisecond):
		}
	}
	if _, err := dir.Join(ctx, "100.0.0.2:1", code); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if a := agentNamed(t, dir, "grokbot"); a.Kind != "vm-webhook" || a.NodeID != "nGROK" {
		t.Fatalf("after join and set kind = %+v", a)
	}
}

func agentNamed(t *testing.T, d *identity.Directory, name string) identity.Agent {
	t.Helper()
	a, ok, err := d.Agent(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("no agent %s", name)
	}
	return a
}
