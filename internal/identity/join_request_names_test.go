package identity_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/identity/identitytest"
)

// newNode is an unjoined machine asking to join.
var newNode = identity.Node{ID: "nNEW", Name: "new-mac", User: "mvanhorn@gmail.com", Key: "nodekey:aa", Tags: []string{"tag:agents"}}

type joinFixture struct {
	dir   *identity.Directory
	store *identity.MemoryStore
	who   *identitytest.Resolver
	now   *time.Time
}

func newJoinFixture(t *testing.T) joinFixture {
	t.Helper()
	now := time.Unix(1_790_000_000, 0)
	who := identitytest.New(map[string]identity.Node{"100.0.0.1:1": macNode, "100.0.0.9:1": newNode})
	st := identity.NewMemoryStore()
	dir := identity.NewDirectory(st, who, identity.Config{
		Admins:      []string{"macbook-pro-44"},
		AdminLogins: []string{"mvanhorn@gmail.com"},
		Now:         func() time.Time { return now },
	})
	return joinFixture{dir: dir, store: st, who: who, now: &now}
}

func (f joinFixture) file(t *testing.T, names ...string) identity.JoinRequest {
	t.Helper()
	var agents []identity.RequestedAgent
	for _, n := range names {
		agents = append(agents, identity.RequestedAgent{Name: n})
	}
	r, err := f.dir.FileJoinRequest(context.Background(), newNode, agents)
	if err != nil {
		t.Fatalf("file %v: %v", names, err)
	}
	return r
}

// Every path that mints or binds a name refuses one a pending request holds.
func TestHeldNameRefusesInvitesAndBinds(t *testing.T) {
	f := newJoinFixture(t)
	ctx := context.Background()
	f.file(t, "codex")
	for what, err := range map[string]error{
		"plain invite":    func() error { _, err := f.dir.InviteKind(ctx, "100.0.0.1:1", "codex", ""); return err }(),
		"new-only invite": func() error { _, err := f.dir.InviteNew(ctx, "100.0.0.1:1", "codex", ""); return err }(),
		"guest invite": func() error {
			_, err := f.dir.InviteGuest(ctx, "100.0.0.1:1", "codex", identity.GuestTerms{Until: f.now.Add(time.Hour)})
			return err
		}(),
		"connect":       f.dir.BindVirtual(ctx, "codex", false),
		"guest connect": f.dir.BindVirtualGuest(ctx, "codex", identity.GuestTerms{Until: f.now.Add(time.Hour)}),
	} {
		if !errors.Is(err, identity.ErrNameTaken) {
			t.Errorf("%s for a held name = %v, want ErrNameTaken", what, err)
		}
	}
	if _, ok, _ := f.dir.Agent(ctx, "codex"); ok {
		t.Fatal("a held name was bound")
	}
}

