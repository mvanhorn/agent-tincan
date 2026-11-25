package identity

import (
	"net/netip"
	"slices"
	"testing"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// WhoIs reports the node key, tailnet addresses, full MagicDNS name and
// whether the node is shared in from another tailnet.
func TestWhoIsNodeFields(t *testing.T) {
	k := key.NewNode().Public()
	addrs := []netip.Prefix{netip.MustParsePrefix("100.64.0.9/32"), netip.MustParsePrefix("fd7a:115c:a1e0::9/128")}
	res := &apitype.WhoIsResponse{
		Node:        &tailcfg.Node{StableID: "nNEW", Name: "new-mac.tail2b6977.ts.net.", Key: k, Addresses: addrs, Tags: []string{"tag:agents"}},
		UserProfile: &tailcfg.UserProfile{LoginName: "owner@example.com"},
	}
	n := whoIsNode(res)
	want := []netip.Addr{netip.MustParseAddr("100.64.0.9"), netip.MustParseAddr("fd7a:115c:a1e0::9")}
	if n.ID != "nNEW" || n.Name != "new-mac" || n.DNSName != "new-mac.tail2b6977.ts.net" || n.Key != k.String() || !slices.Equal(n.Addrs, want) || n.User != "owner@example.com" || n.SharedIn {
		t.Fatalf("node = %+v", n)
	}

	shared := res.Node.Clone()
	shared.Sharer = 42
	if !whoIsNode(&apitype.WhoIsResponse{Node: shared}).SharedIn {
		t.Fatal("a node with a sharer is not marked shared in")
	}
	sharee := res.Node.Clone()
	sharee.Hostinfo = (&tailcfg.Hostinfo{ShareeNode: true}).View()
	if !whoIsNode(&apitype.WhoIsResponse{Node: sharee}).SharedIn {
		t.Fatal("a sharee node is not marked shared in")
	}
}

// LookupNode reports the same fields from the peer list.
func TestPeerNodeFields(t *testing.T) {
	k := key.NewNode().Public()
	st := &ipnstate.Status{User: map[tailcfg.UserID]tailcfg.UserProfile{7: {LoginName: "owner@example.com"}}}
	p := &ipnstate.PeerStatus{ID: "nNEW", DNSName: "new-mac.tail2b6977.ts.net.", PublicKey: k, UserID: 7, TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.9")}}
	n := peerNode(st, p)
	if n.ID != "nNEW" || n.Name != "new-mac" || n.DNSName != "new-mac.tail2b6977.ts.net" || n.Key != k.String() || len(n.Addrs) != 1 || n.User != "owner@example.com" || n.SharedIn {
		t.Fatalf("node = %+v", n)
	}
	p.ShareeNode = true
	if !peerNode(st, p).SharedIn {
		t.Fatal("a sharee peer is not marked shared in")
	}
	p.ShareeNode, p.AltSharerUserID = false, 9
	if !peerNode(st, p).SharedIn {
		t.Fatal("a peer with a sharer is not marked shared in")
	}
}
