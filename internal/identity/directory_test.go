package identity_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/identity/identitytest"
)

// Only an admin device names or clears a task host. A refused attempt leaves
// the mark as it was, an unknown agent is reported, and a guest is never
// named.
func TestSetTaskHostIsAdminOnly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.dir.Join(ctx, "100.0.0.2:1", f.invite(t, "grokbot"))
	if err := f.dir.SetTaskHost(ctx, "100.0.0.2:1", "grokbot", true); !errors.Is(err, identity.ErrNotAdmin) {
		t.Fatalf("an agent named itself a task host: %v", err)
	}
	if a := agentNamed(t, f.dir, "grokbot"); a.TaskHost {
		t.Fatalf("mark after a refused set = %+v", a)
	}
	if err := f.dir.SetTaskHost(ctx, "100.0.0.1:1", "grokbot", true); err != nil {
		t.Fatal(err)
	}
	if a := agentNamed(t, f.dir, "grokbot"); !a.TaskHost {
		t.Fatalf("mark after an admin set = %+v", a)
	}
	if err := f.dir.SetTaskHost(ctx, "100.0.0.2:1", "grokbot", false); !errors.Is(err, identity.ErrNotAdmin) {
		t.Fatalf("an agent cleared a task host: %v", err)
	}
	if a := agentNamed(t, f.dir, "grokbot"); !a.TaskHost {
		t.Fatalf("mark after a refused clear = %+v", a)
	}
	if err := f.dir.SetTaskHost(ctx, identity.LocalAdmin, "grokbot", false); err != nil {
		t.Fatal(err)
	}
	if a := agentNamed(t, f.dir, "grokbot"); a.TaskHost {
		t.Fatalf("mark after an admin clear = %+v", a)
	}
	if err := f.dir.SetTaskHost(ctx, identity.LocalAdmin, "nobody", true); !errors.Is(err, identity.ErrUnknownAgent) {
		t.Fatalf("unknown agent: %v", err)
	}
}

func TestSetTaskHostRefusesAGuest(t *testing.T) {
	ctx := context.Background()
	who := identitytest.New(map[string]identity.Node{"100.0.0.1:1": macNode})
	dir := identity.NewDirectory(identity.NewMemoryStore(), identity.WithVirtual(who), identity.Config{Admins: []string{"macbook-pro-44"}})
	if err := dir.BindVirtualGuest(ctx, "friend", identity.GuestTerms{Until: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := dir.SetTaskHost(ctx, "100.0.0.1:1", "friend", true); err == nil || !strings.Contains(err.Error(), "friend is a guest") {
		t.Fatalf("a guest was named a task host: %v", err)
	}
	if a := agentNamed(t, dir, "friend"); a.TaskHost {
		t.Fatalf("guest = %+v", a)
	}
}

// Redeeming an invite for a task host's name clears the mark and says so,
// whether the join lands on another machine or on the one the host ran on.
// A join for a name that was not a host reports no clear.
func TestJoinClearsTaskHostAndReportsIt(t *testing.T) {
	for name, addr := range map[string]string{"on another machine": "100.0.0.4:1", "on the same machine": "100.0.0.2:1"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			first, err := f.dir.JoinAgent(ctx, "100.0.0.2:1", f.invite(t, "grokbot"))
			if err != nil || first.Name != "grokbot" || first.TaskHostCleared {
				t.Fatalf("first join = %+v, %v", first, err)
			}
			if err := f.dir.SetTaskHost(ctx, "100.0.0.1:1", "grokbot", true); err != nil {
				t.Fatal(err)
			}
			again, err := f.dir.JoinAgent(ctx, addr, f.invite(t, "grokbot"))
			if err != nil || again.Name != "grokbot" || !again.TaskHostCleared {
				t.Fatalf("join over a host = %+v, %v", again, err)
			}
			if a := agentNamed(t, f.dir, "grokbot"); a.TaskHost {
				t.Fatalf("agent after the join = %+v", a)
			}
			third, err := f.dir.JoinAgent(ctx, addr, f.invite(t, "grokbot"))
			if err != nil || third.TaskHostCleared {
				t.Fatalf("join over an unmarked agent = %+v, %v", third, err)
			}
		})
	}
}

// The login recorded on an agent's first call from its own node rewrites the
// agent's row, and the row keeps its task host mark.
func TestLoginBackfillKeepsTaskHost(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.who.Set("100.0.0.2:1", identity.Node{ID: grokNode.ID, Name: grokNode.Name})
	if _, err := f.dir.Join(ctx, "100.0.0.2:1", f.invite(t, "grokbot")); err != nil {
		t.Fatal(err)
	}
	if err := f.dir.SetTaskHost(ctx, "100.0.0.1:1", "grokbot", true); err != nil {
		t.Fatal(err)
	}
	f.who.Set("100.0.0.2:1", grokNode)
	res, err := f.dir.ResolveAgent(ctx, "100.0.0.2:1", "")
	if err != nil || res.Name != "grokbot" || res.Rebind != nil {
		t.Fatalf("resolve = %+v, %v", res, err)
	}
	if a := agentNamed(t, f.dir, "grokbot"); a.NodeUser != grokNode.User || !a.TaskHost {
		t.Fatalf("agent after the backfill = %+v, want the login recorded and the mark kept", a)
	}
}

// Re-admitting a rebuilt machine moves the agent to a new node, which clears
// its task host mark, and the rebind says so. A rebind of an agent that was
// not a host reports no clear.
func TestRebindClearsTaskHostAndReportsIt(t *testing.T) {
	for _, host := range []bool{true, false} {
		f := newRebindFixture(t, identity.Config{}, newBox)
		ctx := context.Background()
		if host {
			if err := f.dir.SetTaskHost(ctx, "100.0.0.1:1", "instinct", true); err != nil {
				t.Fatal(err)
			}
		}
		res, err := f.dir.ResolveAgent(ctx, rebuiltAddr, "")
		if err != nil || res.Rebind == nil || res.Rebind.Agent != "instinct" {
			t.Fatalf("host %v: resolve rebuilt = %+v, %v", host, res, err)
		}
		if res.Rebind.TaskHostCleared != host {
			t.Fatalf("host %v: rebind reported cleared = %v", host, res.Rebind.TaskHostCleared)
		}
		if a := agentNamed(t, f.dir, "instinct"); a.NodeID != "nBOX2" || a.TaskHost {
			t.Fatalf("host %v: agent after the rebind = %+v", host, a)
		}
	}
}

// Binding a name to the gateway clears a task host mark it carried, both
// when the bind takes the name from a machine and when the name was on the
// gateway already and is connected again.
func TestVirtualBindClearsTaskHost(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.dir.Join(ctx, "100.0.0.2:1", f.invite(t, "grokbot"))
	if err := f.dir.SetTaskHost(ctx, "100.0.0.1:1", "grokbot", true); err != nil {
		t.Fatal(err)
	}
	if err := f.dir.BindVirtual(ctx, "grokbot", true); err != nil {
		t.Fatal(err)
	}
	if a := agentNamed(t, f.dir, "grokbot"); a.TaskHost {
		t.Fatalf("agent after a bind from a machine = %+v", a)
	}
	if err := f.dir.SetTaskHost(ctx, "100.0.0.1:1", "grokbot", true); err != nil {
		t.Fatal(err)
	}
	if err := f.dir.BindVirtual(ctx, "grokbot", false); err != nil {
		t.Fatal(err)
	}
	if a := agentNamed(t, f.dir, "grokbot"); a.TaskHost {
		t.Fatalf("agent after a second bind = %+v", a)
	}
}
