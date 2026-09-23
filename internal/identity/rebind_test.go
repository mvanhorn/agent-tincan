package identity_test

import (
	"context"
	"errors"
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

// Agents joined before logins were recorded still re-admit.
func TestAgentWithoutRecordedLoginIsReadmitted(t *testing.T) {
	f := newRebindFixture(t, identity.Config{}, newBox)
	ctx := context.Background()
	a, _, _ := f.store.AgentByName(ctx, "instinct")
	a.NodeUser = ""
	f.store.PutAgent(ctx, a)
	if got, err := f.dir.Resolve(ctx, rebuiltAddr, ""); err != nil || got != "instinct" {
		t.Fatalf("resolve = %q, %v", got, err)
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
