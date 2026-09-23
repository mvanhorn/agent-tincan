package identity_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/identity/identitytest"
)

const (
	matt        = "mvanhorn@gmail.com"
	boxAddr     = "100.0.0.5:1"
	rebuiltAddr = "100.0.0.6:1"
)

// The e2b sandbox before and after a rebuild: same machine name, new node.
var (
	oldBox = identity.Node{ID: "nBOX", Name: "instinct", User: matt}
	newBox = identity.Node{ID: "nBOX2", Name: "instinct", User: matt}
)

type rebindFixture struct {
	fixture
	store *identity.MemoryStore
}

// newRebindFixture joins "instinct" (kind hermes) on oldBox and then
// "rebuilds" the machine: oldBox leaves the tailnet and the caller at
// rebuiltAddr is the new node.
func newRebindFixture(t *testing.T, cfg identity.Config, rebuilt identity.Node) rebindFixture {
	t.Helper()
	now := time.Unix(1_790_000_000, 0)
	who := identitytest.New(map[string]identity.Node{"100.0.0.1:1": macNode, boxAddr: oldBox, "100.0.0.4:1": museNode})
	st := identity.NewMemoryStore()
	cfg.Admins = []string{"macbook-pro-44"}
	cfg.Now = func() time.Time { return now }
	f := rebindFixture{fixture{dir: identity.NewDirectory(st, who, cfg), who: who, clock: &now}, st}
	ctx := context.Background()
	for _, j := range []struct{ name, addr string }{{"instinct", boxAddr}, {"muse", "100.0.0.4:1"}} {
		code, err := f.dir.InviteKind(ctx, "100.0.0.1:1", j.name, "hermes")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.dir.Join(ctx, j.addr, code); err != nil {
			t.Fatal(err)
		}
	}
	who.Remove(boxAddr)
	who.Set(rebuiltAddr, rebuilt)
	return f
}

func TestJoinRecordsLogin(t *testing.T) {
	f := newRebindFixture(t, identity.Config{}, newBox)
	if a := agentNamed(t, f.dir, "instinct"); a.NodeUser != matt || a.NodeName != "instinct" {
		t.Fatalf("joined agent = %+v, want login and machine name recorded", a)
	}
}

func TestRebuiltMachineIsReadmitted(t *testing.T) {
	f := newRebindFixture(t, identity.Config{}, newBox)
	res, err := f.dir.ResolveAgent(context.Background(), rebuiltAddr, "")
	if err != nil || res.Name != "instinct" {
		t.Fatalf("resolve rebuilt = %+v, %v", res, err)
	}
	if res.Rebind == nil || res.Rebind.Agent != "instinct" || res.Rebind.OldNode != "nBOX" || res.Rebind.NewNode != "nBOX2" {
		t.Fatalf("rebind = %+v", res.Rebind)
	}
	a := agentNamed(t, f.dir, "instinct")
	if a.NodeID != "nBOX2" || a.Kind != "hermes" || a.NodeUser != matt {
		t.Fatalf("after rebind = %+v", a)
	}
	// The next call is an ordinary attribution, not another rebind.
	if res, err := f.dir.ResolveAgent(context.Background(), rebuiltAddr, ""); err != nil || res.Rebind != nil {
		t.Fatalf("second resolve = %+v, %v", res, err)
	}
}

func TestRebuiltMachineWithDedupSuffixIsReadmitted(t *testing.T) {
	f := newRebindFixture(t, identity.Config{}, identity.Node{ID: "nBOX2", Name: "instinct-1", User: matt})
	if got, err := f.dir.Resolve(context.Background(), rebuiltAddr, "instinct"); err != nil || got != "instinct" {
		t.Fatalf("resolve instinct-1 = %q, %v", got, err)
	}
}

