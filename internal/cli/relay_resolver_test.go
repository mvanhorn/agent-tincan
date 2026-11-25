package cli

import (
	"context"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/identity/identitytest"
)

var (
	_ identity.NodeLookup = tailnetResolver{}
	_ identity.PeerLister = tailnetResolver{}
)

// fakeTailnet is the tsnet resolver's tailnet side without a tailnet: the
// fake peer list plus stub URLs.
type fakeTailnet struct {
	*identitytest.Resolver
}

func (fakeTailnet) SelfURLs(context.Context, int) []string { return nil }
func (fakeTailnet) SelfNames(context.Context) []string     { return nil }

// The tsnet wrapper must see past itself to the tailnet: the directory
// unwraps one layer to tailnetResolver, which has to offer the optional
// interfaces itself. This is the production shape (WithVirtual around
// the tsnet resolver), where the device list and admin verdict read
// through both wrappers.
func TestTailnetResolverSeesTailnet(t *testing.T) {
	ctx := context.Background()
	who := identitytest.New(map[string]identity.Node{
		"100.0.5.1:1": {ID: "nT", Name: "tablet", User: "owner@example.com", Tags: []string{"tag:tincan-admin"}},
	})
	who.SetOnline("nT", true)
	dir := identity.NewDirectory(identity.NewMemoryStore(),
		identity.WithVirtual(tailnetResolver{fakeTailnet{who}, nil, nil}),
		identity.Config{AdminTag: "tag:tincan-admin"})

	facts, ok := dir.MachineFacts(ctx, "nT")
	if !ok || !facts.Admin || len(facts.Tags) != 1 || facts.Tags[0] != "tag:tincan-admin" {
		t.Fatalf("facts through wrappers: %+v %v", facts, ok)
	}
	peers, err := dir.Peers(ctx)
	if err != nil || len(peers) != 1 || peers[0].Name != "tablet" || !peers[0].Online {
		t.Fatalf("peers through wrappers: %v %v", peers, err)
	}
}
