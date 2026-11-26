package identity_test

import (
	"context"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/identity/identitytest"
)

// bareResolver implements only Resolver, none of the optional extensions.
type bareResolver struct{ identity.Resolver }

func testNodes() *identitytest.Resolver {
	return identitytest.New(map[string]identity.Node{
		"100.0.4.1:1": {ID: "nA", Name: "alpha", User: "u@example.com"},
	})
}

// The virtual wrapper must not hide the tailnet's optional interfaces:
// it delegates when the inner resolver implements them and reports
// absence the way a missing interface would.
func TestVirtualResolverDelegates(t *testing.T) {
	ctx := context.Background()

	full := identity.WithVirtual(testNodes())
	if lk, ok := full.(identity.NodeLookup); !ok {
		t.Fatal("wrapped full resolver should offer LookupNode")
	} else if n, found, err := lk.LookupNode(ctx, "nA"); err != nil || !found || n.Name != "alpha" {
		t.Fatalf("lookup through wrapper: %+v %v %v", n, found, err)
	}
	if pl, ok := full.(identity.PeerLister); !ok {
		t.Fatal("wrapped full resolver should offer Peers")
	} else if peers, err := pl.Peers(ctx); err != nil || len(peers) != 1 {
		t.Fatalf("peers through wrapper: %v %v", peers, err)
	}
	if st, ok := full.(identity.NodeStatus); !ok {
		t.Fatal("wrapped full resolver should offer NodeStatus")
	} else if _, _, err := st.NodeOnline(ctx, "nA"); err != nil {
		t.Fatalf("online through wrapper: %v", err)
	}

	bare := identity.WithVirtual(bareResolver{testNodes()})
	if lk, ok := bare.(identity.NodeLookup); !ok {
		t.Fatal("wrapper should always offer LookupNode")
	} else if _, found, err := lk.LookupNode(ctx, "nA"); err != nil || found {
		t.Fatalf("lookup without inner lookup: found=%v err=%v", found, err)
	}
	if pl, ok := bare.(identity.PeerLister); !ok {
		t.Fatal("wrapper should always offer Peers")
	} else if _, err := pl.Peers(ctx); err == nil {
		t.Fatal("peers without inner list should error")
	}
	if st, ok := bare.(identity.NodeStatus); !ok {
		t.Fatal("wrapper should always offer NodeStatus")
	} else if on, found, err := st.NodeOnline(ctx, "nA"); err != nil || on || found {
		t.Fatalf("online without inner status: %v %v %v", on, found, err)
	}
}