func TestNotReadmitted(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     identity.Config
		rebuilt identity.Node
		claim   string
		online  bool
	}{
		{name: "old node still online", rebuilt: newBox, online: true},
		{name: "tagged caller", rebuilt: identity.Node{ID: "nBOX2", Name: "instinct", User: matt, Tags: []string{"tag:agent"}}},
		{name: "different login", rebuilt: identity.Node{ID: "nBOX2", Name: "instinct", User: "someone@else.com"}},
		{name: "header names another agent", rebuilt: newBox, claim: "muse"},
		{name: "different machine name", rebuilt: identity.Node{ID: "nBOX2", Name: "instinct-bot", User: matt}},
		{name: "auto rebind disabled", cfg: identity.Config{NoAutoRebind: true}, rebuilt: newBox},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRebindFixture(t, tc.cfg, tc.rebuilt)
			if tc.online {
				f.who.Set(boxAddr, oldBox)
				f.who.SetOnline("nBOX", true)
			}
			_, err := f.dir.Resolve(context.Background(), rebuiltAddr, tc.claim)
			if !errors.Is(err, identity.ErrNotJoined) {
				t.Fatalf("want not joined, got %v", err)
			}
			if a := agentNamed(t, f.dir, "instinct"); a.NodeID != "nBOX" {
				t.Fatalf("instinct moved to %s", a.NodeID)
			}
			if a := agentNamed(t, f.dir, "muse"); a.NodeID != "nMUSE" {
				t.Fatalf("muse moved to %s", a.NodeID)
			}
		})
	}
}

// An old node that is still on the tailnet but offline is replaced.
func TestOfflineOldNodeIsReplaced(t *testing.T) {
	f := newRebindFixture(t, identity.Config{}, newBox)
	f.who.Set(boxAddr, oldBox)
	f.who.SetOnline("nBOX", false)
	if got, err := f.dir.Resolve(context.Background(), rebuiltAddr, ""); err != nil || got != "instinct" {
		t.Fatalf("resolve = %q, %v", got, err)
	}
}

// clearLogin makes name look like an agent joined before logins were
// recorded.
func (f rebindFixture) clearLogin(t *testing.T, name string) {
	t.Helper()
	ctx := context.Background()
	a, _, _ := f.store.AgentByName(ctx, name)
	a.NodeUser = ""
	if err := f.store.PutAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
}

// An agent joined before logins were recorded has no login to compare, so a
// rebuilt machine cannot take it over on the machine name alone.
func TestAgentWithoutRecordedLoginIsNotReadmitted(t *testing.T) {
	f := newRebindFixture(t, identity.Config{}, newBox)
	f.clearLogin(t, "instinct")
	for _, claim := range []string{"", "instinct"} {
		if _, err := f.dir.Resolve(context.Background(), rebuiltAddr, claim); !errors.Is(err, identity.ErrNotJoined) {
			t.Fatalf("claim %q: want not joined, got %v", claim, err)
		}
	}
	if a := agentNamed(t, f.dir, "instinct"); a.NodeID != "nBOX" {
		t.Fatalf("instinct moved to %s", a.NodeID)
	}
}

// One ordinary call from the agent's own node records its login, after
// which a rebuild re-admits it.
func TestAgentWithoutLoginGainsItOnNextCall(t *testing.T) {
	for _, claim := range []string{"", "instinct"} {
		t.Run("claim="+claim, func(t *testing.T) {
			f := newRebindFixture(t, identity.Config{}, newBox)
			ctx := context.Background()
			f.clearLogin(t, "instinct")
			f.who.Set(boxAddr, oldBox)
			if got, err := f.dir.Resolve(ctx, boxAddr, claim); err != nil || got != "instinct" {
				t.Fatalf("resolve from own node = %q, %v", got, err)
			}
			if a := agentNamed(t, f.dir, "instinct"); a.NodeUser != matt || a.NodeID != "nBOX" || a.Kind != "hermes" {
				t.Fatalf("after own call = %+v, want login recorded", a)
			}
			f.who.Remove(boxAddr)
			res, err := f.dir.ResolveAgent(ctx, rebuiltAddr, claim)
			if err != nil || res.Name != "instinct" || res.Rebind == nil {
				t.Fatalf("resolve rebuilt = %+v, %v", res, err)
			}
		})
	}
}