// An approved request still holds its names until it is claimed.
func TestApprovedRequestStillHoldsNames(t *testing.T) {
	f := newJoinFixture(t)
	ctx := context.Background()
	r := f.file(t, "codex")
	if _, err := f.store.ApproveJoinRequest(ctx, r.ID, "local-admin", *f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dir.InviteKind(ctx, "100.0.0.1:1", "codex", ""); !errors.Is(err, identity.ErrNameTaken) {
		t.Fatalf("invite for a name an approved request holds = %v", err)
	}
}

// heldGateStore answers the first held-name lookup, then stops until release
// closes, so a test can hold minting between its check and its write and see
// whether filing slips in.
type heldGateStore struct {
	*identity.MemoryStore
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (s *heldGateStore) JoinNameHeld(ctx context.Context, name, exceptID string, now time.Time) (bool, error) {
	held, err := s.MemoryStore.JoinNameHeld(ctx, name, exceptID, now)
	first := false
	s.once.Do(func() { first = true })
	if first {
		close(s.entered)
		<-s.release
	}
	return held, err
}

// Minting an invite and filing a request for the same name at the same
// moment: minting checks and stores under the lock filing holds, so exactly
// one of them takes the name.
func TestMintAndFileForOneNameAtOnce(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_790_000_000, 0)
	st := &heldGateStore{MemoryStore: identity.NewMemoryStore(), entered: make(chan struct{}), release: make(chan struct{})}
	who := identitytest.New(map[string]identity.Node{"100.0.0.1:1": macNode, "100.0.0.9:1": newNode})
	dir := identity.NewDirectory(st, who, identity.Config{
		Admins: []string{"macbook-pro-44"}, AdminLogins: []string{"mvanhorn@gmail.com"},
		Now: func() time.Time { return now },
	})

	var mintErr error
	minted := make(chan struct{})
	go func() {
		defer close(minted)
		_, mintErr = dir.InviteKind(ctx, "100.0.0.1:1", "racer", "")
	}()
	<-st.entered // minting is paused at its held-name check

	var fileErr error
	filed := make(chan struct{})
	go func() {
		defer close(filed)
		_, fileErr = dir.FileJoinRequest(ctx, newNode, []identity.RequestedAgent{{Name: "racer"}})
	}()
	select {
	case <-filed: // filing did not wait for minting
	case <-time.After(200 * time.Millisecond):
	}
	close(st.release)
	<-minted
	<-filed

	if mintErr != nil {
		t.Fatalf("mint = %v, want it to win: it reached the name first", mintErr)
	}
	if !errors.Is(fileErr, identity.ErrNameTaken) {
		t.Fatalf("filing while an invite for the name was being minted = %v, want ErrNameTaken", fileErr)
	}
}

// A code minted before a request came to hold its name is refused when it is
// redeemed.
func TestInviteMintedBeforeHoldIsRefusedAtJoin(t *testing.T) {
	f := newJoinFixture(t)
	ctx := context.Background()
	code, err := f.dir.InviteKind(ctx, "100.0.0.1:1", "codex", "")
	if err != nil {
		t.Fatal(err)
	}
	// Written straight to the store: filing itself refuses a name with a live
	// invite, so only a request that got in first can hold it here.
	if err := f.store.InsertJoinRequest(ctx, identity.JoinRequest{
		ID: "jr1", NodeID: "nOTHER", NodeName: "other", Agents: []identity.RequestedAgent{{Name: "codex"}},
		Code: "AAA-BBB", Status: identity.JoinPending, Filed: *f.now, Expires: f.now.Add(identity.JoinRequestTTL),
	}, *f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dir.Join(ctx, "100.0.0.9:1", code); !errors.Is(err, identity.ErrNameTaken) {
		t.Fatalf("join with an invite for a held name = %v, want ErrNameTaken", err)
	}
	if _, ok, _ := f.dir.Agent(ctx, "codex"); ok {
		t.Fatal("the held name was bound")
	}
}

// Filing refuses a name that is an agent, the name of a live invite, or held
// by another request; an expired invite does not block it.
func TestFilingNeedsNewNames(t *testing.T) {
	f := newJoinFixture(t)
	ctx := context.Background()
	code, err := f.dir.InviteKind(ctx, "100.0.0.1:1", "claude-code", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.dir.Join(ctx, "100.0.0.1:1", code); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dir.InviteKind(ctx, "100.0.0.1:1", "muse", ""); err != nil {
		t.Fatal(err)
	}
	other := identity.Node{ID: "nOTHER", Name: "other", Key: "nodekey:bb"}
	if _, err := f.dir.FileJoinRequest(ctx, other, []identity.RequestedAgent{{Name: "held"}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claude-code", "muse", "held"} {
		if _, err := f.dir.FileJoinRequest(ctx, newNode, []identity.RequestedAgent{{Name: "fresh"}, {Name: name}}); !errors.Is(err, identity.ErrNameTaken) {
			t.Errorf("filing for %s = %v, want ErrNameTaken", name, err)
		}
	}
	*f.now = f.now.Add(identity.InviteTTL + time.Second)
	if _, err := f.dir.FileJoinRequest(ctx, newNode, []identity.RequestedAgent{{Name: "muse"}}); err != nil {
		t.Fatalf("filing for a name whose invite expired: %v", err)
	}
}

// Filing records the node as WhoIs saw it and returns a fresh id and a
// matching code shaped like K7Q-4MD.
func TestFilingRecordsNode(t *testing.T) {
	f := newJoinFixture(t)
	r := f.file(t, "codex", "claude-code")
	if r.ID == "" || len(r.Code) != 7 || r.Code[3] != '-' {
		t.Fatalf("id %q code %q", r.ID, r.Code)
	}
	if r.NodeID != newNode.ID || r.NodeKey != newNode.Key || r.NodeName != newNode.Name || r.NodeUser != newNode.User || len(r.Tags) != 1 {
		t.Fatalf("request node = %+v", r)
	}
	if r.Status != identity.JoinPending || !r.Expires.Equal(f.now.Add(identity.JoinRequestTTL)) {
		t.Fatalf("status %s expires %v", r.Status, r.Expires)
	}
	if got, ok, err := f.store.JoinRequest(context.Background(), r.ID, *f.now); err != nil || !ok || got.Code != r.Code || len(got.Agents) != 2 {
		t.Fatalf("stored = %+v, %v, %v", got, ok, err)
	}
}

// Approval binds nothing; the claim from the recorded node binds every name
// and moves the request to delivered, after which it cannot be claimed again.
func TestClaimBindsEveryNameOnce(t *testing.T) {
	f := newJoinFixture(t)
	ctx := context.Background()
	r, err := f.dir.FileJoinRequest(ctx, newNode, []identity.RequestedAgent{{Name: "codex", Kind: "codex"}, {Name: "helper"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ApproveJoinRequest(ctx, r.ID, "local-admin", *f.now); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := f.dir.Agent(ctx, "codex"); ok {
		t.Fatal("approval bound a name")
	}
	got, err := f.dir.ClaimJoinRequest(ctx, r.ID, newNode)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != identity.JoinDelivered || len(got.Agents) != 2 {
		t.Fatalf("claim = %+v", got)
	}
	a, ok, _ := f.dir.Agent(ctx, "codex")
	if !ok || a.NodeID != newNode.ID || a.NodeName != newNode.Name || a.NodeUser != newNode.User || a.Kind != "codex" || !a.JoinedAt.Equal(*f.now) {
		t.Fatalf("codex = %+v, %v", a, ok)
	}
	if _, ok, _ := f.dir.Agent(ctx, "helper"); !ok {
		t.Fatal("helper not bound")
	}
	again, err := f.dir.ClaimJoinRequest(ctx, r.ID, newNode)
	if !errors.Is(err, identity.ErrJoinState) || again.Status != identity.JoinDelivered {
		t.Fatalf("second claim = %+v, %v", again, err)
	}
}

// A claim from another node is refused and changes nothing; the recorded
// node calling with a changed key voids the request.
func TestClaimIsBoundToTheRecordedNode(t *testing.T) {
	f := newJoinFixture(t)
	ctx := context.Background()
	r := f.file(t, "codex")
	if _, err := f.store.ApproveJoinRequest(ctx, r.ID, "local-admin", *f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dir.ClaimJoinRequest(ctx, r.ID, grokNode); !errors.Is(err, identity.ErrJoinWrongNode) {
		t.Fatalf("claim from another node = %v", err)
	}
	if got, _, _ := f.store.JoinRequest(ctx, r.ID, *f.now); got.Status != identity.JoinApproved {
		t.Fatalf("status after a wrong-node claim = %s", got.Status)
	}
	rekeyed := newNode
	rekeyed.Key = "nodekey:cc"
	got, err := f.dir.ClaimJoinRequest(ctx, r.ID, rekeyed)
	if !errors.Is(err, identity.ErrJoinState) || got.Status != identity.JoinVoided {
		t.Fatalf("claim with a changed key = %+v, %v", got, err)
	}
	if _, ok, _ := f.dir.Agent(ctx, "codex"); ok {
		t.Fatal("a voided request bound a name")
	}
}

// A claim before approval answers the status and binds nothing.
func TestClaimBeforeApproval(t *testing.T) {
	f := newJoinFixture(t)
	r := f.file(t, "codex")
	got, err := f.dir.ClaimJoinRequest(context.Background(), r.ID, newNode)
	if !errors.Is(err, identity.ErrJoinState) || got.Status != identity.JoinPending {
		t.Fatalf("claim while pending = %+v, %v", got, err)
	}
	if _, err := f.dir.ClaimJoinRequest(context.Background(), "nope", newNode); !errors.Is(err, identity.ErrJoinNotFound) {
		t.Fatalf("claim of an unknown id = %v", err)
	}
}

// When a name can no longer be joined at claim, nothing is bound, the
// request fails, and the error names the refusing name.
func TestClaimRefusedNameBindsNothing(t *testing.T) {
	f := newJoinFixture(t)
	ctx := context.Background()
	r := f.file(t, "codex", "helper")
	if _, err := f.store.ApproveJoinRequest(ctx, r.ID, "local-admin", *f.now); err != nil {
		t.Fatal(err)
	}
	// Written straight to the store: every directory path refuses the held
	// name, so this stands in for a binding that got past them.
	if err := f.store.PutAgent(ctx, identity.Agent{Name: "helper", NodeID: "nOTHER", NodeName: "other"}); err != nil {
		t.Fatal(err)
	}
	got, err := f.dir.ClaimJoinRequest(ctx, r.ID, newNode)
	var refused *identity.JoinRefusedError
	if !errors.As(err, &refused) || refused.Name != "helper" || !errors.Is(err, identity.ErrJoinRefused) || !errors.Is(err, identity.ErrNameTaken) {
		t.Fatalf("claim = %v", err)
	}
	if got.Status != identity.JoinFailed {
		t.Fatalf("status = %s", got.Status)
	}
	if _, ok, _ := f.dir.Agent(ctx, "codex"); ok {
		t.Fatal("codex bound although helper was refused")
	}
}

// A node carrying a guest cannot gain teammates through a request.
func TestClaimOntoGuestNodeRefused(t *testing.T) {
	f := newJoinFixture(t)
	ctx := context.Background()
	r := f.file(t, "codex")
	if _, err := f.store.ApproveJoinRequest(ctx, r.ID, "local-admin", *f.now); err != nil {
		t.Fatal(err)
	}
	if err := f.store.PutAgent(ctx, identity.Agent{Name: "friend", NodeID: newNode.ID, Guest: identity.GuestTerms{Until: f.now.Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dir.ClaimJoinRequest(ctx, r.ID, newNode); !errors.Is(err, identity.ErrGuestNode) {
		t.Fatalf("claim onto a guest's node = %v", err)
	}
	if _, ok, _ := f.dir.Agent(ctx, "codex"); ok {
		t.Fatal("codex bound beside a guest")
	}
}

// The live-invite lookup ignores expired invites.
func TestLiveInviteNamedIgnoresExpired(t *testing.T) {
	st := identity.NewMemoryStore()
	ctx := context.Background()
	now := time.Unix(1_790_000_000, 0)
	if err := st.PutInvite(ctx, identity.Invite{Code: "d1", Name: "muse", Expires: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.LiveInviteNamed(ctx, "muse", now); err != nil || !ok {
		t.Fatalf("live invite = %v, %v", ok, err)
	}
	if ok, _ := st.LiveInviteNamed(ctx, "muse", now.Add(2*time.Minute)); ok {
		t.Fatal("an expired invite counted as live")
	}
	if ok, _ := st.LiveInviteNamed(ctx, "codex", now); ok {
		t.Fatal("an invite for another name counted")
	}
}
