package identity

import (
	"net/netip"
	"slices"
	"testing"

	"tailscale.com/ipn/ipnstate"
)

// The relay's Host allowlist names it every way a client can: MagicDNS full
// and short names and both tailnet IPs.
func TestSelfNames(t *testing.T) {
	got := selfNames(&ipnstate.PeerStatus{
		DNSName:      "tincan-relay.tail2b6977.ts.net.",
		TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.1"), netip.MustParseAddr("fd7a:115c:a1e0::1")},
	})
	want := []string{"tincan-relay.tail2b6977.ts.net", "tincan-relay", "100.64.0.1", "fd7a:115c:a1e0::1"}
	if !slices.Equal(got, want) {
		t.Fatalf("selfNames = %q, want %q", got, want)
	}
	if got := selfNames(nil); got != nil {
		t.Fatalf("selfNames(nil) = %q, want none", got)
	}
}