// A node's own agent is not handed to a nameless call while another agent
// from the same rebuilt machine still waits to be re-admitted: the call
// could be that other agent's first.
func TestNamelessCallIsAmbiguousWhileSiblingAwaitsReadmit(t *testing.T) {
	f := newRebindFixture(t, identity.Config{}, newBox)
	ctx := context.Background()
	f.store.PutAgent(ctx, identity.Agent{Name: "codex", NodeID: "nBOX", NodeName: "instinct", NodeUser: matt, Kind: "codex"})

	if res, err := f.dir.ResolveAgent(ctx, rebuiltAddr, "codex"); err != nil || res.Name != "codex" || res.Rebind == nil {
		t.Fatalf("header codex = %+v, %v", res, err)
	}
	_, err := f.dir.Resolve(ctx, rebuiltAddr, "")
	if !errors.Is(err, identity.ErrAgentAmbiguous) || !strings.Contains(err.Error(), "instinct") {
		t.Fatalf("no header while instinct awaits: want ambiguous naming instinct, got %v", err)
	}
	if a := agentNamed(t, f.dir, "instinct"); a.NodeID != "nBOX" {
		t.Fatalf("instinct should stay until it names itself, got %s", a.NodeID)
	}
	// codex naming itself is unaffected.
	if got, err := f.dir.Resolve(ctx, rebuiltAddr, "codex"); err != nil || got != "codex" {
		t.Fatalf("header codex again = %q, %v", got, err)
	}
	if res, err := f.dir.ResolveAgent(ctx, rebuiltAddr, "instinct"); err != nil || res.Name != "instinct" || res.Rebind == nil {
		t.Fatalf("header instinct = %+v, %v", res, err)
	}
}

// An agent on a live machine that only shares the base name does not make
// the caller's own agent ambiguous.
func TestNamelessCallIgnoresAgentOnLiveMachine(t *testing.T) {
	f := newRebindFixture(t, identity.Config{}, identity.Node{ID: "nBOX2", Name: "instinct-1", User: matt})
	ctx := context.Background()
	f.who.Set(boxAddr, oldBox)
	f.who.SetOnline("nBOX", true)
	f.store.PutAgent(ctx, identity.Agent{Name: "codex", NodeID: "nBOX2", NodeName: "instinct-1", NodeUser: matt})
	if got, err := f.dir.Resolve(ctx, rebuiltAddr, ""); err != nil || got != "codex" {
		t.Fatalf("resolve = %q, %v", got, err)
	}
}

// The recorded machine name is compared without its dedup suffix, so a
// machine rebuilt twice re-admits whichever suffix Tailscale hands out.
func TestReadmitAcrossDedupSuffixes(t *testing.T) {
	f := newRebindFixture(t, identity.Config{}, identity.Node{ID: "nBOX2", Name: "instinct-1", User: matt})
	ctx := context.Background()
	if got, err := f.dir.Resolve(ctx, rebuiltAddr, ""); err != nil || got != "instinct" {
		t.Fatalf("resolve instinct-1 = %q, %v", got, err)
	}
	prev := rebuiltAddr
	for i, name := range []string{"instinct", "instinct-2"} {
		addr := fmt.Sprintf("100.0.1.%d:1", i)
		f.who.Remove(prev)
		f.who.Set(addr, identity.Node{ID: "nBOX" + name, Name: name, User: matt})
		res, err := f.dir.ResolveAgent(ctx, addr, "")
		if err != nil || res.Name != "instinct" || res.Rebind == nil {
			t.Fatalf("resolve %s = %+v, %v", name, res, err)
		}
		if a := agentNamed(t, f.dir, "instinct"); a.NodeName != name {
			t.Fatalf("after rebind to %s = %+v", name, a)
		}
		prev = addr
	}
}

// failingStatus stands in for a LocalAPI that cannot answer NodeOnline.
type failingStatus struct{ *identitytest.Resolver }

func (failingStatus) NodeOnline(context.Context, string) (bool, bool, error) {
	return false, false, errors.New("localapi: connection refused")
}

// A failed old-node check is not a refusal: the caller may well be the
// agent, so the error says the check failed rather than not joined.
func TestReadmitNodeCheckFailure(t *testing.T) {
	f := newRebindFixture(t, identity.Config{}, newBox)
	dir := identity.NewDirectory(f.store, failingStatus{f.who}, identity.Config{})
	_, err := dir.Resolve(context.Background(), rebuiltAddr, "")
	if !errors.Is(err, identity.ErrRebindCheckFailed) || errors.Is(err, identity.ErrNotJoined) {
		t.Fatalf("want rebind check failed, got %v", err)
	}
	if a := agentNamed(t, dir, "instinct"); a.NodeID != "nBOX" {
		t.Fatalf("instinct moved to %s", a.NodeID)
	}
}

func TestTwoAgentsOnRebuiltMachine(t *testing.T) {
	f := newRebindFixture(t, identity.Config{}, newBox)
	ctx := context.Background()
	// codex also lived on the old box.
	f.store.PutAgent(ctx, identity.Agent{Name: "codex", NodeID: "nBOX", NodeName: "instinct", NodeUser: matt, Kind: "codex"})

	if _, err := f.dir.Resolve(ctx, rebuiltAddr, ""); !errors.Is(err, identity.ErrAgentAmbiguous) {
		t.Fatalf("no header: want ambiguous, got %v", err)
	}
	res, err := f.dir.ResolveAgent(ctx, rebuiltAddr, "codex")
	if err != nil || res.Name != "codex" || res.Rebind == nil {
		t.Fatalf("header codex = %+v, %v", res, err)
	}
	if a := agentNamed(t, f.dir, "instinct"); a.NodeID != "nBOX" {
		t.Fatalf("instinct should stay until it asks, got %s", a.NodeID)
	}
	// The second agent heals the same way on its first call.
	if got, err := f.dir.Resolve(ctx, rebuiltAddr, "instinct"); err != nil || got != "instinct" {
		t.Fatalf("header instinct = %q, %v", got, err)
	}
	if a := agentNamed(t, f.dir, "instinct"); a.NodeID != "nBOX2" {
		t.Fatalf("instinct = %+v", a)
	}
}

// The gateway's virtual wrapper must not hide an online old node.
func TestVirtualWrapperForwardsNodeStatus(t *testing.T) {
	who := identitytest.New(map[string]identity.Node{boxAddr: oldBox})
	who.SetOnline("nBOX", true)
	st, ok := identity.WithVirtual(who).(identity.NodeStatus)
	if !ok {
		t.Fatal("WithVirtual should implement NodeStatus")
	}
	if online, found, err := st.NodeOnline(context.Background(), "nBOX"); err != nil || !online || !found {
		t.Fatalf("NodeOnline = %v, %v, %v", online, found, err)
	}
}

// hookResolver runs hook inside NodeOnline, standing in for a request that
// changes the directory while readmit waits on the resolver.
type hookResolver struct {
	*identitytest.Resolver
	hook func()
}

func (h *hookResolver) NodeOnline(ctx context.Context, id string) (bool, bool, error) {
	if h.hook != nil {
		hook := h.hook
		h.hook = nil
		hook()
	}
	return h.Resolver.NodeOnline(ctx, id)
}

// readmit checks the old node without holding the directory lock, so it
// commits only if the agent is still where it was checked.
func TestReadmitRechecksBindingAfterNodeCheck(t *testing.T) {
	for _, tc := range []struct {
		name     string
		moveTo   identity.Node
		wantErr  error
		wantName string
		wantNode string
	}{
		// Another request re-admitted the same new node first.
		{name: "readmitted concurrently", moveTo: newBox, wantName: "instinct", wantNode: "nBOX2"},
		// The agent was re-joined on some other machine meanwhile.
		{name: "moved elsewhere", moveTo: identity.Node{ID: "nOTHER", Name: "other-box", User: matt}, wantErr: identity.ErrNotJoined, wantNode: "nOTHER"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newRebindFixture(t, identity.Config{}, newBox)
			h := &hookResolver{Resolver: f.who}
			dir := identity.NewDirectory(f.store, h, identity.Config{})
			h.hook = func() {
				a, _, _ := f.store.AgentByName(ctx, "instinct")
				a.NodeID, a.NodeName, a.NodeUser = tc.moveTo.ID, tc.moveTo.Name, tc.moveTo.User
				f.store.PutAgent(ctx, a)
			}
			res, err := dir.ResolveAgent(ctx, rebuiltAddr, "")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("want %v, got %+v, %v", tc.wantErr, res, err)
				}
			} else if err != nil || res.Name != tc.wantName || res.Rebind != nil {
				t.Fatalf("resolve = %+v, %v; want %s with no rebind of its own", res, err, tc.wantName)
			}
			if a := agentNamed(t, dir, "instinct"); a.NodeID != tc.wantNode {
				t.Fatalf("instinct on %s, want %s", a.NodeID, tc.wantNode)
			}
		})
	}
}
